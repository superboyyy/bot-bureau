package config

import (
	"botbureau/backend/internal/i18n"

	"fmt"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	DefaultAnthropicModel = "claude-opus-5"

	// Opus 5 enables thinking by default and thinking tokens count too, so don't set this too small
	MaxTokens = 16000

	// Cap on agent-loop iterations triggered by a single message. A backstop rather than a budget for
	// work: what actually stops a bot going in circles is MaxRepeatedCalls below, which judges progress,
	// where this one only counts laps.

	// It used to be 30. Thirty laps is nowhere near enough for something like "review every document in
	// this repository", so the user ends up saying "carry on" over and over — while doing little against
	// a real loop, since a task working hard and a task spinning look identical to a lap counter. Split
	// in two, each does its own job: spinning is cut below, and this catches only the extreme case of a
	// loop that varies its arguments every time.
	MaxToolIterations = 200

	// Cut the turn once the same tool has been called with the same arguments this many times in a row.

	// This is what running away actually looks like: a tool errors, the identical call goes out again,
	// errors again, and again. Not one argument changed, so nothing about the result will either, and a
	// hundred more laps would go the same way. The auto tier runs unattended, and that spinning burns
	// the user's quota.

	// Five rather than two or three: retrying once is ordinary (a network blip, a file written a moment
	// ago and not yet visible). Five identical calls in a row is no longer a retry.
	MaxRepeatedCalls = 5
	HistoryLimit     = 60 // max messages per conversation context
	BashTimeout      = 120 * time.Second
	ToolOutputLimit  = 20000 // tool output truncation length (chars)
)

// How long an approval waits before it is auto-rejected. Tests shorten it, which is why this is an
// atomic rather than a plain variable: it is read by every goroutine waiting on an approval, so the
// moment a test writes it there is a genuine data race (-race says so) — even though nothing in
// production ever writes it after start-up.
const DefaultApprovalTimeout = 10 * time.Minute

var approvalTimeout atomic.Int64

func ApprovalTimeout() time.Duration {
	if v := approvalTimeout.Load(); v > 0 {
		return time.Duration(v)
	}
	return DefaultApprovalTimeout
}

// SetApprovalTimeout is used only by tests today; passing 0 restores the default.
func SetApprovalTimeout(d time.Duration) { approvalTimeout.Store(int64(d)) }

var (
	botNameRe   = regexp.MustCompile(`^[a-z0-9_-]{1,24}$`)
	avatarHexRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
)

// An avatar is either a fill color or a small embedded image; empty means "derive a default from the name".
// Embedded images live in bots.yaml, so the size is capped — the client downscales to 96px before sending.
const (
	AvatarMaxBytes = 250000
	DisplayNameMax = 32

	// Cap on the custom role instructions. An imported member template easily runs to thousands of
	// characters, and it is sent again on every single turn.
	PromptMax = 8000
)

