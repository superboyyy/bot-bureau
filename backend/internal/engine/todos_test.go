package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTodoMarkdownRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := []TodoItem{
		{ID: "edit", Content: "change a.go", Status: "pending"},
		{ID: "verify", Content: "run tests", Status: "done"},
	}
	if err := SaveTodos(dir, want); err != nil {
		t.Fatal(err)
	}
	got := LoadTodos(dir)
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("round-trip: got %#v", got)
	}
	raw, err := os.ReadFile(todoPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "- [ ] edit: change a.go") || !strings.Contains(string(raw), "- [x] verify: run tests") {
		t.Fatalf("markdown should be readable, got %s", raw)
	}
}

func TestEmptyTodosRemoveFile(t *testing.T) {
	dir := t.TempDir()
	if err := SaveTodos(dir, []TodoItem{{ID: "a", Content: "x", Status: "pending"}}); err != nil {
		t.Fatal(err)
	}
	if err := SaveTodos(dir, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(todoPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("empty list should delete TODO.md, stat=%v", err)
	}
	if n := len(LoadTodos(dir)); n != 0 {
		t.Fatalf("missing file should load as empty, got %d", n)
	}
}

func TestLoadTodosSlugsMissingIDs(t *testing.T) {
	dir := t.TempDir()
	p := todoPath(dir)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("# Todos\n- [ ] write the parser\n- [x] already done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := LoadTodos(dir)
	if len(got) != 2 || got[0].ID == "" || got[1].Status != "done" {
		t.Fatalf("expected slugged items, got %#v", got)
	}
	if got[0].Content != "write the parser" {
		t.Fatalf("content: %#v", got[0])
	}
}

func TestEmptyTodosStayOutOfPrompt(t *testing.T) {
	w, _, _ := newTestWorker(t, "a", nil)
	for _, chat := range []string{"dm", "group"} {
		prompt := w.systemPrompt(chat)
		if strings.Contains(prompt, "# Todos") {
			t.Fatalf("%s prompt must not carry an empty todos heading:\n%s", chat, prompt)
		}
	}
}

func TestTodoWriteInjectsWhenNonEmpty(t *testing.T) {
	w, _, _ := newTestWorker(t, "a", nil)
	tb := w.toolbox
	tb.currentChat = "dm"
	out, _, isErr := tb.Execute("todo_write", map[string]any{
		"items": []any{
			map[string]any{"id": "edit", "content": "touch both files", "status": "pending"},
			map[string]any{"id": "test", "content": "run go test", "status": "done"},
		},
	})
	if isErr || !strings.Contains(out, "2") {
		t.Fatalf("todo_write in a DM: %q %v", out, isErr)
	}
	prompt := w.systemPrompt("dm")
	if !strings.Contains(prompt, "# Todos") || !strings.Contains(prompt, "touch both files") || !strings.Contains(prompt, "run go test") {
		t.Fatalf("non-empty list should land in the prompt:\n%s", prompt)
	}

	tb.currentChat = "group"
	out, _, isErr = tb.Execute("todo_write", map[string]any{
		"items": []any{map[string]any{"id": "group-item", "content": "still personal", "status": "pending"}},
	})
	if isErr {
		t.Fatalf("todo_write in a group should still work: %q", out)
	}
	prompt = w.systemPrompt("group")
	if !strings.Contains(prompt, "still personal") {
		t.Fatalf("group prompt should carry the personal list:\n%s", prompt)
	}
	if strings.Contains(prompt, "touch both files") {
		t.Fatal("todo_write replaces the whole list")
	}

	out, _, isErr = tb.Execute("todo_write", map[string]any{"items": []any{}})
	if isErr || !strings.Contains(out, "cleared") {
		t.Fatalf("clearing should succeed: %q %v", out, isErr)
	}
	if strings.Contains(w.systemPrompt("dm"), "# Todos") {
		t.Fatal("clearing the list must drop the heading")
	}
}

func TestTodoWriteRejectsBadItems(t *testing.T) {
	w, _, _ := newTestWorker(t, "a", nil)
	tb := w.toolbox
	cases := []struct {
		items any
		want  string
	}{
		{nil, "array"},
		{[]any{map[string]any{"id": "a"}}, "content"},
		{[]any{
			map[string]any{"id": "a", "content": "one"},
			map[string]any{"id": "a", "content": "two"},
		}, "twice"},
		{[]any{map[string]any{"id": "a", "content": "one", "status": "doing"}}, "pending or done"},
		{[]any{map[string]any{"id": "NOPE!", "content": "one"}}, "letters"},
	}
	for _, c := range cases {
		out, _, isErr := tb.Execute("todo_write", map[string]any{"items": c.items})
		if !isErr || !strings.Contains(out, c.want) {
			t.Errorf("items=%v: want error containing %q, got %q err=%v", c.items, c.want, out, isErr)
		}
	}
}

func TestTodoWriteNotParallelizable(t *testing.T) {
	w, _, _ := newTestWorker(t, "a", nil)
	if w.toolbox.parallelizable("todo_write") || w.toolbox.parallelizable("submit_plan") {
		t.Fatal("writes and human waits must stay in order")
	}
}

func TestTodoAndPlanToolsAreOffered(t *testing.T) {
	w, _, _ := newTestWorker(t, "a", nil)
	prompt := w.systemPrompt("dm")
	if !strings.Contains(prompt, "todo_write") || !strings.Contains(prompt, "submit_plan") {
		t.Fatalf("prompt should tell the model to plan multi-file work:\n%s", prompt)
	}
	names := map[string]bool{}
	for _, d := range w.toolbox.Defs() {
		names[d.Name] = true
	}
	for _, want := range []string{"todo_write", "submit_plan"} {
		if !names[want] {
			t.Fatalf("defs missing %s", want)
		}
	}
}
