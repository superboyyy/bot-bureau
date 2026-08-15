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

// ---- 统一抽象：每个 bot 绑定一个 Provider；每个会话（群聊/私聊）一个 Session ----
// ---- Unified abstraction: each bot binds to one Provider; each chat (group chat/DM) gets one Session ----

type ToolDef struct {
	Name        string
	Description string
	Properties  map[string]any // JSON Schema 的 properties / the properties of a JSON Schema
	Required    []string
	// SchemaExtras 是参数 schema 根层的其余关键字（$defs、oneOf、additionalProperties…）。
	// 插件工具的 schema 不是我们写的，丢掉这些等于把别人的 $ref 指向空气。
	// SchemaExtras holds the remaining root-level keywords of the argument schema ($defs, oneOf,
	// additionalProperties, ...). A plugin tool's schema is not ours to author, and dropping these
	// points someone else's $ref at nothing.
	SchemaExtras map[string]any
}

// toolParams 拼出 OpenAI 系那边的 parameters 对象：固定的 type/properties/required
// 加上 schema 的其余关键字。extras 里的键不允许盖掉这三个固定项，否则一个写得随便的插件
// 就能把参数 schema 整个改形。
//
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
	// 工具返回的图片（插件才会有）。做成附加字段而不是把 Content 改成块列表，
	// 是因为十几个内置工具都只返回文字，为一个来源改掉所有人的返回类型不划算。
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
	Notes      []string // 服务端工具动态（如 web_search） / server-side tool activity (e.g. web_search)
	ToolCalls  []ToolCall
}

// Session 持有一段对话的历史（provider 原生形状），并保证回合级回滚与安全修剪。
// Session holds the history of one conversation (in the provider's native shape) and guarantees turn-level rollback and safe trimming.
type Session interface {
	MarkTurn()           // 记录回合起点（AddUser 之前调用） / mark the turn start (call before AddUser)
	Rollback()           // 回滚到回合起点（refusal 时用） / roll back to turn start (on refusal)
	AddUser(text string) // 追加一条用户/背景消息 / append a user/background message
	AddToolResults(rs []ToolResult)
	Step(ctx context.Context, system string, tools []ToolDef, includeWeb bool) (StepResult, error)
	// 历史修剪（只在完整用户回合边界切）
	// Trim history (cut only at full user-turn boundaries).
	Trim(limit int)
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
		// 无需 API key 的回声模型，用于离线试用/测试
		// An echo model that needs no API key, for offline trials/tests.
		return &fakeProvider{}, nil
	default:
		return nil, fmt.Errorf(i18n.T("Unknown provider: %q (supported: anthropic / openai / fake)"), c.Provider)
	}
}

// ---- 额度/余额类错误：需要显式提醒用户，与普通限流区分 ----
// ---- Quota/balance errors: the user must be alerted explicitly, distinct from ordinary rate limiting ----

type QuotaError struct{ Msg string }

func (e *QuotaError) Error() string { return e.Msg }

