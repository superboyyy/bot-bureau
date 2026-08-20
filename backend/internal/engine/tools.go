package engine

import (
	"botbureau/backend/internal/config"
	"botbureau/backend/internal/model"
	"botbureau/backend/internal/plugin"
	"botbureau/backend/internal/sandbox"
	"botbureau/backend/internal/secret"
	"botbureau/backend/internal/skill"
	"botbureau/backend/internal/textutil"

	"botbureau/backend/internal/i18n"

	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The whitelist and the segment-by-segment reading live in shellscan.go. Command substitution makes
// what a command will actually touch undecidable before it runs; scanBash reports it and it always
// counts as "may escape" — the auto tier only auto-approves commands whose targets are knowable.

func truncateOutput(s string) string {
	if len(s) > config.ToolOutputLimit {
		return s[:config.ToolOutputLimit] + i18n.T("\n...(output too long, truncated)")
	}
	return s
}

// Toolbox is one bot's client-side toolbox. currentChat is set by the bot before each message is handled.
type Toolbox struct {
	botName   string
	workspace string
	// directories the user named, treated like workspace
	roots       *Roots
	mem         *Memory            // personal memory
	teamMem     *Memory            // team memory, one copy shared by all bots
	board       *TaskBoard         // the team task board
	mcp         *plugin.MCPManager // plugin (MCP server) manager
	mcpServers  []string           // plugins this bot subscribes to
	deps        *TeamDeps          // live team deps (SubscribeMCP / MCPAuth may be wired after construction)
	skills      *skill.Manager     // the skill library, shared by the whole team
	bus         *Bus
	sched       *Scheduler
	currentChat string // "group" or "dm"
	turnCtx     context.Context
	botPerm     string           // this bot's tier; empty follows the global one
	settings    *config.Settings // source of the global tier
	ks          *secret.KeyStore
	audit       *AuditLog
	sbx         sandbox.Runner
	sbxTmp      string
}

// perm settles the tier in force for this call. It is resolved per call rather than fixed at startup:
// changing the tier in config.Settings must take effect immediately, without restarting the engine or the bot.
func (t *Toolbox) perm() config.PermLevel {
	global := ""
	if t.settings != nil {
		global = t.settings.Perm()
	}
	return config.ResolvePerm(t.botPerm, global)
}

// gate is the single approval checkpoint: it suspends for a human when required and returns true to
// proceed. Every tool goes through it, so a new tool that forgets the gate stands out.
func (t *Toolbox) gate(act config.ToolAct, action, waitMsg string) (string, bool, bool) {
	_, reason, rejected, ok := t.gateRun(act, action, waitMsg, "", "", "", "")
	return reason, rejected, ok
}

func (t *Toolbox) gatePath(act config.ToolAct, action, waitMsg, diff, path string) (string, bool, bool) {
	_, reason, rejected, ok := t.gateRun(act, action, waitMsg, diff, path, "", "")
	return reason, rejected, ok
}

// gateRun is the gate plus the bash line that should actually run. command is the original bash
// (empty for writes and plugins). After an approve with an edited command, run is that string.
func (t *Toolbox) gateRun(act config.ToolAct, action, waitMsg, diff, path, command, isolate string) (run, reason string, rejected, proceed bool) {
	run = command
	record := func(allowed bool, id int, why, ran string) {
		if t == nil || !t.shouldAudit(act) {
			return
		}
		rec := auditLine{
			ID: id, Bot: t.botName, Act: auditKind(act, action),
			Path: path, Command: ran, Allowed: allowed, Reason: why,
			Isolate: isolate,
		}
		if command != "" && ran != "" && command != ran {
			rec.Command = ran
			rec.Original = command
		} else if command != "" {
			rec.Command = command
		}
		if rec.Act == "plugin" && rec.Command == "" {
			rec.Command = action
		}
		t.audit.Record(rec)
	}
	if !t.perm().NeedsApproval(act) {
		record(true, 0, "", command)
		return command, "", false, true
	}
	req := t.bus.requestApproval(t.botName, action, t.eventChat(), act.Dir, diff, command)
	t.bus.Emit("tool", t.eventChat(), t.botName, fmt.Sprintf(waitMsg, req.ID), nil)
	approved, reason := t.awaitApproval(req)
	if approved {
		if c := strings.TrimSpace(req.RunCommand()); c != "" {
			run = c
		}
		record(true, req.ID, "", run)
		return run, "", false, true
	}
	record(false, req.ID, reason, command)
	return run, reason, true, false
}

// denied renders the rejection reason into one sentence for the model.
func denied(what, reason string) string {
	if reason == "" {
		return what
	}
	return what + i18n.T(", reason: ") + reason
}

func NewToolbox(botName, workspace string, roots *Roots, mem *Memory, deps *TeamDeps, mcpServers []string, bus *Bus, sched *Scheduler, botPerm string) *Toolbox {
	sbx := deps.Sandbox
	if sbx == nil {
		sbx = sandbox.Detect()
	}
	return &Toolbox{
		botName: botName, workspace: workspace, roots: roots, mem: mem,
		teamMem: deps.TeamMem, board: deps.Board, mcp: deps.MCP, mcpServers: mcpServers, skills: deps.Skills,
		deps: deps,
		bus:  bus, sched: sched, botPerm: botPerm, settings: deps.Settings, ks: deps.KS, audit: deps.Audit,
		sbx: sbx,
	}
}

// SetMCPServers changes which plugins this bot subscribes to.
func (t *Toolbox) SetMCPServers(names []string) { t.mcpServers = names }

// eventChat maps the internal chat name to the chat identifier used in the event stream.
func (t *Toolbox) eventChat() string {
	if IsGroupChat(t.currentChat) {
		return t.currentChat
	}
	return "dm:" + t.botName
}

// Defs returns the client-side tool definitions (server-side web tools are appended by the provider layer as needed).
func (t *Toolbox) Defs() []model.ToolDef {
	var members []string
	for _, n := range t.bus.GroupMembersOf(t.currentChat) {
		if n != t.botName {
			members = append(members, n)
		}
	}
	defs := []model.ToolDef{
		t.bashToolDef(),
		{
			Name:        "fetch_url",
			Description: i18n.T("Read a web page or API response and get it back as text (HTML is stripped down to its readable text). This is how you look something up online — it runs straight away and never needs approval, so reach for it rather than curl. It only fetches; it cannot post, log in, or reach addresses on this machine or this local network."),
			Properties: map[string]any{
				"url": map[string]any{"type": "string", "description": i18n.T("The http or https address to read")},
			},
			Required: []string{"url"},
		},
		{
			Name:        "web_search",
			Description: i18n.T("Search the public web and get up to eight title/url/snippet rows. Then fetch_url a URL you chose. It runs straight away and never needs approval. A Brave or Tavily key in Settings is used when present; otherwise a DuckDuckGo HTML page. It will not open addresses on this machine or this local network."),
			Properties: map[string]any{
				"query": map[string]any{"type": "string", "description": i18n.T("What to search for")},
			},
			Required: []string{"query"},
		},
		{
			Name:        "read_file",
			Description: i18n.T("Read a text file in your workspace. Optional offset (1-based line) and limit (line count) return a numbered window; omit them to read the whole file."),
			Properties: map[string]any{
				"path":   map[string]any{"type": "string", "description": i18n.T("Path relative to the workspace")},
				"offset": map[string]any{"type": "integer", "description": i18n.T("1-based line to start from")},
				"limit":  map[string]any{"type": "integer", "description": i18n.T("Number of lines to return")},
			},
			Required: []string{"path"},
		},
		{
			Name:        "edit_file",
			Description: i18n.T("Replace text in an existing file. Prefer this over write_file when changing a file that is already there: it fails if old_string is not unique, so it will not silently edit the wrong site. Writes go through user approval."),
			Properties: map[string]any{
				"path":        map[string]any{"type": "string", "description": i18n.T("Path relative to the workspace")},
				"old_string":  map[string]any{"type": "string", "description": i18n.T("The exact text to find; must match once unless replace_all is set")},
				"new_string":  map[string]any{"type": "string", "description": i18n.T("Replacement text")},
				"replace_all": map[string]any{"type": "boolean", "description": i18n.T("Replace every match instead of requiring a unique match")},
			},
			Required: []string{"path", "old_string", "new_string"},
		},
		{
			Name:        "write_file",
			Description: i18n.T("Write (overwrite) a text file in your workspace; parent directories are created automatically. Use this to create a new file. For an existing file prefer edit_file. Writes are submitted to the user for approval first."),
			Properties: map[string]any{
				"path":    map[string]any{"type": "string", "description": i18n.T("Path relative to the workspace")},
				"content": map[string]any{"type": "string", "description": i18n.T("The complete file content")},
			},
			Required: []string{"path", "content"},
		},
		{
			Name:        "grep",
			Description: i18n.T("Search file contents in your workspace and the directories the user pointed you at. The pattern is a regular expression. Prefer this over bash grep."),
			Properties: map[string]any{
				"pattern": map[string]any{"type": "string", "description": i18n.T("Regular expression to search for")},
				"path":    map[string]any{"type": "string", "description": i18n.T("Directory or file to search; defaults to the workspace")},
				"glob":    map[string]any{"type": "string", "description": i18n.T("Only files whose relative path matches this glob (e.g. **/*.go)")},
				"max":     map[string]any{"type": "integer", "description": i18n.T("Maximum matches to return")},
			},
			Required: []string{"pattern"},
		},
		{
			Name:        "glob",
			Description: i18n.T("List files whose names match a glob pattern (supports **). Prefer this over bash find."),
			Properties: map[string]any{
				"pattern": map[string]any{"type": "string", "description": i18n.T("Glob pattern relative to the workspace, e.g. **/*.md")},
			},
			Required: []string{"pattern"},
		},
		{
			Name:        "remember",
			Description: i18n.T("Write a piece of information worth keeping across sessions into long-term memory. scope=self stores it in your personal memory; scope=team stores it in the team-wide shared memory (visible to every bot, suitable for user preferences and team conventions). Pass id to replace that note. Do not record something that is already remembered."),
			Properties: map[string]any{
				"note":  map[string]any{"type": "string", "description": i18n.T("One sentence covering the point and why it matters")},
				"scope": map[string]any{"type": "string", "enum": []string{"self", "team"}, "description": i18n.T("Defaults to self")},
				"id":    map[string]any{"type": "string", "description": i18n.T("Existing note id to replace; omit to append")},
			},
			Required: []string{"note"},
		},
		{
			Name:        "recall",
			Description: i18n.T("Load the full text of stored notes. Match by id or by a substring of the body. The system prompt only lists ids and first clauses."),
			Properties: map[string]any{
				"query": map[string]any{"type": "string", "description": i18n.T("Note id or a word from the body")},
				"scope": map[string]any{"type": "string", "enum": []string{"self", "team", "both"}, "description": i18n.T("Defaults to both")},
			},
			Required: []string{"query"},
		},
		{
			Name:        "forget",
			Description: i18n.T("Delete a stored note by id so it no longer appears in the memory roster."),
			Properties: map[string]any{
				"id":    map[string]any{"type": "string", "description": i18n.T("The note id from the roster or from recall")},
				"scope": map[string]any{"type": "string", "enum": []string{"self", "team", "both"}, "description": i18n.T("Defaults to both")},
			},
			Required: []string{"id"},
		},
		{
			Name:        "search_history",
			Description: i18n.T("Search this conversation's saved messages. Use it after context has been compacted to look up an earlier decision or a line the user said."),
			Properties: map[string]any{
				"query": map[string]any{"type": "string", "description": i18n.T("Plain text to find in this chat")},
			},
			Required: []string{"query"},
		},
		{
			Name:        "todo_write",
			Description: i18n.T("Replace your personal checklist with this list. Each item is {id, content, status: pending|done}. This is not the group task board and never assigns work to anyone else. Use it to track multi-step work; call it again to update statuses."),
			Properties: map[string]any{
				"items": map[string]any{
					"type":        "array",
					"description": i18n.T("The complete list; omit items or pass [] to clear it"),
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"id":      map[string]any{"type": "string", "description": i18n.T("Short stable id (letters, digits, - or _)")},
							"content": map[string]any{"type": "string", "description": i18n.T("What to do")},
							"status":  map[string]any{"type": "string", "enum": []string{"pending", "done"}},
						},
						"required": []string{"content"},
					},
				},
			},
			Required: []string{"items"},
		},
		{
			Name:        "submit_plan",
			Description: i18n.T("Show the user a plan and wait for them to accept or reject it. If the work will touch more than one file, call todo_write first, then this, and wait. Do not start those edits until the plan is accepted."),
			Properties: map[string]any{
				"title": map[string]any{"type": "string", "description": i18n.T("Short title for the plan")},
				"body":  map[string]any{"type": "string", "description": i18n.T("The plan: what you will change and in what order")},
			},
			Required: []string{"title", "body"},
		},
		{
			Name:        "save_routine",
			Description: i18n.T("When the user wants something done automatically on a regular / recurring basis, save it as a scheduled routine. When it fires, the prompt is sent to you automatically as a group chat task."),
			Properties: map[string]any{
				"name":   map[string]any{"type": "string", "description": i18n.T("Routine name (short and unique; an existing routine with the same name is overwritten)")},
				"prompt": map[string]any{"type": "string", "description": i18n.T("Description of the task to run when it fires; must be self-contained")},
				"every_minutes": map[string]any{
					"type": "integer", "description": i18n.T("Interval in minutes, e.g. hourly = 60, daily = 1440"),
				},
			},
			Required: []string{"name", "prompt", "every_minutes"},
		},
	}

	// read_skill is only offered when skills actually exist: a tool for reading them with none installed
	// invites the model to guess at names and collect a run of "no such skill" replies.
	if names := t.skills.Names(); len(names) > 0 {
		defs = append(defs, model.ToolDef{
			Name:        "read_skill",
			Description: i18n.T("Load the full instructions for one of the skills listed in your system prompt. Call it before starting work of that kind, not after."),
			Properties: map[string]any{
				"name": map[string]any{
					"type": "string", "enum": names,
					"description": i18n.T("Skill name, exactly as listed"),
				},
			},
			Required: []string{"name"},
		})
	}
	defs = append(defs,
		model.ToolDef{
			Name:        "assign_task",
			Description: i18n.T("(Group chat only) Create a subtask on the team task board and assign it a single owner. You must use it when breaking work down: exactly one owner per subtask, so nobody duplicates work. When you assign a task to someone else, they get notified."),
			Properties: map[string]any{
				"to":     map[string]any{"type": "string", "description": i18n.T("Owner (the name of a group chat member; may be yourself)")},
				"title":  map[string]any{"type": "string", "description": i18n.T("Task title (short)")},
				"detail": map[string]any{"type": "string", "description": i18n.T("What the task requires; must be self-contained")},
			},
			Required: []string{"to", "title"},
		},
		model.ToolDef{
			Name:        "update_task",
			Description: i18n.T("(Group chat only) Update a task's status on the board: set doing before you start, and set done with a result note once you finish. This lets everyone see that the task has been claimed/completed, so nobody redoes it."),
			Properties: map[string]any{
				"id":     map[string]any{"type": "integer", "description": i18n.T("Task ID")},
				"status": map[string]any{"type": "string", "enum": []string{"todo", "doing", "done"}},
				"note":   map[string]any{"type": "string", "description": i18n.T("Progress or result note (optional)")},
			},
			Required: []string{"id", "status"},
		},
		model.ToolDef{
			Name:        "list_tasks",
			Description: i18n.T("(Group chat only) View the team task board. Check here first when you get a broad task, and do not redo work that already has an owner."),
			Properties:  map[string]any{},
			Required:    []string{},
		},
		model.ToolDef{
			Name:        "list_connectors",
			Description: i18n.T("List built-in connectors (GitHub, Atlassian/Jira, Linear, Notion, Sentry, …): which are installed, which you already have, and what setup they need. Call this when the user mentions a service you do not yet have tools for."),
			Properties:  map[string]any{},
			Required:    []string{},
		},
		model.ToolDef{
			Name:        "enable_connector",
			Description: i18n.T("Install a built-in connector from the catalog (if needed) and enable it for you so its mcp_* tools appear. Requires user approval. Only catalog names are allowed — never invent a command or URL. For Jira use name=atlassian (or jira). After enabling, call the new mcp_* tools on your next step."),
			Properties: map[string]any{
				"name": map[string]any{
					"type":        "string",
					"description": i18n.T("Catalog id (github, atlassian, linear, notion, sentry, fs, …) or alias (jira, gh)"),
				},
				"path": map[string]any{
					"type":        "string",
					"description": i18n.T("Directory to expose (required for filesystem/fs)"),
				},
				"api_key": map[string]any{
					"type":        "string",
					"description": i18n.T("API key / token when the connector needs one and none is saved yet (e.g. GitHub PAT)"),
				},
			},
			Required: []string{"name"},
		},
	)

	// Tools from subscribed MCP plugins (names get an mcp_<plugin>_ prefix to avoid cross-plugin name collisions).
	for _, server := range t.mcpServers {
		for _, mt := range t.mcp.Tools(server) {
			gate := i18n.T("Operations that are not read-only go through user approval first.")
			if mt.ReadOnly {
				gate = i18n.T("Read-only operation, runs directly.")
			}
			defs = append(defs, model.ToolDef{
				Name:         plugin.MCPToolName(server, mt.Name),
				Description:  fmt.Sprintf(i18n.T("[Plugin %s] %s %s"), server, mt.Description, gate),
				Properties:   mt.Properties,
				Required:     mt.Required,
				SchemaExtras: mt.Extras,
			})
		}
	}
	if len(members) > 0 {
		defs = append(defs, model.ToolDef{
			Name:        "message_bot",
			Description: i18n.T("(Group chat only) Hand a subtask off to another AI member, or report results to them / ask them a question. The message must be self-contained (they cannot see your context). They handle it asynchronously, and their reply reaches you as a new group chat message."),
			Properties: map[string]any{
				"to":      map[string]any{"type": "string", "enum": members, "description": i18n.T("Recipient bot")},
				"content": map[string]any{"type": "string", "description": i18n.T("Message content")},
			},
			Required: []string{"to", "content"},
		})
	}
	return defs
}

