package engine

import (
	"botbureau/backend/internal/config"
	"botbureau/backend/internal/model"

	"context"
	"path/filepath"
	"strings"
	"testing"
)

// 原地打转要被掐掉：同样的工具、同样的参数连着 5 次，结果不会变，再跑多少圈都一样。
// auto 档是无人看管跑的，这种空转烧的是用户的额度。
//
// Spinning gets cut: the same tool with the same arguments five times over cannot come back any
// different however many more laps it runs. The auto tier runs unattended, and that burns the user's
// quota.
func TestSpinningOnTheSameCallStopsTheTurn(t *testing.T) {
	dir := t.TempDir()
	bus := NewBus()
	sched := NewScheduler(bus, filepath.Join(dir, "routines.json"))
	deps := newTestDeps(t, dir)

	// 一直请求同一个 read_file，参数一个字不变——模型卡住的典型样子
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

	// 剩下的脚本没跑完，说明确实是在第 5 次上停的，而不是把 20 圈跑穿
	// Script left unconsumed: it stopped at the fifth, rather than running all twenty laps
	if used := 20 - len(script); used != config.MaxRepeatedCalls {
		t.Fatalf("should have stopped after %d identical calls, made %d", config.MaxRepeatedCalls, used)
	}
	// 停下来要说明白，不能悄没声地结束
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
	// tool_result 照样补齐了：断开的 tool_use 会让整段历史作废，下一条消息就没法用了
	// The tool_results still went in: a tool_use left dangling invalidates the whole history and the
	// next message would have nothing usable to sit on
	if len(sess.toolResults) != config.MaxRepeatedCalls {
		t.Fatalf("every executed call needs its result: %d", len(sess.toolResults))
	}
}

// 参数变了就是在往前走，不该被当成打转。
// Changed arguments are progress and must not read as spinning.
func TestVaryingCallsAreNotSpinning(t *testing.T) {
	var r repeatWatch
	for i, path := range []string{"a", "b", "a", "b", "a", "b", "a", "b"} {
		if n := r.saw(model.ToolCall{Name: "read_file", Input: map[string]any{"path": path}}); n >= config.MaxRepeatedCalls {
			t.Fatalf("alternating arguments should not count as repeats (step %d, count %d)", i, n)
		}
	}
	// 同一个工具、同样的参数才算；中间插进别的调用就归零
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

// 群里没点你名的那句话，进上下文时要说清楚它不是给你的。
//
// 这是一个真实发生过的事故：用户在群里问了一句，引擎按规矩只派给了一个成员，
// 而另一位当时正在干活，pumpInbox 把这句话并进了他的上下文——渲染出来和一条正经的用户提问
// 一模一样，于是他丢下手上的任务去答了。一个问题两个答案，其中一个还是猜的。
//
// Respond 这个字段在投递时就已经回答了"这是不是给你的"，它必须一路传到模型眼前。
//
// A line in the room that did not name you has to say so when it enters your context.
//
// This actually happened: the user asked something in the group, the engine handed it to exactly one
// member as it should, and another was mid-task at the time — pumpInbox merged the line into their
// context, where it rendered identically to a genuine user question, so they dropped what they were
// doing and answered it. One question, two answers, one of them guessed.
//
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

	// 派给你的那一条不带这个标记，否则它就该沉默了
	// The one addressed to you carries no such note, or it would fall silent instead
	mine := w.renderMsg(Msg{Sender: "user", Chat: "group", Respond: true, Content: "这是什么限制？"})
	if strings.Contains(mine, "was not addressed to you") {
		t.Fatalf("派给你的消息不该被标成背景 / a message addressed to you must not read as background: %q", mine)
	}

	// 两个标记互斥：干活途中收到的背景消息说"别接话，继续手上的"，
	// 而不是插话那句"消化掉再接着干"——后者会把它引去回答
	// The two notes are exclusive: background arriving mid-task says stay out of it and carry on, not the
	// interjection's take it on board — which would draw it into answering
	both := w.renderMsg(Msg{Sender: "user", Chat: "group", Respond: false, Interject: true, Content: "这是什么限制？"})
	if !strings.Contains(both, "was not addressed to you") || strings.Contains(both, "broke in while you were working") {
		t.Fatalf("背景优先于插话标记 / background must win over the interjection note: %q", both)
	}
}
