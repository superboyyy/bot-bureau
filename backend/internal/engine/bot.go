package engine

import (
	"botbureau/backend/internal/config"
	"botbureau/backend/internal/model"
	"botbureau/backend/internal/plugin"
	"botbureau/backend/internal/secret"
	"botbureau/backend/internal/skill"

	"botbureau/backend/internal/i18n"

	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// TeamDeps 是全团队共享的资源：共享记忆、任务看板、密钥仓库、插件管理器。
// TeamDeps holds the team-wide shared resources: shared memory, the task board, the key store, and the plugin (MCP server) manager.
type TeamDeps struct {
	TeamMem *Memory
	Board   *TaskBoard
	KS      *secret.KeyStore
	MCP     *plugin.MCPManager
	// 技能库。和插件不同，技能不按 bot 订阅：它只在提示词里占一行 name+description，
	// 全队共享的成本可以忽略，而"谁该用哪个技能"本来就由描述匹配决定，不必再加一层勾选。
	// The skill library. Unlike plugins, skills are not subscribed per bot: one costs a single
	// name + description line in the prompt, so sharing them team-wide is free, and which skill suits
	// whom is already decided by description matching — no second layer of checkboxes needed.
	Skills *skill.Manager
	// 已安装的插件包（Claude/Codex 格式），技能与同事模板都由它带进来
	// Installed plugin bundles (the Claude/Codex format); skills and teammate templates arrive through it
	Bundles *plugin.BundleManager
	XAI     *secret.XaiOAuth
	ChatGPT *secret.ChatGPTOAuth
	// 远程连接器的 OAuth（动态注册 + 授权码 + PKCE）
	// OAuth for remote connectors (dynamic registration, authorization code, PKCE)
	MCPAuth *secret.MCPOAuth
	// 全局设置，工具箱靠它读当前权限档位（用户在设置里改完即时生效）
	// Global settings; the toolbox reads the current permission tier from it so changes take effect at once
	Settings *config.Settings
}

func NewTeamDeps(dataDir string, ks *secret.KeyStore, mcpPath string) *TeamDeps {
	mcp := plugin.NewMCPManager(mcpPath, ks)
	deps := &TeamDeps{
		TeamMem: NewMemory(filepath.Join(dataDir, "TEAM_MEMORY.md")),
		Board:   NewTaskBoard(filepath.Join(dataDir, "tasks.json")),
		KS:      ks,
		MCP:     mcp,
		Skills:  skill.NewManager(filepath.Join(dataDir, "skills")),
		Bundles: plugin.NewBundleManager(filepath.Join(dataDir, "plugins"), mcp),
		XAI:     secret.NewXaiOAuth(filepath.Join(dataDir, "xai_oauth.json")),
		ChatGPT: secret.NewChatGPTOAuth(filepath.Join(dataDir, "chatgpt_oauth.json")),
		MCPAuth: secret.NewMCPOAuth(filepath.Join(dataDir, "mcp_oauth.json")),
	}
	mcp.SetTokenSource(deps.MCPAuth)
	deps.SyncSkillRoots()
	deps.Bundles.SetOnChange(deps.SyncSkillRoots)
	return deps
}

// SyncSkillRoots 让技能库跟上已装插件包：装一个插件，它带的技能立刻可用；卸掉就一并消失。
// 插件包管理器不认识 skill 包（层级上它更底层），所以由 TeamDeps 在中间接一下。
//
// SyncSkillRoots keeps the skill library in step with the installed bundles: install a plugin and its
// skills are available at once, remove it and they go with it. The bundle manager does not know about
// the skill package (it sits lower down), so TeamDeps joins the two.
func (d *TeamDeps) SyncSkillRoots() {
	var roots []skill.Root
	for _, r := range d.Bundles.SkillRoots() {
		roots = append(roots, skill.Root{Source: r.Source, Dir: r.Dir})
	}
	d.Skills.SetRoots(roots)
}

// BotWorker 是一位常驻 goroutine 的「AI 同事」：
// 自己的收件箱（群聊/私聊消息、例程触发按序处理）、独立工作目录、
// 长期记忆，以及群聊/私聊两套相互独立的对话上下文。
// BotWorker is an "AI teammate" running in a resident goroutine:
// it has its own inbox (group chat/DM messages and routine triggers, processed in order), its own workspace,
// long-term memory, and two mutually independent conversation contexts for group chat and DM.
type BotWorker struct {
	Cfg       config.BotConfig
	provider  model.Provider
	sessions  map[string]model.Session // key: "group" / "dm"
	inbox     chan Msg
	busy      atomic.Bool
	cancelFn  atomic.Value // context.CancelFunc
	bus       *Bus
	toolbox   *Toolbox
	mem       *Memory
	deps      *TeamDeps
	workspace string
}

func NewBotWorker(cfg config.BotConfig, bus *Bus, sched *Scheduler, dataDir string, deps *TeamDeps) (*BotWorker, error) {
	provider, err := model.BuildProvider(cfg, deps.KS, deps.XAI, deps.ChatGPT)
	if err != nil {
		return nil, err
	}
	workspace := filepath.Join(dataDir, "workspaces", cfg.Name)
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		return nil, err
	}
	mem := NewMemory(filepath.Join(workspace, "MEMORY.md"))
	w := &BotWorker{
		Cfg:       cfg,
		provider:  provider,
		sessions:  map[string]model.Session{},
		inbox:     make(chan Msg, 256),
		bus:       bus,
		mem:       mem,
		deps:      deps,
		workspace: workspace,
	}
	w.toolbox = NewToolbox(cfg.Name, workspace, mem, deps, cfg.MCP, bus, sched, cfg.Permission)
	w.loadSessions()
	return w, nil
}

