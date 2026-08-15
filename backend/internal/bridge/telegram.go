package bridge

// Telegram 桥：把群聊/私聊/审批接到 Telegram（官方 Bot API，长轮询，无需公网 IP）。
// - 首个发 /start 的账号独占绑定（data/telegram.json）
// - 私聊 bot 的普通消息 = Bot Bureau 群聊（@bot名 点名照常）；/dm <bot> <内容> = Bot Bureau 私聊
// - 审批推送带内联按钮，点一下即批准/拒绝
// Telegram bridge: hooks group chat / DMs / approvals up to Telegram (official Bot API, long polling, no public IP required).
// - The first account to send /start owns the exclusive binding (data/telegram.json)
// - Plain messages in the bot's DM = Bot Bureau group chat (@botname mentions work as usual); /dm <bot> <content> = Bot Bureau DM
// - engine.Approval pushes carry inline buttons; a single tap approves or rejects

import (
	"botbureau/backend/internal/engine"
	"botbureau/backend/internal/secret"

	"botbureau/backend/internal/i18n"

	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type tgConfig struct {
	Enabled     bool   `json:"enabled"`
	OwnerChatID int64  `json:"owner_chat_id"`
	OwnerName   string `json:"owner_name"`
	// "group"（默认）或某个 bot 名：普通消息去向 + 默认转发哪个会话
	// "group" (default) or a bot name: where plain messages go + which conversation is forwarded by default
	BindTarget string `json:"bind_target"`
}

type TGBridge struct {
	bus     *engine.Bus
	ks      *secret.KeyStore
	cfgPath string
	apiBase string // 测试时可指向假服务器 / tests can point this at a fake server

	mu      sync.Mutex
	cfg     tgConfig
	running bool
	botName string
	errMsg  string
	stopCh  chan struct{}
	httpc   *http.Client
	// 通过 /dm 点名过的私聊会话，其回复也转发
	// DM conversations addressed via /dm; their replies are forwarded too
	dmTouched map[string]bool
}

func NewTGBridge(bus *engine.Bus, ks *secret.KeyStore, cfgPath string) *TGBridge {
	b := &TGBridge{
		bus: bus, ks: ks, cfgPath: cfgPath,
		apiBase:   "https://api.telegram.org",
		httpc:     &http.Client{Timeout: 70 * time.Second},
		dmTouched: map[string]bool{},
	}
	if raw, err := os.ReadFile(cfgPath); err == nil {
		_ = json.Unmarshal(raw, &b.cfg)
	}
	if b.cfg.BindTarget == "" {
		b.cfg.BindTarget = "group"
	}
	return b
}

// bindTarget 返回当前绑定；绑定的 bot 已被删除时回退群聊。
// bindTarget returns the current binding; falls back to the group chat if the bound bot has been deleted.
// SetAPIBase 改 Telegram API 的地址：走自建反代或指向测试桩时用。
// SetAPIBase points the bridge at a different Telegram API base: a self-hosted proxy, or a test stub.
func (b *TGBridge) SetAPIBase(u string) { b.apiBase = u }

func (b *TGBridge) bindTarget() string {
	b.mu.Lock()
	t := b.cfg.BindTarget
	b.mu.Unlock()
	if t != "group" && b.bus.Bot(t) == nil {
		return "group"
	}
	return t
}

func (b *TGBridge) saveLocked() {
	out, _ := json.MarshalIndent(b.cfg, "", "  ")
	_ = os.WriteFile(b.cfgPath, out, 0o600)
}

func (b *TGBridge) token() string { return b.ks.Get("TELEGRAM_BOT_TOKEN") }

// ---- Bot API 调用 ----
// ---- Bot API calls ----

func (b *TGBridge) api(method string, payload any, timeout time.Duration) (json.RawMessage, error) {
	raw, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", b.apiBase+"/bot"+b.token()+"/"+method, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := b.httpc
	if timeout > 0 {
		client = &http.Client{Timeout: timeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var parsed struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf(i18n.T("Invalid Telegram response (HTTP %d)"), resp.StatusCode)
	}
	if !parsed.OK {
		return nil, fmt.Errorf("Telegram: %s", parsed.Description)
	}
	return parsed.Result, nil
}

func (b *TGBridge) send(chatID int64, text string, markup any) {
	if len(text) > 4000 {
		text = text[:4000] + "…"
	}
	payload := map[string]any{"chat_id": chatID, "text": text}
	if markup != nil {
		payload["reply_markup"] = markup
	}
	if _, err := b.api("sendMessage", payload, 15*time.Second); err != nil {
		fmt.Fprintln(os.Stderr, i18n.T("Telegram send failed:"), err)
	}
}

// ---- 生命周期 ----
// ---- lifecycle ----

func (b *TGBridge) SetEnabled(enabled bool) error {
	if enabled && strings.TrimSpace(b.token()) == "" {
		return errors.New(i18n.T("First save the bot token as TELEGRAM_BOT_TOKEN in Settings (create a bot with @BotFather to get one)"))
	}
	b.mu.Lock()
	b.cfg.Enabled = enabled
	b.saveLocked()
	b.mu.Unlock()
	if enabled {
		return b.Start()
	}
	b.Stop()
	return nil
}

func (b *TGBridge) Start() error {
	b.mu.Lock()
	if b.running {
		b.mu.Unlock()
		return nil
	}
	b.errMsg = ""
	b.mu.Unlock()

	raw, err := b.api("getMe", map[string]any{}, 15*time.Second)
	if err != nil {
		b.mu.Lock()
		b.errMsg = err.Error()
		b.mu.Unlock()
		return err
	}
	var me struct {
		Username string `json:"username"`
	}
	_ = json.Unmarshal(raw, &me)

	b.mu.Lock()
	b.botName = me.Username
	b.running = true
	b.stopCh = make(chan struct{})
	stop := b.stopCh
	b.mu.Unlock()

	go b.pollLoop(stop)
	go b.forwardLoop(stop)
	b.bus.Emit("system", "", "system", fmt.Sprintf(i18n.T("Telegram bridge connected: @%s"), me.Username), nil)
	return nil
}

func (b *TGBridge) Stop() {
	b.mu.Lock()
	if b.running {
		close(b.stopCh)
		b.running = false
	}
	b.mu.Unlock()
}

func (b *TGBridge) Status() map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	owner := ""
	if b.cfg.OwnerChatID != 0 {
		owner = b.cfg.OwnerName
	}
	bind := b.cfg.BindTarget
	if bind == "" {
		bind = "group"
	}
	return map[string]any{
		"enabled": b.cfg.Enabled, "running": b.running,
		"bot": b.botName, "owner": owner, "error": b.errMsg,
		"bind":      bind,
		"has_token": strings.TrimSpace(b.token()) != "",
	}
}

// ---- 收：Telegram → Bot Bureau ----
// ---- inbound: Telegram → Bot Bureau ----

type TGUpdate struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		Text string `json:"text"`
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		From struct {
			ID        int64  `json:"id"`
			FirstName string `json:"first_name"`
			Username  string `json:"username"`
		} `json:"from"`
	} `json:"message"`
	Callback *struct {
		ID      string `json:"id"`
		Data    string `json:"data"`
		Message struct {
			Chat struct {
				ID int64 `json:"id"`
			} `json:"chat"`
		} `json:"message"`
	} `json:"callback_query"`
}

