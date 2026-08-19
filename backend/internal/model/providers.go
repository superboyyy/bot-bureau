package model

import (
	"botbureau/backend/internal/config"
	"botbureau/backend/internal/secret"

	"botbureau/backend/internal/i18n"

	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// ---- Unified abstraction: each bot binds to one Provider; each chat (group chat/DM) gets one Session ----

type ToolDef struct {
	Name        string
	Description string
	Properties  map[string]any // the properties of a JSON Schema
	Required    []string

	// SchemaExtras holds the remaining root-level keywords of the argument schema ($defs, oneOf,
	// additionalProperties, ...). A plugin tool's schema is not ours to author, and dropping these
	// points someone else's $ref at nothing.
	SchemaExtras map[string]any
}

// toolParams assembles the parameters object for the OpenAI-shaped APIs: the fixed
// type/properties/required plus the schema's remaining keywords. Keys in extras may not overwrite those
// three fixed entries, or one carelessly written plugin could reshape the argument schema entirely.
func toolParams(t ToolDef) map[string]any {
	params := map[string]any{"type": "object", "properties": t.Properties, "required": t.Required}
	for k, v := range t.SchemaExtras {
		if _, fixed := params[k]; fixed {
			continue
		}
		params[k] = v
	}
	return params
}

type ToolCall struct {
	ID    string
	Name  string
	Input map[string]any
}

type ToolResult struct {
	ID      string
	Content string
	IsError bool

	// Images returned by the tool (only plugins produce any). This is an extra field rather than a
	// Content turned into a block list, because a dozen built-in tools return nothing but text and
	// changing everyone's return type for one source is a poor trade.
	Images []ResultImage
}

type ResultImage struct {
	MIME   string
	Base64 string
}

type StepResult struct {
	StopReason string // end_turn | tool_use | max_tokens | pause_turn | refusal
	Texts      []string
	Notes      []string // server-side tool activity (e.g. web_search)
	ToolCalls  []ToolCall
}

// Session holds the history of one conversation (in the provider's native shape) and guarantees turn-level rollback and safe trimming.
type Session interface {
	MarkTurn() // mark the turn start (call before AddUser)
	Rollback() // roll back to turn start (on refusal)

	// AddUser appends a user/background message. images are those the user attached to it; the
	// overwhelming majority of messages have none, hence the variadic form — not one of the dozen call
	// sites has to change.
	AddUser(text string, images ...ResultImage)
	AddToolResults(rs []ToolResult)
	Step(ctx context.Context, system string, tools []ToolDef, includeWeb bool) (StepResult, error)

	// Trim drops oldest complete turns until the history fits maxMessages and maxChars.
	// maxChars <= 0 means no character budget. Cuts stay on user-turn boundaries; a compact
	// note is inserted at the new head so the model knows what disappeared.
	Trim(maxMessages, maxChars int)
	Snapshot() json.RawMessage
	Restore(json.RawMessage) bool
}

type Provider interface {
	NewSession() Session
	SupportsWebTools() bool
	Label() string
}

func BuildProvider(c config.BotConfig, ks *secret.KeyStore, xai *secret.XaiOAuth, chatgpt *secret.ChatGPTOAuth) (Provider, error) {
	switch strings.ToLower(strings.TrimSpace(c.Provider)) {
	case "":
		return &unsetProvider{}, nil
	case "anthropic":
		model := c.Model
		if model == "" {
			model = config.DefaultAnthropicModel
		}
		keyEnv := c.APIKeyEnv
		if keyEnv == "" {
			keyEnv = "ANTHROPIC_API_KEY"
		}
		return newAnthropicProvider(model, keyEnv, c.BaseURL, ks, c.Effort), nil
	case "openai", "openai-compatible", "openai_compatible":
		if strings.TrimSpace(c.Model) == "" {
			return nil, errors.New(i18n.T("The openai-compatible provider requires a model"))
		}
		return newOpenAIProvider(c.Model, c.BaseURL, c.APIKeyEnv, c.Auth, ks, xai, chatgpt, c.Effort), nil
	case "fake":

		// An echo model that needs no API key, for offline trials/tests.
		return &fakeProvider{}, nil
	default:
		return nil, fmt.Errorf(i18n.T("Unknown provider: %q (supported: anthropic / openai / fake)"), c.Provider)
	}
}

// ---- Quota/balance errors: the user must be alerted explicitly, distinct from ordinary rate limiting ----

type QuotaError struct{ Msg string }

func (e *QuotaError) Error() string { return e.Msg }

// classifyQuota reports whether this is a "balance/quota exhausted" error; if so it returns a user-facing message.
func classifyQuota(status int, label, msg string) string {
	lower := strings.ToLower(msg)
	hit := status == 402 ||
		strings.Contains(lower, "credit balance") ||
		strings.Contains(lower, "insufficient_quota") ||
		strings.Contains(lower, "insufficient quota") ||
		strings.Contains(lower, "exceeded your current quota") ||
		strings.Contains(lower, "billing") ||

		// These are not our strings but substrings of vendor error text: Chinese vendors (DeepSeek,
		// Kimi) report an exhausted balance in Chinese, and what must be matched is exactly what
		// they send back, so translating it would defeat the purpose.
		strings.Contains(lower, "余额")
	if !hit {
		return ""
	}
	detail := msg
	if len(detail) > 160 {
		detail = detail[:160] + "…"
	}
	return fmt.Sprintf(i18n.T("%s is out of API credit/quota, so bots on that model can't work for now. Top up on the provider's platform or check your usage plan. Original message: %s"), label, detail)
}

// ---- Generic "safe cut point" bookkeeping, shared by the three implementations ----

type cutTracker struct {
	mark int

	// Start indices of full user turns; trimming may only cut at these positions.
	cuts []int

	// Latest compact note, persisted so a restart does not look like the dropped turns vanished silently.
	note string
}

func (t *cutTracker) markTurn(historyLen int) { t.mark = historyLen }

func (t *cutTracker) addCut(historyLen int) { t.cuts = append(t.cuts, historyLen) }

func (t *cutTracker) rollbackTo() int {
	kept := t.cuts[:0]
	for _, c := range t.cuts {
		if c < t.mark {
			kept = append(kept, c)
		}
	}
	t.cuts = kept
	return t.mark
}

// trimPoint returns the index from which history should be kept, or 0 when there is no suitable cut point.
// It still rebases the tracker, which is what TestCutTrackerTrim exercises.
func (t *cutTracker) trimPoint(historyLen, limit int) int {
	p := t.pickTrim(historyLen, limit, 0, nil)
	if p > 0 {
		t.rebase(p)
	}
	return p
}

func (t *cutTracker) rebase(offset int) {
	kept := t.cuts[:0]
	for _, c := range t.cuts {
		if c >= offset {
			kept = append(kept, c-offset)
		}
	}
	t.cuts = kept
	if t.mark >= offset {
		t.mark -= offset
	} else {
		t.mark = 0
	}
}

// ---- Anthropic (official Go SDK, beta endpoint: server-side fallback + web tools) ----

type anthropicProvider struct {
	model   string
	keyEnv  string
	baseURL string
	ks      *secret.KeyStore

	mu        sync.Mutex
	client    anthropic.Client
	clientKey string
	built     bool
	effort    string
}

func newAnthropicProvider(model, keyEnv, baseURL string, ks *secret.KeyStore, effort string) *anthropicProvider {
	return &anthropicProvider{model: model, keyEnv: keyEnv, baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"), ks: ks, effort: effort}
}

// getClient lazily builds/rebuilds the client against the current key: a key changed in the UI takes effect immediately,
// and when no key is stored it falls back to the SDK's default resolution (environment variables / the `ant auth login` profile).
func (p *anthropicProvider) getClient() anthropic.Client {
	key := ""
	if p.ks != nil {
		key = p.ks.Get(p.keyEnv)
	}
	cache := key + "\n" + p.baseURL
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.built || cache != p.clientKey {
		var opts []option.RequestOption
		if key != "" {
			opts = append(opts, option.WithAPIKey(key))
		}
		if p.baseURL != "" {
			opts = append(opts, option.WithBaseURL(p.baseURL))
		}
		p.client = anthropic.NewClient(opts...)
		p.clientKey = cache
		p.built = true
	}
	return p.client
}

func (p *anthropicProvider) SupportsWebTools() bool { return true }
func (p *anthropicProvider) Label() string          { return "anthropic:" + p.model }
func (p *anthropicProvider) NewSession() Session    { return &anthropicSession{p: p} }

type anthropicSession struct {
	p       *anthropicProvider
	history []anthropic.BetaMessageParam
	tracker cutTracker
}

func (s *anthropicSession) MarkTurn() { s.tracker.markTurn(len(s.history)) }

func (s *anthropicSession) Rollback() { s.history = s.history[:s.tracker.rollbackTo()] }

func (s *anthropicSession) AddUser(text string, images ...ResultImage) {
	s.tracker.addCut(len(s.history))
	blocks := make([]anthropic.BetaContentBlockParamUnion, 0, len(images)+1)
	blocks = append(blocks, anthropic.NewBetaTextBlock(text))
	for _, img := range images {
		blocks = append(blocks, anthropic.BetaContentBlockParamUnion{
			OfImage: &anthropic.BetaImageBlockParam{
				Source: anthropic.BetaImageBlockParamSourceUnion{
					OfBase64: &anthropic.BetaBase64ImageSourceParam{
						Data:      img.Base64,
						MediaType: anthropic.BetaBase64ImageSourceMediaType(img.MIME),
					},
				},
			},
		})
	}
	s.history = append(s.history, anthropic.NewBetaUserMessage(blocks...))
}

func (s *anthropicSession) AddToolResults(rs []ToolResult) {
	blocks := make([]anthropic.BetaContentBlockParamUnion, 0, len(rs))
	for _, r := range rs {
		if len(r.Images) == 0 {
			blocks = append(blocks, anthropic.NewBetaToolResultBlock(r.ID, r.Content, r.IsError))
			continue
		}

		// Images go straight into the tool_result's content blocks, which is where the spec puts them: the
		// model sees "this tool returned this image" rather than a separate message pretending to be from
		// the user.
		content := make([]anthropic.BetaToolResultBlockParamContentUnion, 0, len(r.Images)+1)
		if strings.TrimSpace(r.Content) != "" {
			content = append(content, anthropic.BetaToolResultBlockParamContentUnion{
				OfText: &anthropic.BetaTextBlockParam{Text: r.Content},
			})
		}
		for _, img := range r.Images {
			content = append(content, anthropic.BetaToolResultBlockParamContentUnion{
				OfImage: &anthropic.BetaImageBlockParam{
					Source: anthropic.BetaImageBlockParamSourceUnion{
						OfBase64: &anthropic.BetaBase64ImageSourceParam{
							Data:      img.Base64,
							MediaType: anthropic.BetaBase64ImageSourceMediaType(img.MIME),
						},
					},
				},
			})
		}
		blocks = append(blocks, anthropic.BetaContentBlockParamUnion{
			OfToolResult: &anthropic.BetaToolResultBlockParam{
				ToolUseID: r.ID,
				Content:   content,
				IsError:   anthropic.Bool(r.IsError),
			},
		})
	}
	s.history = append(s.history, anthropic.NewBetaUserMessage(blocks...))
}

func (s *anthropicSession) Trim(maxMessages, maxChars int) {
	applyTrim(&s.history, &s.tracker, maxMessages, maxChars, func(note string) anthropic.BetaMessageParam {
		return anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(note))
	}, sketchesJSON[anthropic.BetaMessageParam], charsFromJSON[anthropic.BetaMessageParam])
}

type sessionDisk struct {
	Kind    string          `json:"kind"`
	History json.RawMessage `json:"history"`
	Cuts    []int           `json:"cuts"`
	Mark    int             `json:"mark"`
	Note    string          `json:"note,omitempty"`
}

func packSession(kind string, history any, t cutTracker) json.RawMessage {
	h, err := json.Marshal(history)
	if err != nil {
		return nil
	}
	raw, err := json.Marshal(sessionDisk{Kind: kind, History: h, Cuts: t.cuts, Mark: t.mark, Note: t.note})
	if err != nil {
		return nil
	}
	return raw
}

func unpackSession(raw json.RawMessage, kind string, history any) (cutTracker, bool) {
	var d sessionDisk
	if json.Unmarshal(raw, &d) != nil || d.Kind != kind {
		return cutTracker{}, false
	}
	if json.Unmarshal(d.History, history) != nil {
		return cutTracker{}, false
	}
	return cutTracker{cuts: d.Cuts, mark: d.Mark, note: d.Note}, true
}

func (s *anthropicSession) Snapshot() json.RawMessage {
	return packSession("anthropic", s.history, s.tracker)
}

func (s *anthropicSession) Restore(raw json.RawMessage) bool {
	var hist []anthropic.BetaMessageParam
	t, ok := unpackSession(raw, "anthropic", &hist)
	if !ok {
		return false
	}
	s.history = hist
	s.tracker = t
	return true
}

func (s *anthropicSession) Step(ctx context.Context, system string, tools []ToolDef, includeWeb bool) (StepResult, error) {
	var toolUnions []anthropic.BetaToolUnionParam
	for _, t := range tools {
		toolUnions = append(toolUnions, anthropic.BetaToolUnionParam{
			OfTool: &anthropic.BetaToolParam{
				Name:        t.Name,
				Description: anthropic.String(t.Description),
				InputSchema: anthropic.BetaToolInputSchemaParam{
					Properties: t.Properties,
					Required:   t.Required,

					// The SDK's schema param names only properties/required; the rest ride in ExtraFields
					ExtraFields: t.SchemaExtras,
				},
			},
		})
	}
	if includeWeb {
		toolUnions = append(toolUnions,
			anthropic.BetaToolUnionParam{OfWebSearchTool20260209: &anthropic.BetaWebSearchTool20260209Param{}},
			anthropic.BetaToolUnionParam{OfWebFetchTool20260209: &anthropic.BetaWebFetchTool20260209Param{}},
		)
	}
	client := s.p.getClient()
	params := anthropic.BetaMessageNewParams{
		Model:     anthropic.Model(s.p.model),
		MaxTokens: config.MaxTokens,
		System:    []anthropic.BetaTextBlockParam{{Text: system}},
		Messages:  s.history,
		Tools:     toolUnions,

		// Opus 5's safety classifiers may refuse; enable server-side fallback ("default" auto-selects a continuation model per refusal category)
		Betas:     []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01},
		Fallbacks: anthropic.BetaFallbacksParamOfDefault(),
	}

	// The thinking parameter travels only when a tier was chosen: an unset bot keeps the vendor default,
	// and the field never reaches a model that does not know it (which would be a plain 400).
	if budget := config.ThinkingBudget(s.p.effort); budget > 0 {
		params.Thinking = anthropic.BetaThinkingConfigParamOfEnabled(budget)
	}
	resp, err := client.Beta.Messages.New(ctx, params)
	if err != nil {
		return StepResult{}, humanizeAnthropicErr(err)
	}
	s.history = append(s.history, resp.ToParam())

	res := StepResult{StopReason: string(resp.StopReason)}
	if res.StopReason == "" {
		res.StopReason = "end_turn"
	}
	for _, block := range resp.Content {
		switch v := block.AsAny().(type) {
		case anthropic.BetaTextBlock:
			if strings.TrimSpace(v.Text) != "" {
				res.Texts = append(res.Texts, v.Text)
			}
		case anthropic.BetaToolUseBlock:
			input := map[string]any{}
			_ = json.Unmarshal([]byte(v.JSON.Input.Raw()), &input)
			res.ToolCalls = append(res.ToolCalls, ToolCall{ID: v.ID, Name: v.Name, Input: input})
		case anthropic.BetaServerToolUseBlock:
			res.Notes = append(res.Notes, string(v.Name))
		}
	}
	return res, nil
}

