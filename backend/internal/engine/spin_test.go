package engine

import (
	"botbureau/backend/internal/config"
	"botbureau/backend/internal/model"

	"context"
	"path/filepath"
	"strings"
	"testing"
)

// Spinning gets cut: the same tool with the same arguments five times over cannot come back any
// different however many more laps it runs. The auto tier runs unattended, and that burns the user's
// quota.
func TestSpinningOnTheSameCallStopsTheTurn(t *testing.T) {
	dir := t.TempDir()
	bus := NewBus()
	sched := NewScheduler(bus, filepath.Join(dir, "routines.json"))
	deps := newTestDeps(t, dir)

	// The same read_file over and over, not one argument changed: what a stuck model looks like
	var script []model.StepResult
	for i := 0; i < 20; i++ {
		script = append(script, model.StepResult{StopReason: "tool_use", ToolCalls: []model.ToolCall{
			{ID: "t", Name: "read_file", Input: map[string]any{"path": "nope.txt"}},
		}})
	}
	sess := &scriptedSession{script: &script}
	w, err := NewBotWorker(config.BotConfig{Name: "a", Role: "test", Provider: "fake"}, bus, sched, dir, deps)
	if err != nil {
		t.Fatal(err)
	}
	bus.Register(w)
	w.toolbox.currentChat = "dm"
	w.agentLoop(context.Background(), "dm", sess, Msg{Sender: "user", Chat: "dm"})

	// Script left unconsumed: it stopped at the fifth, rather than running all twenty laps
	if used := 20 - len(script); used != config.MaxRepeatedCalls {
		t.Fatalf("should have stopped after %d identical calls, made %d", config.MaxRepeatedCalls, used)
	}

	// Stopping is explained, not silent
	var said string
	for _, ev := range bus.Recent(50) {
		if ev["kind"] == "msg" {
			said, _ = ev["text"].(string)
		}
	}
	if !strings.Contains(said, "read_file") {
		t.Fatalf("the line should name the tool that got stuck: %q", said)
	}

	// The tool_results still went in: a tool_use left dangling invalidates the whole history and the
	// next message would have nothing usable to sit on
	if len(sess.toolResults) != config.MaxRepeatedCalls {
		t.Fatalf("every executed call needs its result: %d", len(sess.toolResults))
	}
}

// Changed arguments are progress and must not read as spinning.
func TestVaryingCallsAreNotSpinning(t *testing.T) {
	var r repeatWatch
	for i, path := range []string{"a", "b", "a", "b", "a", "b", "a", "b"} {
		if n := r.saw(model.ToolCall{Name: "read_file", Input: map[string]any{"path": path}}); n >= config.MaxRepeatedCalls {
			t.Fatalf("alternating arguments should not count as repeats (step %d, count %d)", i, n)
		}
	}

	// Only the same tool with the same arguments counts, and any other call in between resets it
	same := model.ToolCall{Name: "bash", Input: map[string]any{"command": "ls"}}
	for i := 1; i <= 4; i++ {
		if n := r.saw(same); n != i {
			t.Fatalf("consecutive identical calls should accumulate: %d != %d", n, i)
		}
	}
	r.saw(model.ToolCall{Name: "bash", Input: map[string]any{"command": "pwd"}})
	if n := r.saw(same); n != 1 {
		t.Fatalf("a different call in between should reset the count, got %d", n)
	}
}

// A line in the room that did not name you has to say so when it enters your context.

// This actually happened: the user asked something in the group, the engine handed it to exactly one
// member as it should, and another was mid-task at the time — pumpInbox merged the line into their
// context, where it rendered identically to a genuine user question, so they dropped what they were
// doing and answered it. One question, two answers, one of them guessed.

// Respond already answers "is this for you" at delivery time; that answer has to survive all the way to
// the model.
func TestBackgroundMessageSaysItIsNotForYou(t *testing.T) {
	dir := t.TempDir()
	bus := NewBus()
	sched := NewScheduler(bus, filepath.Join(dir, "routines.json"))
	deps := newTestDeps(t, dir)
	w, err := NewBotWorker(config.BotConfig{Name: "a", Role: "test", Provider: "fake"}, bus, sched, dir, deps)
	if err != nil {
		t.Fatal(err)
	}

	background := w.renderMsg(Msg{Sender: "user", Chat: "group", Respond: false, Content: "这是什么限制？"})
	if !strings.Contains(background, "这是什么限制？") {
		t.Fatalf("the text itself must still be there: %q", background)
	}
	if !strings.Contains(background, "was not addressed to you") {
		t.Fatalf("背景消息要自报身份 / a background message must announce itself: %q", background)
	}

	// The one addressed to you must say so: without that, an unnamed room message looks like a
	// shout into the void, and the model stays silent even though the engine gave it the turn.
	mine := w.renderMsg(Msg{Sender: "user", Chat: "group", Respond: true, Content: "这是什么限制？"})
	if strings.Contains(mine, "was not addressed to you") {
		t.Fatalf("派给你的消息不该被标成背景 / a message addressed to you must not read as background: %q", mine)
	}
	if !strings.Contains(mine, "Assigned to you") {
		t.Fatalf("派给你的群聊消息要自报身份 / an assigned group message must announce itself: %q", mine)
	}

	// A DM has no default-member ambiguity, so the assigned note would just be noise
	dm := w.renderMsg(Msg{Sender: "user", Chat: "dm", Respond: true, Content: "这是什么限制？"})
	if strings.Contains(dm, "Assigned to you") {
		t.Fatalf("私聊不需要派活标记 / a DM must not carry the group assigned note: %q", dm)
	}

	// The two notes are exclusive: background arriving mid-task says stay out of it and carry on, not the
	// interjection's take it on board — which would draw it into answering
	both := w.renderMsg(Msg{Sender: "user", Chat: "group", Respond: false, Interject: true, Content: "这是什么限制？"})
	if !strings.Contains(both, "was not addressed to you") || strings.Contains(both, "broke in while you were working") {
		t.Fatalf("背景优先于插话标记 / background must win over the interjection note: %q", both)
	}
}
