package engine

import (
	"botbureau/backend/internal/config"
	"botbureau/backend/internal/sandbox"

	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeRunner struct {
	name  string
	avail bool
	net   bool
	fn    func(ctx context.Context, spec sandbox.Spec) (*exec.Cmd, error)
}

func (f fakeRunner) Name() string          { return f.name }
func (f fakeRunner) Available() bool       { return f.avail }
func (f fakeRunner) IsolatesNetwork() bool { return f.net }
func (f fakeRunner) Command(ctx context.Context, spec sandbox.Spec) (*exec.Cmd, error) {
	if f.fn != nil {
		return f.fn(ctx, spec)
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", spec.Command)
	cmd.Dir = spec.Dir
	return cmd, nil
}

func sandboxWorker(t *testing.T, r sandbox.Runner, perm config.PermLevel) (*Toolbox, *Bus, string) {
	t.Helper()
	dir := t.TempDir()
	bus := NewBus()
	sched := NewScheduler(bus, filepath.Join(dir, "routines.json"))
	deps := newTestDeps(t, dir)
	deps.Sandbox = r
	deps.Settings.SetPermission(string(perm))
	w, err := NewBotWorker(config.BotConfig{Name: "worker", Role: "test", Provider: "fake"}, bus, sched, dir, deps)
	if err != nil {
		t.Fatal(err)
	}
	bus.Register(w)
	tb := w.toolbox
	tb.currentChat = "group"
	return tb, bus, dir
}

func waitPending(t *testing.T, bus *Bus, done <-chan string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for len(bus.PendingApprovals()) == 0 {
		select {
		case <-deadline:
			t.Fatal("expected an approval")
		case res := <-done:
			t.Fatalf("returned without waiting: %s", res)
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
}

func TestIsolatedAskStillAsksWithoutAutoAllow(t *testing.T) {
	tb, bus, _ := sandboxWorker(t, fakeRunner{name: "test", avail: true}, config.PermAsk)
	done := make(chan string, 1)
	go func() {
		out, _, _ := tb.Execute("bash", map[string]any{"command": "echo hi > note.txt"})
		done <- out
	}()
	waitPending(t, bus, done)
	bus.Decide(bus.PendingApprovals()[0].ID, false, "no")
	if res := <-done; !strings.Contains(res, "reject") {
		t.Fatalf("rejection: %s", res)
	}
}

func TestIsolatedAskAutoAllowSkipsBashPrompt(t *testing.T) {
	tb, bus, _ := sandboxWorker(t, fakeRunner{name: "test", avail: true}, config.PermAsk)
	on := true
	tb.settings.SetSandbox(nil, &on, nil)
	out, _, isErr := tb.Execute("bash", map[string]any{"command": "echo hi > note.txt && cat note.txt"})
	if isErr {
		t.Fatalf("auto-allow should run: %s", out)
	}
	if !strings.Contains(out, "hi") {
		t.Fatalf("command did not run: %q", out)
	}
	if n := len(bus.PendingApprovals()); n != 0 {
		t.Fatalf("auto-allow must not raise an approval, got %d", n)
	}
}

func TestIsolatedAutoRunsEscapingCommandWithoutApproval(t *testing.T) {
	tb, bus, _ := sandboxWorker(t, fakeRunner{name: "test", avail: true}, config.PermAuto)
	out, _, isErr := tb.Execute("bash", map[string]any{"command": "cat /etc/hosts"})
	if isErr {
		t.Fatalf("isolated auto should run: %s", out)
	}
	if n := len(bus.PendingApprovals()); n != 0 {
		t.Fatalf("kernel-held bash must not ask under auto, got %d", n)
	}
	if !strings.Contains(out, "localhost") && !strings.Contains(out, "127.0.0.1") && out == "" {
		t.Fatalf("expected /etc/hosts contents, got %q", out)
	}
}

func TestUnsandboxedAlwaysAsksUnderAuto(t *testing.T) {
	tb, bus, _ := sandboxWorker(t, fakeRunner{name: "test", avail: true}, config.PermAuto)
	on := true
	tb.settings.SetSandbox(nil, &on, nil)
	done := make(chan string, 1)
	go func() {
		out, _, _ := tb.Execute("bash", map[string]any{"command": "echo hi", "unsandboxed": true})
		done <- out
	}()
	waitPending(t, bus, done)
	bus.Decide(bus.PendingApprovals()[0].ID, false, "no")
	<-done
}

func TestUnsandboxedDisabledErrors(t *testing.T) {
	tb, bus, _ := sandboxWorker(t, fakeRunner{name: "test", avail: true}, config.PermAsk)
	off := false
	tb.settings.SetSandbox(nil, nil, &off)
	out, _, isErr := tb.Execute("bash", map[string]any{"command": "echo hi", "unsandboxed": true})
	if !isErr || !strings.Contains(out, "Unsandboxed commands are disabled") {
		t.Fatalf("want disabled error, got %q err=%v", out, isErr)
	}
	if n := len(bus.PendingApprovals()); n != 0 {
		t.Fatalf("disabled hatch must not raise an approval, got %d", n)
	}
}

func TestDisabledSandboxKeepsHeuristicEscape(t *testing.T) {
	tb, bus, _ := sandboxWorker(t, fakeRunner{name: "test", avail: true}, config.PermAuto)
	off := false
	tb.settings.SetSandbox(&off, nil, nil)
	done := make(chan string, 1)
	go func() {
		out, _, _ := tb.Execute("bash", map[string]any{"command": "cat /etc/hosts"})
		done <- out
	}()
	waitPending(t, bus, done)
	bus.Decide(bus.PendingApprovals()[0].ID, false, "no")
	<-done
}

func TestAuditIsolateWorkspaceAndHost(t *testing.T) {
	tb, _, dir := sandboxWorker(t, fakeRunner{name: "test", avail: true}, config.PermAuto)
	if out, _, isErr := tb.Execute("bash", map[string]any{"command": "echo hi"}); isErr {
		t.Fatal(out)
	}
	recs := readAudit(t, dir)
	if len(recs) == 0 || recs[len(recs)-1].Isolate != "workspace" {
		t.Fatalf("sandboxed bash should record isolate=workspace: %#v", recs)
	}

	tb2, bus, dir2 := sandboxWorker(t, fakeRunner{name: "test", avail: true}, config.PermAuto)
	done := make(chan string, 1)
	go func() {
		out, _, _ := tb2.Execute("bash", map[string]any{"command": "echo hi", "unsandboxed": true})
		done <- out
	}()
	waitPending(t, bus, done)
	bus.Decide(bus.PendingApprovals()[0].ID, true, "")
	<-done
	recs = readAudit(t, dir2)
	if len(recs) == 0 || recs[len(recs)-1].Isolate != "host" {
		t.Fatalf("unsandboxed bash should record isolate=host: %#v", recs)
	}
}

func TestBashDefGainsUnsandboxedWhenSandboxReady(t *testing.T) {
	tb, _, _ := sandboxWorker(t, fakeRunner{name: "test", avail: true}, config.PermAsk)
	var bash modelTool
	for _, d := range tb.Defs() {
		if d.Name == "bash" {
			bash.desc = d.Description
			bash.props = d.Properties
		}
	}
	if _, ok := bash.props["unsandboxed"]; !ok {
		t.Fatal("unsandboxed param missing while hatch is on")
	}
	if !strings.Contains(bash.desc, "OS sandbox") {
		t.Fatalf("sandbox description missing: %s", bash.desc)
	}

	tbHost, _, _ := sandboxWorker(t, sandbox.Passthrough(), config.PermAsk)
	for _, d := range tbHost.Defs() {
		if d.Name == "bash" {
			if _, ok := d.Properties["unsandboxed"]; ok {
				t.Fatal("passthrough must not advertise unsandboxed")
			}
		}
	}
}

type modelTool struct {
	desc  string
	props map[string]any
}

func TestSandboxCommandErrorHintsRetry(t *testing.T) {
	tb, bus, _ := sandboxWorker(t, fakeRunner{
		name: "test", avail: true,
		fn: func(ctx context.Context, spec sandbox.Spec) (*exec.Cmd, error) {
			return nil, context.Canceled
		},
	}, config.PermAuto)
	on := true
	tb.settings.SetSandbox(nil, &on, nil)
	out, _, isErr := tb.Execute("bash", map[string]any{"command": "echo hi"})
	if !isErr || !strings.Contains(out, "unsandboxed=true") {
		t.Fatalf("construction error should hint retry, got %q err=%v", out, isErr)
	}
	if n := len(bus.PendingApprovals()); n != 0 {
		t.Fatalf("got %d approvals", n)
	}
}

func TestOSSandboxEngineWriteDeniedHintsUnsandboxed(t *testing.T) {
	r := sandbox.Detect()
	if !r.Available() {
		t.Skip("no OS sandbox backend: " + r.Name())
	}
	if os.Geteuid() == 0 {
		t.Skip("root can write outside the sandbox")
	}
	tb, bus, _ := sandboxWorker(t, r, config.PermAuto)
	on := true
	tb.settings.SetSandbox(nil, &on, nil)
	out, _, isErr := tb.Execute("bash", map[string]any{"command": "echo pwned > /etc/botbureau-sandbox-probe"})
	if !isErr {
		t.Fatalf("write outside should fail, output %q", out)
	}
	if n := len(bus.PendingApprovals()); n != 0 {
		t.Fatalf("isolated auto must not ask, got %d", n)
	}
	if _, err := os.Stat("/etc/botbureau-sandbox-probe"); !os.IsNotExist(err) {
		t.Fatal("sandbox leaked a write to /etc")
	}
	if !strings.Contains(out, "unsandboxed=true") {
		t.Fatalf("denial should hint unsandboxed retry: %s", out)
	}
}