func humanizeAnthropicErr(err error) error {
	var apierr *anthropic.Error
	if errors.As(err, &apierr) {
		if q := classifyQuota(apierr.StatusCode, "Anthropic", err.Error()); q != "" {
			return &QuotaError{Msg: q}
		}
		switch apierr.StatusCode {
		case 401:
			return errors.New(i18n.T("Anthropic API authentication failed: set an API key or run `ant auth login` first"))
		case 429:
			return errors.New(i18n.T("Anthropic API rate limited (429), try again later"))
		default:
			return fmt.Errorf(i18n.T("Anthropic API error (%d): %v"), apierr.StatusCode, err)
		}
	}
	return fmt.Errorf(i18n.T("Can't reach the Anthropic API: %v"), err)
}

// ---- OpenAI-compatible endpoints (OpenAI / xAI Grok / DeepSeek / Kimi / Ollama ...), plain net/http ----

type openAIProvider struct {
	model   string
	baseURL string
	keyEnv  string
	auth    string
	ks      *secret.KeyStore
	xai     *secret.XaiOAuth
	chatgpt *secret.ChatGPTOAuth
	httpc   *http.Client
	effort  string
}

func newOpenAIProvider(model, baseURL, keyEnv, auth string, ks *secret.KeyStore, xai *secret.XaiOAuth, chatgpt *secret.ChatGPTOAuth, effort string) *openAIProvider {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	if keyEnv == "" {
		keyEnv = "OPENAI_API_KEY"
	}
	return &openAIProvider{
		model:   model,
		effort:  effort,
		baseURL: strings.TrimRight(baseURL, "/"),
		keyEnv:  keyEnv,
		auth:    auth,
		ks:      ks,
		xai:     xai,
		chatgpt: chatgpt,
		httpc:   &http.Client{Timeout: 300 * time.Second},
	}
}