// Toolbox 暴露工具箱，供上层做插件订阅这类调整。
// Toolbox exposes the toolbox so the layer above can adjust things like plugin subscriptions.
func (w *BotWorker) Toolbox() *Toolbox { return w.toolbox }

func (w *BotWorker) Name() string          { return w.Cfg.Name }
func (w *BotWorker) Busy() bool            { return w.busy.Load() }
func (w *BotWorker) Queued() int           { return len(w.inbox) }
func (w *BotWorker) ProviderLabel() string { return w.provider.Label() }
func (w *BotWorker) MemoryText() string    { return w.mem.Load() }

func (w *BotWorker) Start() { go w.run() }

func (w *BotWorker) Stop() {
	w.Cancel()
	w.saveSessions()
	select {
	case w.inbox <- Msg{Sender: stopSentinel}:
	default:
	}
}

func (w *BotWorker) Cancel() {
	w.bus.RejectBotApprovals(w.Name(), i18n.T("The user cancelled the task"))
	if fn, ok := w.cancelFn.Load().(context.CancelFunc); ok && fn != nil {
		fn()
	}
}

func (w *BotWorker) run() {
	for msg := range w.inbox {
		if msg.Sender == stopSentinel {
			return
		}
		w.handle(msg)
	}
}

func (w *BotWorker) session(chat string) model.Session {
	s, ok := w.sessions[chat]
	if !ok {
		s = w.provider.NewSession()
		w.sessions[chat] = s
	}
	return s
}

func (w *BotWorker) eventChat(chat string) string {
	if IsGroupChat(chat) {
		return chat
	}
	return "dm:" + w.Name()
}