// Execute runs one client-side tool and returns (result text, images, whether it errored).
func (t *Toolbox) Execute(name string, input map[string]any) (string, []model.ResultImage, bool) {
	str := func(k string) string { v, _ := input[k].(string); return v }
	text := func(s string, err bool) (string, []model.ResultImage, bool) { return s, nil, err }
	switch name {
	case "bash":
		s, err := t.runBash(str("command"), boolArg(input, "unsandboxed"))
		return text(s, err)
	case "fetch_url":
		s, err := t.runFetchURL(str("url"))
		return text(s, err)
	case "web_search":
		s, err := t.runWebSearch(str("query"))
		return text(s, err)
	case "read_file":
		off, hasOff := intArg(input, "offset")
		lim, hasLim := intArg(input, "limit")
		s, err := t.runReadFile(str("path"), off, lim, hasOff, hasLim)
		return text(s, err)
	case "edit_file":
		s, err := t.runEditFile(str("path"), str("old_string"), str("new_string"), boolArg(input, "replace_all"))
		return text(s, err)
	case "write_file":
		s, err := t.runWriteFile(str("path"), str("content"))
		return text(s, err)
	case "grep":
		max, hasMax := intArg(input, "max")
		s, err := t.runGrep(str("pattern"), str("path"), str("glob"), max, hasMax)
		return text(s, err)
	case "glob":
		s, err := t.runGlob(str("pattern"))
		return text(s, err)
	case "remember":
		s, err := t.runRemember(str("note"), str("scope"), str("id"))
		return text(s, err)
	case "recall":
		s, err := t.runRecall(str("query"), str("scope"))
		return text(s, err)
	case "forget":
		s, err := t.runForget(str("id"), str("scope"))
		return text(s, err)
	case "search_history":
		s, err := t.runSearchHistory(str("query"))
		return text(s, err)
	case "todo_write":
		s, err := t.runTodoWrite(input["items"])
		return text(s, err)
	case "submit_plan":
		s, err := t.runSubmitPlan(str("title"), str("body"))
		return text(s, err)
	case "read_skill":
		s, err := t.runReadSkill(str("name"))
		return text(s, err)
	case "message_bot":
		s, err := t.runMessageBot(str("to"), str("content"))
		return text(s, err)
	case "assign_task":
		s, err := t.runAssignTask(str("to"), str("title"), str("detail"))
		return text(s, err)
	case "update_task":
		id := 0
		if n, ok := intArg(input, "id"); ok {
			id = n
		}
		s, err := t.runUpdateTask(id, str("status"), str("note"))
		return text(s, err)
	case "list_tasks":
		if !IsGroupChat(t.currentChat) {
			return text(i18n.T("The task board is for group chat collaboration and is not available in a DM"), true)
		}
		return text(t.board.Render(), false)
	case "list_connectors":
		s, err := t.runListConnectors()
		return text(s, err)
	case "enable_connector":
		s, err := t.runEnableConnector(str("name"), str("path"), str("api_key"))
		return text(s, err)
	case "save_routine":
		every := 0
		if n, ok := intArg(input, "every_minutes"); ok {
			every = n
		}
		s, err := t.runSaveRoutine(str("name"), str("prompt"), every)
		return text(s, err)
	}
	if strings.HasPrefix(name, "mcp_") {
		return t.runMCPTool(name, input)
	}
	return text(i18n.T("Unknown tool: ")+name, true)
}

