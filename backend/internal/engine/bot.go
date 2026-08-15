package engine

import (
	"botbureau/backend/internal/config"
	"botbureau/backend/internal/model"
	"botbureau/backend/internal/plugin"
	"botbureau/backend/internal/secret"
	"botbureau/backend/internal/skill"

	"botbureau/backend/internal/i18n"

	"context"
	"encoding/base64"
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
	// 已安装的插件包（Claude/Codex 格式），技能与成员模板都由它带进来
	// Installed plugin bundles (the Claude/Codex format); skills and member templates arrive through it
	Bundles *plugin.BundleManager
	XAI     *secret.XaiOAuth
	ChatGPT *secret.ChatGPTOAuth
	// 远程连接器的 OAuth（动态注册 + 授权码 + PKCE）
	// OAuth for remote connectors (dynamic registration, authorization code, PKCE)
	MCPAuth *secret.MCPOAuth
	// 全局设置，工具箱靠它读当前权限档位（用户在设置里改完即时生效）
	// Global settings; the toolbox reads the current permission tier from it so changes take effect at once
	Settings *config.Settings
	// 用户从聊天框传上来的文件（见 attach.go）
	// Files the user attached in the composer (see attach.go)
	Uploads *Uploads
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
		Uploads: NewUploads(dataDir),
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

// BotWorker 是一位常驻 goroutine 的「AI 成员」：
// 自己的收件箱（群聊/私聊消息、例程触发按序处理）、独立工作目录、
// 长期记忆，以及群聊/私聊两套相互独立的对话上下文。
// BotWorker is an "AI member" running in a resident goroutine:
// it has its own inbox (group chat/DM messages and routine triggers, processed in order), its own workspace,
// long-term memory, and two mutually independent conversation contexts for group chat and DM.
type BotWorker struct {
	Cfg      config.BotConfig
	provider model.Provider
	sessions map[string]model.Session // key: "group" / "dm"
	inbox    chan Msg
	// deferred 是干活途中从收件箱里捞出来、但不该并进本轮的消息（例程触发、别的 bot 派的活、
	// 别的会话）。它们排在收件箱之前——先来的先办。
	// injected 则是已经并进本轮上下文的插话，留着是为了万一本轮被安全系统拒绝回滚，
	// 用户那几句话不至于跟着一起消失。
	// 两者都只有 worker 自己的 goroutine 碰，不用加锁；对外报的排队数另走原子量。
	//
	// deferred holds messages pulled from the inbox mid-turn that do not belong in the running turn
	// (routine triggers, work handed over by another bot, another conversation). They queue ahead of
	// the inbox, so first come is still first served.
	// injected holds the interjections already merged into this turn, kept so that a turn refused by
	// the safety system can roll back without taking the user's words down with it.
	// Only the worker's own goroutine touches either, so neither needs a lock; the queue length
	// reported outward travels through an atomic instead.
	deferred  []Msg
	deferredN atomic.Int32
	injected  []Msg
	busy      atomic.Bool
	cancelFn  atomic.Value // context.CancelFunc
	bus       *Bus
	toolbox   *Toolbox
	mem       *Memory
	deps      *TeamDeps
	workspace string
	// 用户在对话里指定过的目录。和 workspace 并列而不是塞进它，是因为这两样东西的来历不同：
	// 一个是引擎给的，一个是用户给的，能不能撤销、界面上要不要露出来，答案也不一样。
	// Directories the user named in conversation. Kept beside workspace rather than folded into it
	// because the two come from different places — one the engine hands out, one the user does — and
	// they differ on whether they can be revoked and whether the UI should show them.
	roots *Roots
}

func NewBotWorker(cfg config.BotConfig, bus *Bus, sched *Scheduler, dataDir string, deps *TeamDeps) (*BotWorker, error) {
	provider, err := model.BuildProvider(cfg, deps.KS, deps.XAI, deps.ChatGPT)
	if err != nil {
		return nil, err
	}
	workspace := WorkspaceDir(dataDir, cfg.Name)
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		return nil, err
	}
	mem := NewMemory(filepath.Join(workspace, "MEMORY.md"))
	roots := NewRoots(workspace, dataDir)
	w := &BotWorker{
		Cfg:       cfg,
		provider:  provider,
		sessions:  map[string]model.Session{},
		inbox:     make(chan Msg, 256),
		bus:       bus,
		mem:       mem,
		deps:      deps,
		workspace: workspace,
		roots:     roots,
	}
	w.toolbox = NewToolbox(cfg.Name, workspace, roots, mem, deps, cfg.MCP, bus, sched, cfg.Permission)
	w.loadSessions()
	return w, nil
}