func (b *TGBridge) pollLoop(stop chan struct{}) {
	var offset int64
	for {
		select {
		case <-stop:
			return
		default:
		}
		raw, err := b.api("getUpdates", map[string]any{"timeout": 50, "offset": offset}, 65*time.Second)
		if err != nil {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}
		var updates []TGUpdate
		if json.Unmarshal(raw, &updates) != nil {
			continue
		}
		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			b.handleUpdate(u)
		}
	}
}

func (b *TGBridge) handleUpdate(u TGUpdate) {
	if u.Callback != nil {
		b.handleCallback(u)
		return
	}
	if u.Message == nil || strings.TrimSpace(u.Message.Text) == "" {
		return
	}
	chatID := u.Message.Chat.ID
	text := strings.TrimSpace(u.Message.Text)

	b.mu.Lock()
	owner := b.cfg.OwnerChatID
	b.mu.Unlock()

	// 绑定：首个 /start 的账号独占
	// binding: the first account to send /start gets exclusive ownership
	if owner == 0 {
		if strings.HasPrefix(text, "/start") {
			name := u.Message.From.Username
			if name == "" {
				name = u.Message.From.FirstName
			}
			b.mu.Lock()
			b.cfg.OwnerChatID = chatID
			b.cfg.OwnerName = name
			b.saveLocked()
			b.mu.Unlock()
			b.send(chatID, i18n.T("Bound to your account.\n"+
				"Plain messages = team group chat (mention with @botname)\n"+
				"/bind <bot name> = switch this conversation to a bot's DM; /bind group switches back to the group chat\n"+
				"/dm <bot> <content> = send a one-off message to a bot\n"+
				"/bots = list the team"), nil)
		}
		return
	}
	if chatID != owner {
		b.send(chatID, i18n.T("This bot is already bound to another user."), nil)
		return
	}

	switch {
	case strings.HasPrefix(text, "/start"):
		b.send(chatID, i18n.T("Already bound — just send a message."), nil)
	case strings.HasPrefix(text, "/bots"):
		var lines []string
		for _, w := range b.bus.Bots() {
			tag := ""
			if b.bus.IsGroupMember(w.Name()) {
				tag = i18n.T(" (in group)")
			}
			lines = append(lines, fmt.Sprintf("%s · %s%s", w.Name(), w.Cfg.Role, tag))
		}
		b.send(chatID, strings.Join(lines, "\n"), nil)
	case strings.HasPrefix(text, "/bind"):
		arg := strings.TrimSpace(strings.TrimPrefix(text, "/bind"))
		if arg == "" {
			cur := b.bindTarget()
			desc := i18n.T("the team group chat")
			if cur != "group" {
				desc = fmt.Sprintf(i18n.T("the DM with %s"), cur)
			}
			b.send(chatID, fmt.Sprintf(i18n.T("Current binding: %s\nUse /bind group or /bind <bot name> to switch (/bots lists the team)"), desc), nil)
			return
		}
		if arg != "group" && b.bus.Bot(arg) == nil {
			b.send(chatID, fmt.Sprintf(i18n.T("No bot named %s (/bots lists the team)"), arg), nil)
			return
		}
		b.mu.Lock()
		b.cfg.BindTarget = arg
		b.saveLocked()
		b.mu.Unlock()
		if arg == "group" {
			b.send(chatID, i18n.T("Connected to the team group chat: everyone sees your messages, and @botname mentions work"), nil)
		} else {
			b.send(chatID, fmt.Sprintf(i18n.T("Connected to the DM with %s: further messages go only to them (/bind group to switch back)"), arg), nil)
		}
	case strings.HasPrefix(text, "/dm "):
		rest := strings.TrimSpace(strings.TrimPrefix(text, "/dm "))
		name, content, _ := strings.Cut(rest, " ")
		content = strings.TrimSpace(content)
		if b.bus.Bot(name) == nil || content == "" {
			b.send(chatID, i18n.T("Usage: /dm <bot name> <content> (/bots lists the team)"), nil)
			return
		}
		b.mu.Lock()
		// 之后该私聊的回复也转发过来
		// from now on, replies in this DM are forwarded over as well
		b.dmTouched["dm:"+name] = true
		b.mu.Unlock()
		b.bus.Emit("msg", "dm:"+name, "user", content, nil)
		b.bus.Deliver("user", name, "dm", content, true)
	default:
		target := b.bindTarget()
		if target == "group" {
			targets := b.bus.MentionedBots(text)
			if len(targets) == 0 {
				def := b.bus.DefaultGroupMember()
				if def == "" {
					b.send(chatID, i18n.T("The group chat has no members."), nil)
					return
				}
				targets = []string{def}
			}
			b.bus.PostGroup("user", text, targets)
			return
		}
		// 绑定到某个 bot：普通消息直达其私聊
		// bound to a specific bot: plain messages go straight to its DM
		b.bus.Emit("msg", "dm:"+target, "user", text, nil)
		b.bus.Deliver("user", target, "dm", text, true)
	}
}

