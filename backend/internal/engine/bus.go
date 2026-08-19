package engine

import (
	"botbureau/backend/internal/config"
	"botbureau/backend/internal/i18n"

	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// kind: msg / tool / approval / approval_done / status / system / refresh

// Event is the sole data source for the presentation layer (Electron / CLI).
// Shape: {id, ts, kind, chat, source, text, ...extra}
// kind: msg / tool / approval / approval_done / status / system / refresh
// chat: "group", "dm:<bot name>", or "" (global)
type Event map[string]any

// ConversationPreview is the small, independent record used by conversation lists. It is not a page
// of chat history: one latest activity summary per chat is enough for a sidebar or a mobile inbox.
type ConversationPreview struct {
	ID       int    `json:"id"`
	TS       int64  `json:"ts"`
	Kind     string `json:"kind"`
	Chat     string `json:"chat"`
	Source   string `json:"source"`
	Text     string `json:"text"`
	HasFiles bool   `json:"has_files,omitempty"`
}

// Msg is a single message delivered to a bot's inbox.
type Msg struct {

	// "user", a bot name, "routine:<routine name>", or stopSentinel
	Sender  string
	Content string
	Chat    string // "group" or "dm"

	// false = written into context as group chat background only; does not trigger a response
	Respond bool

	// This message's id in the event stream (0 = not in the stream, e.g. a routine trigger).
	// A bot's reply points back through it to say which message it is answering.
	ID int

	// Which earlier message this one quotes (set when the user replies to a specific message)
	Quote *Quote

	// Whether the directories the user named in this message should be taken into this recipient's
	// working area (see roots.go).

	// Separate from Respond because the two answer different questions: Respond asks "is this job
	// yours", this one asks "was that grant addressed to you". Name someone in the group and only they
	// count; name nobody and everyone does — "treat /Users/me/proj as your working directory", said in
	// a group, is said to the group.
	GrantRoots bool

	// Files the user attached to this message. On arrival they are placed in the recipient's own
	// workspace (see attach.go), and images additionally enter the model's context as image blocks.
	Files []Attachment

	// Set when this message broke into the middle of the recipient's turn (taken by pumpInbox at a safe
	// point). It only means anything within that turn, so pumpInbox stamps it there rather than it
	// travelling with the delivery.
	Interject bool
}

// Quote is a quoted message: its event id, who said it, and a shortened copy of the text.
// The text is stored rather than just the id because the event stream is trimmed as it grows (see
// Emit), and a quotation must not go blank just because the original scrolled out of the log.
type Quote struct {
	ID   int    `json:"id"`
	From string `json:"from"`
	Text string `json:"text"`
}

// A quotation keeps about a line: it is a signpost back to the original, not a copy of it.
const quoteLimit = 160

// QuoteEvent turns a message in the event stream into a quotation. Only msg events qualify: pointing
// at a tool line or a system notice leads back to nothing anyone said.
func QuoteEvent(ev Event) *Quote {
	if ev == nil {
		return nil
	}
	if kind, _ := ev["kind"].(string); kind != "msg" {
		return nil
	}
	text, _ := ev["text"].(string)
	if strings.TrimSpace(text) == "" {
		return nil
	}
	from, _ := ev["source"].(string)
	return &Quote{ID: EventID(ev), From: from, Text: quoteSnippet(text)}
}

// quoteOf turns the message that triggered a turn into a quotation the reply can point back at.
// A message that never entered the event stream (a routine trigger) has nothing to point at.
func quoteOf(m Msg) *Quote {
	if m.ID == 0 || strings.TrimSpace(m.Content) == "" {
		return nil
	}
	return &Quote{ID: m.ID, From: m.Sender, Text: quoteSnippet(m.Content)}
}

// Extra flattens a quotation into three event fields. A nil quotation returns nil, which Emit accepts
// as-is, so callers never have to branch on whether there is one.
func (q *Quote) Extra() map[string]any {
	if q == nil {
		return nil
	}
	return map[string]any{"reply_to": q.ID, "reply_src": q.From, "reply_text": q.Text}
}

// MsgExtra flattens a quotation and any attachments into fields on the event. With neither it returns
// nil, which Emit accepts as-is, so callers never branch on whether this message carries anything.
func MsgExtra(q *Quote, files []Attachment) map[string]any {
	extra := q.Extra()
	if len(files) == 0 {
		return extra
	}
	if extra == nil {
		extra = map[string]any{}
	}

	// Metadata only. The bytes lie under data/uploads and the UI fetches them from /api/file by id:
	// base64 inside the event log would blow the whole chat log apart after a few screenshots.
	extra["files"] = files
	return extra
}

func quoteSnippet(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > quoteLimit {
		return string(r[:quoteLimit]) + "…"
	}
	return s
}

const stopSentinel = "__stop__"

// Approval is a sensitive action awaiting user approval (e.g. a non-read-only bash command).
type Approval struct {
	ID     int    `json:"id"`
	Bot    string `json:"bot"`
	Action string `json:"action"`
	Chat   string `json:"chat"`

	// The directory which, made a working directory, would stop this action from escaping; empty when
	// there is none. The UI turns it into a third choice beside Approve and Reject.
	Dir string `json:"dir,omitempty"`

	// Unified diff of a pending write or edit. Empty for bash and plugins. Optional so older clients
	// ignore it.
	Diff string `json:"diff,omitempty"`

	Title string `json:"title,omitempty"`
	Body  string `json:"body,omitempty"`

	// Kind is "plan" for submit_plan cards; empty for command/file/plugin approvals.
	Kind string `json:"kind,omitempty"`

	decided  chan struct{}
	approved bool
	reason   string
}

// Wait blocks until the user makes a decision.
func (a *Approval) Wait() (approved bool, reason string) {
	<-a.decided
	return a.approved, a.reason
}

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
	latestByChat map[string]ConversationPreview
	nextID       int
	notify       chan struct{}
	bots         map[string]*BotWorker
	order        []string
	approvals    map[int]*Approval
	nextApproval int

	groups     []*Group
	groupsPath string
	groupsFile bool

	// quota-alert throttling (keyed by provider label)
	quotaLast map[string]time.Time

	// The chat history on disk. The events slice in memory is only a cache of its tail.
	log *eventLog
}