// classifyQuota 判断是否为「余额/配额耗尽」类错误；是则返回面向用户的提示。
// classifyQuota reports whether this is a "balance/quota exhausted" error; if so it returns a user-facing message.
func classifyQuota(status int, label, msg string) string {
	lower := strings.ToLower(msg)
	hit := status == 402 ||
		strings.Contains(lower, "credit balance") ||
		strings.Contains(lower, "insufficient_quota") ||
		strings.Contains(lower, "insufficient quota") ||
		strings.Contains(lower, "exceeded your current quota") ||
		strings.Contains(lower, "billing") ||
		// 这几个不是我们的文案，是在匹配服务商返回的错误原文——中文服务商（DeepSeek、Kimi）
		// 的余额提示就是中文，翻译它没有意义，要认的就是对方原样吐出来的那几个字。
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

// ---- 通用的“可安全切割点”管理：三种实现共用 ----
// ---- Generic "safe cut point" bookkeeping, shared by the three implementations ----

type cutTracker struct {
	mark int
	// 完整用户回合的起始下标，只能从这些位置修剪
	// Start indices of full user turns; trimming may only cut at these positions.
	cuts []int
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

// trimPoint 返回应当从哪个下标开始保留；无合适切割点时返回 0。
// trimPoint returns the index from which history should be kept, or 0 when there is no suitable cut point.
func (t *cutTracker) trimPoint(historyLen, limit int) int {
	if historyLen <= limit {
		return 0
	}
	want := historyLen - limit
	for _, c := range t.cuts {
		if c >= want {
			t.rebase(c)
			return c
		}
	}
	return 0
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

// ---- Anthropic（官方 Go SDK，beta 端点：服务端 fallback + 联网工具） ----
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

// getClient 按当前 key 惰性构建/重建客户端：UI 里改了 key 立即生效，
// 没有存 key 时走 SDK 默认解析（环境变量 / `ant auth login` 档案）。
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

func (s *anthropicSession) AddUser(text string) {
	s.tracker.addCut(len(s.history))
	s.history = append(s.history, anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(text)))
}

func (s *anthropicSession) AddToolResults(rs []ToolResult) {
	blocks := make([]anthropic.BetaContentBlockParamUnion, 0, len(rs))
	for _, r := range rs {
		if len(r.Images) == 0 {
			blocks = append(blocks, anthropic.NewBetaToolResultBlock(r.ID, r.Content, r.IsError))
			continue
		}
		// 图片直接进 tool_result 的内容块——这是规范里正经的位置，模型看到的就是
		// "这个工具返回了这张图"，而不是另起一条消息假装是用户发的。
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

func (s *anthropicSession) Trim(limit int) {
	if p := s.tracker.trimPoint(len(s.history), limit); p > 0 {
		s.history = append([]anthropic.BetaMessageParam(nil), s.history[p:]...)
	}
}

type sessionDisk struct {
	Kind    string          `json:"kind"`
	History json.RawMessage `json:"history"`
	Cuts    []int           `json:"cuts"`
	Mark    int             `json:"mark"`
}

func packSession(kind string, history any, t cutTracker) json.RawMessage {
	h, err := json.Marshal(history)
	if err != nil {
		return nil
	}
	raw, err := json.Marshal(sessionDisk{Kind: kind, History: h, Cuts: t.cuts, Mark: t.mark})
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
	return cutTracker{cuts: d.Cuts, mark: d.Mark}, true
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
					// SDK 的 schema 参数只有 properties/required 两个具名字段，其余关键字走 ExtraFields
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
		// Opus 5 安全分类器可能拒答；开启服务端 fallback（"default" 按拒答类别自动选择接续模型）
		// Opus 5's safety classifiers may refuse; enable server-side fallback ("default" auto-selects a continuation model per refusal category)
		Betas:     []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01},
		Fallbacks: anthropic.BetaFallbacksParamOfDefault(),
	}
	// 只有明确选了强度才带思考参数：留空的 bot 保持服务商默认，
	// 也避免把这个字段塞给不认识它的模型（那是直接 400）。
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

// ---- OpenAI 兼容端点（OpenAI / xAI Grok / DeepSeek / Kimi / Ollama …），原生 net/http ----
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

// usingCodex 判断这个 bot 是不是走 ChatGPT 订阅（端点和鉴权都跟 API key 不同）。
// usingCodex reports whether this bot rides a ChatGPT subscription (different endpoint and auth than an API key).
func (p *openAIProvider) usingCodex() bool {
	if p.auth != "" {
		return p.auth == AuthChatGPT && p.chatgpt != nil && p.chatgpt.Connected()
	}
	// auth 为空 = 老配置，沿用按 base_url 猜的旧行为
	// An empty auth means a pre-existing config; keep the old base-URL guess for it
	return p.storedKey() == "" && p.chatgpt != nil && p.chatgpt.Connected() && secret.IsOfficialOpenAIBase(p.baseURL)
}

// resolveKey 取这次请求要用的凭据。选了订阅就只用订阅的 token——
// 以前是"没存 key 才回退到订阅"，于是存过一个无关的 OPENAI_API_KEY 就会把订阅悄悄顶掉。
//
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
	// 老配置的隐式回退 / implicit fallback for pre-existing configs
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

func (s *openAISession) AddUser(text string) {
	s.tracker.addCut(len(s.history))
	s.history = append(s.history, oaiMessage{Role: "user", Content: oaiText(text)})
}

// OpenAI 兼容端点这边图片进不去工具结果：role:"tool" 的消息 content 只接受字符串，
// 这是接口本身的形状，不是我们省事。
//
// 剩下的选项是"另发一条 user 消息把图片带上"——但那对纯文本模型（本地 Ollama 的小模型是常态）
// 是一个 400，会把一次本来只是少看一张图的调用变成整轮失败。用得上图的场景现在几乎都在
// Claude 系 bot 上，所以这边如实说明返回了什么，让模型自己决定换个形式再要一次
// （Playwright 就有文字版的页面快照）。
//
// On OpenAI-compatible endpoints an image cannot go into the tool result: a role:"tool" message's
// content accepts a string only. That is the shape of the API, not a shortcut on our part.
//
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

func (s *openAISession) Trim(limit int) {
	if p := s.tracker.trimPoint(len(s.history), limit); p > 0 {
		s.history = append([]oaiMessage(nil), s.history[p:]...)
	}
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
	// chat/completions 这条路上，思考强度是顶层的 reasoning_effort（xAI 等同款）
	// On chat/completions the knob is the top-level reasoning_effort (xAI and friends use the same name)
	if s.p.effort != "" {
		body["reasoning_effort"] = s.p.effort
	}
	data, status, err := s.postJSON(ctx, s.p.baseURL+"/chat/completions", key, body)
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
			items = append(items, map[string]any{"role": "user", "content": decodeOAIContent(m.Content)})
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

func (s *openAISession) stepResponses(ctx context.Context, system string, tools []ToolDef, key string) (StepResult, error) {
	var oaiTools []map[string]any
	for _, t := range tools {
		oaiTools = append(oaiTools, map[string]any{
			"type": "function", "name": t.Name, "description": t.Description,
			"parameters": toolParams(t),
		})
	}
	body := map[string]any{"model": s.p.model, "input": responsesInput(s.history)}
	// Responses / Codex 这条路上，强度在 reasoning.effort 里
	// On Responses / Codex the knob lives at reasoning.effort
	if s.p.effort != "" {
		body["reasoning"] = map[string]any{"effort": s.p.effort}
	}
	// Codex 后端对这两项没得商量：不写 store=false 是 400 "Store must be set to false"，
	// 不写 stream=true 是 400 "Stream must be set to true"。回来的事件流由 postJSON 收敛回
	// 单个响应对象。api.openai.com 那边两项的默认值都能用，保持原样不动。
	//
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
	// 出错时服务端回的是普通 JSON，只有成功那条才是事件流，所以按响应头分流而不是按端点。
	// Errors come back as ordinary JSON and only the successful call streams, so branch on the
	// response header rather than on which endpoint was called.
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		data, err := collapseResponsesStream(resp.Body)
		return data, resp.StatusCode, err
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return data, resp.StatusCode, nil
}

// collapseResponsesStream 把 Responses API 的 SSE 流收敛成它非流式时会返回的那一个对象。
//
// Codex 后端只接受 stream=true，但这里一次只走一轮对话，逐字回显没有意义：读到收尾事件
// （response.completed / .incomplete / .failed）就把里面的 response 整个交出去，
// 后面的解析代码于是跟非流式响应共用一套，不必知道传输方式变过。
//
// collapseResponsesStream folds the Responses API's SSE stream back into the single object the
// non-streaming call would have returned.
//
// The Codex backend accepts nothing but stream=true, yet a turn here is taken whole and there is no
// token-by-token surface to feed. So the terminal event (response.completed / .incomplete / .failed)
// hands its response object over intact, and the parsing below it stays shared with the non-streaming
// path, none the wiser that the transport changed.
func collapseResponsesStream(r io.Reader) ([]byte, error) {
	sc := bufio.NewScanner(io.LimitReader(r, 32<<20))
	// 单个 data: 行可以很长（整段回答都在里面），默认 64KB 的行上限不够用
	// A single data: line can be long — a whole answer rides in it — and the 64KB default is not enough
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	var last []byte
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
		}
		if json.Unmarshal([]byte(payload), &ev) != nil {
			continue
		}
		switch {
		case strings.HasPrefix(ev.Type, "response.") && len(ev.Response) > 0:
			last = ev.Response
			if ev.Type == "response.completed" || ev.Type == "response.incomplete" || ev.Type == "response.failed" {
				return last, nil
			}
		case ev.Type == "error" && len(ev.Error) > 0:
			// 流中途报错：包成非流式那种 {"error":…}，走同一条错误路径
			// A mid-stream error: wrapped as the non-streaming {"error":…} so it takes the same path
			return append(append([]byte(`{"error":`), ev.Error...), '}'), nil
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf(i18n.T("The response stream broke off: %v"), err)
	}
	if len(last) > 0 {
		// 流断在收尾事件之前：手上那份半成品也比一句"无响应"有用
		// The stream stopped short of its terminal event; the partial object still beats "no response"
		return last, nil
	}
	return nil, errors.New(i18n.T("The response stream ended without a result"))
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

// decodeOAIContent 兼容 content 为字符串或 [{type:"text",text:...}] 两种形状。
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

// ---- unset：还没选模型，占位，不打任何 API ----
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
func (s *unsetSession) AddUser(text string) {
	s.tracker.addCut(len(s.history))
	s.history = append(s.history, "user: "+text)
}
func (s *unsetSession) AddToolResults(rs []ToolResult) {
	for _, r := range rs {
		s.history = append(s.history, "tool: "+r.Content)
	}
}
func (s *unsetSession) Trim(limit int) {
	if p := s.tracker.trimPoint(len(s.history), limit); p > 0 {
		s.history = append([]string(nil), s.history[p:]...)
	}
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

// ---- Fake（离线回声，测试与无 key 试用） ----
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
func (s *fakeSession) AddUser(text string) {
	s.tracker.addCut(len(s.history))
	s.history = append(s.history, "user: "+text)
}
func (s *fakeSession) AddToolResults(rs []ToolResult) {
	for _, r := range rs {
		s.history = append(s.history, "tool: "+r.Content)
	}
}
func (s *fakeSession) Trim(limit int) {
	if p := s.tracker.trimPoint(len(s.history), limit); p > 0 {
		s.history = append([]string(nil), s.history[p:]...)
	}
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