// Toolbox 暴露工具箱，供上层做插件订阅这类调整。
// Toolbox exposes the toolbox so the layer above can adjust things like plugin subscriptions.
func (w *BotWorker) Toolbox() *Toolbox { return w.toolbox }

// Workspace 给出这位成员自己的工作目录。界面要能把它原样印出来：
// 打包之后它落在 ~/Library/Application Support 底下，Finder 默认不显示那一层，
// 目录名又是个从不露面的随机 id——不写出来，用户没有第二条路知道"工作目录"指哪儿。
//
// Workspace is this member's own working directory. The UI needs to be able to print it verbatim: once
// packaged it lives under ~/Library/Application Support, a level Finder hides by default, under a
// random id that never appears anywhere else — without printing it the user has no second way of
// learning which directory "the workspace" means.
func (w *BotWorker) Workspace() string { return w.workspace }

// Roots 暴露这位成员被授予的目录，供设置界面列出与撤销。
// Roots exposes the directories granted to this member, for the settings pane to list and revoke.
func (w *BotWorker) Roots() *Roots { return w.roots }

// grantUserRoots 把用户这条消息里点名的目录收进这位成员能自由活动的范围。
//
// 两个条件缺一不可：这句话是用户亲口说的（Sender=="user"），而且是说给他听的（Respond）。
// 群里飘过一句不指向他的话不该悄悄扩大他的权限；模型的输出和工具的输出则根本不到这里——
// 授权只能由人发起，这是 roots.go 里那条线在调用侧的落点。
//
// 加进去就在会话里说一句。默默扩大一位成员能碰的范围是不行的：用户下次看到的会是
// 「它怎么直接改了我的仓库」，而他确实授过权，只是当时没人告诉他这一句话有这个效果。
//
// grantUserRoots takes the directories named in this message into the area the member may move
// around in freely.
//
// Both conditions are required: the user said it themselves (Sender is "user") and said it to this
// member (Respond). A remark drifting past in the group that was not addressed to them must not
// quietly widen what they may touch, and model or tool output never reaches here at all — a grant can
// only start with a person, which is where the line drawn in roots.go lands on the calling side.
//
// Anything added is announced in the conversation. Widening what a member can reach in silence will
// not do: what the user meets later is "why did it edit my repository", when they did grant it — only
// nobody told them at the time that the sentence carried that weight.
func (w *BotWorker) grantUserRoots(msg Msg) {
	// Sender 这一条是重复的（投递方设 GrantRoots 时已经查过），故意留着。
	// 这是"授权只能由人发起"那条线唯一的执行点，它不该依赖别处某个布尔值填对了。
	//
	// The Sender check is redundant — whoever set GrantRoots already made it — and deliberately kept.
	// This is the one place the "a grant can only start with a person" line is enforced, and it must not
	// rest on some boolean elsewhere having been filled in correctly.
	if msg.Sender != "user" || !msg.GrantRoots {
		return
	}
	for _, dir := range UserPaths(msg.Content) {
		if !w.roots.Add(dir) {
			continue
		}
		w.bus.Emit("system", w.eventChat(msg.Chat), w.Name(),
			fmt.Sprintf(i18n.T("%s counts as a working directory for %s from now on: it may read, write and run commands there without asking, as far as its permission tier allows. Remove it in this member's settings."),
				dir, w.Cfg.Title()), nil)
	}
}