func NewBus() *Bus {
	return &Bus{
		nextID:       1,
		notify:       make(chan struct{}),
		bots:         map[string]*BotWorker{},
		approvals:    map[int]*Approval{},
		latestByChat: map[string]ConversationPreview{},
		nextApproval: 1,
	}
}

// ---- bot registration ----

func (b *Bus) Register(w *BotWorker) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bots[w.Name()] = w
	b.order = append(b.order, w.Name())

	// Registering is not joining. Membership comes from LoadGroups (the upgrade fallback) and from
	// what the user explicitly does — this used to push every newly registered bot into the default
	// group, so "create a bot" quietly added it to the group chat as well.
}

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

func (b *Bus) SetGroupMember(name string, in bool) (ok, changed bool) {
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

// Resolve maps the name people see back to the internal id.

// The UI shows a single name, while workspaces, task ownership and group membership are keyed by the
// id — fixed at creation, because changing it would mean relocating a member's whole working
// directory. When the two differ (id "test", display name "Wren"), the model sees "Wren" and passes
// "Wren" to message_bot; this translates it back. An unknown name is returned unchanged, so callers
// still report "no such bot" as usual.
func (b *Bus) Resolve(name string) string {
	name = strings.TrimSpace(name)
	name = strings.TrimPrefix(name, "@")
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.bots[name]; ok {
		return name
	}
	for id, w := range b.bots {
		if strings.EqualFold(w.Cfg.Title(), name) {
			return id
		}
	}
	return name
}

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
	b.rememberConversationEventLocked(ev)

	// Memory holds only the recent tail. Scrolling back reads the file (see eventlog.go), so trimming
	// here makes no message disappear.
	if len(b.events) > memEvents*2 {
		b.events = append([]Event(nil), b.events[len(b.events)-memEvents:]...)
	}

	// One appended line rather than a rewritten file. Every event used to re-serialise thousands.
	if !transientKind(kind) {
		b.log.append(ev)
	}
	close(b.notify)
	b.notify = make(chan struct{})
	b.mu.Unlock()
	return ev
}

