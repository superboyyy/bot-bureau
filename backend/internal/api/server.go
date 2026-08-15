package api

import (
	"botbureau/backend/internal/bridge"
	"botbureau/backend/internal/config"
	"botbureau/backend/internal/engine"
	"botbureau/backend/internal/httpx"
	"botbureau/backend/internal/model"
	"botbureau/backend/internal/plugin"
	"crypto/rand"
	"encoding/hex"

	"botbureau/backend/internal/i18n"

	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// App 聚合运行时对象，并负责 bots.yaml 的持久化。
// App aggregates the runtime objects and owns persistence of bots.yaml.
type App struct {
	bus      *engine.Bus
	sched    *engine.Scheduler
	deps     *engine.TeamDeps
	tg       *bridge.TGBridge
	settings *config.Settings
	cfgs     []config.BotConfig
	cfgPath  string
	dataDir  string
	mu       sync.Mutex
}

func NewApp(bus *engine.Bus, sched *engine.Scheduler, deps *engine.TeamDeps, tg *bridge.TGBridge, settings *config.Settings, cfgs []config.BotConfig, cfgPath, dataDir string) *App {
	return &App{bus: bus, sched: sched, deps: deps, tg: tg, settings: settings, cfgs: cfgs, cfgPath: cfgPath, dataDir: dataDir}
}

// 每次启动随机一个实例 id，通过 /api/ping 公开。
//
// 局域网发现拿回来的是"某个地址上有个引擎"，客户端得能认出其中哪个是自己——本机引擎也在
// mDNS 上广播，不然界面每次都会提示你"跟自己配对"。主机名不够用：同一台机器可以跑好几个
// 数据目录各不相同的引擎。
//
// 用随机值而不是配对码的哈希：/api/ping 是免认证的，任何和配对码有关的东西都不该出现在这里。
// 只需要在一次会话里区分"我"和"别人"，重启后换一个也无所谓。
//
// A random instance id per start, published on /api/ping.
//
// LAN discovery returns "there is an engine at this address", and the client has to recognise which
// one is itself — the local engine advertises over mDNS too, or the UI would offer to pair you with
// yourself on every launch. The hostname will not do: one machine can run several engines with
// different data directories.
//
// Random rather than a hash of the pairing code: /api/ping is unauthenticated, and nothing derived
// from the pairing code belongs there. Telling "me" from "them" within one session is all that is
// needed, and a fresh value after a restart is harmless.
var instanceID = newInstanceID()

func newInstanceID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(buf[:])
}

var reservedNames = map[string]bool{"user": true, "scheduler": true, "system": true, "group": true}

// credentials 把钥匙串和两份订阅令牌打包成模型层要的凭据解析器。
// credentials packages the key store and the two subscription tokens into the resolver the model layer wants.
func (a *App) credentials() model.CredentialFunc {
	return model.Credentials(a.deps.KS, a.deps.XAI, a.deps.ChatGPT)
}

func (a *App) AddBot(cfg config.BotConfig) (*engine.BotWorker, error) {
	cfg.Name = strings.TrimSpace(cfg.Name)
	if !config.ValidBotName(cfg.Name) {
		return nil, errors.New(i18n.T("Name must be 1-24 lowercase letters/digits/-/_"))
	}
	if reservedNames[cfg.Name] || a.bus.Bot(cfg.Name) != nil {
		return nil, fmt.Errorf(i18n.T("The name %s is already taken"), cfg.Name)
	}
	if err := a.validateBot(&cfg); err != nil {
		return nil, err
	}
	w, err := engine.NewBotWorker(cfg, a.bus, a.sched, a.dataDir, a.deps)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.cfgs = append(a.cfgs, cfg)
	err = config.SaveBotConfigs(a.cfgPath, a.cfgs)
	a.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf(i18n.T("Failed to save bots.yaml: %v"), err)
	}
	a.bus.Register(w)
	// 新建的 bot 不自动进任何群。
	//
	// 群聊是"这几位一起做这件事"，谁在里面是用户的决定；新人自动入群意味着他会读到群里的
	// 全部往来、可能被点名接活，而用户根本没表态。要拉进去，在群聊设置里勾一下就行。
	//
	// A new bot joins no group automatically.
	//
	// A group chat means "these people, on this piece of work", and who belongs there is the user's
	// call. Auto-joining a newcomer would let it read everything said in the room and be handed work,
	// none of which the user asked for. Adding it is one checkbox in the group's settings.
	w.Start()
	a.bus.Emit("refresh", "", "system", i18n.T("New member ")+cfg.Name+i18n.T(" joined the team"), nil)
	return w, nil
}

