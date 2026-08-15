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
	// 单条消息触发的智能体循环上限。这是兜底，不是工作量上限——真正拦住原地打转的是
	// MaxRepeatedCalls，它按"有没有进展"判，而这个数按"跑了多少圈"判。
	//
	// 原来是 30。30 圈对"审查整个仓库的文档"这类活远远不够，于是用户要反复说「继续」；
	// 而它对死循环也没什么用——一个认真干活的任务和一个空转的任务，在圈数眼里一模一样。
	// 分成两个数之后各司其职：空转由下面那个掐，这个只防"变着花样转"的极端情况。
	//
	// Cap on agent-loop iterations triggered by a single message. A backstop rather than a budget for
	// work: what actually stops a bot going in circles is MaxRepeatedCalls below, which judges progress,
	// where this one only counts laps.
	//
	// It used to be 30. Thirty laps is nowhere near enough for something like "review every document in
	// this repository", so the user ends up saying "carry on" over and over — while doing little against
	// a real loop, since a task working hard and a task spinning look identical to a lap counter. Split
	// in two, each does its own job: spinning is cut below, and this catches only the extreme case of a
	// loop that varies its arguments every time.
	MaxToolIterations = 200
	// 同一个工具、同样的参数连着调这么多次就掐掉本轮。
	//
	// 这才是"失控"真正的样子：工具报错 → 原样再调一次 → 又报错 → 再调。参数一个字没变，
	// 结果也就不会变，再跑一百圈也一样。auto 档是无人看管跑的，这种空转烧的是用户的额度。
	//
	// 定 5 而不是 2、3：偶尔重试一次是正常的（网络抖动、文件刚被写完还没落盘），
	// 连着五次一模一样就不是重试了。
	//
	// Cut the turn once the same tool has been called with the same arguments this many times in a row.
	//
	// This is what running away actually looks like: a tool errors, the identical call goes out again,
	// errors again, and again. Not one argument changed, so nothing about the result will either, and a
	// hundred more laps would go the same way. The auto tier runs unattended, and that spinning burns
	// the user's quota.
	//
	// Five rather than two or three: retrying once is ordinary (a network blip, a file written a moment
	// ago and not yet visible). Five identical calls in a row is no longer a retry.
	MaxRepeatedCalls = 5
	HistoryLimit     = 60 // 每个会话上下文的消息数上限 / max messages per conversation context
	BashTimeout      = 120 * time.Second
	ToolOutputLimit  = 20000 // 工具输出截断长度（字符） / tool output truncation length (chars)
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
	// 自定义角色说明的上限。导入的成员模板正文动辄几千字，但它每轮都要发一遍。
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
	EffortMinimal = "minimal"
	EffortLow     = "low"
	EffortMedium  = "medium"
	EffortHigh    = "high"
)

func ValidEffort(s string) bool {
	switch s {
	case "", EffortMinimal, EffortLow, EffortMedium, EffortHigh:
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
	case EffortMinimal:
		return 1024 // Anthropic 接受的最小预算 / the smallest budget Anthropic accepts
	case EffortLow:
		return 2048
	case EffortMedium:
		return 6144
	case EffortHigh:
		return 12288
	}
	return 0
}

