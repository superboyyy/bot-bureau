package engine

import (
	"botbureau/backend/internal/config"
	"botbureau/backend/internal/model"
	"botbureau/backend/internal/secret"

	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- 测试用的脚本化 provider：按预设队列返回 model.StepResult ----
// ---- Scripted provider for tests: returns model.StepResult from a preset queue ----

type scriptedProvider struct{ script []model.StepResult }

func (p *scriptedProvider) SupportsWebTools() bool    { return false }
func (p *scriptedProvider) Label() string             { return "scripted" }
func (p *scriptedProvider) NewSession() model.Session { return &scriptedSession{script: &p.script} }

type scriptedSession struct {
	script      *[]model.StepResult
	history     []string
	mark        int
	toolResults [][]model.ToolResult
}

func (s *scriptedSession) MarkTurn() { s.mark = len(s.history) }
func (s *scriptedSession) Rollback() { s.history = s.history[:s.mark] }
func (s *scriptedSession) AddUser(text string) {
	s.history = append(s.history, "user:"+text)
}
func (s *scriptedSession) AddToolResults(rs []model.ToolResult) {
	s.toolResults = append(s.toolResults, rs)
	s.history = append(s.history, "toolresults")
}

// 这个假 session 只服务 engine 的用例，所以自带最朴素的裁剪与存档实现——
// 借用 model 包内部的游标只会把两边的测试绑死在一起。
// This fake only serves the engine's tests, so it carries the plainest possible trim and snapshot:
// borrowing model's internal cursor would just tie the two test suites together.
func (s *scriptedSession) Trim(limit int) {
	if len(s.history) > limit {
		s.history = append([]string(nil), s.history[len(s.history)-limit:]...)
	}
}
func (s *scriptedSession) Snapshot() json.RawMessage {
	raw, _ := json.Marshal(s.history)
	return raw
}
func (s *scriptedSession) Restore(raw json.RawMessage) bool {
	var hist []string
	if json.Unmarshal(raw, &hist) != nil {
		return false
	}
	s.history = hist
	return true
}
func (s *scriptedSession) Step(ctx context.Context, system string, tools []model.ToolDef, includeWeb bool) (model.StepResult, error) {
	if len(*s.script) == 0 {
		return model.StepResult{StopReason: "end_turn"}, nil
	}
	res := (*s.script)[0]
	*s.script = (*s.script)[1:]
	s.history = append(s.history, "assistant")
	return res, nil
}

func newTestDeps(t *testing.T, dataDir string) *TeamDeps {
	t.Helper()
	deps := NewTeamDeps(dataDir, secret.NewKeyStore(filepath.Join(dataDir, "keys.json")),
		filepath.Join(dataDir, "mcp.yaml"))
	// config.NewSettings 会按偏好重设全局语言，这里同样钉死 zh，否则用例断言的中文文案会跟着系统环境漂
	// config.NewSettings re-applies the global locale, so pin it to zh here too or the Chinese assertions drift with the environment
	deps.Settings = config.NewSettings(dataDir)
	// 钉 en：断言对的是源码里的英文原文，否则中文机器上跑出来的是译文
	// Pin en: the assertions read the English source, which translated output would never match
	deps.Settings.SetLocalePref("en")
	return deps
}

func newTestWorker(t *testing.T, name string, p model.Provider) (*BotWorker, *Bus, *Scheduler) {
	t.Helper()
	dataDir := t.TempDir()
	bus := NewBus()
	sched := NewScheduler(bus, filepath.Join(dataDir, "routines.json"))
	w, err := NewBotWorker(config.BotConfig{Name: name, Role: "test", Provider: "fake"}, bus, sched, dataDir, newTestDeps(t, dataDir))
	if err != nil {
		t.Fatal(err)
	}
	if p != nil {
		w.provider = p
	}
	bus.Register(w)
	// 显式入群：注册不再自动拉人（见 Bus.Register），但绝大多数用例测的是群聊行为
	// Join explicitly: registering no longer adds anyone (see Bus.Register), yet most cases here
	// exercise group behaviour
	bus.SetGroupMemberIn("group", name, true)
	return w, bus, sched
}

func addTestBot(t *testing.T, bus *Bus, sched *Scheduler, name string) *BotWorker {
	t.Helper()
	dir := t.TempDir()
	w, err := NewBotWorker(config.BotConfig{Name: name, Role: "r", Provider: "fake"}, bus, sched, dir, newTestDeps(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	bus.Register(w)
	bus.SetGroupMemberIn("group", name, true)
	return w
}

// ---- bash 审批门 ----
// ---- bash approval gate ----

func TestBashReadonlyGate(t *testing.T) {
	w, bus, _ := newTestWorker(t, "a", nil)
	tb := w.toolbox
	tb.currentChat = "dm"

	// 只读命令直接执行
	// Read-only commands execute directly
	if out, isErr := tb.Execute("bash", map[string]any{"command": "echo hi"}); isErr || !strings.Contains(out, "hi") {
		t.Fatalf("echo should run directly: %q %v", out, isErr)
	}
	// 非只读、元字符、find 都要走审批（后台拒绝）
	// Non-read-only commands, metacharacters, and find all go through approval (rejected in the background)
	for _, cmd := range []string{"touch x", "ls $(echo hi)", "find . -delete", "echo hi > f"} {
		go func() {
			for len(bus.PendingApprovals()) == 0 {
				time.Sleep(5 * time.Millisecond)
			}
			bus.Decide(bus.PendingApprovals()[0].ID, false, "test rejection")
		}()
		out, isErr := tb.Execute("bash", map[string]any{"command": cmd})
		if !isErr || !strings.Contains(out, "rejected") {
			t.Fatalf("%q should go through approval and be rejected: %q %v", cmd, out, isErr)
		}
	}
	// 批准后执行
	// Executes after approval
	go func() {
		for len(bus.PendingApprovals()) == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		bus.Decide(bus.PendingApprovals()[0].ID, true, "")
	}()
	if out, isErr := tb.Execute("bash", map[string]any{"command": "touch approved.txt"}); isErr {
		t.Fatalf("should run after approval: %q", out)
	}
	if _, err := os.Stat(filepath.Join(w.workspace, "approved.txt")); err != nil {
		t.Fatal("the approved command never actually ran")
	}
}

func TestSandboxPaths(t *testing.T) {
	w, bus, _ := newTestWorker(t, "b", nil)
	tb := w.toolbox
	tb.currentChat = "dm"
	go func() {
		for len(bus.PendingApprovals()) == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		bus.Decide(bus.PendingApprovals()[0].ID, true, "")
	}()
	if out, isErr := tb.Execute("write_file", map[string]any{"path": "notes/x.txt", "content": "hello"}); isErr {
		t.Fatalf("write failed: %q", out)
	}
	if out, isErr := tb.Execute("read_file", map[string]any{"path": "notes/x.txt"}); isErr || out != "hello" {
		t.Fatalf("read failed: %q %v", out, isErr)
	}
	if out, isErr := tb.Execute("read_file", map[string]any{"path": "../../../etc/passwd"}); !isErr || !strings.Contains(out, "Path escapes the workspace") {
		t.Fatalf("an escaping path should be rejected: %q %v", out, isErr)
	}
}

func TestMessageBotDMGate(t *testing.T) {
	w, bus, sched := newTestWorker(t, "a", nil)
	w2 := addTestBot(t, bus, sched, "c")

	w.toolbox.currentChat = "dm"
	if out, isErr := w.toolbox.Execute("message_bot", map[string]any{"to": "c", "content": "x"}); !isErr || !strings.Contains(out, "A DM is one-on-one") {
		t.Fatalf("message_bot should be rejected in a dm: %q %v", out, isErr)
	}
	w.toolbox.currentChat = "group"
	if out, isErr := w.toolbox.Execute("message_bot", map[string]any{"to": "c", "content": "do the work"}); isErr {
		t.Fatalf("message_bot should succeed in a group: %q", out)
	}
	select {
	case m := <-w2.inbox:
		if !m.Respond || m.Chat != "group" || !strings.Contains(m.Content, "do the work") {
			t.Fatalf("wrong handoff message shape: %+v", m)
		}
	case <-time.After(time.Second):
		t.Fatal("c never received the handoff")
	}
}

// ---- 群聊语义 ----
// ---- Group chat semantics ----

func TestPostGroupRespondFlags(t *testing.T) {
	_, bus, sched := newTestWorker(t, "a", nil)
	w2 := addTestBot(t, bus, sched, "c")

	bus.PostGroup("user", "@c do the work", []string{"c"})
	a := bus.Bot("a")
	ma, mc := <-a.inbox, <-w2.inbox
	if ma.Respond {
		t.Fatal("unmentioned a should not be triggered")
	}
	if !mc.Respond {
		t.Fatal("mentioned c should be triggered")
	}
}

func TestMentions(t *testing.T) {
	_, bus, sched := newTestWorker(t, "scout", nil)
	addTestBot(t, bus, sched, "scout2")
	got := bus.MentionedBots("@scout2 your turn")
	if len(got) != 1 || got[0] != "scout2" {
		t.Fatalf("@scout2 must not match scout: %v", got)
	}
}

func TestGroupMembership(t *testing.T) {
	_, bus, sched := newTestWorker(t, "a", nil)
	w2 := addTestBot(t, bus, sched, "c")

	// 默认全员在群
	// Everyone is in the group chat by default
	if got := bus.GroupMembers(); len(got) != 2 {
		t.Fatalf("everyone should be in the group by default: %v", got)
	}
	// 移出后：收不到群聊、@ 无效、message_bot 指不到
	// After removal: receives no group chat messages, @ has no effect, message_bot cannot target it
	bus.SetGroupMember("c", false)
	bus.PostGroup("user", "@c do the work", []string{"c"})
	select {
	case m := <-w2.inbox:
		t.Fatalf("a non-member must not receive group messages: %+v", m)
	case <-time.After(100 * time.Millisecond):
	}
	if got := bus.MentionedBots("@c your turn"); len(got) != 0 {
		t.Fatalf("mentioning a non-member should do nothing: %v", got)
	}
	a := bus.Bot("a")
	a.toolbox.currentChat = "group"
	if out, isErr := a.toolbox.Execute("message_bot", map[string]any{"to": "c", "content": "x"}); !isErr {
		t.Fatalf("message_bot to a non-member should error: %q", out)
	}
	// 私聊不受影响
	// DMs are unaffected
	if !bus.Deliver("user", "c", "dm", "dm", true) {
		t.Fatal("a non-member's dm should still work")
	}
	<-w2.inbox
	// 拉回群
	// Add back into the group chat
	bus.SetGroupMember("c", true)
	if got := bus.GroupMembers(); len(got) != 2 {
		t.Fatalf("should recover after re-adding: %v", got)
	}
}

func TestGroupMembersPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "group.json")
	_, bus, sched := newTestWorker(t, "a", nil)
	addTestBot(t, bus, sched, "c")
	bus.LoadGroupMembers(path) // 文件不存在：保持默认全员 / file missing: keep default full membership
	bus.SetGroupMember("c", false)

	// 新总线加载同一文件：c 不在群里
	// A new bus loads the same file: c is not in the group chat
	bus2 := NewBus()
	dir2 := t.TempDir()
	sched2 := NewScheduler(bus2, filepath.Join(dir2, "routines.json"))
	w1, _ := NewBotWorker(config.BotConfig{Name: "a", Provider: "fake"}, bus2, sched2, dir2, newTestDeps(t, dir2))
	bus2.Register(w1)
	w2, _ := NewBotWorker(config.BotConfig{Name: "c", Provider: "fake"}, bus2, sched2, dir2, newTestDeps(t, dir2))
	bus2.Register(w2)
	bus2.LoadGroupMembers(path)
	got := bus2.GroupMembers()
	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("the persisted member list should apply: %v", got)
	}
}

