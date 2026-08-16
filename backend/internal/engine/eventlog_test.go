package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loggedBus(t *testing.T) (*Bus, string) {
	t.Helper()
	dir := t.TempDir()
	b := NewBus()
	b.EnableEventLog(filepath.Join(dir, "events.json"))
	return b, dir
}

// Messages do not disappear because a lot of them piled up. That is the only reason this store exists.
func TestMessagesSurviveFarPastTheMemoryWindow(t *testing.T) {
	b, _ := loggedBus(t)

	// Far past the memory window, mixed with the tool logs that in real use crowd messages out
	const msgs = 500
	for i := 0; i < msgs; i++ {
		b.Emit("msg", "group", "user", fmt.Sprintf("message %d", i), nil)
		for j := 0; j < 12; j++ {
			b.Emit("tool", "group", "bot", "$ ls", nil)
		}
	}
	if len(b.Recent(100000)) >= msgs*13 {
		t.Fatal("内存本来就该只留一截 / memory was supposed to keep only a tail")
	}

	// Page back until the start of the conversation
	seen := map[string]bool{}
	before, pages := 0, 0
	for {
		evs, more := b.History("group", before, 100)
		if len(evs) == 0 {
			break
		}
		for _, ev := range evs {
			if ev["kind"] == "msg" {
				seen[ev["text"].(string)] = true
			}
		}
		before = evs[0]["id"].(int)
		if pages++; !more || pages > 200 {
			break
		}
	}
	for i := 0; i < msgs; i++ {
		if !seen[fmt.Sprintf("message %d", i)] {
			t.Fatalf("message %d 翻不到了 / can no longer be reached (found %d of %d)", i, len(seen), msgs)
		}
	}
}

// The sidebar must still know about a conversation after its events fall out of the recent in-memory
// window. This is deliberately checked both before and after a restart: the index is a view of the
// durable log, not a lucky consequence of the chat pane retaining those messages.
func TestConversationPreviewsAreIndependentOfHistoryWindow(t *testing.T) {
	b, dir := loggedBus(t)
	b.Emit("msg", "dm:old", "user", "first", nil)
	b.Emit("msg", "dm:old", "user", "latest", nil)
	for i := 0; i < memEvents*2+100; i++ {
		b.Emit("tool", "group", "bot", fmt.Sprintf("step %d", i), nil)
	}

	find := func(b *Bus, chat string) (ConversationPreview, bool) {
		for _, p := range b.ConversationPreviews() {
			if p.Chat == chat {
				return p, true
			}
		}
		return ConversationPreview{}, false
	}

	p, ok := find(b, "dm:old")
	if !ok || p.Text != "latest" || p.Source != "user" {
		t.Fatalf("sidebar preview was lost from the live index: %+v, found=%v", p, ok)
	}

	restarted := NewBus()
	restarted.EnableEventLog(filepath.Join(dir, "events.json"))
	p, ok = find(restarted, "dm:old")
	if !ok || p.Text != "latest" {
		t.Fatalf("sidebar preview was not rebuilt from the log: %+v, found=%v", p, ok)
	}
}

// Compaction touches the activity log only and never a message.
func TestCompactionKeepsEveryMessage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	log := newEventLog(path)
	for i := 0; i < keepActivity*3; i++ {
		log.append(Event{"id": i, "kind": "tool", "chat": "group", "text": "$ ls"})
		if i%10 == 0 {
			log.append(Event{"id": 1000000 + i, "kind": "msg", "chat": "group", "text": fmt.Sprintf("m%d", i)})
		}
	}
	if !log.compact() {
		t.Fatal("这么多活动日志该整理了 / this much activity should have been compacted")
	}
	msgs, tools := 0, 0
	_ = log.scan(func(ev Event) {
		if ev["kind"] == "msg" {
			msgs++
		} else {
			tools++
		}
	})
	if want := keepActivity * 3 / 10; msgs != want {
		t.Fatalf("消息一条都不该少 / not one message may go: %d != %d", msgs, want)
	}
	if tools > keepActivity {
		t.Fatalf("活动日志该被削到 %d 以内 / activity should have come down to %d: %d", keepActivity, keepActivity, tools)
	}
}

// Deleting the conversation is a message's only way out, and it has to be real.
func TestDeleteChatReallyDeletes(t *testing.T) {
	b, _ := loggedBus(t)
	b.Emit("msg", "group", "user", "in the group", nil)
	b.Emit("msg", "dm:a", "user", "in the dm", nil)
	b.Emit("msg", "group", "user", "also the group", nil)

	if n := b.DeleteChat("group"); n != 2 {
		t.Fatalf("should have removed both group entries, removed %d", n)
	}
	if evs, _ := b.History("group", 0, 100); len(evs) != 0 {
		t.Fatalf("群聊记录该没了 / the group's history should be gone: %v", evs)
	}

	// Other conversations are untouched
	evs, _ := b.History("dm:a", 0, 100)
	if len(evs) != 1 || evs[0]["text"] != "in the dm" {
		t.Fatalf("the DM should be untouched: %v", evs)
	}
	for _, ev := range b.Recent(100) {
		if ev["chat"] == "group" {
			t.Fatal("内存里也该清掉 / memory should have been cleared too")
		}
	}
}

// Somebody upgrading must not lose the history they already had because the format changed.
func TestMigrationFromTheOldArrayFile(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "events.json")
	raw, _ := json.Marshal([]Event{
		{"id": 1, "kind": "msg", "chat": "group", "text": "从前说过的话"},
		{"id": 2, "kind": "msg", "chat": "group", "text": "还有这一句"},
	})
	if err := os.WriteFile(old, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	b := NewBus()
	b.EnableEventLog(old)
	evs, _ := b.History("group", 0, 100)
	if len(evs) != 2 || evs[0]["text"] != "从前说过的话" {
		t.Fatalf("migration lost the old history: %v", evs)
	}

	// New events continue the old ids rather than restarting
	if ev := b.Emit("msg", "group", "user", "新说的", nil); ev["id"].(int) <= 2 {
		t.Fatalf("ids should continue past the migrated ones: %v", ev["id"])
	}

	// The old file is kept, so the original is still there if the move went wrong
	if _, err := os.Stat(old + ".migrated"); err != nil {
		t.Fatalf("the old file should have been kept aside: %v", err)
	}
}

// The last line of an append-only log can be torn by a power cut, and that must not condemn the lot.
func TestATornLastLineDoesNotLoseTheRest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	log := newEventLog(path)
	log.append(Event{"id": 1, "kind": "msg", "chat": "group", "text": "good one"})
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(`{"id":2,"kind":"msg","chat":"group","te`)
	f.Close()

	evs, _ := log.page("group", 0, 10)
	if len(evs) != 1 || evs[0]["text"] != "good one" {
		t.Fatalf("the intact entries should still be there: %v", evs)
	}
}

// A very long message — someone pasted a wall of text — has to be readable again
func TestVeryLongEntriesSurviveTheRoundTrip(t *testing.T) {
	b, _ := loggedBus(t)
	long := strings.Repeat("很长的一段话。", 20000)
	b.Emit("msg", "group", "user", long, nil)
	evs, _ := b.History("group", 0, 10)
	if len(evs) != 1 || evs[0]["text"] != long {
		t.Fatalf("a long entry did not survive: got %d entries", len(evs))
	}
}