// 各家能接受的档位并不一致，所以选择器得按服务商来发，不能一张表套所有人。
//
// Anthropic 那边根本没有"档位"这回事，是一个连续的 thinking token 预算，上面那几个数字是本项目
// 自己切的；OpenAI 走 reasoning_effort，比别人多一档 minimal；xAI 的推理模型只认 low 和 high，
// 把 medium 递过去是直接报错。以前四个档位不分服务商地发给所有人，选中不被支持的那档就是一个
// 看不出所以然的 400——用户能做的只有退回"服务商默认"。
//
// The tiers each vendor accepts are not the same, so the picker has to be served per provider rather
// than as one table for everyone.
//
// Anthropic has no notion of tiers at all — it takes a continuous thinking-token budget, and the
// numbers above are this project's own cut of it. OpenAI drives reasoning_effort and has one more tier
// than the rest, minimal. xAI's reasoning models accept only low and high; handing them medium is an
// outright error. The four tiers used to go to every provider alike, so picking an unsupported one
// produced an inscrutable 400 and the only way out was falling back to "vendor default".
// 档位的名字就是接口上那个词本身——minimal / low / medium / high 原样显示，不翻译，也不
// 另起"快 / 均衡 / 充分"这类自造的名字。那套名字是在原词之上又加一层：用户在服务商文档里
// 读到的、填进 bots.yaml 的、出错时被引擎回嘴的都是 low，界面上却写着"快"，对不上号。
// 每档下面那行说明才是解释它的地方，那行照旧翻译。
//
// A tier is named by the word the API itself uses — minimal / low / medium / high, shown verbatim and
// untranslated, rather than under invented names like Quick / Balanced / Thorough. Those names were a
// second layer on top of the real one: what the user reads in the vendor's docs, writes into
// bots.yaml, and gets quoted back in an error is low, while the screen said "Quick". The note under
// each tier is where the explaining belongs, and it stays translated.
func effortTier(id string) map[string]any {
	switch id {
	case EffortMinimal:
		return map[string]any{"id": EffortMinimal, "label": EffortMinimal,
			"note": i18n.T("Barely thinks at all; cheapest and quickest")}
	case EffortLow:
		return map[string]any{"id": EffortLow, "label": EffortLow,
			"note": i18n.T("Answers sooner and costs less; good for chores and short questions")}
	case EffortMedium:
		return map[string]any{"id": EffortMedium, "label": EffortMedium,
			"note": i18n.T("Thinks before most answers")}
	case EffortHigh:
		return map[string]any{"id": EffortHigh, "label": EffortHigh,
			"note": i18n.T("Thinks the longest on hard problems; slower and pricier")}
	}
	// 这一档没有对应的接口取值——它恰恰是"什么都不发"，所以只能给它一个说人话的名字
	// This one has no API value behind it — it is precisely "send nothing", so a spoken name is all
	// there is to give it
	return map[string]any{"id": "", "label": i18n.T("Default"),
		"note": i18n.T("Send no thinking setting at all — the safest choice, and the only one every model accepts")}
}

// EffortOptionsFor 给出某个服务商能用的档位，第一个永远是"默认"。
// 未知的服务商（自建、本地模型）按 OpenAI 兼容处理，那是这类接口的通行写法。
//
// EffortOptionsFor lists the tiers a provider accepts, always leading with the default.
// An unknown provider (self-hosted, local models) is treated as OpenAI-compatible, which is the usual
// spelling for that class of endpoint.
func EffortOptionsFor(providerID string) []map[string]any {
	var ids []string
	switch providerID {
	case "anthropic":
		ids = []string{EffortLow, EffortMedium, EffortHigh}
	case "openai":
		ids = []string{EffortMinimal, EffortLow, EffortMedium, EffortHigh}
	case "xai":
		ids = []string{EffortLow, EffortHigh}
	case "fake":
		// 离线回声没有模型可想 / the offline echo has no model to think with
	default:
		ids = []string{EffortLow, EffortMedium, EffortHigh}
	}
	out := []map[string]any{effortTier("")}
	for _, id := range ids {
		out = append(out, effortTier(id))
	}
	return out
}

// EffortSupported 判断某档位在某服务商上是否可用。保存时用它兜底：
// 换服务商时界面会重挑，但配置文件是可以手写的。
//
// EffortSupported reports whether a tier is available on a provider. It backstops saving: the UI
// re-picks when the provider changes, but the config file can also be written by hand.
func EffortSupported(providerID, effort string) bool {
	if effort == "" {
		return true
	}
	for _, o := range EffortOptionsFor(providerID) {
		if o["id"] == effort {
			return true
		}
	}
	return false
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
	// a plugin bundle lands here — a "subagent" elsewhere becomes a real member in Bot Bureau, and its
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
