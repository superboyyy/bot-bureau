package engine

import (
	"botbureau/backend/internal/config"
	"botbureau/backend/internal/i18n"

	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// Event 是展示层（Electron / CLI）的唯一数据源。
// 形状: {id, ts, kind, chat, source, text, ...extra}
// kind: msg / tool / approval / approval_done / status / system / refresh
// chat: "group"、"dm:<bot名>" 或 ""（全局）
// Event is the sole data source for the presentation layer (Electron / CLI).
// Shape: {id, ts, kind, chat, source, text, ...extra}
// kind: msg / tool / approval / approval_done / status / system / refresh
// chat: "group", "dm:<bot name>", or "" (global)
type Event map[string]any

// Msg 是投递给某个 bot 收件箱的一条消息。
// Msg is a single message delivered to a bot's inbox.
type Msg struct {
	// "user"、bot 名、"routine:<例程名>" 或 stopSentinel
	// "user", a bot name, "routine:<routine name>", or stopSentinel
	Sender  string
	Content string
	Chat    string // "group" 或 "dm" / "group" or "dm"
	// false = 只作为群聊背景写入上下文，不触发回应
	// false = written into context as group chat background only; does not trigger a response
	Respond bool
}

const stopSentinel = "__stop__"

// Approval 是一次待用户审批的敏感操作（如非只读的 bash 命令）。
// Approval is a sensitive action awaiting user approval (e.g. a non-read-only bash command).
type Approval struct {
	ID     int    `json:"id"`
	Bot    string `json:"bot"`
	Action string `json:"action"`
	Chat   string `json:"chat"`

	decided  chan struct{}
	approved bool
	reason   string
}

// Wait 阻塞直到用户做出决定。
// Wait blocks until the user makes a decision.
func (a *Approval) Wait() (approved bool, reason string) {
	<-a.decided
	return a.approved, a.reason
}

// WaitCtx 同上；ctx 取消时返回 canceled=true（调用方应把该审批记为拒绝）。
// WaitCtx is the same, but returns canceled=true if ctx is cancelled (the caller should reject the approval).
func (a *Approval) WaitCtx(ctx context.Context) (approved bool, reason string, canceled bool) {
	if ctx == nil {
		a, r := a.Wait()
		return a, r, false
	}
	select {
	case <-a.decided:
		return a.approved, a.reason, false
	case <-ctx.Done():
		return false, i18n.T("The task was cancelled"), true
	}
}

type Bus struct {
	mu           sync.Mutex
	events       []Event
	nextID       int
	notify       chan struct{}
	bots         map[string]*BotWorker
	order        []string
	approvals    map[int]*Approval
	nextApproval int

	groups     []*Group
	groupsPath string
	groupsFile bool

	// 额度告警节流（按 provider 标签）
	// quota-alert throttling (keyed by provider label)
	quotaLast map[string]time.Time

	eventsPath string
}

func NewBus() *Bus {
	return &Bus{
		nextID:       1,
		notify:       make(chan struct{}),
		bots:         map[string]*BotWorker{},
		approvals:    map[int]*Approval{},
		nextApproval: 1,
	}
}

// ---- bot 注册 ----
// ---- bot registration ----

func (b *Bus) Register(w *BotWorker) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bots[w.Name()] = w
	b.order = append(b.order, w.Name())
	// 注册不等于入群。谁在群里由 LoadGroups（升级兜底）和用户的显式操作决定——
	// 早先这里会把每个新注册的 bot 塞进默认群，于是"新建一个 bot"就顺手把它拉进了群聊。
	//
	// Registering is not joining. Membership comes from LoadGroups (the upgrade fallback) and from
	// what the user explicitly does — this used to push every newly registered bot into the default
	// group, so "create a bot" quietly added it to the group chat as well.
}