func (p *openAIProvider) storedKey() string {
	if p.ks != nil {
		return p.ks.Get(p.keyEnv)
	}
	return ""
}

// usingCodex reports whether this bot rides a ChatGPT subscription (different endpoint and auth than an API key).
func (p *openAIProvider) usingCodex() bool {
	if p.auth != "" {
		return p.auth == AuthChatGPT && p.chatgpt != nil && p.chatgpt.Connected()
	}

	// An empty auth means a pre-existing config; keep the old base-URL guess for it
	return p.storedKey() == "" && p.chatgpt != nil && p.chatgpt.Connected() && secret.IsOfficialOpenAIBase(p.baseURL)
}

// resolveKey picks the credential for this request. Choosing a subscription means the subscription token
// is the only thing used: the old rule only fell back to it when no key was stored, so an unrelated
// stored OPENAI_API_KEY would silently shadow the subscription the user had just signed into.
func (p *openAIProvider) resolveKey() string {
	switch p.auth {
	case AuthChatGPT:
		if p.chatgpt != nil {
			if t, err := p.chatgpt.Bearer(); err == nil {
				return t
			}
		}
		return ""
	case AuthXai:
		if p.xai != nil {
			if t, err := p.xai.Bearer(); err == nil {
				return t
			}
		}
		return ""
	case AuthNone:
		return ""
	case AuthKey:
		return p.storedKey()
	}
	// implicit fallback for pre-existing configs
	if k := p.storedKey(); k != "" {
		return k
	}
	if p.xai != nil && secret.IsXAIBase(p.baseURL) {
		if t, err := p.xai.Bearer(); err == nil && t != "" {
			return t
		}
	}
	if p.usingCodex() {
		if t, err := p.chatgpt.Bearer(); err == nil && t != "" {
			return t
		}
	}
	return ""
}