func (w *BotWorker) Name() string          { return w.Cfg.Name }
func (w *BotWorker) Busy() bool            { return w.busy.Load() }
func (w *BotWorker) Queued() int           { return len(w.inbox) + int(w.deferredN.Load()) }
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
	for {
		msg, ok := w.next()
		if !ok || msg.Sender == stopSentinel {
			return
		}
		w.handle(msg)
	}
}

// next 先发上一轮干活期间攒下的消息，攒完了才回收件箱等新的。
// next serves whatever piled up during the last turn before going back to wait on the inbox.
func (w *BotWorker) next() (Msg, bool) {
	if len(w.deferred) > 0 {
		msg := w.deferred[0]
		w.deferred = w.deferred[1:]
		w.deferredN.Store(int32(len(w.deferred)))
		return msg, true
	}
	msg, ok := <-w.inbox
	return msg, ok
}

// defer_ 把一条消息留到本轮之后再办。
// defer_ holds a message over until the current turn is done.
func (w *BotWorker) defer_(msgs ...Msg) {
	w.deferred = append(w.deferred, msgs...)
	w.deferredN.Store(int32(len(w.deferred)))
}

// pumpInbox 在一轮工作的安全点上清一次收件箱：属于当前会话的用户插话和群聊背景消息
// 立刻并进上下文，其余的留到本轮之后。
//
// 这是"插话"和"排队"的分界。用户中途改主意——「别动那个文件」——这句话的价值全在当下；
// 让它等到活干完，纠正到达时错误已经犯完了，而用户手上只剩「停止」这一个整轮作废的钝器。
// 反过来，例程触发和别的 bot 派来的活是新工单，不是对本轮的修正，插进来只会串味，照旧排队。
//
// 调用点必须在补齐 tool_result 之后、发下一次请求之前：assistant 的 tool_use 和它配对的结果
// 之间插一条用户消息，历史就非法了。
//
// pumpInbox drains the inbox at a safe point mid-turn: the user's interjections and group-chat
// background messages for the running conversation merge into the context at once, everything else
// waits for the turn to end.
//
// This is the line between interjecting and queueing. When the user changes their mind mid-task —
// "don't touch that file" — the whole value of the sentence is that it arrives now; make it wait and
// the correction lands after the damage, leaving Stop, which throws the entire turn away, as the only
// recourse. Routine triggers and work handed over by another bot are new assignments rather than
// corrections to this turn, so those keep queueing.
//
// It may only be called once tool_results are complete and before the next request goes out: slipping
// a user message between an assistant's tool_use and its matching result makes the history invalid.
func (w *BotWorker) pumpInbox(chat string, sess model.Session) []Msg {
	var taken []Msg
	for {
		select {
		case msg, ok := <-w.inbox:
			if !ok {
				return taken
			}
			// 停止信号、别的会话、别的 bot 派的活 → 原样留到本轮之后
			// The stop signal, another conversation, work from another bot → held over
			if msg.Sender == stopSentinel || msg.Chat != chat || (msg.Respond && msg.Sender != "user") {
				w.defer_(msg)
				continue
			}
			msg.Interject = true
			sess.AddUser(w.renderMsg(msg))
			taken = append(taken, msg)
		default:
			return taken
		}
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

// renderMsg 把一条收件箱消息拼成模型看到的那段文字：谁说的、在回哪一条（若是引用回复）、正文。
// renderMsg turns an inbox message into the text the model sees: who spoke, which message it answers
// (when it is a quote reply), and the body.
func (w *BotWorker) renderMsg(msg Msg) string {
	var text string
	switch {
	case msg.Sender == "user":
		if IsGroupChat(msg.Chat) {
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
	if q := msg.Quote; q != nil {
		text = fmt.Sprintf(i18n.T("[Replying to %s: \"%s\"]\n"), w.speakerName(q.From), q.Text) + text
	}
	// 插话要自报身份。
	//
	// 不标的话，模型看到的和一条崭新的用户消息一模一样，于是很容易答完就收工——而它手上
	// 那件事引擎根本没记，没有任何代码会把它捡回来（这一轮结束就结束了，看板上那条却还写着
	// doing）。所以「答完接着干」这句话必须由消息本身带着，不能指望模型自己想起来。
	//
	// 也留了改主意的口子：用户中途插话，本来就有可能是要换个方向，逼它一定回去做原来的事
	// 同样不对。判断权给模型，前提是它知道自己正被打断。
	//
	// An interjection says so itself.
	//
	// Unmarked it looks exactly like a fresh user message, which makes it easy to answer and call the
	// turn done — while the job it was in the middle of is recorded nowhere in the engine and no code
	// will pick it back up (the turn simply ends, though the board still says doing). So "carry on
	// afterwards" has to travel with the message rather than be left for the model to remember.
	//
	// Room is left to change course, too: someone breaking in mid-task may well mean to redirect it, and
	// forcing a return to the original work would be just as wrong. The judgement stays with the model,
	// on condition that it knows it is being interrupted.
	// 背景消息同样要自报身份，而且这一条比插话那条更要紧。
	//
	// 投递的那一刻引擎是知道的——Respond=false 就是「这句不是派给你的」——可这个判断
	// 一个字都没传给模型：群聊里的用户消息一律渲染成 "User: 正文"，是背景还是指派看不出来。
	// 于是正在干活的那位看到一句普通的用户提问，丢下手上的事去答了，而这句话早已派给了别人。
	// 一个问题两个答案，用户还分不出哪个是查过的、哪个是猜的。
	//
	// 分工协议第 1 条（"没被点名就别动"）本来正是管这个的，但它管不着——模型判断不了
	// 自己有没有被点名，因为那个结论没给它。这里补上。
	//
	// 和插话那条互斥：Respond 为真才是插话（活是你的，答完接着干），为假就是背景
	// （不是你的，别接话，继续手上的）。
	//
	// A background message announces itself too, and this one matters more than the interjection note.
	//
	// The engine knows at delivery time — Respond being false is exactly "this was not addressed to you"
	// — and not one word of that judgement reaches the model: every user message in a group renders as
	// "User: <text>", with nothing to separate background from assignment. So whoever is mid-task sees an
	// ordinary user question, drops what they were doing and answers it, when it went to someone else
	// entirely. One question, two answers, and no way for the user to tell the researched one from the
	// guessed one.
	//
	// Rule 1 of the division-of-labor protocol ("not called on, no action") is meant for precisely this
	// and cannot reach it: the model has no way to tell whether it was called on, because the conclusion
	// was never handed over. This hands it over.
	//
	// Mutually exclusive with the interjection note: Respond true means an interjection (the job is
	// yours, deal with it and carry on), false means background (not yours, stay out of it).
	if msg.Sender == "user" && !msg.Respond {
		return i18n.T("[Background — the user said this in the room but it was not addressed to you; it went to someone else. Do not answer it. Carry on with what you were doing; this is here only so you know what is going on.]\n") + text
	}
	if msg.Interject && msg.Sender == "user" {
		text = i18n.T("[The user broke in while you were working. Take it on board, then carry on with what you were doing — unless it replaces the task, in which case say so and drop the old one.]\n") + text
	}
	return text
}

// receiveFiles 把这条消息带来的附件放进自己的工作目录，并给出该进上下文的正文和图片。
//
// 两件事都要做，不是二选一：
//
//   - 落盘。附件进 <工作目录>/inbox/，正文末尾附一份清单写明路径。有了它，这份文件才是
//     它能 read_file、能跑命令去处理的东西；只在上下文里"看见"一张图，是没法拿去裁剪或转换的。
//   - 喂图。图片同时作为图片块进上下文，模型才真的看得见内容，而不是只知道有这么个文件。
//
// 文件放不进去（磁盘满、原件被删）就不硬凑：清单里不列它，模型也就不会去读一个不存在的路径。
//
// receiveFiles places this message's attachments into the member's own workspace and returns the text
// and images that should enter the context.
//
// Both halves are needed, not one or the other:
//
//   - On disk. Attachments land in <workspace>/inbox/ and a list of their paths is appended to the
//     text. That is what makes a file something it can read_file or run a command over; merely "seeing"
//     an image in context gives it nothing to crop or convert.
//   - In context. Images also travel as image blocks, so the model sees what is in them rather than
//     only learning that a file exists.
//
// A file that cannot be placed (a full disk, a missing original) is not papered over: it stays off the
// list, so the model is never pointed at a path that is not there.
func (w *BotWorker) receiveFiles(msg Msg) (string, []model.ResultImage) {
	text := w.renderMsg(msg)
	if len(msg.Files) == 0 || w.deps.Uploads == nil {
		return text, nil
	}
	paths := w.deps.Uploads.Deliver(w.workspace, msg.Files)
	text += Describe(msg.Files, paths)

	var images []model.ResultImage
	for i, f := range msg.Files {
		if i >= len(paths) || !f.IsImage() {
			continue
		}
		raw, err := w.deps.Uploads.Read(f)
		if err != nil {
			continue
		}
		images = append(images, model.ResultImage{MIME: f.MIME, Base64: base64.StdEncoding.EncodeToString(raw)})
	}
	return text, images
}

// speakerName 把发言人 id 说成模型认得的称呼。
// speakerName renders a speaker id as the name the model knows them by.
func (w *BotWorker) speakerName(id string) string {
	switch {
	case id == "user":
		return i18n.T("the user")
	case id == w.Name():
		return i18n.T("you")
	}
	if b := w.bus.Bot(id); b != nil {
		return b.Cfg.Title()
	}
	return id
}

func (w *BotWorker) handle(msg Msg) {
	chat := msg.Chat
	sess := w.session(chat)

	w.grantUserRoots(msg)
	text, images := w.receiveFiles(msg)

	sess.MarkTurn()
	sess.AddUser(text, images...)
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

	w.injected = nil
	refused := w.agentLoop(ctx, chat, sess, msg)
	if refused {
		// 回滚会把本轮的一切都撤掉，包括中途插进来的话。那几句用户还没得到任何回应，
		// 不能就这么消失——放回队首，当成新的一轮重来。
		// The rollback undoes everything in this turn, interjections included. The user has had no
		// answer to those yet, so they must not just vanish: they go back to the head of the queue
		// and start a turn of their own.
		sess.Rollback()
		w.deferred = append(append([]Msg(nil), w.injected...), w.deferred...)
		w.deferredN.Store(int32(len(w.deferred)))
	}
	w.injected = nil
	sess.Trim(config.HistoryLimit)
}

// agentLoop 跑完一个回合。返回 true 表示该回合被安全系统拒绝，需要回滚。
// agentLoop runs one turn to completion. It returns true if the turn was refused by the safety system and must be rolled back.
func (w *BotWorker) agentLoop(ctx context.Context, chat string, sess model.Session, trigger Msg) bool {
	evChat := w.eventChat(chat)
	w.toolbox.currentChat = chat
	// 本轮开口的第一句引用触发它的那条消息，之后的不再引用：一段回复只需要指一次"我在答什么"，
	// 每句都挂个引文就成了噪音。中途有人插话时会换成插话那条——那时候答的已经是新问题了。
	//
	// The first thing said this turn quotes the message that triggered it, and nothing after does: a
	// reply needs to point at what it answers once, and a quotation on every line is just noise. An
	// interjection replaces it, because from then on the answer is to the new question.
	quote := quoteOf(trigger)
	post := func(text string) {
		q := quote
		quote = nil
		if IsGroupChat(chat) {
			w.bus.PostGroupQuoted(chat, w.Name(), text, nil, q)
		} else {
			w.bus.Emit("msg", evChat, w.Name(), text, q.Extra())
		}
	}
	cancelled := func() bool {
		if ctx == nil || ctx.Err() == nil {
			return false
		}
		w.bus.Emit("msg", evChat, w.Name(), i18n.T("(Cancelled)"), nil)
		return true
	}

	// 服务端工具把这一轮暂停时（pause_turn）历史要原样再发一遍才能续上，
	// 那个位置不能塞消息进去；除此之外的每次请求之前都是安全的收话点。
	// When a server-side tool pauses the turn, the history has to go back out unchanged to resume, so
	// nothing may be slipped in at that point; before every other request is a safe place to listen.
	listen := true
	var repeat repeatWatch
	spinning, spinningTool := false, ""
	for i := 0; i < config.MaxToolIterations; i++ {
		if cancelled() {
			return false
		}
		if listen {
			for _, m := range w.pumpInbox(chat, sess) {
				w.injected = append(w.injected, m)
				if m.Sender != "user" || !m.Respond {
					continue
				}
				w.bus.Emit("tool", evChat, w.Name(), i18n.T("Read your new message")+" …", nil)
				quote = quoteOf(m)
			}
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
			listen = false
			continue
		case "tool_use":
			listen = true
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
				// 一批里只要有一个转到头就算，而且记住是哪一个——后面那句话要说清楚
				// 是哪个工具卡住了，不能被同一批里紧随其后的别的调用盖掉。
				// One call in a batch reaching the limit is enough, and which one is remembered: the line
				// printed afterwards has to name the tool that got stuck, not whichever call happened to
				// come next in the same batch.
				if !spinning && repeat.saw(call) >= config.MaxRepeatedCalls {
					spinning, spinningTool = true, call.Name
				}
			}
			// 结果照样补齐再停。tool_use 和它配对的 tool_result 之间断开，历史就非法了，
			// 下一条消息进来时整段上下文都用不了——为了少写一次结果把会话废掉，不划算。
			//
			// The results still go in before stopping. A tool_use left without its matching tool_result
			// makes the history invalid, and the whole context is unusable when the next message arrives
			// — a poor trade for skipping one write.
			sess.AddToolResults(results)
			if cancelled() {
				return false
			}
			if spinning {
				w.bus.Emit("msg", evChat, w.Name(), fmt.Sprintf(
					i18n.T("(I called %s with the same arguments %d times over and got nowhere, so I stopped. Tell me what to try instead.)"),
					spinningTool, config.MaxRepeatedCalls), nil)
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

// repeatWatch 数同一个工具带同样参数连着调了几次。
//
// 比的是"工具名 + 参数"的整体，不是工具名：同一个 bash 换条命令、同一个 read_file 换个文件，
// 都是在往前走，只有参数一个字没变的重复才说明它卡住了——结果不会变，再跑多少圈都一样。
//
// 只数"连着"的。中间插进任何别的调用就归零：那说明它换了思路，前面那几次重复已经过去了。
//
// repeatWatch counts how many times in a row the same tool has been called with the same arguments.
//
// It compares the tool name and its arguments together rather than the name alone: the same bash with a
// different command, or the same read_file on a different file, is progress. Only a repeat with not one
// argument changed says it is stuck, because the result cannot come back any different however many
// more laps it runs.
//
// Consecutive only. Any other call in between resets it: that means the approach changed, and whatever
// repeated before is behind it.
type repeatWatch struct {
	sig   string
	count int
}

func (r *repeatWatch) saw(call model.ToolCall) int {
	// map 的键在 encoding/json 里是排好序输出的，所以同样的参数必然得到同一串签名
	// Map keys are emitted in sorted order by encoding/json, so identical arguments always yield the
	// same signature
	args, _ := json.Marshal(call.Input)
	sig := call.Name + "\x00" + string(args)
	if sig != r.sig {
		r.sig, r.count = sig, 0
	}
	r.count++
	return r.count
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
	case "fetch_url":
		return "fetch_url: " + str("url")
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
The user and the AI members below are all in this room. Messages carry a speaker prefix ("User:" or a bot name);
you see everything as background, but you only act when you are called on by name (with or without an @),
assigned a task, handed work, or triggered by a routine. Stay silent otherwise.

## Division-of-labor protocol (hard rules against duplicate work)
1. Not called on, not assigned, no action — stay silent when you see someone else's task, and equally when
   the user asks the room something without naming you. Exactly one member is given each message; if it was
   not you, it was somebody else, and answering anyway means the user gets the same question answered twice.
2. Handing work over and doing it yourself are alternatives, never both. Once you have passed something to
   someone, that is your contribution: stop there, add nothing further on the subject, and wait for them to
   report back. Two answers to one question is worse than one, because the user cannot tell which to trust —
   and the one that sounds equally certain may be the one that was guessed.
3. Given a broad multi-step task: call list_tasks first, then break it down and assign each subtask with
   assign_task, exactly one owner per subtask (you may assign to yourself). Never message_bot the same job to
   several members.
4. Once assigned: set update_task to doing before you start (that is your public claim), then to done with a
   note on the outcome when you finish.
5. Check the board before acting: never redo a task that is doing/done or owned by someone else.
6. Use message_bot to coordinate (messages must be self-contained — the other side cannot see your context);
   report results back to whoever handed you the work. Skip pleasantries and empty acknowledgements so no
   message loop forms.

## Current task board
%s`), w.deps.Board.Render())
	} else {
		mode = i18n.T(`# Current setting: one-on-one DM with the user
Only you and the user are here. Finish the work yourself — you cannot hand it off (message_bot and the task
board are unavailable in DMs). If the task suits someone else or needs several members, suggest the user
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

	// 用户指定的目录。一个都没有时整行不出现——空着的一句"你还可以去这些地方：（无）"
	// 只会让模型琢磨它是不是漏了什么。
	// Directories the user named. The line disappears when there are none: "you may also reach: (none)"
	// only invites the model to wonder what it is missing.
	rootsLine := ""
	if d := w.roots.Describe(); d != "" {
		rootsLine = d + "\n"
	}

	// 没有服务端联网工具的 provider 也有 fetch_url 可用，所以这里不再把人往 curl 上引——
	// 那条路每取一个网页都要用户点一次头，而点下去交出的是一整个 shell。
	// A provider without server-side web tools still has fetch_url, so nothing points at curl any more:
	// that route cost the user one approval per page, and what it approved was a whole shell.
	webLine := i18n.T("- Use fetch_url to read a page or an API online; you have no search engine, so work from addresses you already know or that the user gave you")
	if w.provider.SupportsWebTools() {
		webLine = i18n.T("- Use web_search / web_fetch to research online, or fetch_url for one specific address")
	}
	if len(w.Cfg.MCP) > 0 {
		webLine += fmt.Sprintf(i18n.T("\n- Connected plugins (MCP): %s. Plugin tool names start with mcp_; non-read-only plugin actions go through user approval first"),
			strings.Join(w.Cfg.MCP, i18n.T(", ")))
	}

	return fmt.Sprintf(i18n.T(`You are %s, the %s on the user's team of AI members. %s

The user hands you work as if messaging a team member: you finish multi-step tasks on your own, and you only stop
and wait when a human decision is required (a command approval, say). Report
the outcome concisely when you are done; while working, speak up only for a significant finding or a change of
direction.

%s

# Other team members
%s%s
# Working environment
- Your workspace: %s (bash runs there, and a relative path in read_file or write_file is relative to it)
%s%s
- /tmp is scratch space you may use freely for intermediate files; it needs no approval
- Non-read-only bash, paths outside the directories above, and write_file all need user approval; waiting, being refused, and being cancelled are all normal

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
		w.Cfg.Title(), w.Cfg.RoleText(), w.Cfg.DescText(), mode, roster.String(), skillSection, w.workspace, rootsLine, webLine, teamMemory, memory,
		customPrompt(w.Cfg.Prompt))
}

// customPrompt 渲染 bot 自带的附加说明（导入的成员模板就靠它）。
// 加一层小标题而不是裸拼：模型要能分辨"这是我这个角色的说明"和"这是引擎的规则"。
//
// customPrompt renders a bot's own extra instructions (how an imported member template takes effect).
// It gets a heading rather than being pasted on raw: the model has to be able to tell "this describes my
// role" apart from "this is the engine's rules".
func customPrompt(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return fmt.Sprintf(i18n.T("\n\n# Your role in detail\n%s"), s)
}