func (b *TGBridge) handleCallback(u TGUpdate) {
	answer := func(text string) {
		_, _ = b.api("answerCallbackQuery", map[string]any{
			"callback_query_id": u.Callback.ID, "text": text,
		}, 15*time.Second)
	}
	b.mu.Lock()
	owner := b.cfg.OwnerChatID
	b.mu.Unlock()
	if u.Callback.Message.Chat.ID != owner {
		answer(i18n.T("Unauthorized"))
		return
	}
	var id int
	var ok int
	if _, err := fmt.Sscanf(u.Callback.Data, "ap:%d:%d", &id, &ok); err != nil {
		answer(i18n.T("Invalid action"))
		return
	}
	if b.bus.Decide(id, ok == 1, "via Telegram") {
		if ok == 1 {
			answer(i18n.T("Approved"))
		} else {
			answer(i18n.T("Rejected"))
		}
	} else {
		answer(i18n.T("Already handled"))
	}
}

// ---- 发：Bot Bureau → Telegram ----
// ---- outbound: Bot Bureau → Telegram ----

func (b *TGBridge) forwardLoop(stop chan struct{}) {
	after := b.bus.LatestID() // 只转发接入之后的新事件 / forward only events newer than the connect time
	for {
		select {
		case <-stop:
			return
		default:
		}
		events := b.bus.EventsSinceCtx(stop, after, 25*time.Second)
		for _, ev := range events {
			after = ev["id"].(int)
			b.forwardEvent(ev)
		}
	}
}

