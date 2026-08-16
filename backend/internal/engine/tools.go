package engine

import (
	"botbureau/backend/internal/config"
	"botbureau/backend/internal/model"
	"botbureau/backend/internal/plugin"
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
	skills      *skill.Manager     // the skill library, shared by the whole team
	bus         *Bus
	sched       *Scheduler
	currentChat string // "group" or "dm"
	turnCtx     context.Context
	botPerm     string           // this bot's tier; empty follows the global one
	settings    *config.Settings // source of the global tier

	// Images produced by the most recent tool call, collected by the bot layer right after Execute.
	// Each bot has one Toolbox and one resident goroutine handling messages in order, so nothing writes
	// this concurrently.
	lastImages []model.ResultImage
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
	if !t.perm().NeedsApproval(act) {
		return "", false, true
	}
	req := t.bus.RequestApproval(t.botName, action, t.eventChat(), act.Dir)
	t.bus.Emit("tool", t.eventChat(), t.botName, fmt.Sprintf(waitMsg, req.ID), nil)
	approved, reason := t.awaitApproval(req)
	if approved {
		return "", false, true
	}
	return reason, true, false
}

// denied renders the rejection reason into one sentence for the model.
func denied(what, reason string) string {
	if reason == "" {
		return what
	}
	return what + i18n.T(", reason: ") + reason
}