func TestTaskBoardFlow(t *testing.T) {
	w, bus, sched := newTestWorker(t, "chief", nil)
	w2 := addTestBot(t, bus, sched, "coder")

	tb := w.toolbox
	tb.currentChat = "dm"
	if _, isErr := tb.Execute("assign_task", map[string]any{"to": "coder", "title": "x"}); !isErr {
		t.Fatal("assign_task should be rejected in a dm")
	}
	tb.currentChat = "group"
	out, isErr := tb.Execute("assign_task", map[string]any{"to": "coder", "title": "write the script", "detail": "count the lines"})
	if isErr {
		t.Fatalf("assign_task failed: %q", out)
	}
	// 负责人收到点名通知
	// The assignee receives a mention notification
	m := <-w2.inbox
	if !m.Respond || !strings.Contains(m.Content, "[Task #1]") {
		t.Fatalf("the owner should be notified: %+v", m)
	}
	// 群外的人不能被指派
	// Bots outside the group chat cannot be assigned tasks
	bus.SetGroupMember("coder", false)
	if _, isErr := tb.Execute("assign_task", map[string]any{"to": "coder", "title": "y"}); !isErr {
		t.Fatal("assigning to a non-member should error")
	}
	bus.SetGroupMember("coder", true)
	// 更新状态 + 看板渲染
	// Update status + render the board
	if out, isErr := tb.Execute("update_task", map[string]any{"id": float64(1), "status": "done", "note": "done"}); isErr {
		t.Fatalf("update_task failed: %q", out)
	}
	board, _ := tb.Execute("list_tasks", map[string]any{})
	if !strings.Contains(board, "#1 [done] write the script — owner: coder") {
		t.Fatalf("wrong board rendering: %q", board)
	}
	if _, isErr := tb.Execute("update_task", map[string]any{"id": float64(99), "status": "done"}); !isErr {
		t.Fatal("updating a nonexistent task should error")
	}
}

