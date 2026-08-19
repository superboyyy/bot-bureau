package engine

import (
	"botbureau/backend/internal/config"
	"botbureau/backend/internal/model"

	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResetChatArchivesAndLeavesMemory(t *testing.T) {
	dataDir := t.TempDir()
	bus := NewBus()
	sched := NewScheduler(bus, filepath.Join(dataDir, "routines.json"))
	w, err := NewBotWorker(config.BotConfig{Name: "a", Role: "r", Provider: "fake"}, bus, sched, dataDir, newTestDeps(t, dataDir))
	if err != nil {
		t.Fatal(err)
	}
	bus.Register(w)

	memPath := filepath.Join(w.Workspace(), "MEMORY.md")
	if err := os.WriteFile(memPath, []byte("- keep this memory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.handle(Msg{Sender: "user", Content: "hello persist", Chat: "dm", Respond: true})
	if !strings.Contains(string(w.session("dm").Snapshot()), "hello persist") {
		t.Fatal("precondition: the session should hold the user text")
	}

	w.ResetChat("dm")

	matches, err := filepath.Glob(filepath.Join(w.Workspace(), "sessions-*.json"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected one sessions archive, got %v (%v)", matches, err)
	}
	archived, err := os.ReadFile(matches[0])
	if err != nil || !strings.Contains(string(archived), "hello persist") {
		t.Fatalf("the archive should keep the old session: %s", archived)
	}
	live, _ := os.ReadFile(w.sessionsPath())
	if strings.Contains(string(live), "hello persist") {
		t.Fatalf("the live sessions file should not keep the old chat: %s", live)
	}
	if strings.Contains(string(w.session("dm").Snapshot()), "hello persist") {
		t.Fatal("the new session should be empty of the old user text")
	}
	got, err := os.ReadFile(memPath)
	if err != nil || !strings.Contains(string(got), "keep this memory") {
		t.Fatalf("MEMORY.md must stay: %s", got)
	}
	saw := false
	for _, ev := range bus.Recent(20) {
		text, _ := ev["text"].(string)
		if ev["kind"] == "system" && strings.Contains(text, "New conversation started") {
			saw = true
		}
	}
	if !saw {
		t.Fatal("reset should announce itself in the chat log")
	}
}

func TestAutoContinueOnceOnMaxTokens(t *testing.T) {
	p := &scriptedProvider{script: []model.StepResult{
		{StopReason: "max_tokens", Texts: []string{"part-one"}},
		{StopReason: "end_turn", Texts: []string{"part-two"}},
	}}
	w, bus, _ := newTestWorker(t, "a", p)
	sess := w.session("dm")
	sess.MarkTurn()
	sess.AddUser("write a long thing")
	w.agentLoop(context.Background(), "dm", sess, Msg{Sender: "user", Content: "write a long thing", Chat: "dm", Respond: true})

	var msgs []string
	continued := false
	for _, ev := range bus.Recent(50) {
		text, _ := ev["text"].(string)
		switch ev["kind"] {
		case "msg":
			msgs = append(msgs, text)
		case "tool":
			if strings.Contains(text, "continuing once") {
				continued = true
			}
		}
	}
	joined := strings.Join(msgs, "\n")
	if !strings.Contains(joined, "part-one") || !strings.Contains(joined, "part-two") {
		t.Fatalf("both halves should be posted: %q", joined)
	}
	if strings.Contains(joined, "tell me to continue") {
		t.Fatalf("a successful continue should not ask the user: %q", joined)
	}
	if !continued {
		t.Fatal("the UI should see a tool-trace line for the auto-continue")
	}
	if len(p.script) != 0 {
		t.Fatalf("both scripted steps should have been consumed, leftover %d", len(p.script))
	}
}

func TestAutoContinueStopsAfterOne(t *testing.T) {
	p := &scriptedProvider{script: []model.StepResult{
		{StopReason: "max_tokens", Texts: []string{"part-one"}},
		{StopReason: "max_tokens", Texts: []string{"part-two"}},
		{StopReason: "end_turn", Texts: []string{"part-three"}},
	}}
	w, bus, _ := newTestWorker(t, "a", p)
	sess := w.session("dm")
	sess.MarkTurn()
	sess.AddUser("write a long thing")
	w.agentLoop(context.Background(), "dm", sess, Msg{Sender: "user", Content: "write a long thing", Chat: "dm", Respond: true})

	var msgs []string
	for _, ev := range bus.Recent(50) {
		if ev["kind"] == "msg" {
			text, _ := ev["text"].(string)
			msgs = append(msgs, text)
		}
	}
	joined := strings.Join(msgs, "\n")
	if !strings.Contains(joined, "part-one") || !strings.Contains(joined, "part-two") {
		t.Fatalf("the first two halves should be posted: %q", joined)
	}
	if strings.Contains(joined, "part-three") {
		t.Fatalf("a second truncation must not take another step: %q", joined)
	}
	if !strings.Contains(joined, "tell me to continue") {
		t.Fatalf("after one auto-continue the user should be asked to continue: %q", joined)
	}
	if len(p.script) != 1 {
		t.Fatalf("the third scripted step should remain, leftover %d", len(p.script))
	}
}
