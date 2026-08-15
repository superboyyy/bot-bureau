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
	"regexp"
	"strings"
)

// 以这些词开头且不含 shell 元字符的 bash 命令视为只读，直接执行；其余命令先提交用户审批。
// find 不在名单里：它自带 -delete/-exec 等破坏性动作，无需任何元字符即可绕过审批门。
// Bash commands starting with one of these words and containing no shell metacharacters are treated as read-only and run directly; everything else is submitted for user approval first.
// find is not on the list: its built-in destructive actions like -delete/-exec could bypass the approval gate without needing any metacharacter.
var safeBashCommands = map[string]bool{
	"ls": true, "cat": true, "head": true, "tail": true, "grep": true,
	"wc": true, "pwd": true, "echo": true, "date": true, "du": true,
	"file": true, "diff": true,
}

// 管道/重定向/命令替换等都可能让“只读”首词携带副作用（如 ls $(rm -rf …)、cat x > y）
// Pipes/redirects/command substitution can all give a "read-only" first word side effects (e.g. ls $(rm -rf ...), cat x > y).
var shellMetaRe = regexp.MustCompile("[;&|`$<>]|\n")

// 命令替换是另一回事：它让命令真正要碰的路径在执行前无法确定，
// 逐 token 扫描（bashLeavesWorkspace）对 `cat $(cat target)` 这种完全失效。
// 所以命令替换一律按“可能越界”处理——auto 档只自动放行目标可知的命令。
//
// Command substitution is a different matter: it makes the paths a command will actually touch
// undecidable before it runs, and token scanning (bashLeavesWorkspace) is useless against
// `cat $(cat target)`. So substitution always counts as "may escape" — the auto tier only
// auto-approves commands whose targets are knowable.
var cmdSubstRe = regexp.MustCompile("`|\\$\\(")

func truncateOutput(s string) string {
	if len(s) > config.ToolOutputLimit {
		return s[:config.ToolOutputLimit] + i18n.T("\n...(output too long, truncated)")
	}
	return s
}

// Toolbox 是一个 bot 的客户端工具箱。currentChat 由 bot 在处理每条消息前设置。
// Toolbox is one bot's client-side toolbox. currentChat is set by the bot before each message is handled.
type Toolbox struct {
	botName     string
	workspace   string
	mem         *Memory            // 个人记忆 / personal memory
	teamMem     *Memory            // 团队共享记忆（全部 bot 读写同一份） / team memory, one copy shared by all bots
	board       *TaskBoard         // 团队任务看板 / the team task board
	mcp         *plugin.MCPManager // 插件管理器 / plugin (MCP server) manager
	mcpServers  []string           // 该 bot 订阅的插件 / plugins this bot subscribes to
	skills      *skill.Manager     // 技能库（全队共享） / the skill library, shared by the whole team
	bus         *Bus
	sched       *Scheduler
	currentChat string // "group" 或 "dm" / "group" or "dm"
	turnCtx     context.Context
	botPerm     string           // 该 bot 自己的档位，空 = 跟随全局 / this bot's tier; empty follows the global one
	settings    *config.Settings // 读全局档位 / source of the global tier
	// 最近一次工具调用产出的图片，由 bot 层在 Execute 之后立刻取走。
	// 每个 bot 一个 Toolbox、一个常驻 goroutine 串行处理消息，所以这里不会有并发写。
	// Images produced by the most recent tool call, collected by the bot layer right after Execute.
	// Each bot has one Toolbox and one resident goroutine handling messages in order, so nothing writes
	// this concurrently.
	lastImages []model.ResultImage
}

// perm 定出这次调用生效的档位。每次现算而不是启动时定死：
// 用户在设置里调档应当立刻管用，不该要求重启引擎或重建 bot。
//
// perm settles the tier in force for this call. It is resolved per call rather than fixed at startup:
// changing the tier in config.Settings must take effect immediately, without restarting the engine or the bot.
func (t *Toolbox) perm() config.PermLevel {
	global := ""
	if t.settings != nil {
		global = t.settings.Perm()
	}
	return config.ResolvePerm(t.botPerm, global)
}