func TestRememberScopes(t *testing.T) {
	w, _, _ := newTestWorker(t, "a", nil)
	tb := w.toolbox
	if _, isErr := tb.Execute("remember", map[string]any{"note": "a personal preference"}); isErr {
		t.Fatal("self remember failed")
	}
	if _, isErr := tb.Execute("remember", map[string]any{"note": "a team convention", "scope": "team"}); isErr {
		t.Fatal("team remember failed")
	}
	if !strings.Contains(w.mem.Load(), "a personal preference") {
		t.Fatal("personal memory was not written")
	}
	team := w.deps.TeamMem.Load()
	if !strings.Contains(team, "a team convention") || !strings.Contains(team, "a:") {
		t.Fatalf("shared memory should carry the content and the author: %q", team)
	}
}

func TestKeyStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.json")
	ks := secret.NewKeyStore(path)
	if err := ks.Set("XAI_API_KEY", "xai-1234567890abcd"); err != nil {
		t.Fatal(err)
	}
	if ks.Get("XAI_API_KEY") != "xai-1234567890abcd" {
		t.Fatal("Get should return the stored value")
	}
	// 掩码不泄漏明文
	// Masking must not leak the plaintext
	list := ks.List()
	if len(list) != 1 || strings.Contains(list[0]["masked"], "1234567890") {
		t.Fatalf("List should mask: %v", list)
	}
	// 文件权限 0600
	// File permissions must be 0600
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("keys.json should be mode 0600: %v", info.Mode())
	}
	// 环境变量回退
	// Environment variable fallback
	t.Setenv("FALLBACK_KEY", "from-env")
	if ks.Get("FALLBACK_KEY") != "from-env" {
		t.Fatal("should fall back to the environment variable")
	}
	// 重新加载持久化
	// Reload persisted data
	if secret.NewKeyStore(path).Get("XAI_API_KEY") != "xai-1234567890abcd" {
		t.Fatal("persistence failed")
	}
	// 非法名字
	// Invalid name
	if err := ks.Set("bad name", "v"); err == nil {
		t.Fatal("an invalid name should error")
	}
	if !ks.Delete("XAI_API_KEY") || ks.Delete("XAI_API_KEY") {
		t.Fatal("wrong Delete semantics")
	}
}

