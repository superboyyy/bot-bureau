package config

// 权限分级。
//
// 四档，每一档的边界都必须是引擎能真正判定的东西，否则就是假分级：
//
//	ask   每次都问      只读操作直接放行，其余一律等你点头（默认）
//	edit  可改文件      + 工作目录内的写文件免审批
//	auto  自动干活      + 工作目录内的普通命令也免审批
//	full  完全放开      什么都不问
//
// 「工作目录」是两处地方的合称：这位成员自己的目录（data/workspaces/<id>），
// 加上你在对话里亲口指定过的目录（engine/roots.go）。后者是必需的——没有它，
// 用户说「审查一下 /Users/me/proj」之后，那个目录里的每一条命令都算越界，
// auto 档就退化成 ask 档，而用户以为自己已经把工作目录说清楚了。
//
// 两条贯穿所有档位的线：
//
//   - 越出工作目录的动作永远要问，只有 full 例外——否则 auto 就等于 full；
//   - 插件（MCP）的非只读调用同样只有 full 才免审批。引擎判断不了插件的作用域：
//     fs 插件的写文件和 GitHub 插件的建 issue，在协议层长得一模一样，
//     前者只动本机沙箱，后者动的是你的线上仓库。
//
// Permission levels.
//
// Four tiers, where every boundary has to be something the engine can actually decide — otherwise the
// tiers are decorative:
//
//	ask   ask every time     read-only actions pass; everything else waits for you (default)
//	edit  may edit files     + file writes inside the workspace skip approval
//	auto  work unattended    + ordinary commands inside the workspace skip approval too
//	full  no approvals       nothing is ever asked
//
// "The workspace" names two places together: the member's own directory (data/workspaces/<id>) plus
// any directory the user named themselves in conversation (engine/roots.go). The second half is
// necessary — without it, after the user says "review /Users/me/proj" every command in that directory
// counts as escaping and the auto tier decays into ask, while the user believes they have already said
// where the work is.
//
// Two rules cut across every tier:
//
//   - anything leaving the workspace is always asked, except under full — otherwise auto would equal full;
//   - non-read-only plugin (MCP) calls likewise skip approval only under full. The engine cannot judge a
//     plugin's blast radius: an fs plugin writing a file and a GitHub plugin opening an issue look
//     identical at the protocol level, yet one touches a local sandbox and the other your live repo.

import (
	"botbureau/backend/internal/i18n"
	"strings"
)

type PermLevel string

const (
	PermAsk  PermLevel = "ask"
	PermEdit PermLevel = "edit"
	PermAuto PermLevel = "auto"
	PermFull PermLevel = "full"
)

// DefaultPerm 是全局默认。故意选最保守的一档：放开权限必须是用户主动的选择。
// DefaultPerm is the global default. Deliberately the most conservative tier: loosening it must be a
// deliberate act by the user.
const DefaultPerm = PermAsk

type ActKind string

const (
	ActBash   ActKind = "bash"
	ActWrite  ActKind = "write"
	ActPlugin ActKind = "plugin"
)

// ToolAct 是一次工具调用的风险画像，由调用点填好后交给权限档位裁决。
// ToolAct is the risk profile of one tool call, filled in at the call site and judged by the tier.
type ToolAct struct {
	Kind     ActKind
	ReadOnly bool
	// Escapes 表示这次动作可能碰到工作目录之外的东西。
	// bash 那边是启发式判断（见 bashLeavesWorkspace），不是沙箱——所以 shell 元字符
	// 也一律算越界：`ls $(rm -rf ~)` 的首词是只读的，参数看起来也在目录内。
	//
	// Escapes means the action may touch something outside the workspace. For bash this is a heuristic
	// (see bashLeavesWorkspace), not containment — which is why shell metacharacters count as escaping
	// too: `ls $(rm -rf ~)` has a read-only first word and arguments that look local.
	Escapes bool
	// Dir 是"把哪个目录收进工作目录，这次动作就不再越界"。只有 Escapes 为真时才有值。
	//
	// 它存在是因为解析一句中文里的目录名解析不出来：用户说「把 bot-bureau 当工作目录」，
	// 引擎不知道 bot-bureau 在哪；而到了审批这一刻，命令里写的就是那个目录的全路径，
	// 是哪儿已经没有悬念了。审批卡把它原样印出来，用户点的是他看见的那一个。
	//
	// Dir is "the directory which, taken into the workspace, stops this action from escaping". Set only
	// when Escapes is true.
	//
	// It exists because a directory named in a sentence cannot be resolved: told "treat bot-bureau as
	// your working directory", the engine has no idea where bot-bureau is. By approval time the command
	// spells the full path out, and there is nothing left to guess. The approval card prints it
	// verbatim, so what the user clicks is the directory they can see.
	Dir string
}

