package model

import (
	"botbureau/backend/internal/i18n"

	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func historyLines(t *testing.T, s Session) []string {
	t.Helper()
	var disk sessionDisk
	if err := json.Unmarshal(s.Snapshot(), &disk); err != nil {
		t.Fatal(err)
	}
	var lines []string
	if err := json.Unmarshal(disk.History, &lines); err == nil {
		return lines
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(disk.History, &raw); err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(raw))
	for i, h := range raw {
		out[i] = string(h)
	}
	return out
}

func TestPickTrimNeverCutsAfterMark(t *testing.T) {
	tr := &cutTracker{}
	tr.addCut(0)
	tr.addCut(3)
	tr.markTurn(6)
	tr.addCut(6)
	tr.addCut(8) // interjection inside the current turn
	n := 10
	p := tr.pickTrim(n, 2, 0, nil)
	if p == 0 {
		t.Fatal("over the message limit, a cut should be chosen")
	}
	if p > 6 {
		t.Fatalf("must not cut inside the current turn: %d", p)
	}
	if p != 6 {
		t.Fatalf("the last allowed cut is the current-turn start, got %d", p)
	}
}

func TestCompactUnderBudgetIsNoop(t *testing.T) {
	i18n.SetLocale("en")
	s := &fakeSession{}
	s.MarkTurn()
	s.AddUser("hello")
	before := string(s.Snapshot())
	s.Trim(60, 100_000)
	after := string(s.Snapshot())
	if before != after {
		t.Fatalf("history under both budgets should be untouched\n before %s\n after  %s", before, after)
	}
	if s.tracker.note != "" {
		t.Fatalf("no compact note should be stored: %q", s.tracker.note)
	}
}

func TestCompactDropsOldestTurnsAndKeepsCurrent(t *testing.T) {
	i18n.SetLocale("en")
	s := &fakeSession{}
	for i := 0; i < 6; i++ {
		s.MarkTurn()
		s.AddUser(fmt.Sprintf("OLD-TURN-%d %s", i, strings.Repeat("x", 80)))
	}
	s.MarkTurn()
	s.AddUser("KEEP-ASSIGNMENT " + strings.Repeat("y", 20))
	s.AddToolResults([]ToolResult{{ID: "t1", Content: strings.Repeat("TOOLBLOB", 8)}})
	s.Trim(60, 180)

	raw := string(s.Snapshot())
	if !strings.Contains(raw, "KEEP-ASSIGNMENT") {
		t.Fatalf("the current assignment should survive compact: %s", raw)
	}
	if !strings.Contains(raw, "TOOLBLOB") {
		t.Fatalf("the current turn's tool result should survive compact: %s", raw)
	}
	if !strings.Contains(raw, "Earlier turns were dropped") {
		t.Fatalf("a compact note should be inserted: %s", raw)
	}
	for i, line := range historyLines(t, s)[1:] {
		if strings.Contains(line, "OLD-TURN-0") {
			t.Fatalf("the oldest turn should not remain as a live message (%d): %s", i, line)
		}
	}
	if s.tracker.note == "" || !strings.Contains(s.tracker.note, "Earlier turns were dropped") {
		t.Fatalf("the tracker should remember the compact note: %q", s.tracker.note)
	}
}

func TestCompactNotePersistsAcrossRestore(t *testing.T) {
	i18n.SetLocale("en")
	s := &fakeSession{}
	for i := 0; i < 8; i++ {
		s.MarkTurn()
		s.AddUser(fmt.Sprintf("GONE-%d %s", i, strings.Repeat("z", 60)))
	}
	s.MarkTurn()
	s.AddUser("LIVE-ASSIGNMENT")
	s.Trim(4, 200)
	snap := s.Snapshot()

	var disk sessionDisk
	if err := json.Unmarshal(snap, &disk); err != nil {
		t.Fatal(err)
	}
	if disk.Note == "" {
		t.Fatal("the snapshot should store the compact note")
	}

	s2 := &fakeSession{}
	if !s2.Restore(snap) {
		t.Fatal("restore failed")
	}
	joined := string(s2.Snapshot())
	if !strings.Contains(joined, "LIVE-ASSIGNMENT") {
		t.Fatalf("the kept assignment should survive a restart: %s", joined)
	}
	for i, line := range historyLines(t, s2)[1:] {
		if strings.Contains(line, "GONE-0") {
			t.Fatalf("a restart must not resurrect dropped turns (%d): %s", i, line)
		}
	}
	if s2.tracker.note == "" {
		t.Fatal("the restored session should still carry the compact note")
	}
}

func TestCompactMessageLimitBackstop(t *testing.T) {
	i18n.SetLocale("en")
	s := &fakeSession{}
	for i := 0; i < 10; i++ {
		s.MarkTurn()
		s.AddUser(fmt.Sprintf("msg-%d", i))
	}
	s.Trim(3, 0)
	if n := len(s.history); n > 4 { // compact note + up to 3 kept messages
		t.Fatalf("message-limit trim left %d messages", n)
	}
	if !strings.Contains(s.history[0], "Earlier turns were dropped") {
		t.Fatalf("message-limit trim should still insert a compact note: %q", s.history[0])
	}
	if !strings.Contains(strings.Join(s.history, "\n"), "msg-9") {
		t.Fatal("the latest message should remain")
	}
}

func TestCompactNoteKeepsPercentInDroppedText(t *testing.T) {
	i18n.SetLocale("en")
	s := &fakeSession{}
	s.MarkTurn()
	s.AddUser("progress 100% done %s leftover " + strings.Repeat("w", 80))
	s.MarkTurn()
	s.AddUser("KEEP")
	s.Trim(60, 40)
	if !strings.Contains(s.tracker.note, "100%") {
		t.Fatalf("dropped text containing %% must stay literal in the note: %q", s.tracker.note)
	}
}

func TestCompactAnthropicAndOpenAI(t *testing.T) {
	i18n.SetLocale("en")
	sessions := []Session{&anthropicSession{}, &openAISession{}}
	for _, sess := range sessions {
		for i := 0; i < 6; i++ {
			sess.MarkTurn()
			sess.AddUser(fmt.Sprintf("OLD-%d %s", i, strings.Repeat("q", 80)))
		}
		sess.MarkTurn()
		sess.AddUser("KEEP-PROVIDER")
		sess.Trim(60, 220)
		raw := string(sess.Snapshot())
		if !strings.Contains(raw, "KEEP-PROVIDER") {
			t.Fatalf("%T dropped the current assignment: %s", sess, raw)
		}
		if !strings.Contains(raw, "Earlier turns were dropped") {
			t.Fatalf("%T missing compact note: %s", sess, raw)
		}
		for i, line := range historyLines(t, sess)[1:] {
			if strings.Contains(line, "OLD-0") {
				t.Fatalf("%T kept the oldest turn as a live message (%d): %s", sess, i, line)
			}
		}
	}
}
