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
	// Opus 5 默认开思考，思考 token 也计入，不要设太小
	// Opus 5 enables thinking by default and thinking tokens count too, so don't set this too small
	MaxTokens = 16000
	// 单条消息触发的智能体循环上限（防失控）
	// Cap on agent-loop iterations triggered by a single message (runaway guard)
	MaxToolIterations = 30
	HistoryLimit      = 60 // 每个会话上下文的消息数上限 / max messages per conversation context
	BashTimeout       = 120 * time.Second
	ToolOutputLimit   = 20000 // 工具输出截断长度（字符） / tool output truncation length (chars)
)

// 待审批操作无人处理时自动拒绝的时限。测试里会改短，所以它是原子量而不是普通变量：
// 读它的是每个等待审批的 goroutine，测试改它的那一刻就是一次真实的数据竞争（-race 会报），
// 哪怕生产里启动之后再没人写过。
//
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

// SetApprovalTimeout 目前只有测试会用；传 0 恢复默认。
// SetApprovalTimeout is used only by tests today; passing 0 restores the default.
func SetApprovalTimeout(d time.Duration) { approvalTimeout.Store(int64(d)) }

var (
	botNameRe   = regexp.MustCompile(`^[a-z0-9_-]{1,24}$`)
	avatarHexRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
)