func ValidPerm(s string) bool {
	switch PermLevel(s) {
	case PermAsk, PermEdit, PermAuto, PermFull:
		return true
	}
	return false
}

// NormalizePerm 把外部传进来的值收敛成合法档位；空串或非法值返回空串，
// 由调用方决定回落到全局默认还是 DefaultPerm。
//
// NormalizePerm coerces an external value into a valid tier; an empty or invalid one yields the empty
// string, leaving the caller to fall back to the global default or DefaultPerm.
func NormalizePerm(s string) PermLevel {
	s = strings.ToLower(strings.TrimSpace(s))
	if ValidPerm(s) {
		return PermLevel(s)
	}
	return ""
}

// ResolvePerm 定出实际生效的档位：bot 自己设了就用自己的，否则跟随全局，全局也没有就用默认。
// ResolvePerm settles the effective tier: a bot's own setting wins, else the global default, else DefaultPerm.
func ResolvePerm(botLevel, globalLevel string) PermLevel {
	if p := NormalizePerm(botLevel); p != "" {
		return p
	}
	if p := NormalizePerm(globalLevel); p != "" {
		return p
	}
	return DefaultPerm
}

// NeedsApproval 判断这次动作在该档位下要不要先问人。
// NeedsApproval reports whether this action must wait for a human under this tier.
func (p PermLevel) NeedsApproval(a ToolAct) bool {
	if a.ReadOnly {
		return false
	}
	if p == PermFull {
		return false
	}
	if a.Escapes {
		return true
	}
	switch a.Kind {
	case ActWrite:
		return p != PermEdit && p != PermAuto
	case ActBash:
		return p != PermAuto
	}
	// 插件以及将来新增的动作类型：默认要问，新增能力不该因为忘了改这里就自动放行。
	// Plugins, and any action kind added later: ask by default — a new capability must not become
	// auto-approved just because someone forgot to update this switch.
	return true
}

// PermScopeNote 说清楚「工作目录」在这些说明里指哪儿。
//
// 三档说明里都写着"工作目录内免审批"，而每一档都不适合再解释一遍那是哪——
// 选择器里的一行注释放不下，重复三遍也读不进去。这句话由界面挨着选择器单独放一次。
//
// PermScopeNote says which directories "the workspace" means in the notes below.
//
// Three of the tiers promise "no approvals inside the workspace", and none of them is the place to
// explain which directories those are: a one-line note in a picker has no room, and saying it three
// times over gets read none. The UI places this once, next to the picker.
func PermScopeNote() string {
	return i18n.T("Working directories are the member's own folder plus any directory you name in the conversation — say \"look at /Users/me/proj\" and that folder becomes one of theirs. Each member's settings list them, and you can remove any of them there.")
}

// PermOptions 给客户端渲染选择器用（标签随界面语言）。
// PermOptions feeds the client's picker (labels follow the UI language).
func PermOptions() []map[string]any {
	return []map[string]any{
		{"id": string(PermAsk), "label": i18n.T("Ask every time"),
			"note": i18n.T("Read-only actions run; file writes, commands and plugin calls all wait for you")},
		{"id": string(PermEdit), "label": i18n.T("May edit files"),
			"note": i18n.T("File writes inside its working directories skip approval; commands and plugins still ask")},
		{"id": string(PermAuto), "label": i18n.T("Work unattended"),
			"note": i18n.T("Writes and ordinary commands inside its working directories skip approval; anything outside them, and plugins, still ask")},
		{"id": string(PermFull), "label": i18n.T("No approvals"),
			"note": i18n.T("No approvals at all: a bot can touch any file on this machine, run any command and call any plugin — only use this if you fully accept that")},
	}
}