func ValidAvatar(s string) bool {
	if s == "" || avatarHexRe.MatchString(s) {
		return true
	}
	if len(s) > AvatarMaxBytes {
		return false
	}
	for _, prefix := range []string{"data:image/png;base64,", "data:image/jpeg;base64,", "data:image/webp;base64,"} {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

// Reasoning effort. Every vendor exposes a different knob, but the API vocabulary is shared and the
// actual set of values is selected per model:

// Anthropic — the extended-thinking token budget (budget_tokens)
// OpenAI Responses / Codex — reasoning.effort
// other OpenAI-compatible endpoints (xAI and friends) — reasoning_effort

// Empty sends no field at all and leaves the vendor's default in place, which matters: not every model
// accepts a thinking parameter, and forcing one on a model that does not know it is a plain 400.
const (
	EffortNone    = "none"
	EffortMinimal = "minimal"
	EffortLow     = "low"
	EffortMedium  = "medium"
	EffortHigh    = "high"
	EffortXHigh   = "xhigh"
	EffortMax     = "max"
)

func ValidEffort(s string) bool {
	switch s {
	case "", EffortNone, EffortMinimal, EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax:
		return true
	}
	return false
}

// ThinkingBudget converts a tier into Anthropic's thinking token budget; 0 leaves extended thinking off.
// It stays below MaxTokens: a budget larger than the total output cap is rejected outright.
func ThinkingBudget(effort string) int64 {
	switch effort {
	case EffortMinimal:
		return 1024 // the smallest budget Anthropic accepts
	case EffortLow:
		return 2048
	case EffortMedium:
		return 6144
	case EffortHigh:
		return 12288
	}
	return 0
}

// The tiers each vendor accepts are not the same, so the picker has to be served per concrete model
// rather than as one table for everyone.

// Anthropic has no notion of these API words — it takes a continuous thinking-token budget, and the
// numbers above are this project's own cut of it. OpenAI and xAI also differ by model: GPT-5 accepts
// minimal, GPT-5.6 accepts none/xhigh/max, and Grok 4.6 adds xhigh. A single provider-wide table is
// therefore not safe.

// A tier is named by the word the API itself uses — none / minimal / low / medium / high / xhigh / max,
// shown verbatim and untranslated, rather than under invented names like Quick / Balanced / Thorough.
// Those invented names were a second layer on top of the real one: what the user reads in the vendor's
// docs, writes into
// bots.yaml, and gets quoted back in an error is low, while the screen said "Quick". The note under
// each tier is where the explaining belongs, and it stays translated.
func effortTier(id string) map[string]any {
	switch id {
	case EffortNone:
		return map[string]any{"id": EffortNone, "label": EffortNone,
			"note": i18n.T("No reasoning; fastest and cheapest")}
	case EffortMinimal:
		return map[string]any{"id": EffortMinimal, "label": EffortMinimal,
			"note": i18n.T("Minimal reasoning; fastest and least expensive")}
	case EffortLow:
		return map[string]any{"id": EffortLow, "label": EffortLow,
			"note": i18n.T("Faster and cheaper; good for routine tasks and short questions")}
	case EffortMedium:
		return map[string]any{"id": EffortMedium, "label": EffortMedium,
			"note": i18n.T("Uses reasoning for most answers")}
	case EffortHigh:
		return map[string]any{"id": EffortHigh, "label": EffortHigh,
			"note": i18n.T("Longest reasoning on hard problems; slower and more expensive")}
	case EffortXHigh:
		return map[string]any{"id": EffortXHigh, "label": EffortXHigh,
			"note": i18n.T("Maximum reasoning; slowest and most expensive")}
	case EffortMax:
		return map[string]any{"id": EffortMax, "label": EffortMax,
			"note": i18n.T("The highest reasoning setting for the hardest problems")}
	}

	// This one has no API value behind it — it is precisely "send nothing", so the label is all it needs.
	return map[string]any{"id": "", "label": i18n.T("Default"),
		"note": i18n.T("Send no thinking setting at all — the safest choice, and the only one every model accepts")}
}

// EffortOptionsFor lists the tiers available before a concrete model is selected. Once the model is
// known, callers must use EffortOptionsForModel; an unknown model gets only the default so the client
// never presents a parameter that may be rejected by that model.
func EffortOptionsFor(providerID string) []map[string]any {
	return EffortOptionsForModel(providerID, "")
}

// EffortProviderFamily maps catalog ids to the implementation family whose model vocabulary they use.
// OpenAI-compatible catalog entries still need model-level matching; an unknown provider is left
// unknown instead of being guessed as OpenAI.
func EffortProviderFamily(providerID string) string {
	switch strings.ToLower(strings.TrimSpace(providerID)) {
	case "anthropic":
		return "anthropic"
	case "xai":
		return "xai"
	case "deepseek":
		return "deepseek"
	case "openai", "openai-compatible", "openai_compatible", "moonshot", "opencode", "opencode-go", "ollama", "custom":
		return "openai"
	case "fake":
		return "fake"
	default:
		return ""
	}
}

// modelPrefix matches a model alias and its dated/suffixed snapshots without treating a similarly
// named model as the same family (for example, gpt-5.6-pro is a suffix of gpt-5.6, but gpt-5.60 is not).
func modelPrefix(model, prefix string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	return model == prefix || strings.HasPrefix(model, prefix+"-")
}

func openAIEffortIDs(model string) []string {
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case modelPrefix(m, "gpt-5.6"):
		return []string{EffortNone, EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax}
	case modelPrefix(m, "gpt-5.5-pro"), modelPrefix(m, "gpt-5.4-pro"), modelPrefix(m, "gpt-5.2-pro"):
		return []string{EffortMedium, EffortHigh, EffortXHigh}
	case modelPrefix(m, "gpt-5.5"), modelPrefix(m, "gpt-5.4"), modelPrefix(m, "gpt-5.2"):
		return []string{EffortNone, EffortLow, EffortMedium, EffortHigh, EffortXHigh}
	case modelPrefix(m, "gpt-5.3-codex"):
		return []string{EffortNone, EffortLow, EffortMedium, EffortHigh, EffortXHigh}
	case modelPrefix(m, "gpt-5.1-codex-max"):
		return []string{EffortLow, EffortMedium, EffortHigh, EffortXHigh}
	case modelPrefix(m, "gpt-5.1-codex"):
		return []string{EffortLow, EffortMedium, EffortHigh}
	case modelPrefix(m, "gpt-5.1"):
		return []string{EffortNone, EffortLow, EffortMedium, EffortHigh}
	case modelPrefix(m, "gpt-5"):
		return []string{EffortMinimal, EffortLow, EffortMedium, EffortHigh}
	case modelPrefix(m, "o3-pro"):
		return []string{EffortHigh}
	case modelPrefix(m, "o3"), modelPrefix(m, "o4-mini"), modelPrefix(m, "o1"):
		return []string{EffortLow, EffortMedium, EffortHigh}
	default:
		return nil
	}
}

func xAIEffortIDs(model string) []string {
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case modelPrefix(m, "grok-4.6"):
		return []string{EffortLow, EffortMedium, EffortHigh, EffortXHigh}
	case modelPrefix(m, "grok-4.5"):
		return []string{EffortLow, EffortMedium, EffortHigh}
	case modelPrefix(m, "grok-4.20-multi-agent"):
		return []string{EffortLow, EffortMedium, EffortHigh, EffortXHigh}
	case modelPrefix(m, "grok-4.3"):
		return []string{EffortNone, EffortLow, EffortMedium, EffortHigh}
	default:
		return nil
	}
}