// Replace 用新 worker 顶替同名的旧 worker，位次和群成员身份都不变。
// 走 Unregister+Register 会把 bot 挪到列表末尾，而群聊的默认收件人就是列表第一个——
// 改个头像不该顺手换掉默认收件人。
// Replace swaps in a new worker for an existing name, keeping its position and group membership.
// Unregister+Register would move the bot to the end of the list, and the group's default recipient is
// whoever is first — editing an avatar must not silently reassign that.
func (b *Bus) Replace(name string, w *BotWorker) *BotWorker {
	b.mu.Lock()
	old := b.bots[name]
	if old != nil {
		b.bots[name] = w
	}
	b.mu.Unlock()
	return old
}

func (b *Bus) Unregister(name string) *BotWorker {
	b.mu.Lock()
	w := b.bots[name]
	delete(b.bots, name)
	b.removeFromAllGroupsLocked(name)
	for i, n := range b.order {
		if n == name {
			b.order = append(b.order[:i], b.order[i+1:]...)
			break
		}
	}
	b.saveGroupsLocked()
	b.mu.Unlock()
	return w
}

func (b *Bus) LoadGroupMembers(path string) {
	b.LoadGroups(path, path, "", "")
}

func (b *Bus) SetGroupMember(name string, in bool) bool {
	return b.SetGroupMemberIn("group", name, in)
}

func (b *Bus) IsGroupMember(name string) bool {
	return b.IsGroupMemberOf("group", name)
}

func (b *Bus) GroupMembers() []string {
	return b.GroupMembersOf("group")
}

func (b *Bus) Bot(name string) *BotWorker {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.bots[name]
}

// Bots 按注册顺序返回所有 bot。
// Bots returns all bots in registration order.
func (b *Bus) Bots() []*BotWorker {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*BotWorker, 0, len(b.order))
	for _, n := range b.order {
		if w, ok := b.bots[n]; ok {
			out = append(out, w)
		}
	}
	return out
}

func (b *Bus) BotNames() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.order))
	for _, n := range b.order {
		if _, ok := b.bots[n]; ok {
			out = append(out, n)
		}
	}
	return out
}

