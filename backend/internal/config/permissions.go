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

// PermOptions 给客户端渲染选择器用（标签随界面语言）。
// PermOptions feeds the client's picker (labels follow the UI language).
func PermOptions() []map[string]any {
	return []map[string]any{
		{"id": string(PermAsk), "label": i18n.T("Ask every time"),
			"note": i18n.T("Read-only actions run; file writes, commands and plugin calls all wait for you")},
		{"id": string(PermEdit), "label": i18n.T("May edit files"),
			"note": i18n.T("File writes inside the workspace skip approval; commands and plugins still ask")},
		{"id": string(PermAuto), "label": i18n.T("Work unattended"),
			"note": i18n.T("Writes and ordinary commands inside the workspace skip approval; anything leaving it, and plugins, still ask")},
		{"id": string(PermFull), "label": i18n.T("No approvals"),
			"note": i18n.T("No approvals at all: a bot can touch any file on this machine, run any command and call any plugin — only use this if you fully accept that")},
	}
}