func intArg(input map[string]any, key string) (int, bool) {
	v, ok := input[key]
	if !ok || v == nil {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	default:
		return 0, false
	}
}

func boolArg(input map[string]any, key string) bool {
	v, _ := input[key].(bool)
	return v
}

// parallelizable reports whether this tool has no side effects the rest of the batch can race with.
// Writes, bash, and anything that waits on a human stay in order.
func (t *Toolbox) parallelizable(name string) bool {
	switch name {
	case "read_file", "grep", "glob", "fetch_url", "read_skill", "list_tasks", "list_connectors", "recall", "search_history", "web_search":
		return true
	}
	if strings.HasPrefix(name, "mcp_") {
		for _, server := range t.mcpServers {
			for _, mt := range t.mcp.Tools(server) {
				if plugin.MCPToolName(server, mt.Name) == name {
					return mt.ReadOnly
				}
			}
		}
	}
	return false
}

// inBounds is the single definition of "inside": this member's own workspace, plus any directory the
// user named in conversation (see roots.go). Both the escape check and path resolution ask here, so
// the two cannot drift apart.
func (t *Toolbox) inBounds(abs string) bool {
	c := canonical(abs)
	return within(c, canonical(t.workspace)) || within(c, scratchDir()) || t.roots.Contains(abs)
}