func (b *Bus) DefaultBot() string {
	names := b.BotNames()
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

// ---- 事件流 ----
// ---- event stream ----

func (b *Bus) Emit(kind, chat, source, text string, extra map[string]any) Event {
	b.mu.Lock()
	ev := Event{
		"id": b.nextID, "ts": time.Now().Unix(),
		"kind": kind, "chat": chat, "source": source, "text": text,
	}
	for k, v := range extra {
		ev[k] = v
	}
	b.nextID++
	b.events = append(b.events, ev)
	if len(b.events) > 4000 {
		b.events = append([]Event(nil), b.events[2000:]...)
	}
	b.saveEventsLocked()
	close(b.notify)
	b.notify = make(chan struct{})
	b.mu.Unlock()
	return ev
}

// EnableEventLog 从 path 恢复事件并在之后每次 Emit 时落盘。测试不调用则不持久化。
// EnableEventLog restores events from path and writes them back on every Emit. Tests that skip this stay in-memory.
func (b *Bus) EnableEventLog(path string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.eventsPath = path
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var evs []Event
	if json.Unmarshal(raw, &evs) != nil {
		return
	}
	maxID := 0
	kept := make([]Event, 0, len(evs))
	for _, e := range evs {
		kind, _ := e["kind"].(string)
		if kind == "status" || kind == "approval" {
			continue
		}
		switch v := e["id"].(type) {
		case float64:
			id := int(v)
			e["id"] = id
			if id > maxID {
				maxID = id
			}
		case int:
			if v > maxID {
				maxID = v
			}
		}
		kept = append(kept, e)
	}
	b.events = kept
	if maxID >= b.nextID {
		b.nextID = maxID + 1
	}
}

func (b *Bus) saveEventsLocked() {
	if b.eventsPath == "" {
		return
	}
	raw, err := json.Marshal(b.events)
	if err != nil {
		return
	}
	tmp := b.eventsPath + ".tmp"
	if os.WriteFile(tmp, raw, 0o644) != nil {
		return
	}
	_ = os.Rename(tmp, b.eventsPath)
}

// EventsSince 阻塞等待 id > after 的新事件；超时返回 nil（供心跳）。
// EventsSince blocks waiting for new events with id > after; returns nil on timeout (for heartbeats).
func (b *Bus) EventsSince(after int, timeout time.Duration) []Event {
	return b.EventsSinceCtx(nil, after, timeout)
}

// EventsSinceCtx 同上，但 done 关闭时立即返回（SSE 客户端断开即释放）。
// EventsSinceCtx is the same as above, but returns immediately when done is closed (released as soon as the SSE client disconnects).
func (b *Bus) EventsSinceCtx(done <-chan struct{}, after int, timeout time.Duration) []Event {
	deadline := time.Now().Add(timeout)
	for {
		b.mu.Lock()
		if len(b.events) > 0 && b.events[len(b.events)-1]["id"].(int) > after {
			var out []Event
			for _, e := range b.events {
				if e["id"].(int) > after {
					out = append(out, e)
				}
			}
			b.mu.Unlock()
			return out
		}
		ch := b.notify
		b.mu.Unlock()
		remain := time.Until(deadline)
		if remain <= 0 {
			return nil
		}
		select {
		case <-ch:
		case <-time.After(remain):
			return nil
		case <-done:
			return nil
		}
	}
}

// QuotaAlert 发一条全局额度告警（chat 为空 → 所有面板可见、必转发到 IM 桥）。
// 同一 provider 十分钟内只告警一次，防止例程/重试刷屏。
// QuotaAlert emits a global quota alert (empty chat → visible on every panel, always forwarded to the IM bridge).
// Each provider alerts at most once per ten minutes, so routines/retries cannot flood the stream.
func (b *Bus) QuotaAlert(label, msg string) bool {
	b.mu.Lock()
	if b.quotaLast == nil {
		b.quotaLast = map[string]time.Time{}
	}
	if time.Since(b.quotaLast[label]) < 10*time.Minute {
		b.mu.Unlock()
		return false
	}
	b.quotaLast[label] = time.Now()
	b.mu.Unlock()
	b.Emit("system", "", "system", msg, map[string]any{"quota": true})
	return true
}

func (b *Bus) LatestID() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.events) == 0 {
		return 0
	}
	return b.events[len(b.events)-1]["id"].(int)
}

func (b *Bus) Recent(limit int) []Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.events) <= limit {
		return append([]Event(nil), b.events...)
	}
	return append([]Event(nil), b.events[len(b.events)-limit:]...)
}

// ---- 消息投递 ----
// ---- message delivery ----

func (b *Bus) Deliver(sender, target, chat, content string, respond bool) bool {
	w := b.Bot(target)
	if w == nil {
		return false
	}
	select {
	case w.inbox <- Msg{Sender: sender, Content: content, Chat: chat, Respond: respond}:
		return true
	// 收件箱满（256 条积压）——丢弃并告警，绝不阻塞投递方
	// inbox full (256-message backlog) — drop the message and alert; never block the sender
	default:
		b.Emit("system", chat, "system",
			fmt.Sprintf(i18n.T("%s's inbox is full, the message was dropped"), target), nil)
		return false
	}
}

// PostGroup 群聊发言：写入事件流；被点名的群成员触发回应，其余群成员只收背景上下文。
// 不在群里的 bot 完全收不到。
// PostGroup posts a group chat message: it is written to the event stream; mentioned group members
// are triggered to respond, while the remaining members only receive it as background context.
// Bots outside the group receive nothing at all.
func (b *Bus) PostGroup(sender, text string, respondTargets []string) {
	b.PostGroupTo("group", sender, text, respondTargets)
}

// MentionedBots 返回文本里 @到的群成员（群外 bot 的 @ 无效）。
// MentionedBots returns the group members @-mentioned in the text (@s to bots outside the group are ignored).
func (b *Bus) MentionedBots(text string) []string {
	return b.MentionedBotsIn("group", text)
}