// EnableEventLog opens the chat history on disk: migrate the old format, compact the activity log, and
// read the recent tail into memory. Tests that skip it stay in memory and write nothing.
func (b *Bus) EnableEventLog(path string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	oldEvents := append([]Event(nil), b.events...)
	dir := filepath.Dir(path)
	b.log = newEventLog(filepath.Join(dir, "events.jsonl"))
	b.log.migrate(path)
	b.log.compact()
	b.latestByChat = map[string]ConversationPreview{}

	maxID := 0
	var tail []Event
	_ = b.log.scan(func(ev Event) {
		if id, _ := ev["id"].(int); id > maxID {
			maxID = id
		}
		// The sidebar index sees the complete durable log; only the chat-pane cache is bounded.
		b.rememberConversationEventLocked(ev)
		if k, _ := ev["kind"].(string); transientKind(k) {
			return
		}
		tail = append(tail, ev)
		if len(tail) > memEvents*2 {
			tail = append([]Event(nil), tail[len(tail)-memEvents:]...)
		}
	})
	b.events = tail
	// Keep events emitted before logging was enabled too. This is useful to tests and makes the
	// method safe for embedders that turn persistence on after constructing the bus.
	for _, ev := range oldEvents {
		b.rememberConversationEventLocked(ev)
	}
	if maxID >= b.nextID {
		b.nextID = maxID + 1
	}
}

// History fetches an earlier page of one conversation, oldest first. A before of 0 starts from the
// newest. more being false means the start of the conversation, and the UI stops asking for more.
func (b *Bus) History(chat string, before, limit int) (evs []Event, more bool) {
	b.mu.Lock()
	log := b.log
	b.mu.Unlock()
	if log == nil {
		return []Event{}, false
	}
	evs, more = log.page(chat, before, limit)
	if evs == nil {
		evs = []Event{}
	}
	return evs, more
}

// DeleteChat wipes one conversation's history. Production reaches it only for an explicit data-deletion
// action: deleting a group conversation or purging a member.
func (b *Bus) DeleteChat(chat string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	kept := b.events[:0]
	for _, ev := range b.events {
		if c, _ := ev["chat"].(string); c != chat {
			kept = append(kept, ev)
		}
	}
	b.events = append([]Event(nil), kept...)
	delete(b.latestByChat, chat)
	return b.log.deleteChat(chat)
}

// EventsSince blocks waiting for new events with id > after; returns nil on timeout (for heartbeats).
func (b *Bus) EventsSince(after int, timeout time.Duration) []Event {
	return b.EventsSinceCtx(nil, after, timeout)
}

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

// EventID reads an event's id. Events are maps, so the assertion would otherwise be repeated everywhere.
func EventID(ev Event) int {
	id, _ := ev["id"].(int)
	return id
}

// EventByID looks one event up by id (a quote reply needs the original text to excerpt).
// It scans backwards, since what people quote is usually what was just said.
func (b *Bus) EventByID(id int) (Event, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := len(b.events) - 1; i >= 0; i-- {
		if EventID(b.events[i]) == id {
			return b.events[i], true
		}
	}
	return nil, false
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

// ConversationPreviews returns one latest activity summary per conversation. It is backed by the
// event log's index-in-memory, not by the recent event window, so an old conversation still appears
// in a sidebar even when its messages are no longer in the chat pane's initial payload.
func (b *Bus) ConversationPreviews() []ConversationPreview {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]ConversationPreview, 0, len(b.latestByChat))
	for _, preview := range b.latestByChat {
		out = append(out, preview)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].ID < out[j].ID
	})
	return out
}

func conversationEvent(kind string) bool {
	return kind == "msg" || kind == "tool" || kind == "system"
}

// rememberConversationEventLocked updates the sidebar index. The caller must hold b.mu.
func (b *Bus) rememberConversationEventLocked(ev Event) {
	kind, _ := ev["kind"].(string)
	chat, _ := ev["chat"].(string)
	if chat == "" || !conversationEvent(kind) {
		return
	}
	if b.latestByChat == nil {
		b.latestByChat = map[string]ConversationPreview{}
	}
	preview := conversationPreview(ev)
	if old, ok := b.latestByChat[chat]; ok && old.ID > preview.ID {
		return
	}
	b.latestByChat[chat] = preview
}

