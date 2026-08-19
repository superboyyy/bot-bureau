package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseLegacyMemoryLines(t *testing.T) {
	raw := `# notes
- [2020-01-02] old dated note about the API
- a line with no date
- [ab12] [2026-08-18] already identified
`
	got, assigned := parseMemory(raw)
	if !assigned {
		t.Fatal("legacy lines should receive ids")
	}
	if len(got) != 3 {
		t.Fatalf("got %d entries: %#v", len(got), got)
	}
	if got[0].Date != "2020-01-02" || got[0].Text != "old dated note about the API" || got[0].ID == "" {
		t.Fatalf("dated legacy: %#v", got[0])
	}
	if got[1].Text != "a line with no date" || got[1].ID == "" {
		t.Fatalf("bare legacy: %#v", got[1])
	}
	if got[2].ID != "ab12" || got[2].Date != "2026-08-18" || got[2].Text != "already identified" {
		t.Fatalf("new format: %#v", got[2])
	}
	again, _ := parseMemory(raw)
	if again[0].ID != got[0].ID || again[1].ID != got[1].ID {
		t.Fatal("legacy ids must be stable across reads")
	}
}

func TestRememberDuplicateAndReplace(t *testing.T) {
	mem := NewMemory(filepath.Join(t.TempDir(), "MEMORY.md"))
	id, existed, err := mem.Remember("Prefer REST for the public API.", "")
	if err != nil || existed || !memIDRe.MatchString(id) {
		t.Fatalf("first remember: id=%q existed=%v err=%v", id, existed, err)
	}
	again, existed, err := mem.Remember("Prefer REST for the public API.", "")
	if err != nil || !existed || again != id {
		t.Fatalf("duplicate should no-op: id=%q existed=%v err=%v", again, existed, err)
	}
	raw := mem.Load()
	if strings.Count(raw, "Prefer REST") != 1 {
		t.Fatalf("duplicate wrote a second line:\n%s", raw)
	}
	if _, _, err := mem.Remember("Prefer GraphQL instead.", id); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(mem.Load(), "Prefer GraphQL instead.") || strings.Contains(mem.Load(), "Prefer REST") {
		t.Fatalf("replace by id:\n%s", mem.Load())
	}
}

func TestRecallAndForget(t *testing.T) {
	mem := NewMemory(filepath.Join(t.TempDir(), "MEMORY.md"))
	id, _, err := mem.Remember("The staging host is staging.example.com and it uses the shared SSH key.", "")
	if err != nil {
		t.Fatal(err)
	}
	hits := mem.Recall("staging.example.com", 8)
	if len(hits) != 1 || hits[0].ID != id {
		t.Fatalf("substring recall: %#v", hits)
	}
	byID := mem.Recall(id, 8)
	if len(byID) != 1 || byID[0].ID != id {
		t.Fatalf("id recall: %#v", byID)
	}
	if err := mem.Forget(id); err != nil {
		t.Fatal(err)
	}
	if n := len(mem.Recall(id, 8)); n != 0 {
		t.Fatalf("forget should drop the note, still have %d", n)
	}
	if mem.Roster() != "" {
		t.Fatalf("roster should be empty after forget: %q", mem.Roster())
	}
	if err := mem.Forget(id); err == nil {
		t.Fatal("forgetting a missing id should error")
	}
}

func TestRosterOmitsBodiesAndCaps(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString("# Memory\n")
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&b, "- [%s] Prefer REST for the public API. SECRET_BODY_%d and a paragraph of details that must not enter the prompt.\n",
			fmt.Sprintf("%04x", i), i)
	}
	path := filepath.Join(dir, "MEMORY.md")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	mem := NewMemory(path)
	roster := mem.Roster()
	if strings.Contains(roster, "SECRET_BODY_") {
		t.Fatalf("roster must not carry bodies:\n%s", roster)
	}
	if !strings.Contains(roster, "Prefer REST for the public API.") {
		t.Fatalf("roster should keep the first clause:\n%s", roster)
	}
	if !strings.Contains(roster, "older notes") {
		t.Fatal("200 notes should mention the ones that were dropped from the roster")
	}
	n := 0
	for _, line := range strings.Split(roster, "\n") {
		if strings.Contains(line, ": ") {
			n++
		}
	}
	if n != maxMemoryRoster {
		t.Fatalf("roster size %d, want %d", n, maxMemoryRoster)
	}
}

func TestLegacyFileGetsIDsOnWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "MEMORY.md")
	if err := os.WriteFile(path, []byte("- [2021-05-05] keep the Friday report\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mem := NewMemory(path)
	if _, _, err := mem.Remember("a second note", ""); err != nil {
		t.Fatal(err)
	}
	raw := mem.Load()
	if !strings.Contains(raw, "- [") || !strings.Contains(raw, "keep the Friday report") {
		t.Fatalf("rewrite should keep the old note with an id:\n%s", raw)
	}
	got, assigned := parseMemory(raw)
	if assigned {
		t.Fatalf("after a write every line should already have an id: %#v", got)
	}
	if len(got) != 2 || got[0].Date != "2021-05-05" {
		t.Fatalf("preserved date: %#v", got)
	}
}

func TestEmptyNoteIsRejected(t *testing.T) {
	mem := NewMemory(filepath.Join(t.TempDir(), "MEMORY.md"))
	if _, _, err := mem.Remember("  ", ""); err == nil {
		t.Fatal("empty note should fail")
	}
}

func TestMemoryToolsOnWorker(t *testing.T) {
	w, _, _ := newTestWorker(t, "a", nil)
	tb := w.toolbox
	out, _, isErr := tb.Execute("remember", map[string]any{"note": "Prefer REST for the public API. SECRET_PROMPT_BODY stays out."})
	if isErr {
		t.Fatalf("remember: %q", out)
	}
	prompt := w.systemPrompt("dm")
	if strings.Contains(prompt, "SECRET_PROMPT_BODY") {
		t.Fatal("the prompt must list the first clause, not the body")
	}
	if !strings.Contains(prompt, "Prefer REST for the public API.") {
		t.Fatalf("roster should be in the prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "recall") || !strings.Contains(prompt, "search_history") {
		t.Fatal("prompt should name recall and search_history")
	}

	hits, _, isErr := tb.Execute("recall", map[string]any{"query": "SECRET_PROMPT_BODY"})
	if isErr || !strings.Contains(hits, "SECRET_PROMPT_BODY") {
		t.Fatalf("recall should return the body: %q %v", hits, isErr)
	}
	found := w.mem.Recall("SECRET_PROMPT_BODY", 8)
	if len(found) != 1 {
		t.Fatalf("expected one stored note, got %#v", found)
	}
	if _, _, isErr := tb.Execute("forget", map[string]any{"id": found[0].ID}); isErr {
		t.Fatal("forget failed")
	}
	if strings.Contains(w.systemPrompt("dm"), "Prefer REST for the public API.") {
		t.Fatal("forgotten notes must leave the roster")
	}

	if _, _, isErr := tb.Execute("remember", map[string]any{"note": "a team convention", "scope": "team"}); isErr {
		t.Fatal("team remember")
	}
	team := w.deps.TeamMem.Load()
	if !strings.Contains(team, "a team convention") || !strings.Contains(team, "a:") {
		t.Fatalf("shared memory should carry the content and the author: %q", team)
	}
}
