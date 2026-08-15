package engine

import (
	"botbureau/backend/internal/config"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 逐格验四档 × 动作类型。这张表就是权限系统的全部承诺，改动它等于改安全边界。
// The full tier × action matrix. This table is the whole promise of the permission system; changing it
// changes the security boundary.
func TestPermissionMatrix(t *testing.T) {
	readBash := config.ToolAct{Kind: config.ActBash, ReadOnly: true}
	writeBash := config.ToolAct{Kind: config.ActBash}
	escapingBash := config.ToolAct{Kind: config.ActBash, Escapes: true}
	write := config.ToolAct{Kind: config.ActWrite}
	readPlugin := config.ToolAct{Kind: config.ActPlugin, ReadOnly: true}
	writePlugin := config.ToolAct{Kind: config.ActPlugin}

	cases := []struct {
		level config.PermLevel
		act   config.ToolAct
		ask   bool
		what  string
	}{
		{config.PermAsk, readBash, false, "ask/read-only command"},
		{config.PermAsk, write, true, "ask/file write"},
		{config.PermAsk, writeBash, true, "ask/ordinary command"},
		{config.PermAsk, writePlugin, true, "ask/writing plugin"},

		{config.PermEdit, write, false, "edit/file write"},
		{config.PermEdit, writeBash, true, "edit/ordinary command"},
		{config.PermEdit, escapingBash, true, "edit/escaping command"},
		{config.PermEdit, writePlugin, true, "edit/writing plugin"},

		{config.PermAuto, write, false, "auto/file write"},
		{config.PermAuto, writeBash, false, "auto/ordinary command"},
		{config.PermAuto, escapingBash, true, "auto/escaping command"},
		{config.PermAuto, writePlugin, true, "auto/writing plugin"},

		{config.PermFull, write, false, "full/file write"},
		{config.PermFull, escapingBash, false, "full/escaping command"},
		{config.PermFull, writePlugin, false, "full/writing plugin"},

		// 只读一律放行，与档位无关 / read-only always passes, at every tier
		{config.PermAsk, readPlugin, false, "ask/read-only plugin"},
		{config.PermFull, readBash, false, "full/read-only command"},
	}
	for _, c := range cases {
		if got := c.level.NeedsApproval(c.act); got != c.ask {
			t.Errorf("%s: want approval=%v, got %v", c.what, c.ask, got)
		}
	}
}

// 非法或空的档位绝不能落到 full 上——配置写错的后果必须是"更严"，不是"全放开"。
// An invalid or empty tier must never resolve to full: a misconfiguration has to fail closed, not open.
func TestPermissionFallsBackClosed(t *testing.T) {
	for _, bad := range []string{"", "  ", "yolo", "FULL ACCESS", "admin", "true"} {
		if got := config.NormalizePerm(bad); got != "" {
			t.Fatalf("NormalizePerm(%q) should be empty, got %q", bad, got)
		}
		if got := config.ResolvePerm(bad, bad); got != config.DefaultPerm {
			t.Fatalf("both levels invalid should fall back to %q, got %q", config.DefaultPerm, got)
		}
		if got := config.ResolvePerm(bad, bad); got == config.PermFull {
			t.Fatal("an invalid value must never land on full")
		}
	}
	if config.DefaultPerm != config.PermAsk {
		t.Fatalf("the default tier must be the most conservative, ask, got %q", config.DefaultPerm)
	}
	// 大小写和空格要认，用户手写 yaml 时容易带上
	// Case and padding are accepted; hand-written YAML often carries them
	if config.NormalizePerm("  AUTO ") != config.PermAuto {
		t.Fatal("case and padding should be tolerated")
	}
}

// bot 自己设的档位压过全局；没设才跟随全局。
// A bot's own tier overrides the global one; only an unset bot follows it.
func TestPermissionPrecedence(t *testing.T) {
	if got := config.ResolvePerm("ask", "full"); got != config.PermAsk {
		t.Fatalf("a bot's own tier should beat the global one, got %q", got)
	}
	if got := config.ResolvePerm("", "auto"); got != config.PermAuto {
		t.Fatalf("an unset bot should follow the global tier, got %q", got)
	}
	if got := config.ResolvePerm("", ""); got != config.DefaultPerm {
		t.Fatalf("neither set should be the default, got %q", got)
	}
}

// 端到端：auto 档下工作目录内的命令直接跑；同一个 bot 碰到越界命令仍要等审批。
// End to end: under auto a workspace command runs straight through, while the same bot still waits for
// approval on a command that reaches outside.
func TestAutoTierRunsSandboxedCommandButGatesEscape(t *testing.T) {
	dir := t.TempDir()
	bus := NewBus()
	sched := NewScheduler(bus, filepath.Join(dir, "routines.json"))
	deps := newTestDeps(t, dir)
	deps.Settings.SetPermission(string(config.PermAuto))

	w, err := NewBotWorker(config.BotConfig{Name: "worker", Role: "test", Provider: "fake"}, bus, sched, dir, deps)
	if err != nil {
		t.Fatal(err)
	}
	bus.Register(w)
	tb := w.toolbox
	tb.currentChat = "group"

	// 工作目录内：不产生审批，直接出结果
	// Inside the workspace: no approval is raised and the result comes back directly
	out, isErr := tb.Execute("bash", map[string]any{"command": "echo hello > note.txt && cat note.txt"})
	if isErr {
		t.Fatalf("a workspace command must not fail under auto: %s", out)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("the command never actually ran: %q", out)
	}
	if n := len(bus.PendingApprovals()); n != 0 {
		t.Fatalf("a workspace command must raise no approval under auto, got %d", n)
	}

	// 越界：必须挂起等人。这里不批，验证它确实在等。
	// Escaping: must suspend. We never approve, proving it really waits.
	done := make(chan string, 1)
	go func() {
		res, _ := tb.Execute("bash", map[string]any{"command": "cat /etc/hosts"})
		done <- res
	}()
	deadline := time.After(2 * time.Second)
	for {
		if len(bus.PendingApprovals()) > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("an escaping command ran without approval under auto")
		case res := <-done:
			t.Fatalf("an escaping command returned without waiting: %s", res)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	ap := bus.PendingApprovals()[0]
	bus.Decide(ap.ID, false, "test rejection")
	if res := <-done; !strings.Contains(res, "reject") {
		t.Fatalf("a rejection should come back explained: %s", res)
	}
}

// full 档下写文件不再有审批；同一个 bot 降回 ask 就立刻恢复审批（改档位不需要重启）。
// Under full a file write raises no approval; dropping the same bot back to ask restores it at once
// (changing the tier needs no restart).
func TestFullTierSkipsApprovalAndIsLiveReloadable(t *testing.T) {
	dir := t.TempDir()
	bus := NewBus()
	sched := NewScheduler(bus, filepath.Join(dir, "routines.json"))
	deps := newTestDeps(t, dir)
	deps.Settings.SetPermission(string(config.PermFull))

	w, err := NewBotWorker(config.BotConfig{Name: "worker", Role: "test", Provider: "fake"}, bus, sched, dir, deps)
	if err != nil {
		t.Fatal(err)
	}
	bus.Register(w)
	tb := w.toolbox
	tb.currentChat = "group"

	if out, isErr := tb.Execute("write_file", map[string]any{"path": "a.txt", "content": "x"}); isErr {
		t.Fatalf("a file write must not fail under full: %s", out)
	}
	if n := len(bus.PendingApprovals()); n != 0 {
		t.Fatalf("there must be no approvals under full, got %d", n)
	}

	// 现场把全局降回 ask，同一个 toolbox 立刻应该重新要审批
	// Drop the global tier back to ask; the same toolbox must start asking again immediately
	deps.Settings.SetPermission(string(config.PermAsk))
	go tb.Execute("write_file", map[string]any{"path": "b.txt", "content": "y"})
	deadline := time.After(2 * time.Second)
	for len(bus.PendingApprovals()) == 0 {
		select {
		case <-deadline:
			t.Fatal("a file write still skipped approval after dropping back to ask")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	bus.Decide(bus.PendingApprovals()[0].ID, false, "")
}

func TestPermOptionsCoverEveryTier(t *testing.T) {
	opts := config.PermOptions()
	if len(opts) != 4 {
		t.Fatalf("there should be 4 options, got %d", len(opts))
	}
	seen := map[string]bool{}
	for _, o := range opts {
		id, _ := o["id"].(string)
		if !config.ValidPerm(id) {
			t.Fatalf("invalid option id: %q", id)
		}
		if o["label"] == "" || o["note"] == "" {
			t.Fatalf("%s is missing a label or note", id)
		}
		seen[id] = true
	}
	for _, want := range []string{"ask", "edit", "auto", "full"} {
		if !seen[want] {
			t.Fatalf("the options are missing %s", want)
		}
	}
}