// scratchDir is /tmp. It counts as inside because it is where intermediate files belong — fetch a page,
// unpack an archive, dump some JSON, and the model's first instinct is /tmp/something. Asking about
// that every time is asking about something with no consequences. Codex draws workspace-write the same way:
// the working directory plus temporary directories.

// /tmp only, never $TMPDIR. On macOS $TMPDIR is /var/folders/…/T, where other applications keep private
// caches, and opening it would hand those over by the way; /tmp is the scratch paper everyone shares
// already.
func scratchDir() string { return canonical("/tmp") }

// resolve turns a path from a tool argument into an absolute path that is genuinely inside.

// An absolute path is now treated as one. It used to be joined onto the workspace, so
// read_file("/etc/passwd") actually read <workspace>/etc/passwd — reaching nothing and reporting no
// error, leaving the model to see "no such file" and try another spelling. A directory the user named
// is reachable only by absolute path, so that route has to be real.
func (t *Toolbox) resolve(rel string) (string, error) {
	if abs, ok := absPath(rel); ok {
		if t.inBounds(abs) {
			return abs, nil
		}
		return "", fmt.Errorf(i18n.T("Path is outside your workspace and the directories the user pointed you at: %s"), rel)
	}
	p := filepath.Clean(filepath.Join(t.workspace, rel))
	if !within(p, t.workspace) {
		return "", fmt.Errorf(i18n.T("Path escapes the workspace: %s"), rel)
	}
	return p, nil
}