// UpdateBot 用新配置替换一个已有 bot。@提及用的 id（Name）不可改，其余字段整体覆盖。
// 保存会让该 bot 带着新配置重启——换 provider/模型时旧的对话上下文本来也没法沿用。
// UpdateBot replaces an existing bot's configuration. Its @mention id (Name) is fixed; everything else is
// overwritten wholesale. Saving restarts the bot with the new config — switching provider or model would
// invalidate the old conversation context anyway.
func (a *App) UpdateBot(cfg config.BotConfig) error {
	cfg.Name = strings.TrimSpace(cfg.Name)
	if a.bus.Bot(cfg.Name) == nil {
		return fmt.Errorf(i18n.T("There is no bot named %s"), cfg.Name)
	}
	if err := a.validateBot(&cfg); err != nil {
		return err
	}
	// 先把替身建出来：provider 建不起来（比如 key 没填）就整个放弃，别把好好的 bot 拆了
	// Build the replacement first: if the provider fails (e.g. a missing key), abort rather than tear down a working bot
	w, err := engine.NewBotWorker(cfg, a.bus, a.sched, a.dataDir, a.deps)
	if err != nil {
		return err
	}
	if old := a.bus.Replace(cfg.Name, w); old != nil {
		old.Cancel()
		w.RestoreFrom(old)
		old.Stop()
	}
	a.mu.Lock()
	for i := range a.cfgs {
		if a.cfgs[i].Name == cfg.Name {
			a.cfgs[i] = cfg
		}
	}
	err = config.SaveBotConfigs(a.cfgPath, a.cfgs)
	a.mu.Unlock()
	w.Start()
	if err != nil {
		return fmt.Errorf(i18n.T("Failed to save bots.yaml: %v"), err)
	}
	a.bus.Emit("refresh", "", "system", fmt.Sprintf(i18n.T("%s's settings were updated"), cfg.Title()), nil)
	return nil
}

// validateBot 规整并校验新建/编辑共用的字段。
// validateBot normalizes and checks the fields shared by create and edit.
func (a *App) validateBot(cfg *config.BotConfig) error {
	cfg.Role = strings.TrimSpace(cfg.Role)
	if cfg.Role == "" {
		cfg.Role = i18n.T("assistant")
	}
	cfg.DisplayName = strings.TrimSpace(cfg.DisplayName)
	if utf8.RuneCountInString(cfg.DisplayName) > config.DisplayNameMax {
		return fmt.Errorf(i18n.T("The display name may be at most %d characters"), config.DisplayNameMax)
	}
	if !config.ValidEffort(cfg.Effort) {
		return errors.New(i18n.T("The reasoning effort must be minimal / low / medium / high, or empty for the vendor default"))
	}
	// 档位得是这家服务商真认的那几个：递一个它不认识的过去就是一个说不清的 400。
	// The tier has to be one this provider actually takes: handing it an unknown one is an inscrutable 400.
	if !config.EffortSupported(cfg.ProviderID, cfg.Effort) {
		return fmt.Errorf(i18n.T("%s does not offer the %s reasoning effort"), cfg.ProviderID, cfg.Effort)
	}
	if cfg.Permission != "" && !config.ValidPerm(cfg.Permission) {
		return errors.New(i18n.T("The permission tier must be ask / edit / auto / full, or empty to follow the global setting"))
	}
	if !config.ValidAvatar(cfg.Avatar) {
		return errors.New(i18n.T("The avatar must be a #rrggbb color or a small png/jpeg/webp image"))
	}
	// 附加提示词进的是每一次请求的系统提示，太长会一直烧上下文，卡一个上限
	// Extra instructions ride in the system prompt of every single request, so an overlong one burns
	// context forever — cap it
	cfg.Prompt = strings.TrimSpace(cfg.Prompt)
	if len(cfg.Prompt) > config.PromptMax {
		return fmt.Errorf(i18n.T("The role instructions may be at most %d characters"), config.PromptMax)
	}
	for _, srv := range cfg.MCP {
		if !a.deps.MCP.Has(srv) {
			return fmt.Errorf(i18n.T("There is no plugin named %s"), srv)
		}
	}
	return nil
}

// RemoveBot 把一个 bot 移出团队。最后一个不许删：bots.yaml 变成空表后引擎下次直接拒绝启动
// （config.LoadBotConfigs 会报 "没有定义任何 bot"），用户就只能手改 yaml 才能把应用救回来。
//
// RemoveBot takes a bot off the team. The last one cannot be removed: an empty bots.yaml makes the
// engine refuse to start next time (config.LoadBotConfigs reports "no bots are defined"), leaving the user
// to hand-edit YAML to get the app back.
func (a *App) RemoveBot(name string) error {
	if a.bus.Bot(name) == nil {
		return fmt.Errorf(i18n.T("No bot named %s exists"), name)
	}
	w := a.bus.Unregister(name)
	if w == nil {
		return fmt.Errorf(i18n.T("No bot named %s exists"), name)
	}
	w.Stop()
	a.mu.Lock()
	kept := a.cfgs[:0]
	for _, c := range a.cfgs {
		if c.Name != name {
			kept = append(kept, c)
		}
	}
	a.cfgs = kept
	_ = config.SaveBotConfigs(a.cfgPath, a.cfgs)
	a.mu.Unlock()
	a.bus.Emit("refresh", "", "system", name+i18n.T(" was removed from the team"), nil)
	return nil
}