func deepSeekEffortIDs(model string) []string {
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case modelPrefix(m, "deepseek-v4-flash"), modelPrefix(m, "deepseek-v4-pro"), modelPrefix(m, "deepseek-reasoner"):
		return []string{EffortHigh, EffortMax}
	default:
		return nil
	}
}

func anthropicFamily(model, family string) bool {
	return modelPrefix(model, "claude-"+family+"-4") || modelPrefix(model, "claude-"+family+"-5")
}

func anthropicEffortIDs(model string) []string {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" || modelPrefix(m, "claude-3-7-sonnet") ||
		strings.HasPrefix(m, "claude-4") || strings.HasPrefix(m, "claude-5") ||
		anthropicFamily(m, "opus") || anthropicFamily(m, "sonnet") || anthropicFamily(m, "haiku") {
		return []string{EffortLow, EffortMedium, EffortHigh}
	}
	return nil
}

// EffortOptionsForModel lists the effort values supported by one concrete model, always leading with
// the vendor default. Unknown providers and models intentionally expose only that safe default.
func EffortOptionsForModel(providerID, model string) []map[string]any {
	var ids []string
	switch EffortProviderFamily(providerID) {
	case "anthropic":
		ids = anthropicEffortIDs(model)
	case "openai":
		ids = openAIEffortIDs(model)
	case "xai":
		ids = xAIEffortIDs(model)
	case "deepseek":
		ids = deepSeekEffortIDs(model)
	case "fake":
		// the offline echo has no model to think with
	}
	return EffortOptionsForIDs(ids)
}

// EffortOptionsForIDs turns a provider-supplied effort list into the same UI shape as the built-in
// table. Invalid or duplicate values are ignored so an external catalog cannot inject an unsupported
// request parameter.
func EffortOptionsForIDs(ids []string) []map[string]any {
	out := []map[string]any{effortTier("")}
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || !ValidEffort(id) || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, effortTier(id))
	}
	return out
}

// EffortSupported reports support before a model is known; EffortSupportedForModel is the precise
// version used when validating a saved bot.
func EffortSupported(providerID, effort string) bool {
	return EffortSupportedForModel(providerID, "", effort)
}

func EffortSupportedForModel(providerID, model, effort string) bool {
	if effort == "" {
		return true
	}
	for _, o := range EffortOptionsForModel(providerID, model) {
		if o["id"] == effort {
			return true
		}
	}
	return false
}

// ValidBotName checks the id used for @mentions. The alphabet is constrained jointly by workspace
// directory names, the membership list and splices such as mcp_<plugin>_<tool>, so it is kept tight.
func ValidBotName(s string) bool { return botNameRe.MatchString(s) }