// absPath normalises an argument that may be an absolute path. Unlike absDir in roots.go it does not
// require the path to exist: writing a file that is not there yet still has to clear the bounds check.
func absPath(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "~" || strings.HasPrefix(raw, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		raw = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(raw, "~"), "/"))
	}
	if !filepath.IsAbs(raw) {
		return "", false
	}
	return filepath.Clean(raw), true
}

// bashEscapes decides token by token whether a command may touch something out of bounds.
// An absolute path no longer counts as escaping on sight: one landing inside a directory the user
// named is inside. Otherwise naming a directory would mean nothing to bash, and bash is where the work
// actually happens.
func (t *Toolbox) bashEscapes(segs []shellSeg) bool {
	for _, s := range segs {
		if t.segEscapes(s) {
			return true
		}
	}
	return false
}

// segEscapes judges one segment. The leading word is skipped: it names the command, not a path — the
// /bin/ls in `/bin/ls foo` is just a program on this machine, not somewhere it intends to reach.
func (t *Toolbox) segEscapes(s shellSeg) bool {
	if len(s.words) < 2 {
		return false
	}
	for _, f := range s.words[1:] {
		if f == "" || strings.HasPrefix(f, "-") {
			continue
		}

		// /dev/null is not a place, it is a bit bucket. Treating the one in `… 2>/dev/null` as an
		// absolute path out of bounds means a human is needed every time the model writes "spare me the
		// error output".
		if f == devNull {
			continue
		}
		if strings.HasPrefix(f, "/") || strings.HasPrefix(f, "~") {
			abs, ok := absPath(f)
			if !ok || !t.inBounds(abs) {
				return true
			}
			continue
		}

		// A .. in a relative path always counts as escaping. It resolves against cmd.Dir (the
		// workspace) and could in principle land in a granted directory, but telling for sure would
		// mean re-enacting the shell's path semantics here — and this check is a heuristic to begin
		// with, so it asks one time too many rather than one too few. To go elsewhere, write it out.
		cleaned := filepath.Clean(f)
		if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// escapeDir picks the first out-of-bounds absolute path in a command and answers which directory,
// granted, would stop it escaping.

// Only the first: the approval card prints one specific directory, and the button the user presses has
// to mean exactly one thing. When a command reaches into several unrelated places this covers one of
// them and the rest are asked again later — granting several at one click is the kind of convenience
// the user cannot check.
func (t *Toolbox) escapeDir(segs []shellSeg) string {
	for _, s := range segs {
		if len(s.words) < 2 {
			continue
		}
		for _, f := range s.words[1:] {
			if f == "" || strings.HasPrefix(f, "-") {
				continue
			}
			if !strings.HasPrefix(f, "/") && !strings.HasPrefix(f, "~") {
				continue
			}
			abs, ok := absPath(f)
			if !ok || t.inBounds(abs) {
				continue
			}
			if dir := t.roots.Candidate(abs); dir != "" {
				return dir
			}
		}
	}
	return ""
}

func (t *Toolbox) awaitApproval(req *Approval) (approved bool, reason string) {
	approved, reason, canceled := req.WaitCtx(t.turnCtx)
	if canceled {
		t.bus.Decide(req.ID, false, reason)
	}
	return approved, reason
}

func (t *Toolbox) sandboxReady() bool {
	return t.settings != nil && t.settings.SandboxEnabled() && t.sbx != nil && t.sbx.Available()
}

// useSandbox reports whether this bash call should be wrapped. full never isolates.
func (t *Toolbox) useSandbox() bool {
	return t.perm() != config.PermFull && t.sandboxReady()
}

func (t *Toolbox) bashToolDef() model.ToolDef {
	desc := i18n.T("Run a shell command; it runs in your workspace. Commands that only read run straight away, including pipelines and sequences of them (ls/cat/grep/rg/sort/wc/diff/sed -n/git log, etc.), anywhere inside your working directories or /tmp. Writes, network access, paths outside those directories, and everything else are submitted to the user for approval first — waiting, being rejected, and being cancelled are all normal.")
	props := map[string]any{
		"command": map[string]any{"type": "string", "description": i18n.T("The shell command to run")},
	}
	if t.useSandbox() {
		hatch := t.settings.SandboxAllowUnsandboxed()
		if hatch {
			desc = i18n.T("Run a shell command inside the workspace OS sandbox. Network is blocked when the backend can isolate it. Reads may use the rest of this machine; writes stay in working directories and /tmp. A .git directory in a working directory is read-only when the backend can re-bind it. Commands that only read still follow the permission tier unless sandbox auto-allow is on. If a command is denied, retry with unsandboxed=true.")
			props["unsandboxed"] = map[string]any{"type": "boolean", "description": i18n.T("Run on the host instead of the sandbox. Always needs approval except under No approvals. Use only when the command must write outside working directories or use the network.")}
		} else {
			desc = i18n.T("Run a shell command inside the workspace OS sandbox. Network is blocked when the backend can isolate it. Reads may use the rest of this machine; writes stay in working directories and /tmp. A .git directory in a working directory is read-only when the backend can re-bind it. Commands that only read still follow the permission tier unless sandbox auto-allow is on.")
		}
	}
	return model.ToolDef{Name: "bash", Description: desc, Properties: props, Required: []string{"command"}}
}

func (t *Toolbox) sandboxTmp() (string, error) {
	if t.sbxTmp != "" {
		return t.sbxTmp, nil
	}
	d, err := os.MkdirTemp("", "botbureau-sbx-*")
	if err != nil {
		return "", err
	}
	t.sbxTmp = d
	return d, nil
}

func sandboxDenied(err error, out string) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(out + " " + err.Error())
	for _, n := range []string{"permission denied", "operation not permitted", "read-only file system"} {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

func sandboxRetryHint() string {
	return i18n.T("The sandbox denied this command. Retry with unsandboxed=true if it must leave the workspace or use the network.")
}

func (t *Toolbox) runBash(command string, unsandboxed bool) (string, bool) {
	command = strings.TrimSpace(command)
	if command == "" {
		return i18n.T("Command is empty"), true
	}
	if unsandboxed && (t.settings == nil || !t.settings.SandboxAllowUnsandboxed()) {
		return i18n.T("Unsandboxed commands are disabled"), true
	}

	// The two checks answer different questions:
	// ReadOnly — does this command have side effects? Judged per segment: the whole is read-only only
	// when every segment is a whitelisted read-only command with no redirect and no
	// substitution anywhere.
	// Escapes  — can those side effects land out of bounds? Pipes and redirects on their own cannot
	// (cmd.Dir is pinned to the workspace, so relative paths stay in); command substitution
	// can, and so do absolute paths, .. and ~ that point outside.
	// Conflating them would make the auto tier gate `echo x > note.txt && cat note.txt`, which leaves
	// that tier unable to do any actual work.
	//
	// When an OS backend is wrapping the process, Escapes is not the admission condition: the kernel
	// holds the write line. The scanner still fills the approval card and still classifies read-only.
	isolated := t.useSandbox() && !unsandboxed
	segs, subst, parsed := scanBash(command)
	escapes := !parsed || subst || t.bashEscapes(segs)
	readOnly := bashReadOnly(segs, subst, parsed) && !escapes
	if isolated {
		readOnly = bashReadOnly(segs, subst, parsed)
		escapes = false
	}
	if unsandboxed {
		escapes = true
		readOnly = false
	}
	act := config.ToolAct{
		Kind:     config.ActBash,
		ReadOnly: readOnly,
		Escapes:  escapes,
	}
	if escapes {
		act.Dir = t.escapeDir(segs)
	}
	isolate := "host"
	if isolated {
		isolate = "workspace"
	}

	autoAllow := isolated && t.settings.SandboxAutoAllowBash()
	if autoAllow {
		if t.audit != nil {
			t.audit.Record(auditLine{Bot: t.botName, Act: "bash", Command: command, Allowed: true, Isolate: isolate})
		}
	} else {
		run, reason, rejected, _ := t.gateRun(act, "bash: "+command,
			i18n.T("Command execution requested, waiting for approval #%d: $ ")+command, "", "", command, isolate)
		if rejected {
			return denied(i18n.T("The user rejected this command"), reason), true
		}
		if strings.TrimSpace(run) != "" {
			command = run
		}
	}
	parent := t.turnCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, config.BashTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Dir = t.workspace
	if isolated {
		tmp, err := t.sandboxTmp()
		if err != nil {
			return err.Error(), true
		}
		writable := []string{t.workspace, scratchDir(), tmp}
		if t.roots != nil {
			writable = append(writable, t.roots.List()...)
		}
		wrapped, err := t.sbx.Command(ctx, sandbox.Spec{
			Command:  command,
			Dir:      t.workspace,
			Writable: writable,
			ReadOnly: sandbox.GitDirs(writable),
			TmpDir:   tmp,
		})
		if err != nil {
			msg := err.Error()
			if t.settings.SandboxAllowUnsandboxed() {
				msg += "\n" + sandboxRetryHint()
			}
			return msg, true
		}
		cmd = wrapped
	}
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if text == "" {
		text = i18n.T("(no output)")
	}
	text = truncateOutput(text)
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Sprintf(i18n.T("Command timed out (%s)"), config.BashTimeout), true
	}
	if err != nil {
		msg := fmt.Sprintf(i18n.T("Command failed (%v)\n%s"), err, text)
		if isolated && t.settings != nil && t.settings.SandboxAllowUnsandboxed() && sandboxDenied(err, text) {
			msg += "\n" + sandboxRetryHint()
		}
		return msg, true
	}
	return text, false
}

// runReadSkill fetches a skill's full text. Reading one has no side effects — it is a file read — so it
// does not go through the approval gate; running a script the skill ships is bash's business and gets
// approved there.
func (t *Toolbox) runReadSkill(name string) (string, bool) {
	s, ok := t.skills.Get(name)
	if !ok {
		avail := strings.Join(t.skills.Names(), ", ")
		if avail == "" {
			return i18n.T("No skills are installed"), true
		}
		return i18n.T("No skill named ") + name + i18n.T("; available: ") + avail, true
	}
	t.bus.Emit("tool", t.eventChat(), t.botName, fmt.Sprintf(i18n.T("Loaded skill %s"), name), nil)
	return s.Render(), false
}

func (t *Toolbox) runRemember(note, scope, id string) (string, bool) {
	note = strings.TrimSpace(note)
	mem := t.mem
	label := i18n.T("personal")
	stored := note
	if scope == "team" {
		mem = t.teamMem
		label = i18n.T("team")
		if id == "" {
			stored = t.botName + ": " + note
		}
	}
	assigned, existed, err := mem.Remember(stored, id)
	if err != nil {
		if scope == "team" {
			return i18n.T("Failed to write to the shared memory: ") + err.Error(), true
		}
		return i18n.T("Failed to write to memory: ") + err.Error(), true
	}
	if existed {
		return fmt.Sprintf(i18n.T("Already remembered as %s (%s)"), assigned, label), false
	}
	if strings.TrimSpace(id) != "" {
		return fmt.Sprintf(i18n.T("Updated note %s (%s)"), assigned, label), false
	}
	return fmt.Sprintf(i18n.T("Remembered as %s (%s)"), assigned, label), false
}

func (t *Toolbox) runRecall(query, scope string) (string, bool) {
	query = strings.TrimSpace(query)
	if query == "" {
		return i18n.T("query cannot be empty"), true
	}
	self, team := memoryScope(scope)
	var parts []string
	if self {
		parts = append(parts, formatRecall("self", t.mem.Recall(query, maxRecallHits))...)
	}
	if team {
		parts = append(parts, formatRecall("team", t.teamMem.Recall(query, maxRecallHits))...)
	}
	if len(parts) == 0 {
		return i18n.T("No matching notes"), true
	}
	return strings.Join(parts, "\n"), false
}

func (t *Toolbox) runForget(id, scope string) (string, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" {
		return i18n.T("id cannot be empty"), true
	}
	self, team := memoryScope(scope)
	var forgot []string
	var last error
	if self {
		if err := t.mem.Forget(id); err != nil {
			last = err
		} else {
			forgot = append(forgot, i18n.T("personal"))
		}
	}
	if team {
		if err := t.teamMem.Forget(id); err != nil {
			last = err
		} else {
			forgot = append(forgot, i18n.T("team"))
		}
	}
	if len(forgot) == 0 {
		if last != nil {
			return last.Error(), true
		}
		return fmt.Sprintf(i18n.T("there is no note %s"), id), true
	}
	return fmt.Sprintf(i18n.T("Forgot %s (%s)"), id, strings.Join(forgot, i18n.T(", "))), false
}

func (t *Toolbox) runSearchHistory(query string) (string, bool) {
	query = strings.TrimSpace(query)
	if query == "" {
		return i18n.T("query cannot be empty"), true
	}
	hits := t.bus.SearchChat(t.eventChat(), query, maxHistoryHits)
	if len(hits) == 0 {
		return i18n.T("No matching messages in this conversation"), true
	}
	var b strings.Builder
	for _, ev := range hits {
		id, _ := ev["id"].(int)
		from, _ := ev["source"].(string)
		text, _ := ev["text"].(string)
		fmt.Fprintf(&b, "- id=%d from=%s: %s\n", id, from, textutil.Brief(strings.TrimSpace(text), 200))
	}
	return strings.TrimSpace(b.String()), false
}

func memoryScope(scope string) (self, team bool) {
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case "self":
		return true, false
	case "team":
		return false, true
	default:
		return true, true
	}
}

