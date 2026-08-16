package engine

import (
	"botbureau/backend/internal/i18n"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTaskBoardCRUDRenderClearAndReload(t *testing.T) {
	i18n.SetLocale("en")
	path := filepath.Join(t.TempDir(), "nested", "tasks.json")
	b := NewTaskBoard(path)
	first := b.Add("chief", "coder", "  Fix bug  ", "  add a regression test ")
	if first.ID != 1 || first.Title != "Fix bug" || first.Detail != "add a regression test" || first.Status != "todo" {
		t.Fatalf("new task = %#v", first)
	}
	if _, err := b.Update(first.ID, "blocked", "no"); err == nil {
		t.Fatal("invalid status should fail")
	}
	if _, err := b.Update(999, "", "no"); err == nil {
		t.Fatal("missing task should fail")
	}
	updated, err := b.Update(first.ID, "doing", "waiting on review")
	if err != nil || updated.Status != "doing" || updated.Note != "waiting on review" {
		t.Fatalf("updated task = %#v, %v", updated, err)
	}
	done := b.Add("chief", "writer", "Ship notes", "")
	if _, err := b.Update(done.ID, "done", ""); err != nil {
		t.Fatal(err)
	}
	if rendered := b.Render(); !strings.Contains(rendered, "#1 [doing] Fix bug") || !strings.Contains(rendered, "note: waiting on review") {
		t.Fatalf("rendered board = %q", rendered)
	}
	if got := b.ClearDone(); got != 1 {
		t.Fatalf("ClearDone() = %d, want 1", got)
	}
	if got := b.ClearDone(); got != 0 {
		t.Fatalf("second ClearDone() = %d, want 0", got)
	}
	reloaded := NewTaskBoard(path)
	items := reloaded.List()
	if len(items) != 1 || items[0].Title != "Fix bug" || items[0].Status != "doing" {
		t.Fatalf("reloaded tasks = %#v", items)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestTaskBoardSkipsBadPersistedEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	if err := os.WriteFile(path, []byte(`[
{"id":1,"title":"ok","status":"todo"},
{"id":2,"title":"","status":"todo"},
{"id":3,"title":"bad status","status":"blocked"},
{"id":4,"title":"done","status":"done"}
]`), 0o644); err != nil {
		t.Fatal(err)
	}
	b := NewTaskBoard(path)
	if got := len(b.List()); got != 2 {
		t.Fatalf("loaded %d tasks, want 2", got)
	}
	if got := b.Add("a", "b", "next", "").ID; got != 5 {
		t.Fatalf("next task id = %d, want 5", got)
	}
}