func conversationPreview(ev Event) ConversationPreview {
	p := ConversationPreview{
		ID:     EventID(ev),
		Kind:   stringValue(ev["kind"]),
		Chat:   stringValue(ev["chat"]),
		Source: stringValue(ev["source"]),
		Text:   previewText(stringValue(ev["text"])),
		TS:     eventTimestamp(ev["ts"]),
	}
	if files, ok := ev["files"]; ok {
		switch v := files.(type) {
		case []Attachment:
			p.HasFiles = len(v) > 0
		case []any:
			p.HasFiles = len(v) > 0
		}
	}
	return p
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

func eventTimestamp(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	default:
		return 0
	}
}

func previewText(s string) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) > 400 {
		return string(r[:400]) + "…"
	}
	return string(r)
}

// ---- message delivery ----

func (b *Bus) Deliver(sender, target, chat, content string, respond bool) bool {
	return b.DeliverFiles(sender, target, chat, content, respond, nil)
}

// DeliverFiles is the same, plus the files the user attached.
func (b *Bus) DeliverFiles(sender, target, chat, content string, respond bool, files []Attachment) bool {

	// In a DM there are only the two of you, so a directory you name is named to it
	return b.DeliverMsg(target, Msg{
		Sender: sender, Content: content, Chat: chat, Respond: respond,
		GrantRoots: sender == "user", Files: files,
	})
}

// DeliverMsg delivers an already-assembled message (used when it carries an event id or a quotation).
func (b *Bus) DeliverMsg(target string, m Msg) bool {
	w := b.Bot(target)
	if w == nil {
		return false
	}
	select {
	case w.inbox <- m:
		return true

	// inbox full (256-message backlog) — drop the message and alert; never block the sender
	default:
		b.Emit("system", m.Chat, "system",
			fmt.Sprintf(i18n.T("%s's inbox is full, the message was dropped"), target), nil)
		return false
	}
}

// PostGroup posts a group chat message: it is written to the event stream; mentioned group members
// are triggered to respond, while the remaining members only receive it as background context.
// Bots outside the group receive nothing at all.
func (b *Bus) PostGroup(sender, text string, respondTargets []string) {
	b.PostGroupTo("group", sender, text, respondTargets)
}

// MentionedBots returns the group members @-mentioned in the text (@s to bots outside the group are ignored).
func (b *Bus) MentionedBots(text string) []string {
	return b.MentionedBotsIn("group", text)
}

// DefaultGroupMember is the default responder when no one is mentioned in the group chat (first on the list).
func (b *Bus) DefaultGroupMember() string {
	ms := b.GroupMembers()
	if len(ms) == 0 {
		return ""
	}
	return ms[0]
}

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

// ---- approvals ----

func (b *Bus) RequestApproval(bot, action, chat, dir string) *Approval {
	return b.requestApproval(bot, action, chat, dir, "")
}

func (b *Bus) requestApproval(bot, action, chat, dir, diff string) *Approval {
	b.mu.Lock()
	a := &Approval{ID: b.nextApproval, Bot: bot, Action: action, Chat: chat, Dir: dir, Diff: diff, decided: make(chan struct{})}
	b.nextApproval++
	b.approvals[a.ID] = a
	b.mu.Unlock()
	extra := map[string]any{"approval_id": a.ID, "approval_dir": dir}
	if diff != "" {
		extra["approval_diff"] = diff
	}
	b.Emit("approval", chat, bot, action, extra)
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

func (b *Bus) requestPlan(bot, chat, title, body string) *Approval {
	b.mu.Lock()
	a := &Approval{
		ID: b.nextApproval, Bot: bot, Action: title, Chat: chat,
		Kind: "plan", Title: title, Body: body, decided: make(chan struct{}),
	}
	b.nextApproval++
	b.approvals[a.ID] = a
	b.mu.Unlock()
	b.Emit("approval", chat, bot, title, map[string]any{
		"approval_id": a.ID, "approval_kind": "plan",
		"approval_title": title, "approval_body": body,
	})
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
		return false // already handled
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

// Approval fetches one approval by id (decided ones remain until cleared).
// "Approve and make this a working directory" needs its Bot and Dir before the decision goes through.
func (b *Bus) Approval(id int) *Approval {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.approvals[id]
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
