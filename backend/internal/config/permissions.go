package config

// Permission levels.

// Four tiers, where every boundary has to be something the engine can actually decide — otherwise the
// tiers are decorative:

// ask   ask every time     read-only actions pass; everything else waits for you (default)
// edit  may edit files     + file writes inside the workspace skip approval
// auto  work unattended    + ordinary commands inside the workspace skip approval too
// full  no approvals       nothing is ever asked

// "The workspace" names two places together: the member's own directory (data/workspaces/<id>) plus
// any directory the user named themselves in conversation (engine/roots.go). The second half is
// necessary — without it, after the user says "review /Users/me/proj" every command in that directory
// counts as escaping and the auto tier decays into ask, while the user believes they have already said
// where the work is.

// Two rules cut across every tier:

// - anything leaving the workspace is always asked, except under full — otherwise auto would equal full;
// - non-read-only plugin (MCP) calls likewise skip approval only under full. The engine cannot judge a
// plugin's blast radius: an fs plugin writing a file and a GitHub plugin opening an issue look
// identical at the protocol level, yet one touches a local sandbox and the other your live repo.

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

// DefaultPerm is the global default. Deliberately the most conservative tier: loosening it must be a
// deliberate act by the user.
const DefaultPerm = PermAsk

type ActKind string

const (
	ActBash   ActKind = "bash"
	ActWrite  ActKind = "write"
	ActPlugin ActKind = "plugin"
)

// ToolAct is the risk profile of one tool call, filled in at the call site and judged by the tier.
type ToolAct struct {
	Kind     ActKind
	ReadOnly bool

	// Escapes means the action may touch something outside the workspace. For bash this is a heuristic
	// (see bashLeavesWorkspace), not containment — which is why shell metacharacters count as escaping
	// too: `ls $(rm -rf ~)` has a read-only first word and arguments that look local.
	Escapes bool

	// Dir is "the directory which, taken into the workspace, stops this action from escaping". Set only
	// when Escapes is true.

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

// NormalizePerm coerces an external value into a valid tier; an empty or invalid one yields the empty
// string, leaving the caller to fall back to the global default or DefaultPerm.
func NormalizePerm(s string) PermLevel {
	s = strings.ToLower(strings.TrimSpace(s))
	if ValidPerm(s) {
		return PermLevel(s)
	}
	return ""
}

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

	// Plugins, and any action kind added later: ask by default — a new capability must not become
	// auto-approved just because someone forgot to update this switch.
	return true
}

// PermScopeNote says which directories "the workspace" means in the notes below.

// Three of the tiers promise "no approvals inside the workspace", and none of them is the place to
// explain which directories those are: a one-line note in a picker has no room, and saying it three
// times over gets read none. The UI places this once, next to the picker.
func PermScopeNote() string {
	return i18n.T("Working directories are the member's own folder plus any directory you name in the conversation — say \"look at /Users/me/proj\" and that folder becomes one of theirs. Each member's settings list them, and you can remove any of them there.")
}

// PermOptions feeds the client's picker (labels follow the UI language).
func PermOptions() []map[string]any {
	return []map[string]any{
		{"id": string(PermAsk), "label": i18n.T("Ask every time"),
			"note": i18n.T("Read-only actions run; file writes, commands, and plugin calls all require your approval.")},
		{"id": string(PermEdit), "label": i18n.T("Can edit files"),
			"note": i18n.T("File writes inside its working directories skip approval; commands and plugins still require approval.")},
		{"id": string(PermAuto), "label": i18n.T("Work unattended"),
			"note": i18n.T("Writes and ordinary commands inside its working directories skip approval; anything outside them and all plugins still require approval.")},
		{"id": string(PermFull), "label": i18n.T("No approvals"),
			"note": i18n.T("No approvals at all: a bot can access any file on this machine, run any command, and call any plugin — use this only if you fully accept the risk")},
	}
}