func NewToolbox(botName, workspace string, roots *Roots, mem *Memory, deps *TeamDeps, mcpServers []string, bus *Bus, sched *Scheduler, botPerm string) *Toolbox {
	return &Toolbox{
		botName: botName, workspace: workspace, roots: roots, mem: mem,
		teamMem: deps.TeamMem, board: deps.Board, mcp: deps.MCP, mcpServers: mcpServers, skills: deps.Skills,
		bus: bus, sched: sched, botPerm: botPerm, settings: deps.Settings,
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
		{
			Name:        "bash",
			Description: i18n.T("Run a shell command; it runs in your workspace. Commands that only read run straight away, including pipelines and sequences of them (ls/cat/grep/rg/sort/wc/diff/sed -n/git log, etc.), anywhere inside your working directories or /tmp. Writes, network access, paths outside those directories, and everything else are submitted to the user for approval first — waiting, being rejected, and being cancelled are all normal."),
			Properties: map[string]any{
				"command": map[string]any{"type": "string", "description": i18n.T("The shell command to run")},
			},
			Required: []string{"command"},
		},
		{
			Name:        "fetch_url",
			Description: i18n.T("Read a web page or API response and get it back as text (HTML is stripped down to its readable text). This is how you look something up online — it runs straight away and never needs approval, so reach for it rather than curl. It only fetches; it cannot post, log in, or reach addresses on this machine or this local network."),
			Properties: map[string]any{
				"url": map[string]any{"type": "string", "description": i18n.T("The http or https address to read")},
			},
			Required: []string{"url"},
		},
		{
			Name:        "read_file",
			Description: i18n.T("Read a text file in your workspace."),
			Properties: map[string]any{
				"path": map[string]any{"type": "string", "description": i18n.T("Path relative to the workspace")},
			},
			Required: []string{"path"},
		},
		{
			Name:        "write_file",
			Description: i18n.T("Write (overwrite) a text file in your workspace; parent directories are created automatically. Writes are submitted to the user for approval first."),
			Properties: map[string]any{
				"path":    map[string]any{"type": "string", "description": i18n.T("Path relative to the workspace")},
				"content": map[string]any{"type": "string", "description": i18n.T("The complete file content")},
			},
			Required: []string{"path", "content"},
		},
		{
			Name:        "remember",
			Description: i18n.T("Write a piece of information worth keeping across sessions into long-term memory. scope=self stores it in your personal memory; scope=team stores it in the team-wide shared memory (visible to every bot, suitable for user preferences and team conventions). Do not record something that is already remembered."),
			Properties: map[string]any{
				"note":  map[string]any{"type": "string", "description": i18n.T("One sentence covering the point and why it matters")},
				"scope": map[string]any{"type": "string", "enum": []string{"self", "team"}, "description": i18n.T("Defaults to self")},
			},
			Required: []string{"note"},
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

// TakeImages collects and clears the images from the last tool call.
// Clearing matters: without it the next tool that returns no image would re-attach the previous one.
func (t *Toolbox) TakeImages() []model.ResultImage {
	imgs := t.lastImages
	t.lastImages = nil
	return imgs
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

// Execute runs one client-side tool and returns (result text, whether it errored).
func (t *Toolbox) Execute(name string, input map[string]any) (string, bool) {
	str := func(k string) string { v, _ := input[k].(string); return v }
	switch name {
	case "bash":
		return t.runBash(str("command"))
	case "fetch_url":
		return t.runFetchURL(str("url"))
	case "read_file":
		return t.runReadFile(str("path"))
	case "write_file":
		return t.runWriteFile(str("path"), str("content"))
	case "remember":
		return t.runRemember(str("note"), str("scope"))
	case "read_skill":
		return t.runReadSkill(str("name"))
	case "message_bot":
		return t.runMessageBot(str("to"), str("content"))
	case "assign_task":
		return t.runAssignTask(str("to"), str("title"), str("detail"))
	case "update_task":
		id := 0
		if f, ok := input["id"].(float64); ok {
			id = int(f)
		}
		return t.runUpdateTask(id, str("status"), str("note"))
	case "list_tasks":
		if !IsGroupChat(t.currentChat) {
			return i18n.T("The task board is for group chat collaboration and is not available in a DM"), true
		}
		return t.board.Render(), false
	case "save_routine":
		every := 0
		if f, ok := input["every_minutes"].(float64); ok {
			every = int(f)
		}
		return t.runSaveRoutine(str("name"), str("prompt"), every)
	}
	if strings.HasPrefix(name, "mcp_") {
		return t.runMCPTool(name, input)
	}
	return i18n.T("Unknown tool: ") + name, true
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

func (t *Toolbox) runBash(command string) (string, bool) {
	command = strings.TrimSpace(command)
	if command == "" {
		return i18n.T("Command is empty"), true
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
	segs, subst, parsed := scanBash(command)
	escapes := !parsed || subst || t.bashEscapes(segs)
	act := config.ToolAct{
		Kind:     config.ActBash,
		ReadOnly: bashReadOnly(segs, subst, parsed) && !escapes,
		Escapes:  escapes,
	}
	if escapes {
		act.Dir = t.escapeDir(segs)
	}
	if reason, rejected, _ := t.gate(act, "bash: "+command,
		i18n.T("Command execution requested, waiting for approval #%d: $ ")+command); rejected {
		return denied(i18n.T("The user rejected this command"), reason), true
	}
	parent := t.turnCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, config.BashTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Dir = t.workspace
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
		return fmt.Sprintf(i18n.T("Command failed (%v)\n%s"), err, text), true
	}
	return text, false
}

func (t *Toolbox) runReadFile(rel string) (string, bool) {
	p, err := t.resolve(rel)
	if err != nil {
		return err.Error(), true
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return i18n.T("Read failed: ") + err.Error(), true
	}
	return truncateOutput(string(raw)), false
}

func (t *Toolbox) runWriteFile(rel, content string) (string, bool) {
	p, err := t.resolve(rel)
	if err != nil {
		return err.Error(), true
	}
	preview := content
	if len(preview) > 400 {
		preview = preview[:400] + "…"
	}
	action := fmt.Sprintf("write_file: %s (%d bytes)\n%s", rel, len(content), preview)

	// resolve has already pinned the path inside the bounds (the workspace, or a directory the user
	// named), so a write can never escape
	act := config.ToolAct{Kind: config.ActWrite}
	if reason, rejected, _ := t.gate(act, action,
		i18n.T("File write requested, waiting for approval #%d: ")+rel); rejected {
		return denied(i18n.T("The user rejected this file write"), reason), true
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return i18n.T("Failed to create directory: ") + err.Error(), true
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		return i18n.T("Write failed: ") + err.Error(), true
	}
	return fmt.Sprintf(i18n.T("Wrote %s (%d bytes)"), rel, len(content)), false
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

func (t *Toolbox) runRemember(note, scope string) (string, bool) {
	if scope == "team" {
		if err := t.teamMem.Append(t.botName + ": " + note); err != nil {
			return i18n.T("Failed to write to the shared memory: ") + err.Error(), true
		}
		return i18n.T("Written to the team shared memory"), false
	}
	if err := t.mem.Append(note); err != nil {
		return i18n.T("Failed to write to memory: ") + err.Error(), true
	}
	return i18n.T("Written to your personal long-term memory"), false
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

// runMCPTool resolves mcp_<plugin>_<tool> back into a plugin call; non-read-only tools go through the approval gate first.
func (t *Toolbox) runMCPTool(name string, input map[string]any) (string, bool) {
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
				return denied(i18n.T("The user rejected this plugin call"), reason), true
			}
			out, isBizErr, err := t.mcp.Call(server, mt.Name, input)
			if err != nil {
				return i18n.T("Plugin call failed: ") + err.Error(), true
			}

			// Images are parked on the toolbox and collected by the bot layer into the ToolResult:
			// Execute's signature is shared by a dozen built-in tools and should not change for this one.
			t.lastImages = toResultImages(out.Images)
			return out.Text, isBizErr
		}
	}
	return i18n.T("Unknown plugin tool: ") + name + i18n.T(" (the plugin may not be connected, or may not be assigned to you)"), true
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