// 头像可以是一个底色，也可以是一张内嵌小图；留空表示按名字哈希出默认底色。
// 内嵌图存进 bots.yaml，所以要卡住体积——客户端会先缩到 96px 再上传。
// An avatar is either a fill color or a small embedded image; empty means "derive a default from the name".
// Embedded images live in bots.yaml, so the size is capped — the client downscales to 96px before sending.
const (
	AvatarMaxBytes = 250000
	DisplayNameMax = 32
	// 自定义角色说明的上限。导入的同事模板正文动辄几千字，但它每轮都要发一遍。
	// Cap on the custom role instructions. An imported teammate template easily runs to thousands of
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

// 思考强度。各家的旋钮不一样，但用户面对的是同一个三档选择：
//
//	Anthropic —— 扩展思考的 token 预算（budget_tokens）
//	OpenAI Responses / Codex —— reasoning.effort
//	其它 OpenAI 兼容端点（xAI 等）—— reasoning_effort
//
// 留空就一个字段都不发，让服务商用自己的默认——这一点很重要：不是所有模型都支持思考参数，
// 硬塞给一个不认识它的模型会直接 400。
//
// Reasoning effort. Every vendor exposes a different knob, but the user faces one three-way choice:
//
//	Anthropic — the extended-thinking token budget (budget_tokens)
//	OpenAI Responses / Codex — reasoning.effort
//	other OpenAI-compatible endpoints (xAI and friends) — reasoning_effort
//
// Empty sends no field at all and leaves the vendor's default in place, which matters: not every model
// accepts a thinking parameter, and forcing one on a model that does not know it is a plain 400.
const (
	EffortLow    = "low"
	EffortMedium = "medium"
	EffortHigh   = "high"
)

func ValidEffort(s string) bool {
	switch s {
	case "", EffortLow, EffortMedium, EffortHigh:
		return true
	}
	return false
}

// ThinkingBudget 把档位换算成 Anthropic 的思考 token 预算；0 表示不开扩展思考。
// 上限压在 MaxTokens 之下：预算比总输出上限还大的话请求直接会被拒。
//
// ThinkingBudget converts a tier into Anthropic's thinking token budget; 0 leaves extended thinking off.
// It stays below MaxTokens: a budget larger than the total output cap is rejected outright.
func ThinkingBudget(effort string) int64 {
	switch effort {
	case EffortLow:
		return 2048
	case EffortMedium:
		return 6144
	case EffortHigh:
		return 12288
	}
	return 0
}

// EffortOptions 给客户端渲染选择器用。
// EffortOptions feeds the client's picker.
func EffortOptions() []map[string]any {
	return []map[string]any{
		{"id": "", "label": i18n.T("Vendor default"),
			"note": i18n.T("Send no thinking setting at all — the safest choice, and the only one every model accepts")},
		{"id": EffortLow, "label": i18n.T("Quick"),
			"note": i18n.T("Answers sooner and costs less; good for chores and short questions")},
		{"id": EffortMedium, "label": i18n.T("Balanced"),
			"note": i18n.T("Thinks before most answers")},
		{"id": EffortHigh, "label": i18n.T("Thorough"),
			"note": i18n.T("Thinks the longest on hard problems; slower and pricier")},
	}
}

// ValidBotName 校验 @提及 用的 id。这个字母表是被工作目录名、群成员表和
// mcp_<插件>_<工具> 这类拼接共同约束的，所以收得比较紧。
// ValidBotName checks the id used for @mentions. The alphabet is constrained jointly by workspace
// directory names, the membership list and splices such as mcp_<plugin>_<tool>, so it is kept tight.
func ValidBotName(s string) bool { return botNameRe.MatchString(s) }

// BotConfig 是 bots.yaml 里的一个成员条目。
// BotConfig is a single member entry in bots.yaml.
type BotConfig struct {
	Name        string `yaml:"name" json:"name"`
	Role        string `yaml:"role" json:"role"`
	Description string `yaml:"description" json:"description"`
	// 可选的英文版人设：语言切到英文时优先使用，留空则沿用上面的字段
	// Optional English persona: preferred when the language is English; falls back to the fields above when empty
	RoleEn        string `yaml:"role_en,omitempty" json:"role_en,omitempty"`
	DescriptionEn string `yaml:"description_en,omitempty" json:"description_en,omitempty"`
	Provider      string `yaml:"provider,omitempty" json:"provider,omitempty"`
	Model         string `yaml:"model,omitempty" json:"model,omitempty"`
	BaseURL       string `yaml:"base_url,omitempty" json:"base_url,omitempty"`
	APIKeyEnv     string `yaml:"api_key_env,omitempty" json:"api_key_env,omitempty"`
	// 该 bot 可用的插件（MCP server 名）
	// Plugins this bot may use (MCP server names)
	MCP []string `yaml:"mcp,omitempty" json:"mcp,omitempty"`
	// 界面上显示的名字（可含中文/空格），留空就用 Name；Name 始终是 @提及 用的 id
	// Name shown in the UI (may contain spaces or non-ASCII); falls back to Name. Name stays the @mention id.
	DisplayName string `yaml:"display_name,omitempty" json:"display_name,omitempty"`
	Avatar      string `yaml:"avatar,omitempty" json:"avatar,omitempty"`
	// 服务商目录里的条目 id（anthropic / openai / xai / …），只用于界面回填表单
	// Catalog entry id (anthropic / openai / xai / …); used only to repopulate the UI form
	ProviderID string `yaml:"provider_id,omitempty" json:"provider_id,omitempty"`
	// 接入方式：key（默认）/ chatgpt / xai / none。决定发请求时拿哪份凭据。
	// Auth mode: key (default) / chatgpt / xai / none. Decides which credential is used per request.
	Auth string `yaml:"auth,omitempty" json:"auth,omitempty"`
	// 权限档位：ask / edit / auto / full，留空表示跟随全局设置。
	// Permission tier: ask / edit / auto / full; empty means "follow the global setting".
	Permission string `yaml:"permission,omitempty" json:"permission,omitempty"`
	// 思考强度：low / medium / high，留空 = 用服务商自己的默认。
	// Reasoning effort: low / medium / high; empty means the vendor's own default.
	Effort string `yaml:"effort,omitempty" json:"effort,omitempty"`
	// 附加到系统提示词末尾的自定义说明。插件包里的 agents/*.md 正文就落在这里——
	// 别处的"子代理"到了 Bot Bureau 是一位真正的团队成员，它的角色说明得有地方安放。
	// 放在末尾而不是开头：团队协作、权限、记忆那些规则是引擎的底线，不该被导入的提示词覆盖掉。
	//
	// Extra instructions appended to the end of the system prompt. The body of an agents/*.md file from
	// a plugin bundle lands here — a "subagent" elsewhere becomes a real teammate in Bot Bureau, and its
	// role description needs somewhere to live. Appended rather than prepended: the collaboration,
	// permission and memory rules are the engine's floor and must not be overridden by imported text.
	Prompt string `yaml:"prompt,omitempty" json:"prompt,omitempty"`
}

// Title 取界面上该用的名字。/ Title returns the name to show in the UI.
func (c BotConfig) Title() string {
	if s := strings.TrimSpace(c.DisplayName); s != "" {
		return s
	}
	return c.Name
}

// RoleText / DescText 按当前语言取人设：英文下优先用 *_en，没填就沿用原字段。
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
		// 还没有 bots.yaml = 全新安装，空团队起步，交给客户端的引导
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
	// 表头跟随界面语言：这个文件用户会打开手改，注释得是他看得懂的那门语言
	// The header follows the UI language: users do open this file to edit it by hand, so the
	// comment has to be in the language they read
	header := []byte(i18n.T("# Team member definitions. Add or remove them in the Electron client, or edit by hand and restart.\n"))
	return os.WriteFile(path, append(header, out...), 0o644)
}