// gate 是唯一的审批闸门：要问就挂起等人，放行就直接返回 true。
// 所有工具都从这里过，新增工具漏接闸门会很显眼。
//
// gate is the single approval checkpoint: it suspends for a human when required and returns true to
// proceed. Every tool goes through it, so a new tool that forgets the gate stands out.
func (t *Toolbox) gate(act config.ToolAct, action, waitMsg string) (string, bool, bool) {
	if !t.perm().NeedsApproval(act) {
		return "", false, true
	}
	req := t.bus.RequestApproval(t.botName, action, t.eventChat())
	t.bus.Emit("tool", t.eventChat(), t.botName, fmt.Sprintf(waitMsg, req.ID), nil)
	approved, reason := t.awaitApproval(req)
	if approved {
		return "", false, true
	}
	return reason, true, false
}

// denied 把拒绝原因拼成给模型看的一句话。
// denied renders the rejection reason into one sentence for the model.
func denied(what, reason string) string {
	if reason == "" {
		return what
	}
	return what + i18n.T(", reason: ") + reason
}

func NewToolbox(botName, workspace string, mem *Memory, deps *TeamDeps, mcpServers []string, bus *Bus, sched *Scheduler, botPerm string) *Toolbox {
	return &Toolbox{
		botName: botName, workspace: workspace, mem: mem,
		teamMem: deps.TeamMem, board: deps.Board, mcp: deps.MCP, mcpServers: mcpServers, skills: deps.Skills,
		bus: bus, sched: sched, botPerm: botPerm, settings: deps.Settings,
	}
}

// SetMCPServers 改这个 bot 订阅的插件。
// SetMCPServers changes which plugins this bot subscribes to.
func (t *Toolbox) SetMCPServers(names []string) { t.mcpServers = names }

// eventChat 把内部会话名映射为事件流里的 chat 标识。
// eventChat maps the internal chat name to the chat identifier used in the event stream.
func (t *Toolbox) eventChat() string {
	if IsGroupChat(t.currentChat) {
		return t.currentChat
	}
	return "dm:" + t.botName
}