func (a *App) State() map[string]any {
	bots := []map[string]any{}
	for _, w := range a.bus.Bots() {
		mcpList := w.Cfg.MCP
		if mcpList == nil {
			mcpList = []string{}
		}
		cfg := w.Cfg
		cfg.MCP = mcpList
		bots = append(bots, map[string]any{
			"name": w.Name(), "role": w.Cfg.RoleText(), "description": w.Cfg.DescText(),
			"provider": w.ProviderLabel(), "busy": w.Busy(), "queued": w.Queued(),
			"mcp": mcpList,
			// 编辑表单要的是未本地化的原始配置，不能拿上面那份显示用的覆盖回去
			// The edit form needs the raw, unlocalized config — writing the display copy back would clobber it
			"config": cfg,
			// 实际生效的档位：bot 没设时这里是全局值，界面用它显示"跟随全局（xx）"
			// The tier actually in force: the global value when the bot sets none, so the UI can show "follow global (xx)"
			"permission": string(config.ResolvePerm(w.Cfg.Permission, a.settings.Perm())),
		})
	}
	approvals := []map[string]any{}
	for _, ap := range a.bus.PendingApprovals() {
		approvals = append(approvals, map[string]any{
			"id": ap.ID, "bot": ap.Bot, "action": ap.Action, "chat": ap.Chat,
		})
	}
	routines := []map[string]any{}
	for _, r := range a.sched.List() {
		routines = append(routines, map[string]any{
			"name": r.Name, "bot": r.Bot, "prompt": r.Prompt,
			"every_minutes": r.EveryMinutes, "next_run": r.NextRun,
		})
	}
	events := a.bus.Recent(400)
	if events == nil {
		events = []engine.Event{}
	}
	members := a.bus.GroupMembers()
	if members == nil {
		members = []string{}
	}
	tasks := a.deps.Board.List()
	return map[string]any{
		"bots": bots, "default_bot": a.bus.DefaultGroupMember(),
		"group_members": members, "groups": a.bus.Groups(), "tasks": tasks, "keys": a.deps.KS.List(),
		"mcp":       a.deps.MCP.Status(),
		"skills":    a.deps.Skills.List(),
		"plugins":   a.deps.Bundles.List(),
		"telegram":  a.tg.Status(),
		"settings":  a.settings.Status(),
		"xai":       a.deps.XAI.Status(),
		"chatgpt":   a.deps.ChatGPT.Status(),
		"approvals": approvals, "routines": routines, "events": events,
	}
}

// ---- HTTP ----