// BotConfig is a single member entry in bots.yaml.
type BotConfig struct {
	Name        string `yaml:"name" json:"name"`
	Role        string `yaml:"role" json:"role"`
	Description string `yaml:"description" json:"description"`

	// Optional English persona: preferred when the language is English; falls back to the fields above when empty
	RoleEn        string `yaml:"role_en,omitempty" json:"role_en,omitempty"`
	DescriptionEn string `yaml:"description_en,omitempty" json:"description_en,omitempty"`
	Provider      string `yaml:"provider,omitempty" json:"provider,omitempty"`
	Model         string `yaml:"model,omitempty" json:"model,omitempty"`
	BaseURL       string `yaml:"base_url,omitempty" json:"base_url,omitempty"`
	APIKeyEnv     string `yaml:"api_key_env,omitempty" json:"api_key_env,omitempty"`

	// Plugins this bot may use (MCP server names)
	MCP []string `yaml:"mcp,omitempty" json:"mcp,omitempty"`

	// Name shown in the UI (may contain spaces or non-ASCII); falls back to Name. Name stays the @mention id.
	DisplayName string `yaml:"display_name,omitempty" json:"display_name,omitempty"`
	Avatar      string `yaml:"avatar,omitempty" json:"avatar,omitempty"`

	// Catalog entry id (anthropic / openai / xai / …); used only to repopulate the UI form
	ProviderID string `yaml:"provider_id,omitempty" json:"provider_id,omitempty"`
	// Auth mode: key (default) / chatgpt / xai / none. Decides which credential is used per request.
	Auth string `yaml:"auth,omitempty" json:"auth,omitempty"`
	// Permission tier: ask / edit / auto / full; empty means "follow the global setting".
	Permission string `yaml:"permission,omitempty" json:"permission,omitempty"`
	// Reasoning effort: model-dependent; empty means the model's own default.
	Effort string `yaml:"effort,omitempty" json:"effort,omitempty"`

	// Extra instructions appended to the end of the system prompt. The body of an agents/*.md file from
	// a plugin bundle lands here — a "subagent" elsewhere becomes a real member in Bot Bureau, and its
	// role description needs somewhere to live. Appended rather than prepended: the collaboration,
	// permission and memory rules are the engine's floor and must not be overridden by imported text.
	Prompt string `yaml:"prompt,omitempty" json:"prompt,omitempty"`
}

func (c BotConfig) Title() string {
	if s := strings.TrimSpace(c.DisplayName); s != "" {
		return s
	}
	return c.Name
}

// RoleText / DescText pick the persona for the current locale: English prefers the *_en fields, falling back to the base ones.
func (c BotConfig) RoleText() string {
	if i18n.Locale() == "en" && strings.TrimSpace(c.RoleEn) != "" {
		return c.RoleEn
	}
	return c.Role
}

func (c BotConfig) DescText() string {
	if i18n.Locale() == "en" && strings.TrimSpace(c.DescriptionEn) != "" {
		return c.DescriptionEn
	}
	return c.Description
}

type botsFile struct {
	Bots []BotConfig `yaml:"bots"`
}

func LoadBotConfigs(path string) ([]BotConfig, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {

		// No bots.yaml yet means a fresh install: start with an empty team and let onboarding take over
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf(i18n.T("Failed to read %s: %w"), path, err)
	}
	var f botsFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf(i18n.T("Failed to parse %s: %w"), path, err)
	}

	seen := map[string]bool{}
	for _, c := range f.Bots {
		if !botNameRe.MatchString(c.Name) {
			return nil, fmt.Errorf(i18n.T("Invalid bot name %q (must be 1-24 lowercase letters/digits/-/_)"), c.Name)
		}
		if seen[c.Name] {
			return nil, fmt.Errorf(i18n.T("Duplicate bot name %q"), c.Name)
		}
		seen[c.Name] = true
	}
	return f.Bots, nil
}

func SaveBotConfigs(path string, cfgs []BotConfig) error {
	out, err := yaml.Marshal(botsFile{Bots: cfgs})
	if err != nil {
		return err
	}

	// The header follows the UI language: users do open this file to edit it by hand, so the
	// comment has to be in the language they read
	header := []byte(i18n.T("# Team member definitions. Add or remove them in the Electron client, or edit by hand and restart.\n"))
	return os.WriteFile(path, append(header, out...), 0o644)
}