// DefaultGroupMember 是群聊里不点名时的默认接单人（名单里的第一位）。
// DefaultGroupMember is the default responder when no one is mentioned in the group chat (first on the list).
func (b *Bus) DefaultGroupMember() string {
	ms := b.GroupMembers()
	if len(ms) == 0 {
		return ""
	}
	return ms[0]
}

// containsMention 判断这段话有没有点到某个 bot。@名字 和直呼其名都算——
// 群里说「scout 去查一下」和「@scout 去查一下」是同一个意思，不该逼用户记得敲 @。
// 裸名字要求两侧都不是名字字符，所以 "scouting" 不会命中 scout；@ 形式只看右侧，
// 因为左边就是 @ 本身。
//
// containsMention reports whether this text calls on a bot. Both "@name" and the bare name count:
// saying "scout take a look" in a group means the same as "@scout take a look", and users should not
// have to remember the @. A bare name must not be flanked by name characters, so "scouting" does not
// match scout; the @ form only checks the right side, since the left is the @ itself.
func containsMention(text, name string) bool {
	if name == "" {
		return false
	}
	return mentionAt(text, "@"+name, false) || mentionAt(text, name, true)
}

func isNameChar(c byte) bool {
	return c == '_' || c == '-' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func mentionAt(text, needle string, checkLeft bool) bool {
	lower := strings.ToLower(text)
	needle = strings.ToLower(needle)
	for i := 0; i+len(needle) <= len(lower); i++ {
		if lower[i:i+len(needle)] != needle {
			continue
		}
		if checkLeft && i > 0 && isNameChar(lower[i-1]) {
			continue
		}
		if j := i + len(needle); j < len(lower) && isNameChar(lower[j]) {
			continue
		}
		return true
	}
	return false
}

// ---- 审批 ----
// ---- approvals ----

func (b *Bus) RequestApproval(bot, action, chat string) *Approval {
	b.mu.Lock()
	a := &Approval{ID: b.nextApproval, Bot: bot, Action: action, Chat: chat, decided: make(chan struct{})}
	b.nextApproval++
	b.approvals[a.ID] = a
	b.mu.Unlock()
	b.Emit("approval", chat, bot, action, map[string]any{"approval_id": a.ID})
	go func() {
		t := time.NewTimer(config.ApprovalTimeout())
		defer t.Stop()
		select {
		case <-a.decided:
		case <-t.C:
			b.Decide(a.ID, false, i18n.T("Approval timed out"))
		}
	}()
	return a
}

func (b *Bus) Decide(id int, approved bool, reason string) bool {
	b.mu.Lock()
	a := b.approvals[id]
	if a == nil {
		b.mu.Unlock()
		return false
	}
	select {
	case <-a.decided:
		b.mu.Unlock()
		return false // 已处理过 / already handled
	default:
	}
	a.approved = approved
	a.reason = reason
	close(a.decided)
	delete(b.approvals, id)
	chat, bot, action := a.Chat, a.Bot, a.Action
	b.mu.Unlock()
	b.Emit("approval_done", chat, bot, action,
		map[string]any{"approval_id": a.ID, "approved": approved})
	return true
}

// RejectBotApprovals 拒绝该 bot 所有未决审批（取消任务时用）。
// RejectBotApprovals rejects every pending approval for this bot (used when a task is cancelled).
func (b *Bus) RejectBotApprovals(bot, reason string) {
	b.mu.Lock()
	var ids []int
	for id, a := range b.approvals {
		if a.Bot != bot {
			continue
		}
		select {
		case <-a.decided:
		default:
			ids = append(ids, id)
		}
	}
	b.mu.Unlock()
	for _, id := range ids {
		b.Decide(id, false, reason)
	}
}

func (b *Bus) PendingApprovals() []*Approval {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []*Approval
	for _, a := range b.approvals {
		select {
		case <-a.decided:
		default:
			out = append(out, a)
		}
	}
	return out
}