func (p *openAIProvider) SupportsWebTools() bool { return false }
func (p *openAIProvider) Label() string          { return "openai:" + p.model }
func (p *openAIProvider) NewSession() Session    { return &openAISession{p: p} }

type oaiFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type oaiToolCall struct {
	ID       string  `json:"id"`
	Type     string  `json:"type"`
	Function oaiFunc `json:"function"`
}

type oaiMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCalls  []oaiToolCall   `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

func oaiText(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

type openAISession struct {
	p       *openAIProvider
	history []oaiMessage
	tracker cutTracker
}

func (s *openAISession) MarkTurn() { s.tracker.markTurn(len(s.history)) }
func (s *openAISession) Rollback() { s.history = s.history[:s.tracker.rollbackTo()] }

func (s *openAISession) AddUser(text string, images ...ResultImage) {
	s.tracker.addCut(len(s.history))
	s.history = append(s.history, oaiMessage{Role: "user", Content: oaiUserContent(text, images)})
}

// oaiUserContent assembles a user message's content. With no images it stays a plain string, which is
// the path almost every message takes — and small locally-run models often answer 400 to a content
// array outright, so everyone else should not pay for the rare message that carries a picture.
func oaiUserContent(text string, images []ResultImage) json.RawMessage {
	if len(images) == 0 {
		return oaiText(text)
	}
	parts := make([]map[string]any, 0, len(images)+1)
	if strings.TrimSpace(text) != "" {
		parts = append(parts, map[string]any{"type": "text", "text": text})
	}
	for _, img := range images {
		parts = append(parts, map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": "data:" + img.MIME + ";base64," + img.Base64},
		})
	}
	raw, err := json.Marshal(parts)
	if err != nil {
		return oaiText(text)
	}
	return raw
}

// On OpenAI-compatible endpoints an image cannot go into the tool result: a role:"tool" message's
// content accepts a string only. That is the shape of the API, not a shortcut on our part.

// The remaining option — sending the image along as a separate user message — is a 400 against a
// text-only model, which local Ollama models routinely are, turning a call that merely missed an image
// into a failed turn. The cases that need images today are essentially all on Claude-based bots, so this
// side states plainly what came back and lets the model ask for it in another form (Playwright, for one,
// has a textual page snapshot).
func (s *openAISession) AddToolResults(rs []ToolResult) {
	for _, r := range rs {
		content := r.Content
		if r.IsError {
			content = i18n.T("[error] ") + content
		}
		if n := len(r.Images); n > 0 {
			content = strings.TrimSpace(content + "\n" +
				fmt.Sprintf(i18n.T("[%d image(s) returned; this model's tool results cannot carry images. Ask for a textual form if one exists.]"), n))
		}
		s.history = append(s.history, oaiMessage{Role: "tool", ToolCallID: r.ID, Content: oaiText(content)})
	}
}

func (s *openAISession) Trim(maxMessages, maxChars int) {
	applyTrim(&s.history, &s.tracker, maxMessages, maxChars, func(note string) oaiMessage {
		return oaiMessage{Role: "user", Content: oaiText(note)}
	}, sketchesJSON[oaiMessage], charsFromJSON[oaiMessage])
}

func (s *openAISession) Snapshot() json.RawMessage {
	return packSession("openai", s.history, s.tracker)
}

func (s *openAISession) Restore(raw json.RawMessage) bool {
	var hist []oaiMessage
	t, ok := unpackSession(raw, "openai", &hist)
	if !ok {
		return false
	}
	s.history = hist
	s.tracker = t
	return true
}

func usesResponsesAPI(base, model string) bool {
	b := strings.ToLower(strings.TrimRight(base, "/"))
	if strings.HasSuffix(b, "/responses") || strings.Contains(b, "chatgpt.com") || strings.Contains(b, "api.openai.com") {
		return true
	}
	if !strings.Contains(b, "opencode.ai/zen") {
		return false
	}
	m := strings.ToLower(model)
	return strings.HasPrefix(m, "gpt-") || strings.HasPrefix(m, "grok-") || strings.HasPrefix(m, "muse-")
}

func responsesURL(base string) string {
	b := strings.TrimRight(base, "/")
	if strings.HasSuffix(strings.ToLower(b), "/responses") {
		return b
	}
	return b + "/responses"
}

func chatCompletionsURL(base, effort string) string {
	b := strings.TrimRight(base, "/")
	if effort != "" && strings.Contains(strings.ToLower(b), "api.deepseek.com") {
		if strings.HasSuffix(strings.ToLower(b), "/v1") {
			b = b[:len(b)-len("/v1")]
		}
		if !strings.HasSuffix(strings.ToLower(b), "/beta") {
			b += "/beta"
		}
	}
	return b + "/chat/completions"
}

func (s *openAISession) Step(ctx context.Context, system string, tools []ToolDef, includeWeb bool) (StepResult, error) {
	key := s.p.resolveKey()
	if key == "" {
		if secret.IsXAIBase(s.p.baseURL) {
			return StepResult{}, errors.New(i18n.T("No Grok credentials: sign in to SuperGrok in Settings, or add XAI_API_KEY"))
		}
		if secret.IsOfficialOpenAIBase(s.p.baseURL) {
			return StepResult{}, errors.New(i18n.T("No GPT credentials: sign in to ChatGPT Plus/Pro in Settings, or add OPENAI_API_KEY"))
		}
		if strings.Contains(s.p.baseURL, "opencode.ai") {
			return StepResult{}, fmt.Errorf(i18n.T("%s not found: add it in Settings or set it as an environment variable"), s.p.keyEnv)
		}
		key = "not-needed"
	}
	if usesResponsesAPI(s.p.baseURL, s.p.model) {
		return s.stepResponses(ctx, system, tools, key)
	}
	return s.stepChatCompletions(ctx, system, tools, key)
}

func (s *openAISession) stepChatCompletions(ctx context.Context, system string, tools []ToolDef, key string) (StepResult, error) {
	msgs := append([]oaiMessage{{Role: "system", Content: oaiText(system)}}, s.history...)
	var oaiTools []map[string]any
	for _, t := range tools {
		oaiTools = append(oaiTools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  toolParams(t),
			},
		})
	}
	body := map[string]any{"model": s.p.model, "messages": msgs}
	if len(oaiTools) > 0 {
		body["tools"] = oaiTools
	}

	// On chat/completions the knob is the top-level reasoning_effort (xAI and friends use the same name)
	if s.p.effort != "" {
		body["reasoning_effort"] = s.p.effort
	}
	data, status, err := s.postJSON(ctx, chatCompletionsURL(s.p.baseURL, s.p.effort), key, body)
	if err != nil {
		return StepResult{}, err
	}

	var parsed struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Choices []struct {
			FinishReason string     `json:"finish_reason"`
			Message      oaiMessage `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil || (parsed.Error == nil && len(parsed.Choices) == 0) {
		return StepResult{}, fmt.Errorf(i18n.T("%s returned an unexpected response (HTTP %d): %.300s"),
			s.p.baseURL, status, string(data))
	}
	if parsed.Error != nil {
		return StepResult{}, oaiAPIError(s.p.Label(), s.p.baseURL, status, parsed.Error.Message)
	}

	choice := parsed.Choices[0]
	s.history = append(s.history, choice.Message)
	return oaiStepFromMessage(choice.Message, choice.FinishReason), nil
}

