package engine

import (
	"botbureau/backend/internal/config"

	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readAudit(t *testing.T, dataDir string) []auditLine {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dataDir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []auditLine
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var rec auditLine
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("audit line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

func TestAuditRejectedWriteRecordsDenyAndPath(t *testing.T) {
	dataDir := t.TempDir()
	bus := NewBus()
	sched := NewScheduler(bus, filepath.Join(dataDir, "routines.json"))
	deps := newTestDeps(t, dataDir)
	w, err := NewBotWorker(config.BotConfig{Name: "a", Role: "eval", Provider: "fake"}, bus, sched, dataDir, deps)
	if err != nil {
		t.Fatal(err)
	}
	bus.Register(w)
	tb := w.toolbox
	tb.currentChat = "dm"

	done := make(chan string, 1)
	go func() {
		out, _, _ := tb.Execute("write_file", map[string]any{"path": "secret.txt", "content": "nope"})
		done <- out
	}()
	deadline := time.After(2 * time.Second)
	for len(bus.PendingApprovals()) == 0 {
		select {
		case <-deadline:
			t.Fatal("write should wait for approval")
		case out := <-done:
			t.Fatalf("write returned without waiting: %s", out)
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	bus.Decide(bus.PendingApprovals()[0].ID, false, "not this file")
	if res := <-done; !strings.Contains(res, "reject") {
		t.Fatalf("rejection should come back explained: %s", res)
	}
	if _, err := os.Stat(filepath.Join(w.workspace, "secret.txt")); !os.IsNotExist(err) {
		t.Fatal("a rejected write must not create the file")
	}

	recs := readAudit(t, dataDir)
	if len(recs) != 1 {
		t.Fatalf("want one audit line, got %#v", recs)
	}
	if recs[0].Act != "write" || recs[0].Path != "secret.txt" || recs[0].Allowed || recs[0].Bot != "a" {
		t.Fatalf("denied write: %#v", recs[0])
	}
	if recs[0].ID == 0 {
		t.Fatal("the approval id belongs on the line")
	}
}

func TestAuditApproveRunsEditedBashCommand(t *testing.T) {
	dataDir := t.TempDir()
	bus := NewBus()
	sched := NewScheduler(bus, filepath.Join(dataDir, "routines.json"))
	deps := newTestDeps(t, dataDir)
	w, err := NewBotWorker(config.BotConfig{Name: "a", Role: "eval", Provider: "fake"}, bus, sched, dataDir, deps)
	if err != nil {
		t.Fatal(err)
	}
	bus.Register(w)
	tb := w.toolbox
	tb.currentChat = "dm"

	done := make(chan string, 1)
	go func() {
		out, _, isErr := tb.Execute("bash", map[string]any{"command": "touch original-flag.txt"})
		if isErr {
			done <- "err:" + out
			return
		}
		done <- out
	}()
	deadline := time.After(2 * time.Second)
	for len(bus.PendingApprovals()) == 0 {
		select {
		case <-deadline:
			t.Fatal("bash should wait for approval")
		case out := <-done:
			t.Fatalf("bash returned without waiting: %s", out)
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	ap := bus.PendingApprovals()[0]
	if ap.Command != "touch original-flag.txt" {
		t.Fatalf("card should carry the original command: %#v", ap)
	}
	if !bus.DecideCmd(ap.ID, true, "", "touch edited-flag.txt") {
		t.Fatal("decide")
	}
	if res := <-done; strings.HasPrefix(res, "err:") {
		t.Fatalf("edited command failed: %s", res)
	}
	if _, err := os.Stat(filepath.Join(w.workspace, "edited-flag.txt")); err != nil {
		t.Fatalf("the edited command should have run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(w.workspace, "original-flag.txt")); !os.IsNotExist(err) {
		t.Fatal("the original command must not run")
	}

	recs := readAudit(t, dataDir)
	if len(recs) != 1 || recs[0].Act != "bash" || !recs[0].Allowed {
		t.Fatalf("allowed bash: %#v", recs)
	}
	if recs[0].Command != "touch edited-flag.txt" || recs[0].Original != "touch original-flag.txt" {
		t.Fatalf("audit should keep both strings: %#v", recs[0])
	}
}