// ---- agentLoop：max_tokens 截断补配对、refusal 回滚 ----
// ---- agentLoop: repair tool_result pairing on max_tokens truncation, roll back on refusal ----

func TestAgentLoopMaxTokensOrphanRepair(t *testing.T) {
	p := &scriptedProvider{script: []model.StepResult{
		{StopReason: "max_tokens", ToolCalls: []model.ToolCall{{ID: "t1", Name: "bash", Input: map[string]any{"command": "x"}}}},
	}}
	w, _, _ := newTestWorker(t, "a", p)
	sess := w.session("dm").(*scriptedSession)
	sess.MarkTurn()
	sess.AddUser("do something")
	w.agentLoop(context.Background(), "dm", w.sessions["dm"])
	if len(sess.toolResults) != 1 || !sess.toolResults[0][0].IsError || sess.toolResults[0][0].ID != "t1" {
		t.Fatalf("a max_tokens cut must append a paired is_error tool_result: %+v", sess.toolResults)
	}
}

func TestRoutinesRobustness(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "routines.json")
	os.WriteFile(path, []byte(`[
	  {"name":"zero","bot":"a","prompt":"p","every_minutes":0,"next_run":0},
	  {"name":"bad","bot":"a","prompt":"p","every_minutes":"x","next_run":"abc"},
	  {"name":"ok","bot":"a","prompt":"p","every_minutes":5,"next_run":100}
	]`), 0o644)
	bus := NewBus()
	s := NewScheduler(bus, path)
	rs := s.List()
	if len(rs) != 2 {
		t.Fatalf("a malformed entry should be skipped: %v", rs)
	}
	for _, r := range rs {
		if r.EveryMinutes < 1 {
			t.Fatal("every_minutes should be clamped to >= 1")
		}
	}
	now := time.Now().Unix()
	done := make(chan struct{})
	go func() { s.tick(now); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("tick looks like an infinite loop")
	}
	for _, r := range s.List() {
		if r.NextRun <= now {
			t.Fatalf("NextRun should advance into the future: %+v", r)
		}
	}
	// 顶层不是数组 → 忽略不崩
	// Top level is not an array → ignored without crashing
	os.WriteFile(path, []byte(`{"a":1}`), 0o644)
	if got := NewScheduler(bus, path).List(); len(got) != 0 {
		t.Fatalf("a non-array top level should be ignored: %v", got)
	}
	// 超大间隔钳制
	// Oversized intervals are clamped
	if r := s.Add("big", "a", "p", 1<<40); r.EveryMinutes != maxEveryMinutes {
		t.Fatalf("an oversized interval should be clamped: %d", r.EveryMinutes)
	}
}