func responsesInput(history []oaiMessage) []map[string]any {
	var items []map[string]any
	for _, m := range history {
		switch m.Role {
		case "user":
			items = append(items, map[string]any{"role": "user", "content": responsesUserContent(m.Content)})
		case "assistant":
			if text := decodeOAIContent(m.Content); strings.TrimSpace(text) != "" {
				items = append(items, map[string]any{"role": "assistant", "content": text})
			}
			for _, tc := range m.ToolCalls {
				items = append(items, map[string]any{
					"type": "function_call", "call_id": tc.ID,
					"name": tc.Function.Name, "arguments": tc.Function.Arguments,
				})
			}
		case "tool":
			items = append(items, map[string]any{
				"type": "function_call_output", "call_id": m.ToolCallID,
				"output": decodeOAIContent(m.Content),
			})
		}
	}
	return items
}

// responsesUserContent restates a chat-completions user content in the shape the Responses API wants.
// The block names differ — text becomes input_text, image_url becomes input_image — and an image's
// address hangs directly off the block. A message with no images is already a string and passes through.
func responsesUserContent(raw json.RawMessage) any {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return decodeOAIContent(raw)
	}
	out := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		if p.Type == "image_url" {
			out = append(out, map[string]any{"type": "input_image", "image_url": p.ImageURL.URL})
			continue
		}
		out = append(out, map[string]any{"type": "input_text", "text": p.Text})
	}
	return out
}