func (w *BotWorker) handle(msg Msg) {
	chat := msg.Chat
	sess := w.session(chat)

	var text string
	switch {
	case msg.Sender == "user":
		if IsGroupChat(chat) {
			text = i18n.T("User: ") + msg.Content
		} else {
			text = msg.Content
		}
	case strings.HasPrefix(msg.Sender, "routine:"):
		text = fmt.Sprintf(i18n.T("[Routine \"%s\" fired] %s"),
			strings.TrimPrefix(msg.Sender, "routine:"), msg.Content)
	default:
		text = msg.Sender + ": " + msg.Content
	}

	sess.MarkTurn()
	sess.AddUser(text)
	// 群聊背景消息：只入上下文，不回应
	// Group-chat background message: goes into the context only, no reply.
	if !msg.Respond {
		sess.Trim(config.HistoryLimit)
		w.saveSessions()
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	w.cancelFn.Store(cancel)
	w.toolbox.turnCtx = ctx
	w.busy.Store(true)
	w.bus.Emit("status", "", w.Name(), "busy", map[string]any{"busy": true})
	defer func() {
		cancel()
		w.cancelFn.Store((context.CancelFunc)(nil))
		w.toolbox.turnCtx = nil
		if r := recover(); r != nil {
			w.bus.Emit("msg", w.eventChat(chat), w.Name(),
				fmt.Sprintf(i18n.T("Error while handling the message: %v"), r), nil)
		}
		w.busy.Store(false)
		w.bus.Emit("status", "", w.Name(), "idle", map[string]any{"busy": false})
		w.saveSessions()
	}()

	refused := w.agentLoop(ctx, chat, sess)
	if refused {
		sess.Rollback()
	}
	sess.Trim(config.HistoryLimit)
}

// agentLoop 跑完一个回合。返回 true 表示该回合被安全系统拒绝，需要回滚。
// agentLoop runs one turn to completion. It returns true if the turn was refused by the safety system and must be rolled back.
func (w *BotWorker) agentLoop(ctx context.Context, chat string, sess model.Session) bool {
	evChat := w.eventChat(chat)
	w.toolbox.currentChat = chat
	post := func(text string) {
		if IsGroupChat(chat) {
			w.bus.PostGroupTo(chat, w.Name(), text, nil)
		} else {
			w.bus.Emit("msg", evChat, w.Name(), text, nil)
		}
	}
	cancelled := func() bool {
		if ctx == nil || ctx.Err() == nil {
			return false
		}
		w.bus.Emit("msg", evChat, w.Name(), i18n.T("(Cancelled)"), nil)
		return true
	}

	for i := 0; i < config.MaxToolIterations; i++ {
		if cancelled() {
			return false
		}
		res, err := sess.Step(ctx, w.systemPrompt(chat), w.toolbox.Defs(), w.provider.SupportsWebTools())
		if err != nil {
			if cancelled() {
				return false
			}
			w.bus.Emit("msg", evChat, w.Name(), ""+err.Error(), nil)
			// 额度耗尽：额外发全局告警（限频），确保用户在任何界面都能看到
			// Quota exhausted: also emit a rate-limited global alert so the user sees it from any view.
			var qe *model.QuotaError
			if errors.As(err, &qe) {
				w.bus.QuotaAlert(w.provider.Label(), qe.Msg)
			}
			return false
		}

		if res.StopReason == "refusal" {
			w.bus.Emit("msg", evChat, w.Name(), i18n.T("The model's safety system refused this request — try rephrasing it"), nil)
			return true
		}

		for _, note := range res.Notes {
			w.bus.Emit("tool", evChat, w.Name(), ""+note+" …", nil)
		}
		for _, text := range res.Texts {
			post(text)
		}

		switch res.StopReason {
		// 服务端工具轮次被暂停：原样再请求即可续跑
		// A server-side tool round got paused: just re-send the request as-is to resume.
		case "pause_turn":
			continue
		case "tool_use":
			results := make([]model.ToolResult, 0, len(res.ToolCalls))
			for _, call := range res.ToolCalls {
				if ctx != nil && ctx.Err() != nil {
					results = append(results, model.ToolResult{
						ID: call.ID, IsError: true,
						Content: i18n.T("The task was cancelled"),
					})
					continue
				}
				w.bus.Emit("tool", evChat, w.Name(), ""+describeToolCall(call), nil)
				content, isErr := w.toolbox.Execute(call.Name, call.Input)
				results = append(results, model.ToolResult{
					ID: call.ID, Content: content, IsError: isErr,
					Images: w.toolbox.TakeImages(),
				})
			}
			sess.AddToolResults(results)
			if cancelled() {
				return false
			}
			continue
		case "max_tokens":
			// 截断时可能已生成完整 tool_use 块：必须补上配对的 tool_result，否则历史非法
			// Truncation may leave complete tool_use blocks behind: matching tool_results must be appended, or the history becomes invalid.
			if len(res.ToolCalls) > 0 {
				results := make([]model.ToolResult, 0, len(res.ToolCalls))
				for _, call := range res.ToolCalls {
					results = append(results, model.ToolResult{
						ID: call.ID, IsError: true,
						Content: i18n.T("(The reply was truncated by the length limit; this tool call was not executed.)"),
					})
				}
				sess.AddToolResults(results)
			}
			w.bus.Emit("msg", evChat, w.Name(), i18n.T("(The reply was cut off for length — tell me to continue.)"), nil)
			return false
		default: // end_turn 等 / end_turn etc.
			return false
		}
	}
	w.bus.Emit("msg", evChat, w.Name(), i18n.T("(Hit the tool-call limit for this task, so I stopped here — tell me to continue if needed.)"), nil)
	return false
}

func (w *BotWorker) sessionsPath() string {
	return filepath.Join(w.workspace, "sessions.json")
}

func (w *BotWorker) saveSessions() {
	out := map[string]json.RawMessage{}
	for k, s := range w.sessions {
		if snap := s.Snapshot(); len(snap) > 0 {
			out[k] = snap
		}
	}
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(w.sessionsPath(), raw, 0o644)
}

func (w *BotWorker) loadSessions() {
	raw, err := os.ReadFile(w.sessionsPath())
	if err != nil {
		return
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	for k, snap := range m {
		s := w.provider.NewSession()
		if s.Restore(snap) {
			w.sessions[k] = s
		}
	}
}

// RestoreFrom 把旧 worker 的会话上下文迁到新 worker 上。
// 编辑 bot 会重建 worker，但只要 provider 家族没变，对话就不该被清空。
//
// RestoreFrom carries the previous worker's conversation context over to a new one. Editing a bot
// rebuilds its worker, yet the conversation must survive as long as the provider family is unchanged.
func (w *BotWorker) RestoreFrom(old *BotWorker) {
	if old == nil {
		return
	}
	for k, s := range old.sessions {
		ns := w.provider.NewSession()
		if ns.Restore(s.Snapshot()) {
			w.sessions[k] = ns
		}
	}
}

func describeToolCall(call model.ToolCall) string {
	str := func(k string) string { v, _ := call.Input[k].(string); return v }
	switch call.Name {
	case "bash":
		return "$ " + str("command")
	case "read_file", "write_file":
		return call.Name + ": " + str("path")
	case "message_bot":
		return "message_bot → " + str("to")
	case "save_routine":
		return "save_routine: " + str("name")
	default:
		return call.Name
	}
}

func (w *BotWorker) systemPrompt(chat string) string {
	// 群聊场景只列群成员；私聊列全员（便于建议用户找对的人）
	// In group chat, list only group members; in DM, list everyone (so the bot can point the user to the right person).
	var names []string
	if IsGroupChat(chat) {
		names = w.bus.GroupMembersOf(chat)
	} else {
		names = w.bus.BotNames()
	}
	var roster strings.Builder
	for _, n := range names {
		if n == w.Name() {
			continue
		}
		if b := w.bus.Bot(n); b != nil {
			fmt.Fprintf(&roster, i18n.T("- %s (%s): %s\n"), b.Cfg.Title(), b.Cfg.RoleText(), b.Cfg.DescText())
		}
	}
	if roster.Len() == 0 {
		roster.WriteString(i18n.T("(no other members yet)\n"))
	}
	memory := w.MemoryText()
	if memory == "" {
		memory = i18n.T("(none yet)")
	}
	teamMemory := w.deps.TeamMem.Load()
	if teamMemory == "" {
		teamMemory = i18n.T("(none yet)")
	}

	var mode string
	if IsGroupChat(chat) {
		mode = fmt.Sprintf(i18n.T(`# Current setting: team group chat
The user and the AI teammates below are all in this room. Messages carry a speaker prefix ("User:" or a bot name);
you see everything as background, but you only act when you are called on by name (with or without an @),
assigned a task, handed work, or triggered by a routine. Stay silent otherwise.

## Division-of-labor protocol (hard rules against duplicate work)
1. Not called on, not assigned, no action — stay silent when you see someone else's task.
2. Given a broad multi-step task: call list_tasks first, then break it down and assign each subtask with
   assign_task, exactly one owner per subtask (you may assign to yourself). Never message_bot the same job to
   several teammates.
3. Once assigned: set update_task to doing before you start (that is your public claim), then to done with a
   note on the outcome when you finish.
4. Check the board before acting: never redo a task that is doing/done or owned by someone else.
5. Use message_bot to coordinate (messages must be self-contained — the other side cannot see your context);
   report results back to whoever handed you the work. Skip pleasantries and empty acknowledgements so no
   message loop forms.

## Current task board
%s`), w.deps.Board.Render())
	} else {
		mode = i18n.T(`# Current setting: one-on-one DM with the user
Only you and the user are here. Finish the work yourself — you cannot hand it off (message_bot and the task
board are unavailable in DMs). If the task suits someone else or needs several teammates, suggest the user
raise it in the group chat.`)
	}

	// 技能清单。没有技能时整段不出现——空标题会让模型以为自己少了什么能力。
	// The skills section. It disappears entirely when there are none: an empty heading reads to the model
	// as a capability it is somehow missing.
	skillSection := ""
	if roster := w.deps.Skills.Roster(); roster != "" {
		skillSection = fmt.Sprintf(i18n.T(`
# Skills
Each line below is a procedure someone wrote down for a recurring kind of work — only its name and a
one-line summary. When one of them matches the task, call read_skill to load the full instructions
BEFORE you start, and then follow them; they encode conventions you cannot infer. Nothing here is
loaded until you ask for it, so reach for a skill whenever it plausibly fits rather than guessing at
the procedure yourself.
%s`), roster)
	}

	webLine := i18n.T("- You have no web search tool; to fetch web content use curl via bash (it goes through user approval)")
	if w.provider.SupportsWebTools() {
		webLine = i18n.T("- Use web_search / web_fetch to research online")
	}
	if len(w.Cfg.MCP) > 0 {
		webLine += fmt.Sprintf(i18n.T("\n- Connected plugins (MCP): %s. Plugin tool names start with mcp_; non-read-only plugin actions go through user approval first"),
			strings.Join(w.Cfg.MCP, i18n.T(", ")))
	}

	return fmt.Sprintf(i18n.T(`You are %s, the %s on the user's team of AI teammates. %s

The user hands you work as if messaging a colleague: you finish multi-step tasks on your own, and you only stop
and wait when a human decision is required (a command approval, say). Report
the outcome concisely when you are done; while working, speak up only for a significant finding or a change of
direction.

%s

# Other team members
%s%s
# Working environment
- Your workspace: %s (bash, read_file, and write_file are all rooted there)
%s
- Non-read-only bash, paths that leave the workspace, and write_file all need user approval; waiting, being refused, and being cancelled are all normal

# Long-term memory
Use remember for anything worth keeping across sessions: scope=self for your own notes, scope=team for what the
whole team should share (user preferences, team conventions). Don't record what memory already holds.

## Shared team memory
%s

## Your personal memory
%s

# Routines
When the user wants something done periodically, save it with save_routine; write the prompt as a self-contained
task description.

# Communication
Reply in the user's language (English by default). Lead with the conclusion, then the details that matter.%s`),
		w.Cfg.Title(), w.Cfg.RoleText(), w.Cfg.DescText(), mode, roster.String(), skillSection, w.workspace, webLine, teamMemory, memory,
		customPrompt(w.Cfg.Prompt))
}

// customPrompt 渲染 bot 自带的附加说明（导入的同事模板就靠它）。
// 加一层小标题而不是裸拼：模型要能分辨"这是我这个角色的说明"和"这是引擎的规则"。
//
// customPrompt renders a bot's own extra instructions (how an imported teammate template takes effect).
// It gets a heading rather than being pasted on raw: the model has to be able to tell "this describes my
// role" apart from "this is the engine's rules".
func customPrompt(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return fmt.Sprintf(i18n.T("\n\n# Your role in detail\n%s"), s)
}