// ---- 事件流 ----
// ---- Event stream ----

func TestEventsSince(t *testing.T) {
	bus := NewBus()
	bus.Emit("msg", "group", "user", "1", nil)
	evs := bus.EventsSince(0, time.Second)
	if len(evs) != 1 {
		t.Fatalf("should get exactly 1: %v", evs)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		bus.Emit("msg", "group", "user", "2", nil)
	}()
	evs = bus.EventsSince(evs[0]["id"].(int), 2*time.Second)
	if len(evs) != 1 || evs[0]["text"] != "2" {
		t.Fatalf("should wait for the new event: %v", evs)
	}
	if evs := bus.EventsSince(999, 50*time.Millisecond); evs != nil {
		t.Fatalf("a timeout should return nil: %v", evs)
	}
}

func TestUnsetProviderNeedsSetup(t *testing.T) {
	w, err := NewBotWorker(config.BotConfig{Name: "a", Role: "r"}, NewBus(), NewScheduler(NewBus(), filepath.Join(t.TempDir(), "r.json")), t.TempDir(), newTestDeps(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if w.ProviderLabel() != "unset" {
		t.Fatalf("an empty provider should be unset: %s", w.ProviderLabel())
	}
	res, err := w.provider.NewSession().Step(context.Background(), "", nil, false)
	if err != nil || len(res.Texts) == 0 {
		t.Fatalf("an unset Step should return the hint: %v %v", res, err)
	}
}

func TestWriteFileAndHostPathGate(t *testing.T) {
	w, bus, _ := newTestWorker(t, "a", nil)
	tb := w.toolbox
	tb.currentChat = "dm"
	for _, cmd := range []string{"cat /etc/passwd", "ls ~", "ls .."} {
		go func() {
			for len(bus.PendingApprovals()) == 0 {
				time.Sleep(5 * time.Millisecond)
			}
			bus.Decide(bus.PendingApprovals()[0].ID, false, "test rejection")
		}()
		out, isErr := tb.Execute("bash", map[string]any{"command": cmd})
		if !isErr || !strings.Contains(out, "rejected") {
			t.Fatalf("%q should go through approval and be rejected: %q %v", cmd, out, isErr)
		}
	}
	go func() {
		for len(bus.PendingApprovals()) == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		bus.Decide(bus.PendingApprovals()[0].ID, false, "test rejection")
	}()
	out, isErr := tb.Execute("write_file", map[string]any{"path": "x.txt", "content": "nope"})
	if !isErr || !strings.Contains(out, "rejected") {
		t.Fatalf("write_file should go through approval and be rejected: %q %v", out, isErr)
	}
	if _, err := os.Stat(filepath.Join(w.workspace, "x.txt")); err == nil {
		t.Fatal("a rejected write_file must not touch disk")
	}
}

func TestApprovalTimeout(t *testing.T) {
	config.SetApprovalTimeout(40 * time.Millisecond)
	defer config.SetApprovalTimeout(0) // 0 = 恢复默认 / 0 restores the default
	w, _, _ := newTestWorker(t, "a", nil)
	w.toolbox.currentChat = "dm"
	start := time.Now()
	out, isErr := w.toolbox.Execute("bash", map[string]any{"command": "touch x"})
	if time.Since(start) > 2*time.Second {
		t.Fatal("an approval timeout must not hang")
	}
	if !isErr {
		t.Fatalf("a timeout counts as a rejection: %q", out)
	}
}

func TestCancelUnblocksApproval(t *testing.T) {
	p := &scriptedProvider{script: []model.StepResult{
		{StopReason: "tool_use", ToolCalls: []model.ToolCall{{ID: "1", Name: "bash", Input: map[string]any{"command": "touch x"}}}},
		{StopReason: "end_turn", Texts: []string{"done"}},
	}}
	w, bus, _ := newTestWorker(t, "a", p)
	go func() {
		for len(bus.PendingApprovals()) == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		w.Cancel()
	}()
	done := make(chan struct{})
	go func() {
		w.handle(Msg{Sender: "user", Content: "x", Chat: "dm", Respond: true})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handle should return after cancelling")
	}
	if n := len(bus.PendingApprovals()); n != 0 {
		t.Fatalf("no approval should remain after cancelling: %d", n)
	}
}

func TestSessionAndEventPersist(t *testing.T) {
	dataDir := t.TempDir()
	bus := NewBus()
	bus.EnableEventLog(filepath.Join(dataDir, "events.json"))
	sched := NewScheduler(bus, filepath.Join(dataDir, "routines.json"))
	w, err := NewBotWorker(config.BotConfig{Name: "a", Role: "r", Provider: "fake"}, bus, sched, dataDir, newTestDeps(t, dataDir))
	if err != nil {
		t.Fatal(err)
	}
	bus.Register(w)
	w.handle(Msg{Sender: "user", Content: "hello persist", Chat: "dm", Respond: true})

	bus2 := NewBus()
	bus2.EnableEventLog(filepath.Join(dataDir, "events.json"))
	found := false
	for _, ev := range bus2.Recent(50) {
		if text, _ := ev["text"].(string); strings.Contains(text, "hello persist") {
			found = true
		}
	}
	if !found {
		t.Fatal("the event log should survive a restart")
	}
	w2, err := NewBotWorker(config.BotConfig{Name: "a", Role: "r", Provider: "fake"}, bus2, sched, dataDir, newTestDeps(t, dataDir))
	if err != nil {
		t.Fatal(err)
	}
	// 改用 Snapshot 断言：会话内部结构归 model 包所有，测试不该伸手进去
	// Assert via Snapshot: the session's innards belong to the model package, not to this test
	joined := string(w2.session("dm").Snapshot())
	if !strings.Contains(joined, "hello persist") {
		t.Fatalf("the session history should survive a restart: %q", joined)
	}
}

// 群聊里直呼其名要算点名——不该逼用户记得敲 @。
// 但不能过头：substring 命中会让一句无关的话把整个群都激活。
// Calling a bot by name in a group must count as a mention; users should not have to remember the @.
// It must not overreach either: substring hits would wake the whole group on an unrelated sentence.
func TestBareNameCountsAsMention(t *testing.T) {
	cases := []struct {
		text, name string
		want       bool
		why        string
	}{
		{"@scout go look into it", "scout", true, "the @ form"},
		{"scout go look into it", "scout", true, "a bare name"},
		{"could scout take a look at this", "scout", true, "mid-sentence"},
		{"Scout, take a look", "scout", true, "case insensitive"},
		{"scout: let us start", "scout", true, "followed by punctuation"},
		{"scouting the area", "scout", false, "must not match a longer word"},
		{"the mainscout is here", "scout", false, "must not match a word ending"},
		{"@scout2 go", "scout", false, "the @ form must not match a longer name"},
		{"let coder write it", "scout", false, "someone else is called"},
		{"", "scout", false, "empty text"},
		{"scout", "", false, "empty name"},
	}
	for _, c := range cases {
		if got := containsMention(c.text, c.name); got != c.want {
			t.Errorf("%s: containsMention(%q, %q) = %v, want %v", c.why, c.text, c.name, got, c.want)
		}
	}
}

func TestAgentLoopRefusalRollback(t *testing.T) {
	p := &scriptedProvider{script: []model.StepResult{{StopReason: "refusal"}}}
	w, _, _ := newTestWorker(t, "a", p)
	w.handle(Msg{Sender: "user", Content: "x", Chat: "dm", Respond: true})
	sess := w.sessions["dm"].(*scriptedSession)
	if len(sess.history) != 0 {
		t.Fatalf("a refusal must roll back the whole turn: %v", sess.history)
	}
}

// ---- cutTracker ----

func TestQuotaErrorSurfacesGlobally(t *testing.T) {
	// 模拟 provider 抛 QuotaError：会话内报错 + 全局告警各一条
	// Simulate the provider throwing QuotaError: one error in the session plus one global alert
	p := &scriptedProvider{}
	w, bus, _ := newTestWorker(t, "a", p)
	w.sessions["dm"] = &quotaFailSession{}
	w.handle(Msg{Sender: "user", Content: "do the work", Chat: "dm", Respond: true})
	var inChat, global bool
	for _, ev := range bus.Recent(20) {
		if ev["kind"] == "msg" && ev["chat"] == "dm:a" && strings.Contains(ev["text"].(string), "quota") {
			inChat = true
		}
		if ev["kind"] == "system" && ev["chat"] == "" && ev["quota"] == true {
			global = true
		}
	}
	if !inChat || !global {
		t.Fatalf("a quota error must alert on both channels: in-chat=%v global=%v", inChat, global)
	}
}

// 只为额度告警这一个用例准备的假 session：每次 Step 都报余额不足。
// A fake session for the quota-alert test alone: every Step reports an exhausted balance.
type quotaFailProvider struct{}

func (quotaFailProvider) SupportsWebTools() bool    { return false }
func (quotaFailProvider) Label() string             { return "quota-fail" }
func (quotaFailProvider) NewSession() model.Session { return &quotaFailSession{} }

type quotaFailSession struct{ history []string }

func (s *quotaFailSession) MarkTurn()                            {}
func (s *quotaFailSession) Rollback()                            {}
func (s *quotaFailSession) AddUser(text string)                  { s.history = append(s.history, text) }
func (s *quotaFailSession) AddToolResults(rs []model.ToolResult) {}
func (s *quotaFailSession) Trim(limit int)                       {}
func (s *quotaFailSession) Snapshot() json.RawMessage            { return nil }
func (s *quotaFailSession) Restore(raw json.RawMessage) bool     { return false }

func (s *quotaFailSession) Step(ctx context.Context, system string, tools []model.ToolDef, includeWeb bool) (model.StepResult, error) {
	return model.StepResult{}, &model.QuotaError{Msg: "test API quota/balance exhausted"}
}

// ---- TLS ----

// 默认群没人时不该出现——不管有没有 bot。新建 bot 不会自动入群，所以"有 bot、群里没人"
// 是完全正常的一种状态，那时也不该摆一个空群。
//
// The default group must not appear while empty — with or without bots. A new bot joins nothing
// automatically, so "bots exist, the group is empty" is an ordinary state, and no empty group belongs
// on screen then either.
func TestEmptyGroupStaysHidden(t *testing.T) {
	bus := NewBus()
	if got := bus.Groups(); len(got) != 0 {
		t.Fatalf("empty team should show no groups, got %+v", got)
	}
	dataDir := t.TempDir()
	sched := NewScheduler(bus, filepath.Join(dataDir, "routines.json"))
	w, err := NewBotWorker(config.BotConfig{Name: "solo", Role: "r", Provider: "fake"}, bus, sched, dataDir, newTestDeps(t, dataDir))
	if err != nil {
		t.Fatal(err)
	}
	bus.Register(w)
	// 有 bot 但没人入群：仍然不该出现
	// A bot exists but nobody has joined: still nothing to show
	if got := bus.Groups(); len(got) != 0 {
		t.Fatalf("an empty default group must stay hidden even with bots, got %+v", got)
	}
	bus.SetGroupMemberIn("group", "solo", true)
	if got := bus.Groups(); len(got) != 1 || got[0].ID != "group" {
		t.Fatalf("the default group should appear once someone joins, got %+v", got)
	}
	// 人走光了又该收起来 / and it goes away again once everyone leaves
	bus.SetGroupMemberIn("group", "solo", false)
	if got := bus.Groups(); len(got) != 0 {
		t.Fatalf("it should hide again once emptied, got %+v", got)
	}
}

// 改了显示名之后，喊那个名字就该管用——用户看到的、嘴里叫的都是显示名，
// 只认内部 id 等于逼他记一个界面上根本不显示的东西。
//
// Once a bot has a display name, calling that name must work: it is what the user sees and says, and
// matching only the internal id forces them to remember something the UI never shows.
func TestDisplayNameIsMentionable(t *testing.T) {
	dataDir := t.TempDir()
	bus := NewBus()
	sched := NewScheduler(bus, filepath.Join(dataDir, "routines.json"))
	w, err := NewBotWorker(config.BotConfig{
		Name: "wren", DisplayName: "小文", Role: "r", Provider: "fake",
	}, bus, sched, dataDir, newTestDeps(t, dataDir))
	if err != nil {
		t.Fatal(err)
	}
	bus.Register(w)
	bus.SetGroupMemberIn("group", "wren", true)

	for _, text := range []string{"小文 帮我查一下", "@wren take a look", "wren take a look", "麻烦小文看看"} {
		if got := bus.MentionedBotsIn("group", text); len(got) != 1 || got[0] != "wren" {
			t.Fatalf("%q should call on wren, got %v", text, got)
		}
	}
	for _, text := range []string{"让 coder 写", "没有点名"} {
		if got := bus.MentionedBotsIn("group", text); len(got) != 0 {
			t.Fatalf("%q should call on nobody, got %v", text, got)
		}
	}

	// 没设显示名的 bot 照常只按 id 匹配 / a bot without a display name still matches on its id alone
	w2 := addTestBot(t, bus, sched, "coder")
	_ = w2
	if got := bus.MentionedBotsIn("group", "coder 上"); len(got) != 1 || got[0] != "coder" {
		t.Fatalf("the id should still work, got %v", got)
	}
}