func (s *openAISession) stepResponses(ctx context.Context, system string, tools []ToolDef, key string) (StepResult, error) {
	var oaiTools []map[string]any
	for _, t := range tools {
		oaiTools = append(oaiTools, map[string]any{
			"type": "function", "name": t.Name, "description": t.Description,
			"parameters": toolParams(t),
		})
	}
	body := map[string]any{"model": s.p.model, "input": responsesInput(s.history)}
	// On Responses / Codex the knob lives at reasoning.effort
	if s.p.effort != "" {
		body["reasoning"] = map[string]any{"effort": s.p.effort}
	}

	// The Codex backend gives no choice on either: without store=false it answers 400 "Store must be
	// set to false", and without stream=true, 400 "Stream must be set to true". postJSON folds the
	// resulting event stream back into one response object. api.openai.com is fine with the defaults
	// for both, so leave it be.
	if s.p.usingCodex() {
		body["store"] = false
		body["stream"] = true
	}
	if strings.TrimSpace(system) != "" {
		body["instructions"] = system
	}
	if len(oaiTools) > 0 {
		body["tools"] = oaiTools
	}
	url := s.p.responsesEndpoint()
	data, status, err := s.postJSON(ctx, url, key, body)
	if err != nil {
		return StepResult{}, err
	}

	var parsed struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Status            string `json:"status"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Output []struct {
			Type      string          `json:"type"`
			CallID    string          `json:"call_id"`
			Name      string          `json:"name"`
			Arguments string          `json:"arguments"`
			Content   json.RawMessage `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil || (parsed.Error == nil && len(parsed.Output) == 0) {
		return StepResult{}, fmt.Errorf(i18n.T("%s returned an unexpected response (HTTP %d): %.300s"),
			url, status, string(data))
	}
	if parsed.Error != nil {
		return StepResult{}, oaiAPIError(s.p.Label(), url, status, parsed.Error.Message)
	}

	asst := oaiMessage{Role: "assistant"}
	var texts []string
	for _, item := range parsed.Output {
		switch item.Type {
		case "function_call":
			asst.ToolCalls = append(asst.ToolCalls, oaiToolCall{
				ID: item.CallID, Type: "function",
				Function: oaiFunc{Name: item.Name, Arguments: item.Arguments},
			})
		case "message", "":
			if text := decodeOAIContent(item.Content); strings.TrimSpace(text) != "" {
				texts = append(texts, text)
			}
		}
	}
	if len(texts) > 0 {
		asst.Content = oaiText(strings.Join(texts, "\n"))
	}
	s.history = append(s.history, asst)
	finish := ""
	if parsed.IncompleteDetails != nil && parsed.IncompleteDetails.Reason == "max_output_tokens" {
		finish = "length"
	}
	return oaiStepFromMessage(asst, finish), nil
}