func chatTag(chat string) string {
	if chat == "group" {
		return i18n.T("[Group] ")
	}
	if strings.HasPrefix(chat, "dm:") {
		return fmt.Sprintf(i18n.T("[DM·%s] "), strings.TrimPrefix(chat, "dm:"))
	}
	return ""
}

func (b *TGBridge) forwardEvent(ev engine.Event) {
	b.mu.Lock()
	owner := b.cfg.OwnerChatID
	b.mu.Unlock()
	if owner == 0 {
		return
	}
	kind, _ := ev["kind"].(string)
	source, _ := ev["source"].(string)
	chat, _ := ev["chat"].(string)
	text, _ := ev["text"].(string)
	switch kind {
	case "msg":
		if source == "user" {
			return // 不回声用户自己的话 / don't echo the user's own messages back
		}
		// 只转发绑定会话的消息（/dm 点名过的私聊也转发），避免无关会话刷屏
		// only forward messages from the bound conversation (plus DMs addressed via /dm), to avoid flooding from unrelated chats
		target := b.bindTarget()
		bound := "group"
		if target != "group" {
			bound = "dm:" + target
		}
		b.mu.Lock()
		touched := b.dmTouched[chat]
		b.mu.Unlock()
		if chat != bound && !touched {
			return
		}
		b.send(owner, fmt.Sprintf(i18n.T("%s%s: %s"), chatTag(chat), source, text), nil)
	case "system":
		b.send(owner, chatTag(chat)+text, nil)
	case "approval":
		id := 0
		if f, ok := ev["approval_id"].(int); ok {
			id = f
		} else if f, ok := ev["approval_id"].(float64); ok {
			id = int(f)
		}
		markup := map[string]any{
			"inline_keyboard": [][]map[string]any{{
				{"text": i18n.T("Approve"), "callback_data": fmt.Sprintf("ap:%d:1", id)},
				{"text": i18n.T("Reject"), "callback_data": fmt.Sprintf("ap:%d:0", id)},
			}},
		}
		b.send(owner, fmt.Sprintf(i18n.T("%s requests approval #%d:\n%s"), source, id, text), markup)
	}
}