func formatRecall(scope string, hits []MemEntry) []string {
	if len(hits) == 0 {
		return nil
	}
	out := make([]string, 0, len(hits))
	for _, e := range hits {
		date := e.Date
		if date != "" {
			date = " [" + date + "]"
		}
		out = append(out, fmt.Sprintf("[%s %s]%s\n%s", scope, e.ID, date, e.Text))
	}
	return out
}

func (t *Toolbox) runTodoWrite(raw any) (string, bool) {
	items, err := parseTodoItems(raw)
	if err != nil {
		return err.Error(), true
	}
	if err := SaveTodos(t.workspace, items); err != nil {
		return i18n.T("Failed to write the personal list: ") + err.Error(), true
	}
	t.bus.Emit("refresh", "", t.botName, "todos", nil)
	if len(items) == 0 {
		return i18n.T("Personal list cleared"), false
	}
	return fmt.Sprintf(i18n.T("Personal list now has %d item(s)"), len(items)), false
}

func (t *Toolbox) runSubmitPlan(title, body string) (string, bool) {
	title = strings.TrimSpace(title)
	body = strings.TrimSpace(body)
	if title == "" || body == "" {
		return i18n.T("submit_plan needs a title and a body"), true
	}
	req := t.bus.requestPlan(t.botName, t.eventChat(), title, body)
	t.bus.Emit("tool", t.eventChat(), t.botName,
		fmt.Sprintf(i18n.T("Plan submitted, waiting for approval #%d: %s"), req.ID, title), nil)
	approved, reason := t.awaitApproval(req)
	if approved {
		return i18n.T("The user accepted the plan. Continue from it."), false
	}
	return denied(i18n.T("The user rejected the plan"), reason), true
}