func (p *openAIProvider) responsesEndpoint() string {
	if p.usingCodex() {
		if p.chatgpt != nil && p.chatgpt.APIURL() != "" {
			return p.chatgpt.APIURL()
		}
		return secret.ChatGPTCodexURL
	}
	return responsesURL(p.baseURL)
}

func (s *openAISession) postJSON(ctx context.Context, url, key string, body map[string]any) ([]byte, int, error) {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(raw))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("User-Agent", "botbureau/0.1.0")
	if s.p.usingCodex() {
		req.Header.Set("originator", "botbureau")
		req.Header.Set("Accept", "text/event-stream")
		if id := s.p.chatgpt.AccountID(); id != "" {
			req.Header.Set("ChatGPT-Account-Id", id)
		}
	}
	resp, err := s.p.httpc.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf(i18n.T("Can't reach %s: %v"), url, err)
	}
	defer resp.Body.Close()

	// Successful Codex calls are always SSE by contract. Some proxies relay the body while
	// stripping Content-Type, so still parse the stream instead of handing "event:
	// response.created" to the JSON layer. Error responses remain ordinary JSON, hence the
	// endpoint-based fallback applies only to successful (2xx) responses.
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") ||
		(s.p.usingCodex() && resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices) {
		data, err := collapseResponsesStream(resp.Body)
		return data, resp.StatusCode, err
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return data, resp.StatusCode, nil
}

// collapseResponsesStream folds the Responses API's SSE stream back into the single object the
// non-streaming call would have returned.

// The Codex backend accepts nothing but stream=true, yet a turn here is taken whole and there is no
// token-by-token surface to feed. So the terminal event (response.completed / .incomplete / .failed)
// hands its response object over intact, and the parsing below it stays shared with the non-streaming
// path, none the wiser that the transport changed. Some Codex gateways return an empty output in that
// final object; for those, completed output items or text deltas fill the gap.
func collapseResponsesStream(r io.Reader) ([]byte, error) {
	sc := bufio.NewScanner(io.LimitReader(r, 32<<20))

	// A single data: line can be long — a whole answer rides in it — and the 64KB default is not enough
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	var last []byte
	var completedItems []json.RawMessage
	var text strings.Builder
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		payload, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		payload = strings.TrimSpace(payload)
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var ev struct {
			Type     string          `json:"type"`
			Response json.RawMessage `json:"response"`
			Error    json.RawMessage `json:"error"`
			Item     json.RawMessage `json:"item"`
			Delta    string          `json:"delta"`
			Text     string          `json:"text"`
		}
		if json.Unmarshal([]byte(payload), &ev) != nil {
			continue
		}
		switch {
		case strings.HasPrefix(ev.Type, "response.") && len(ev.Response) > 0:
			last = ev.Response
			if ev.Type == "response.completed" || ev.Type == "response.incomplete" || ev.Type == "response.failed" {
				return restoreStreamOutput(last, completedItems, text.String()), nil
			}
		case ev.Type == "response.output_item.done" && len(ev.Item) > 0:
			completedItems = append(completedItems, ev.Item)
		case ev.Type == "response.output_text.delta":
			text.WriteString(ev.Delta)
		case ev.Type == "response.output_text.done" && ev.Text != "":

			// .done carries the whole text, so replacing deltas avoids repeating the answer.
			text.Reset()
			text.WriteString(ev.Text)
		case ev.Type == "error" && len(ev.Error) > 0:

			// A mid-stream error: wrapped as the non-streaming {"error":…} so it takes the same path
			return append(append([]byte(`{"error":`), ev.Error...), '}'), nil
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf(i18n.T("The response stream broke off: %v"), err)
	}
	if len(last) > 0 {

		// The stream stopped short of its terminal event; the partial object still beats "no response"
		return restoreStreamOutput(last, completedItems, text.String()), nil
	}
	return nil, errors.New(i18n.T("The response stream ended without a result"))
}

// restoreStreamOutput handles a Codex gateway variant where response.completed has complete metadata
// but an empty output, leaving the actual result only in earlier stream events. A standard Responses
// object with output stays untouched.
func restoreStreamOutput(response []byte, completedItems []json.RawMessage, text string) []byte {
	var obj map[string]json.RawMessage
	if json.Unmarshal(response, &obj) != nil {
		return response
	}
	var existing []json.RawMessage
	if raw := obj["output"]; len(raw) > 0 && json.Unmarshal(raw, &existing) == nil && len(existing) > 0 {
		return response
	}
	if len(completedItems) > 0 {
		obj["output"], _ = json.Marshal(completedItems)
	} else if text != "" {
		obj["output"], _ = json.Marshal([]map[string]any{{
			"type":    "message",
			"content": []map[string]string{{"type": "output_text", "text": text}},
		}})
	} else {
		return response
	}
	patched, err := json.Marshal(obj)
	if err != nil {
		return response
	}
	return patched
}