// cors 允许 Electron 渲染进程（file:// origin）跨域访问本地后端。
// cors lets the Electron renderer process (file:// origin) make cross-origin requests to the local backend.
func cors(next http.HandlerFunc) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Access-Control-Allow-Origin", "*")
		rw.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		// Authorization 必须放行：配对码就是靠这个头带上来的。
		// 少了它，任何跨源的客户端在预检那一步就被自己的浏览器挡掉——Electron 的 file:// 页面
		// 不走预检所以一直没暴露，但浏览器端和远程客户端会直接连不上。
		//
		// Authorization has to be allowed: the pairing code travels in that header. Without it any
		// cross-origin client is stopped by its own browser at the preflight — Electron's file:// pages
		// skip preflight, which is why this stayed hidden, but browser and remote clients simply fail.
		rw.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			rw.WriteHeader(http.StatusNoContent)
			return
		}
		next(rw, r)
	}
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/", cors(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			httpx.WriteJSON(rw, 404, map[string]any{"error": "not found"})
			return
		}
		rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(rw, i18n.T("Bot Bureau backend is running. Use the Electron client (app/) to access it."))
	}))

	// 发现探针：不含敏感信息、无需配对码（mDNS 发现后用它确认是 Bot Bureau）
	// discovery probe: contains no sensitive info and needs no pairing code (used after mDNS discovery to confirm it's Bot Bureau)
	mux.HandleFunc("/api/ping", cors(func(rw http.ResponseWriter, r *http.Request) {
		host, _ := os.Hostname()
		httpx.WriteJSON(rw, 200, map[string]any{
			"app": "botbureau", "name": host, "version": "0.1.0", "instance": instanceID,
		})
	}))

	mux.HandleFunc("/api/state", cors(func(rw http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(rw, 200, a.State())
	}))

	mux.HandleFunc("/api/events", cors(a.handleSSE))

	mux.HandleFunc("/api/send", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Chat string `json:"chat"`
			Text string `json:"text"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		text := strings.TrimSpace(body.Text)
		if text == "" {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Message is empty")})
			return
		}
		switch {
		case engine.IsGroupChat(body.Chat) || body.Chat == "":
			chat := body.Chat
			if chat == "" {
				chat = "group"
			}
			targets := a.bus.MentionedBotsIn(chat, text)
			if len(targets) == 0 {
				def := a.bus.DefaultGroupMemberOf(chat)
				if def == "" {
					httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("The group chat has no members — add someone in the group settings first")})
					return
				}
				targets = []string{def}
			}
			a.bus.PostGroupTo(chat, "user", text, targets)
		case strings.HasPrefix(body.Chat, "dm:"):
			name := strings.TrimPrefix(body.Chat, "dm:")
			if a.bus.Bot(name) == nil {
				httpx.WriteJSON(rw, 404, map[string]any{"error": i18n.T("No bot named ") + name + i18n.T(" exists")})
				return
			}
			a.bus.Emit("msg", body.Chat, "user", text, nil)
			a.bus.Deliver("user", name, "dm", text, true)
		default:
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("chat must be a group or dm:<bot name>")})
			return
		}
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true})
	}))

	mux.HandleFunc("/api/cancel", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil || body.Name == "" {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		w := a.bus.Bot(body.Name)
		if w == nil {
			httpx.WriteJSON(rw, 404, map[string]any{"error": i18n.T("No bot named ") + body.Name + i18n.T(" exists")})
			return
		}
		w.Cancel()
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true})
	}))

	mux.HandleFunc("/api/approve", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			ID       int    `json:"id"`
			Approved bool   `json:"approved"`
			Reason   string `json:"reason"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		if !a.bus.Decide(body.ID, body.Approved, body.Reason) {
			httpx.WriteJSON(rw, 404, map[string]any{"error": i18n.T("Approval not found (it may already be handled)")})
			return
		}
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true})
	}))

	mux.HandleFunc("/api/bots", cors(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			httpx.WriteJSON(rw, 405, map[string]any{"error": "method not allowed"})
			return
		}
		var cfg config.BotConfig
		if err := httpx.ReadJSON(r, &cfg); err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		if _, err := a.AddBot(cfg); err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": err.Error()})
			return
		}
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true})
	}))

	mux.HandleFunc("/api/bots/update", cors(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			httpx.WriteJSON(rw, 405, map[string]any{"error": "method not allowed"})
			return
		}
		var cfg config.BotConfig
		if err := httpx.ReadJSON(r, &cfg); err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		if err := a.UpdateBot(cfg); err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": err.Error()})
			return
		}
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true})
	}))

	mux.HandleFunc("/api/bots/delete", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil || body.Name == "" {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		if err := a.RemoveBot(body.Name); err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": err.Error()})
			return
		}
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true})
	}))

	mux.HandleFunc("/api/xai/oauth/start", cors(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			httpx.WriteJSON(rw, 405, map[string]any{"error": "method not allowed"})
			return
		}
		st, err := a.deps.XAI.Start()
		if err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": err.Error()})
			return
		}
		httpx.WriteJSON(rw, 200, st)
	}))
	mux.HandleFunc("/api/xai/oauth/status", cors(func(rw http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(rw, 200, a.deps.XAI.Status())
	}))
	mux.HandleFunc("/api/xai/oauth/cancel", cors(func(rw http.ResponseWriter, r *http.Request) {
		a.deps.XAI.Cancel()
		httpx.WriteJSON(rw, 200, a.deps.XAI.Status())
	}))
	mux.HandleFunc("/api/xai/oauth/logout", cors(func(rw http.ResponseWriter, r *http.Request) {
		a.deps.XAI.Logout()
		a.bus.Emit("refresh", "", "system", i18n.T("Signed out of SuperGrok"), nil)
		httpx.WriteJSON(rw, 200, a.deps.XAI.Status())
	}))

	mux.HandleFunc("/api/chatgpt/oauth/start", cors(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			httpx.WriteJSON(rw, 405, map[string]any{"error": "method not allowed"})
			return
		}
		st, err := a.deps.ChatGPT.Start()
		if err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": err.Error()})
			return
		}
		httpx.WriteJSON(rw, 200, st)
	}))
	mux.HandleFunc("/api/chatgpt/oauth/status", cors(func(rw http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(rw, 200, a.deps.ChatGPT.Status())
	}))
	mux.HandleFunc("/api/chatgpt/oauth/cancel", cors(func(rw http.ResponseWriter, r *http.Request) {
		a.deps.ChatGPT.Cancel()
		httpx.WriteJSON(rw, 200, a.deps.ChatGPT.Status())
	}))
	mux.HandleFunc("/api/chatgpt/oauth/logout", cors(func(rw http.ResponseWriter, r *http.Request) {
		a.deps.ChatGPT.Logout()
		a.bus.Emit("refresh", "", "system", i18n.T("Signed out of ChatGPT"), nil)
		httpx.WriteJSON(rw, 200, a.deps.ChatGPT.Status())
	}))

	// 服务商目录：客户端照这张表渲染"用哪家模型"的下拉框
	// model.Provider catalog: the client renders the "which vendor" dropdown straight from this table
	// 权限档位选项，客户端照这张表渲染选择器
	// Permission tiers; the client renders its picker from this table
	// 思考强度选项，和权限档位一样由引擎给标签
	// Reasoning-effort options; like the permission tiers, the engine supplies the labels
	mux.HandleFunc("/api/efforts", cors(func(rw http.ResponseWriter, r *http.Request) {
		// 档位随服务商变，所以要带 ?provider= 来问 / the tiers vary by provider, hence ?provider=
		httpx.WriteJSON(rw, 200, map[string]any{"levels": config.EffortOptionsFor(r.URL.Query().Get("provider"))})
	}))

	mux.HandleFunc("/api/permissions", cors(func(rw http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(rw, 200, map[string]any{"levels": config.PermOptions(), "default": string(config.DefaultPerm)})
	}))

	mux.HandleFunc("/api/providers", cors(func(rw http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(rw, 200, map[string]any{"providers": model.ProviderCatalog()})
	}))

	// 现拉模型列表。凭据不在客户端手里，所以必须由引擎代问。
	// Live model listing. The client holds no credentials, so the engine has to ask on its behalf.
	mux.HandleFunc("/api/models", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			ProviderID string `json:"provider_id"`
			BaseURL    string `json:"base_url"`
			KeyEnv     string `json:"key_env"`
			Auth       string `json:"auth"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		models, err := model.ListModels(r.Context(), a.credentials(), body.ProviderID,
			strings.TrimSpace(body.BaseURL), strings.TrimSpace(body.KeyEnv), body.Auth)
		if err != nil {
			httpx.WriteJSON(rw, 200, map[string]any{"models": []string{}, "error": err.Error()})
			return
		}
		httpx.WriteJSON(rw, 200, map[string]any{"models": models})
	}))

	mux.HandleFunc("/api/keys", cors(func(rw http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			httpx.WriteJSON(rw, 200, map[string]any{"keys": a.deps.KS.List()})
		case http.MethodPost:
			var body struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			}
			if err := httpx.ReadJSON(r, &body); err != nil {
				httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
				return
			}
			if err := a.deps.KS.Set(strings.TrimSpace(body.Name), strings.TrimSpace(body.Value)); err != nil {
				httpx.WriteJSON(rw, 400, map[string]any{"error": err.Error()})
				return
			}
			a.bus.Emit("refresh", "", "system", "API key "+body.Name+i18n.T(" updated"), nil)
			httpx.WriteJSON(rw, 200, map[string]any{"ok": true})
		default:
			httpx.WriteJSON(rw, 405, map[string]any{"error": "method not allowed"})
		}
	}))

	mux.HandleFunc("/api/keys/delete", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil || body.Name == "" {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		if !a.deps.KS.Delete(body.Name) {
			httpx.WriteJSON(rw, 404, map[string]any{"error": i18n.T("No stored key named ") + body.Name})
			return
		}
		a.bus.Emit("refresh", "", "system", "API key "+body.Name+i18n.T(" deleted"), nil)
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true})
	}))

	mux.HandleFunc("/api/group/set", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Group string `json:"group"`
			Name  string `json:"name"`
			In    bool   `json:"in"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil || body.Name == "" {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		chat := body.Group
		if chat == "" {
			chat = "group"
		}
		if !a.bus.SetGroupMemberIn(chat, body.Name, body.In) {
			httpx.WriteJSON(rw, 404, map[string]any{"error": i18n.T("No bot named ") + body.Name + i18n.T(" exists")})
			return
		}
		verb := i18n.T("was removed from the group chat")
		if body.In {
			verb = i18n.T("was added to the group chat")
		}
		a.bus.Emit("system", chat, "system", body.Name+" "+verb, nil)
		a.bus.Emit("refresh", "", "system", "group_members", nil)
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true})
	}))

	mux.HandleFunc("/api/groups", cors(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			httpx.WriteJSON(rw, 405, map[string]any{"error": "method not allowed"})
			return
		}
		var body struct {
			Title   string   `json:"title"`
			Avatar  string   `json:"avatar"`
			Members []string `json:"members"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		if body.Avatar != "" && !config.ValidAvatar(body.Avatar) {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("The avatar must be a #rrggbb color or a small png/jpeg/webp image")})
			return
		}
		g, err := a.bus.CreateGroup(body.Title, body.Avatar, body.Members)
		if err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": err.Error()})
			return
		}
		a.bus.Emit("refresh", "", "system", i18n.T("New group chat created"), nil)
		httpx.WriteJSON(rw, 200, g)
	}))
	mux.HandleFunc("/api/groups/update", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			ID      string    `json:"id"`
			Title   *string   `json:"title"`
			Avatar  *string   `json:"avatar"`
			Members *[]string `json:"members"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil || body.ID == "" {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		if body.Avatar != nil && *body.Avatar != "" && !config.ValidAvatar(*body.Avatar) {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("The avatar must be a #rrggbb color or a small png/jpeg/webp image")})
			return
		}
		if err := a.bus.UpdateGroup(body.ID, body.Title, body.Avatar, body.Members); err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": err.Error()})
			return
		}
		if body.ID == "group" && (body.Title != nil || body.Avatar != nil) {
			a.settings.SetGroupMeta(body.Title, body.Avatar)
		}
		a.bus.Emit("refresh", "", "system", "groups", nil)
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true})
	}))
	mux.HandleFunc("/api/groups/delete", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			ID string `json:"id"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil || body.ID == "" {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		if err := a.bus.DeleteGroup(body.ID); err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": err.Error()})
			return
		}
		a.bus.Emit("refresh", "", "system", i18n.T("Group chat deleted"), nil)
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true})
	}))

	// 语言设置：auto（跟随系统）/ zh / en，影响后端消息与 bot 提示词语言
	// Language setting: auto (follow the system) / zh / en; affects backend messages and the bots' prompt language
	mux.HandleFunc("/api/settings", cors(func(rw http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			httpx.WriteJSON(rw, 200, a.settings.Status())
		case http.MethodPost:
			// 指针字段：没带的项保持原样，避免一次改语言把群名清空
			// Pointer fields: omitted entries stay as they are, so changing the language cannot wipe the group name
			var body struct {
				Locale      *string `json:"locale"`
				GroupTitle  *string `json:"group_title"`
				GroupAvatar *string `json:"group_avatar"`
				Permission  *string `json:"permission"`
			}
			if err := httpx.ReadJSON(r, &body); err != nil {
				httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
				return
			}
			if body.Locale != nil && !a.settings.SetLocalePref(strings.TrimSpace(*body.Locale)) {
				httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("locale must be auto / zh / en")})
				return
			}
			if body.GroupAvatar != nil && !config.ValidAvatar(*body.GroupAvatar) {
				httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("The avatar must be a #rrggbb color or a small png/jpeg/webp image")})
				return
			}
			if body.GroupTitle != nil || body.GroupAvatar != nil {
				a.settings.SetGroupMeta(body.GroupTitle, body.GroupAvatar)
			}
			if body.Permission != nil && !a.settings.SetPermission(strings.TrimSpace(*body.Permission)) {
				httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("The permission tier must be ask / edit / auto / full")})
				return
			}
			a.bus.Emit("refresh", "", "system", "settings", nil)
			httpx.WriteJSON(rw, 200, a.settings.Status())
		default:
			httpx.WriteJSON(rw, 405, map[string]any{"error": "method not allowed"})
		}
	}))

	mux.HandleFunc("/api/telegram", cors(func(rw http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			httpx.WriteJSON(rw, 200, a.tg.Status())
		case http.MethodPost:
			var body struct {
				Enabled bool `json:"enabled"`
			}
			if err := httpx.ReadJSON(r, &body); err != nil {
				httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
				return
			}
			if err := a.tg.SetEnabled(body.Enabled); err != nil {
				httpx.WriteJSON(rw, 400, map[string]any{"error": err.Error()})
				return
			}
			a.bus.Emit("refresh", "", "system", "telegram", nil)
			httpx.WriteJSON(rw, 200, a.tg.Status())
		default:
			httpx.WriteJSON(rw, 405, map[string]any{"error": "method not allowed"})
		}
	}))

	mux.HandleFunc("/api/mcp", cors(func(rw http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(rw, 200, map[string]any{"mcp": a.deps.MCP.Status()})
	}))

	mux.HandleFunc("/api/mcp/add", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Name      string            `json:"name"`
			Command   string            `json:"command"`
			Args      string            `json:"args"` // 空格分隔（简化 UI 输入） / space-separated (simpler UI input)
			URL       string            `json:"url"`
			BearerKey string            `json:"bearer_key"`
			Env       map[string]string `json:"env"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		cfg := plugin.MCPServerConfig{
			Name: strings.TrimSpace(body.Name), Command: strings.TrimSpace(body.Command),
			Args: strings.Fields(body.Args), URL: strings.TrimSpace(body.URL),
			BearerKey: strings.TrimSpace(body.BearerKey), Env: body.Env,
		}
		if err := a.deps.MCP.Add(cfg); err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": err.Error()})
			return
		}
		a.bus.Emit("refresh", "", "system", i18n.T("Plugin ")+cfg.Name+i18n.T(" connected"), nil)
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true})
	}))

	mux.HandleFunc("/api/mcp/delete", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil || body.Name == "" {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		if !a.deps.MCP.Remove(body.Name) {
			httpx.WriteJSON(rw, 404, map[string]any{"error": i18n.T("No plugin named ") + body.Name + i18n.T(" exists")})
			return
		}
		// 从各 bot 的订阅里同步移除并持久化
		// remove it from each bot's subscriptions as well, then persist
		a.mu.Lock()
		for i := range a.cfgs {
			kept := a.cfgs[i].MCP[:0]
			for _, s := range a.cfgs[i].MCP {
				if s != body.Name {
					kept = append(kept, s)
				}
			}
			a.cfgs[i].MCP = kept
			if w := a.bus.Bot(a.cfgs[i].Name); w != nil {
				w.Cfg.MCP = kept
				w.Toolbox().SetMCPServers(kept)
			}
		}
		_ = config.SaveBotConfigs(a.cfgPath, a.cfgs)
		a.mu.Unlock()
		a.bus.Emit("refresh", "", "system", i18n.T("Plugin ")+body.Name+i18n.T(" removed"), nil)
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true})
	}))

	// 给一个远程连接器发起 OAuth 授权。返回里的 url 由客户端打开浏览器，之后轮询 status。
	// Begin OAuth authorization for a remote connector. The client opens the returned url in a browser
	// and then polls status.
	mux.HandleFunc("/api/mcp/oauth/start", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil || body.Name == "" {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		// URL 以已存插件的为准，只有新建时才用请求里带的那个——否则调用方可以让引擎去
		// 向任意地址发起授权。
		// The URL of an already-saved plugin wins; the one in the request is only used when adding a new
		// connector, or a caller could aim the engine's authorization flow at any address it liked.
		target := body.URL
		if u := a.deps.MCP.URLOf(body.Name); u != "" {
			target = u
		}
		st, err := a.deps.MCPAuth.Start(body.Name, target)
		if err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": err.Error()})
			return
		}
		httpx.WriteJSON(rw, 200, st)
	}))

	mux.HandleFunc("/api/mcp/oauth/status", cors(func(rw http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		st := a.deps.MCPAuth.Status(name)
		// 授权刚落地时自动重连一次：否则用户点完同意还要再手动点"重连"才见到工具
		// Reconnect once as soon as authorization lands: otherwise the user approves in the browser and
		// still has to hit "reconnect" by hand before any tools appear
		if st["status"] == "done" && a.deps.MCP.Has(name) {
			if err := a.deps.MCP.Reconnect(name); err != nil {
				st["error"] = err.Error()
			}
			a.deps.MCPAuth.ClearPending(name)
			a.bus.Emit("refresh", "", "system", i18n.T("Plugin ")+name+i18n.T(" authorized"), nil)
		}
		httpx.WriteJSON(rw, 200, st)
	}))

	mux.HandleFunc("/api/mcp/oauth/logout", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil || body.Name == "" {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		a.deps.MCPAuth.Logout(body.Name)
		a.bus.Emit("refresh", "", "system", i18n.T("Plugin ")+body.Name+i18n.T(" signed out"), nil)
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true})
	}))

	// 选择一个插件里要暴露给模型的工具子集（空数组 = 全部）。
	// Choose which subset of a plugin's tools is exposed to the model (an empty array means all).
	mux.HandleFunc("/api/mcp/tools", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Name  string   `json:"name"`
			Tools []string `json:"tools"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil || body.Name == "" {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		if err := a.deps.MCP.SetTools(body.Name, body.Tools); err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": err.Error()})
			return
		}
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true})
	}))

	mux.HandleFunc("/api/mcp/reconnect", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil || body.Name == "" {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		if err := a.deps.MCP.Reconnect(body.Name); err != nil {
			a.bus.Emit("refresh", "", "system", i18n.T("Plugin reconnect failed"), nil)
			httpx.WriteJSON(rw, 400, map[string]any{"error": err.Error()})
			return
		}
		a.bus.Emit("refresh", "", "system", i18n.T("Plugin ")+body.Name+i18n.T(" reconnected"), nil)
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true})
	}))

	// 装一个插件包（本地目录或 git 地址）。装的过程可能要 clone，几十秒是正常的。
	// Install a plugin bundle from a local directory or a git URL. It may involve a clone, so tens of
	// seconds is normal.
	mux.HandleFunc("/api/plugins/install", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Source string `json:"source"`
			// 非空表示从这个地址的市场清单里装指定的那一条
			// Non-empty selects one entry from the marketplace listing at that address
			Plugin string `json:"plugin"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		var b *plugin.Bundle
		var err error
		if body.Plugin != "" {
			b, err = a.deps.Bundles.InstallFromMarketplace(body.Source, body.Plugin)
		} else {
			b, err = a.deps.Bundles.Install(body.Source)
		}
		// 地址给的是一份市场清单而不是单个插件：不是失败，把清单交给客户端让用户挑一个。
		// The address gave a marketplace listing rather than a single plugin: not a failure — hand the
		// listing to the client so the user can pick one.
		var mk *plugin.MarketplaceError
		if errors.As(err, &mk) {
			httpx.WriteJSON(rw, 200, map[string]any{
				"marketplace": map[string]any{"name": mk.Marketplace, "plugins": mk.Entries},
				"source":      body.Source,
			})
			return
		}
		if err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": err.Error()})
			return
		}
		a.bus.Emit("refresh", "", "system", i18n.T("Plugin ")+b.Name+i18n.T(" installed"), nil)
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true, "plugin": b})
	}))

	// 原地升级：只调和差集，用户挑好的工具子集和跑过的授权都留着（见 BundleManager.Update）。
	// Upgrade in place: only the difference is reconciled, so the user's tool subset and completed
	// authorization survive (see BundleManager.Update).
	mux.HandleFunc("/api/plugins/update", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil || body.Name == "" {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		b, err := a.deps.Bundles.Update(body.Name)
		if err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": err.Error()})
			return
		}
		a.bus.Emit("refresh", "", "system", i18n.T("Plugin ")+b.Name+i18n.T(" updated"), nil)
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true, "plugin": b})
	}))

	mux.HandleFunc("/api/plugins/remove", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil || body.Name == "" {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		if err := a.deps.Bundles.Remove(body.Name); err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": err.Error()})
			return
		}
		a.bus.Emit("refresh", "", "system", i18n.T("Plugin ")+body.Name+i18n.T(" removed"), nil)
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true})
	}))

	// 技能是文件，用户会直接在编辑器里改；给一个"重扫"入口比要求重启引擎友好得多。
	// Skills are files a user edits in an editor; a rescan entry point is far kinder than asking them to
	// restart the engine.
	mux.HandleFunc("/api/skills/rescan", cors(func(rw http.ResponseWriter, r *http.Request) {
		a.deps.Bundles.Rescan()
		a.deps.SyncSkillRoots()
		a.bus.Emit("refresh", "", "system", "skills", nil)
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true, "skills": a.deps.Skills.List()})
	}))

	mux.HandleFunc("/api/tasks/clear_done", cors(func(rw http.ResponseWriter, r *http.Request) {
		n := a.deps.Board.ClearDone()
		a.bus.Emit("refresh", "", "system", fmt.Sprintf(i18n.T("Cleared %d completed task(s)"), n), nil)
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true, "cleared": n})
	}))

	mux.HandleFunc("/api/routines/delete", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil || body.Name == "" {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		if !a.sched.Remove(body.Name) {
			httpx.WriteJSON(rw, 404, map[string]any{"error": i18n.T("No routine named ") + body.Name + i18n.T(" exists")})
			return
		}
		a.bus.Emit("refresh", "", "system", i18n.T("Routine ")+body.Name+i18n.T(" deleted"), nil)
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true})
	}))

	return mux
}

// handleSSE 推送事件流；?after=<id> 从指定事件之后开始。
// handleSSE pushes the event stream; ?after=<id> starts right after the given event.
func (a *App) handleSSE(rw http.ResponseWriter, r *http.Request) {
	flusher, ok := rw.(http.Flusher)
	if !ok {
		httpx.WriteJSON(rw, 500, map[string]any{"error": "streaming unsupported"})
		return
	}
	after, _ := strconv.Atoi(r.URL.Query().Get("after"))
	rw.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	rw.Header().Set("Cache-Control", "no-cache")
	rw.WriteHeader(200)
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		default:
		}
		events := a.bus.EventsSinceCtx(r.Context().Done(), after, 20*time.Second)
		if len(events) == 0 {
			if _, err := fmt.Fprint(rw, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
			continue
		}
		for _, ev := range events {
			after = ev["id"].(int)
			data, _ := json.Marshal(ev)
			if _, err := fmt.Fprintf(rw, "data: %s\n\n", data); err != nil {
				return
			}
		}
		flusher.Flush()
	}
}