func (t *Toolbox) runAssignTask(to, title, detail string) (string, bool) {
	if !IsGroupChat(t.currentChat) {
		return i18n.T("The task board is for group chat collaboration and is not available in a DM"), true
	}
	to = t.bus.Resolve(to)
	if title == "" {
		return i18n.T("title cannot be empty"), true
	}
	if to != t.botName && !t.bus.IsGroupMemberOf(t.currentChat, to) {
		return t.title(to) + i18n.T(" is not in the group chat and cannot be assigned"), true
	}
	task := t.board.Add(t.botName, to, title, detail)
	if to == t.botName {
		t.bus.Emit("system", t.currentChat, t.botName,
			fmt.Sprintf(i18n.T("%s claimed task #%d: %s"), t.title(t.botName), task.ID, task.Title), nil)
	} else {
		msg := fmt.Sprintf(i18n.T("@%s [Task #%d] %s"), t.title(to), task.ID, task.Title)
		if detail != "" {
			msg += " —— " + detail
		}
		msg += i18n.T(" (call update_task to set doing when you start, and done when you finish)")
		t.bus.PostGroupTo(t.currentChat, t.botName, msg, []string{to})
	}
	t.bus.Emit("refresh", "", t.botName, "tasks", nil)
	return fmt.Sprintf(i18n.T("Created task #%d and assigned it to %s"), task.ID, t.title(to)), false
}