// Defs 返回客户端工具定义（服务端联网工具由 provider 层按需追加）。
// Defs returns the client-side tool definitions (server-side web tools are appended by the provider layer as needed).
func (t *Toolbox) Defs() []model.ToolDef {
	var teammates []string
	for _, n := range t.bus.GroupMembersOf(t.currentChat) {
		if n != t.botName {
			teammates = append(teammates, n)
		}
	}
	defs := []model.ToolDef{
		{
			Name:        "bash",
			Description: i18n.T("Run a shell command in your workspace. Read-only commands that stay inside the workspace (ls/cat/grep, etc.) run directly; writes, paths outside the workspace, and every other command are submitted to the user for approval first — waiting, being rejected, and being cancelled are all normal."),
			Properties: map[string]any{
				"command": map[string]any{"type": "string", "description": i18n.T("The shell command to run")},
			},
			Required: []string{"command"},
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
	// read_skill 只在真有技能时才给出去：没有技能却摆着一个读技能的工具，
	// 模型迟早会拿它去猜名字，然后收到一串"没有这个技能"。
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
	// 订阅的 MCP 插件工具（名字加 mcp_<插件>_ 前缀，避免跨插件重名）
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
	if len(teammates) > 0 {
		defs = append(defs, model.ToolDef{
			Name:        "message_bot",
			Description: i18n.T("(Group chat only) Hand a subtask off to another AI teammate, or report results to them / ask them a question. The message must be self-contained (they cannot see your context). They handle it asynchronously, and their reply reaches you as a new group chat message."),
			Properties: map[string]any{
				"to":      map[string]any{"type": "string", "enum": teammates, "description": i18n.T("Recipient bot")},
				"content": map[string]any{"type": "string", "description": i18n.T("Message content")},
			},
			Required: []string{"to", "content"},
		})
	}
	return defs
}

// TakeImages 取走并清空上一次工具调用产出的图片。
// 必须清空：不取走的话，下一个不返回图片的工具会把上一张图再挂一遍。
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

// Execute 执行一个客户端工具，返回（结果文本, 是否出错）。
// Execute runs one client-side tool and returns (result text, whether it errored).
func (t *Toolbox) Execute(name string, input map[string]any) (string, bool) {
	str := func(k string) string { v, _ := input[k].(string); return v }
	switch name {
	case "bash":
		return t.runBash(str("command"))
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

func (t *Toolbox) resolve(rel string) (string, error) {
	p := filepath.Clean(filepath.Join(t.workspace, rel))
	r, err := filepath.Rel(t.workspace, p)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf(i18n.T("Path escapes the workspace: %s"), rel)
	}
	return p, nil
}

func bashLeavesWorkspace(command string) bool {
	fields := strings.Fields(command)
	if len(fields) < 2 {
		return false
	}
	for _, f := range fields[1:] {
		// 重定向符号可能和目标粘在一起（>/etc/x），先剥掉再判断
		// A redirect operator can be glued to its target (>/etc/x); strip it before judging
		f = strings.TrimLeft(f, "<>&|;")
		if f == "" || strings.HasPrefix(f, "-") {
			continue
		}
		if strings.HasPrefix(f, "/") || strings.HasPrefix(f, "~") {
			return true
		}
		cleaned := filepath.Clean(f)
		if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(os.PathSeparator)) {
			return true
		}
	}
	return false
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
	firstWord := strings.Fields(command)[0]
	// 两个判断分开，因为它们回答的是不同问题：
	//   ReadOnly —— 这条命令有没有副作用？元字符一律否决（`ls $(rm -rf ~)` 首词是只读的）。
	//   Escapes  —— 副作用会不会跑到工作目录外？管道和重定向本身不会（cmd.Dir 钉在工作目录，
	//               相对路径出不去），命令替换会，绝对路径 / .. / ~ 也会。
	// 混为一谈的话，auto 档会把 `echo x > note.txt && cat note.txt` 也拦下来，
	// 那这一档就没法干活了。
	//
	// The two checks answer different questions:
	//   ReadOnly — does this command have side effects? Any metacharacter says yes
	//              (`ls $(rm -rf ~)` has a read-only first word).
	//   Escapes  — can those side effects land outside the workspace? Pipes and redirects on their own
	//              cannot (cmd.Dir is pinned to the workspace, so relative paths stay in); command
	//              substitution can, and so do absolute paths, .. and ~.
	// Conflating them would make the auto tier gate `echo x > note.txt && cat note.txt`, which leaves
	// that tier unable to do any actual work.
	hasMeta := shellMetaRe.MatchString(command)
	escapes := bashLeavesWorkspace(command) || cmdSubstRe.MatchString(command)
	act := config.ToolAct{
		Kind:     config.ActBash,
		ReadOnly: safeBashCommands[firstWord] && !hasMeta && !escapes,
		Escapes:  escapes,
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
	// resolve 已经把路径钉死在工作目录内，所以写文件永远不越界
	// resolve has already pinned the path inside the workspace, so a write can never escape
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

// runReadSkill 取一个技能的全文。读技能本身没有副作用（就是读一个文件），所以不过审批门；
// 技能里的脚本要跑，是 bash 那一关的事，该审批的在那儿审批。
//
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
	if title == "" {
		return i18n.T("title cannot be empty"), true
	}
	if to != t.botName && !t.bus.IsGroupMemberOf(t.currentChat, to) {
		return to + i18n.T(" is not in the group chat and cannot be assigned"), true
	}
	task := t.board.Add(t.botName, to, title, detail)
	if to == t.botName {
		t.bus.Emit("system", t.currentChat, t.botName,
			fmt.Sprintf(i18n.T("%s claimed task #%d: %s"), t.botName, task.ID, task.Title), nil)
	} else {
		msg := fmt.Sprintf(i18n.T("@%s [Task #%d] %s"), to, task.ID, task.Title)
		if detail != "" {
			msg += " —— " + detail
		}
		msg += i18n.T(" (call update_task to set doing when you start, and done when you finish)")
		t.bus.PostGroupTo(t.currentChat, t.botName, msg, []string{to})
	}
	t.bus.Emit("refresh", "", t.botName, "tasks", nil)
	return fmt.Sprintf(i18n.T("Created task #%d and assigned it to %s"), task.ID, to), false
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

func (t *Toolbox) runMessageBot(to, content string) (string, bool) {
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
		return to + i18n.T(" is not in the group chat and cannot be reached; you can ask the user to add them under \"Group members\""), true
	}
	t.bus.PostGroupTo(t.currentChat, t.botName, "@"+to+" "+content, []string{to})
	return i18n.T("Sent in the group chat to ") + to + i18n.T("; they will reply once they are done"), false
}

// runMCPTool 把 mcp_<插件>_<工具> 还原为插件调用；非只读工具先过审批门。
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
			// 图片单独挂在工具箱上，由 bot 那一层取走拼进 ToolResult——
			// Execute 的签名被十几个内置工具共用，不该为插件这一条改掉。
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
