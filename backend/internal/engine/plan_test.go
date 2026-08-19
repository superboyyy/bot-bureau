package engine

import (
	"botbureau/backend/internal/config"

	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func decideFirstApproval(t *testing.T, bus *Bus, approved bool, reason string) <-chan *Approval {
	t.Helper()
	ch := make(chan *Approval, 1)
	go func() {
		deadline := time.After(2 * time.Second)
		for {
			select {
			case <-deadline:
				ch <- nil
				return
			default:
				if aps := bus.PendingApprovals(); len(aps) > 0 {
					ap := aps[0]
					ch <- ap
					bus.Decide(ap.ID, approved, reason)
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
	}()
	return ch
}

func TestSubmitPlanApproveContinues(t *testing.T) {
	config.SetApprovalTimeout(2 * time.Second)
	t.Cleanup(func() { config.SetApprovalTimeout(0) })
	w, bus, _ := newTestWorker(t, "a", nil)
	tb := w.toolbox
	tb.currentChat = "dm"
	pending := decideFirstApproval(t, bus, true, "")
	out, _, isErr := tb.Execute("submit_plan", map[string]any{
		"title": "Split auth",
		"body":  "1. edit a.go\n2. edit b.go",
	})
	ap := <-pending
	if ap == nil {
		t.Fatal("submit_plan must wait for a human")
	}
	if ap.Kind != "plan" || ap.Title != "Split auth" || !strings.Contains(ap.Body, "edit a.go") {
		t.Fatalf("plan card fields: %#v", ap)
	}
	if isErr || !strings.Contains(out, "accepted") {
		t.Fatalf("approve should continue the turn: %q %v", out, isErr)
	}
}

func TestSubmitPlanRejectReturnsReason(t *testing.T) {
	config.SetApprovalTimeout(2 * time.Second)
	t.Cleanup(func() { config.SetApprovalTimeout(0) })
	w, bus, _ := newTestWorker(t, "a", nil)
	tb := w.toolbox
	tb.currentChat = "group"
	pending := decideFirstApproval(t, bus, false, "too broad")
	out, _, isErr := tb.Execute("submit_plan", map[string]any{
		"title": "Rewrite everything",
		"body":  "touch every file",
	})
	ap := <-pending
	if ap == nil || ap.Kind != "plan" || ap.Chat != "group" {
		t.Fatalf("expected a group plan card, got %#v", ap)
	}
	if !isErr || !strings.Contains(out, "rejected") || !strings.Contains(out, "too broad") {
		t.Fatalf("reject should be an error the model can revise from: %q %v", out, isErr)
	}
}

func TestSubmitPlanWaitsOnAutoTier(t *testing.T) {
	config.SetApprovalTimeout(2 * time.Second)
	t.Cleanup(func() { config.SetApprovalTimeout(0) })
	w, bus, _ := newTestWorker(t, "a", nil)
	tb := w.toolbox
	tb.currentChat = "dm"
	tb.botPerm = string(config.PermAuto)

	done := make(chan string, 1)
	go func() {
		out, _, isErr := tb.Execute("write_file", map[string]any{"path": "x.txt", "content": "hi\n"})
		if isErr {
			done <- "write err: " + out
			return
		}
		done <- "ok"
	}()
	select {
	case got := <-done:
		if got != "ok" {
			t.Fatal(got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("write_file under auto should not wait on a human")
	}
	if n := len(bus.PendingApprovals()); n != 0 {
		t.Fatalf("write_file should not leave a pending approval, got %d", n)
	}
	got, err := os.ReadFile(filepath.Join(w.workspace, "x.txt"))
	if err != nil || string(got) != "hi\n" {
		t.Fatalf("auto write should land: %q %v", got, err)
	}

	pending := decideFirstApproval(t, bus, true, "")
	out, _, isErr := tb.Execute("submit_plan", map[string]any{
		"title": "Edit two files",
		"body":  "x.txt and y.txt",
	})
	ap := <-pending
	if ap == nil || ap.Kind != "plan" {
		t.Fatalf("auto tier must still wait on submit_plan, got %#v", ap)
	}
	if isErr {
		t.Fatalf("approved plan: %q", out)
	}
}

func TestSubmitPlanNeedsTitleAndBody(t *testing.T) {
	w, bus, _ := newTestWorker(t, "a", nil)
	tb := w.toolbox
	out, _, isErr := tb.Execute("submit_plan", map[string]any{"title": "x", "body": "  "})
	if !isErr || !strings.Contains(out, "title") {
		t.Fatalf("empty body should fail closed: %q %v", out, isErr)
	}
	if n := len(bus.PendingApprovals()); n != 0 {
		t.Fatalf("a missing body must not raise a card, got %d", n)
	}
}