func (t *Toolbox) runUpdateTask(id int, status, note string) (string, bool) {
	if !IsGroupChat(t.currentChat) {
		return i18n.T("The task board is for group chat collaboration and is not available in a DM"), true
	}
	task, err := t.board.Update(id, status, note)
	if err != nil {
		return err.Error(), true
	}
	line := fmt.Sprintf(i18n.T("#%d \"%s\" → %s (%s)"), task.ID, task.Title, task.Status, t.botName)
	if note != "" {
		line += i18n.T(": ") + note
	}
	t.bus.Emit("system", t.currentChat, t.botName, line, nil)
	t.bus.Emit("refresh", "", t.botName, "tasks", nil)
	return fmt.Sprintf(i18n.T("Task #%d updated to %s"), task.ID, task.Status), false
}

// title returns the name a bot shows to people; that is what appears in chat, while routing stays on the id.
func (t *Toolbox) title(id string) string {
	if b := t.bus.Bot(id); b != nil {
		return b.Cfg.Title()
	}
	return id
}

func (t *Toolbox) runMessageBot(to, content string) (string, bool) {
	// The model sees display names; this layer needs ids
	to = t.bus.Resolve(to)
	if !IsGroupChat(t.currentChat) {
		return i18n.T("A DM is one-on-one, so tasks cannot be handed off here; suggest the user move to the group chat so the team can collaborate"), true
	}
	if to == t.botName {
		return i18n.T("You cannot message yourself"), true
	}
	if t.bus.Bot(to) == nil {
		return i18n.T("There is no bot named ") + to + i18n.T(" on the team"), true
	}
	if !t.bus.IsGroupMemberOf(t.currentChat, to) {
		return t.title(to) + i18n.T(" is not in the group chat and cannot be reached; you can ask the user to add them under \"Group members\""), true
	}
	t.bus.PostGroupTo(t.currentChat, t.botName, "@"+t.title(to)+" "+content, []string{to})
	return i18n.T("Sent in the group chat to ") + t.title(to) + i18n.T("; they will reply once they are done"), false
}

func toResultImages(in []plugin.Image) []model.ResultImage {
	if len(in) == 0 {
		return nil
	}
	out := make([]model.ResultImage, 0, len(in))
	for _, img := range in {
		out = append(out, model.ResultImage{MIME: img.MIME, Base64: img.Base64})
	}
	return out
}

// runMCPTool resolves mcp_<plugin>_<tool> back into a plugin call; non-read-only tools go through the approval gate first.
func (t *Toolbox) runMCPTool(name string, input map[string]any) (string, []model.ResultImage, bool) {
	for _, server := range t.mcpServers {
		for _, mt := range t.mcp.Tools(server) {
			if plugin.MCPToolName(server, mt.Name) != name {
				continue
			}
			argsJSON, _ := json.Marshal(input)
			action := fmt.Sprintf(i18n.T("Plugin %s: %s(%s)"), server, mt.Name, textutil.Brief(string(argsJSON), 300))
			act := config.ToolAct{Kind: config.ActPlugin, ReadOnly: mt.ReadOnly}
			if reason, rejected, _ := t.gate(act, action,
				i18n.T("Plugin call requested, waiting for approval #%d: ")+action); rejected {
				return denied(i18n.T("The user rejected this plugin call"), reason), nil, true
			}
			out, isBizErr, err := t.mcp.Call(server, mt.Name, input)
			if err != nil {
				return i18n.T("Plugin call failed: ") + err.Error(), nil, true
			}
			return out.Text, toResultImages(out.Images), isBizErr
		}
	}
	return i18n.T("Unknown plugin tool: ") + name + i18n.T(" (the plugin may not be connected, or may not be assigned to you)"), nil, true
}

func (t *Toolbox) runSaveRoutine(name, prompt string, everyMinutes int) (string, bool) {
	if name == "" || prompt == "" || everyMinutes <= 0 {
		return i18n.T("Invalid arguments: name, prompt and a positive integer every_minutes are required"), true
	}
	r := t.sched.Add(name, t.botName, prompt, everyMinutes)
	t.bus.Emit("system", t.eventChat(), t.botName,
		fmt.Sprintf(i18n.T("Routine \"%s\" saved: it runs every %d minutes"), r.Name, r.EveryMinutes), nil)
	return fmt.Sprintf(i18n.T("Routine \"%s\" saved; it fires every %d minutes"), r.Name, r.EveryMinutes), false
}