func oaiAPIError(label, url string, status int, msg string) error {
	if q := classifyQuota(status, label, msg); q != "" {
		return &QuotaError{Msg: q}
	}
	return fmt.Errorf(i18n.T("%s reported an error (HTTP %d): %s"), url, status, msg)
}

func oaiStepFromMessage(msg oaiMessage, finish string) StepResult {
	res := StepResult{}
	if text := decodeOAIContent(msg.Content); strings.TrimSpace(text) != "" {
		res.Texts = append(res.Texts, text)
	}
	for _, tc := range msg.ToolCalls {
		input := map[string]any{}
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &input)
		res.ToolCalls = append(res.ToolCalls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Input: input})
	}
	switch {
	case len(res.ToolCalls) > 0:
		res.StopReason = "tool_use"
	case finish == "length":
		res.StopReason = "max_tokens"
	default:
		res.StopReason = "end_turn"
	}
	return res
}

// decodeOAIContent accepts content shaped either as a plain string or as [{type:"text",text:...}].
func decodeOAIContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return ""
}

// ---- unset: no model chosen yet; a placeholder that never calls an API ----

type unsetProvider struct{}

func (p *unsetProvider) SupportsWebTools() bool { return false }
func (p *unsetProvider) Label() string          { return "unset" }
func (p *unsetProvider) NewSession() Session    { return &unsetSession{} }

type unsetSession struct {
	history []string
	tracker cutTracker
}

func (s *unsetSession) MarkTurn() { s.tracker.markTurn(len(s.history)) }
func (s *unsetSession) Rollback() { s.history = s.history[:s.tracker.rollbackTo()] }
func (s *unsetSession) AddUser(text string, _ ...ResultImage) {
	s.tracker.addCut(len(s.history))
	s.history = append(s.history, "user: "+text)
}
func (s *unsetSession) AddToolResults(rs []ToolResult) {
	for _, r := range rs {
		s.history = append(s.history, "tool: "+r.Content)
	}
}
func (s *unsetSession) Trim(maxMessages, maxChars int) {
	applyStringTrim(&s.history, &s.tracker, maxMessages, maxChars)
}
func (s *unsetSession) Snapshot() json.RawMessage {
	return packSession("unset", s.history, s.tracker)
}
func (s *unsetSession) Restore(raw json.RawMessage) bool {
	var hist []string
	t, ok := unpackSession(raw, "unset", &hist)
	if !ok {
		return false
	}
	s.history = hist
	s.tracker = t
	return true
}
func (s *unsetSession) Step(ctx context.Context, system string, tools []ToolDef, includeWeb bool) (StepResult, error) {
	if ctx != nil && ctx.Err() != nil {
		return StepResult{}, ctx.Err()
	}
	msg := i18n.T("No model selected yet. Click my avatar to open settings, pick a model, then try again.")
	s.history = append(s.history, "assistant: "+msg)
	return StepResult{StopReason: "end_turn", Texts: []string{msg}}, nil
}

// ---- Fake (offline echo, for tests and key-less trials) ----

type fakeProvider struct{}

func (p *fakeProvider) SupportsWebTools() bool { return false }
func (p *fakeProvider) Label() string          { return "fake:echo" }
func (p *fakeProvider) NewSession() Session    { return &fakeSession{} }

type fakeSession struct {
	history []string
	tracker cutTracker
}

func (s *fakeSession) MarkTurn() { s.tracker.markTurn(len(s.history)) }
func (s *fakeSession) Rollback() { s.history = s.history[:s.tracker.rollbackTo()] }
func (s *fakeSession) AddUser(text string, _ ...ResultImage) {
	s.tracker.addCut(len(s.history))
	s.history = append(s.history, "user: "+text)
}
func (s *fakeSession) AddToolResults(rs []ToolResult) {
	for _, r := range rs {
		s.history = append(s.history, "tool: "+r.Content)
	}
}
func (s *fakeSession) Trim(maxMessages, maxChars int) {
	applyStringTrim(&s.history, &s.tracker, maxMessages, maxChars)
}
func (s *fakeSession) Snapshot() json.RawMessage {
	return packSession("fake", s.history, s.tracker)
}
func (s *fakeSession) Restore(raw json.RawMessage) bool {
	var hist []string
	t, ok := unpackSession(raw, "fake", &hist)
	if !ok {
		return false
	}
	s.history = hist
	s.tracker = t
	return true
}
func (s *fakeSession) Step(ctx context.Context, system string, tools []ToolDef, includeWeb bool) (StepResult, error) {
	if ctx != nil && ctx.Err() != nil {
		return StepResult{}, ctx.Err()
	}
	last := ""
	if len(s.history) > 0 {
		last = s.history[len(s.history)-1]
	}
	reply := i18n.T("(fake model echo) ") + strings.TrimPrefix(last, "user: ")
	s.history = append(s.history, "assistant: "+reply)
	return StepResult{StopReason: "end_turn", Texts: []string{reply}}, nil
}
