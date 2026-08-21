package engine

import (
	"botbureau/backend/internal/config"
	"botbureau/backend/internal/docparse"
	"botbureau/backend/internal/model"

	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadFileWindow(t *testing.T) {
	w, _, _ := newTestWorker(t, "a", nil)
	tb := w.toolbox
	tb.botPerm = string(config.PermEdit)
	p := filepath.Join(w.workspace, "n.txt")
	var b strings.Builder
	for i := 1; i <= 12; i++ {
		b.WriteString("line ")
		b.WriteByte(byte('A' - 1 + i))
		b.WriteByte('\n')
	}
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	out, _, isErr := tb.Execute("read_file", map[string]any{"path": "n.txt"})
	if isErr || !strings.Contains(out, "line A") || strings.Contains(out, "lines 1-") {
		t.Fatalf("whole-file read should stay unnumbered: %q %v", out, isErr)
	}

	out, _, isErr = tb.Execute("read_file", map[string]any{"path": "n.txt", "offset": float64(10), "limit": float64(3)})
	if isErr {
		t.Fatalf("windowed read failed: %q", out)
	}
	if !strings.Contains(out, "n.txt: lines 10-12 of 12") {
		t.Fatalf("header: %q", out)
	}
	if !strings.Contains(out, "10|line J") || strings.Contains(out, "line A") {
		t.Fatalf("window contents: %q", out)
	}

	out, _, isErr = tb.Execute("read_file", map[string]any{"path": "n.txt", "offset": float64(20)})
	if !isErr || !strings.Contains(out, "past the end") {
		t.Fatalf("offset past EOF should error: %q %v", out, isErr)
	}
}

func TestEditFileUniqueAndReplaceAll(t *testing.T) {
	w, _, _ := newTestWorker(t, "a", nil)
	tb := w.toolbox
	tb.botPerm = string(config.PermEdit)
	p := filepath.Join(w.workspace, "fn.go")
	src := "func a() {}\nfunc b() {}\nfunc a() {}\n"
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	out, _, isErr := tb.Execute("edit_file", map[string]any{
		"path": "fn.go", "old_string": "func a() {}", "new_string": "func a() { return }",
	})
	if !isErr || !strings.Contains(out, "matched 2 times") {
		t.Fatalf("duplicate old_string should refuse: %q %v", out, isErr)
	}
	got, _ := os.ReadFile(p)
	if string(got) != src {
		t.Fatal("a refused edit must not write")
	}

	out, _, isErr = tb.Execute("edit_file", map[string]any{
		"path": "fn.go", "old_string": "func b() {}", "new_string": "func b() { return }",
	})
	if isErr {
		t.Fatalf("unique edit failed: %q", out)
	}
	got, _ = os.ReadFile(p)
	if !strings.Contains(string(got), "func b() { return }") || strings.Count(string(got), "func a() {}") != 2 {
		t.Fatalf("only the unique site should change:\n%s", got)
	}

	out, _, isErr = tb.Execute("edit_file", map[string]any{
		"path": "fn.go", "old_string": "func a() {}", "new_string": "func a() { x }", "replace_all": true,
	})
	if isErr {
		t.Fatalf("replace_all failed: %q", out)
	}
	got, _ = os.ReadFile(p)
	if strings.Count(string(got), "func a() { x }") != 2 {
		t.Fatalf("replace_all should change every match:\n%s", got)
	}

	out, _, isErr = tb.Execute("edit_file", map[string]any{
		"path": "missing.go", "old_string": "x", "new_string": "y",
	})
	if !isErr || !strings.Contains(out, "write_file") {
		t.Fatalf("missing file should point at write_file: %q %v", out, isErr)
	}
}

func TestEditFileApprovalCarriesDiff(t *testing.T) {
	w, bus, _ := newTestWorker(t, "a", nil)
	tb := w.toolbox
	tb.currentChat = "dm"
	if err := os.WriteFile(filepath.Join(w.workspace, "x.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan *Approval, 1)
	go func() {
		deadline := time.After(2 * time.Second)
		for {
			select {
			case <-deadline:
				done <- nil
				return
			default:
				if aps := bus.PendingApprovals(); len(aps) > 0 {
					done <- aps[0]
					bus.Decide(aps[0].ID, true, "")
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
	}()
	out, _, isErr := tb.Execute("edit_file", map[string]any{
		"path": "x.txt", "old_string": "hello", "new_string": "HELLO",
	})
	if isErr {
		t.Fatalf("approved edit failed: %q", out)
	}
	ap := <-done
	if ap == nil {
		t.Fatal("no approval was raised")
	}
	if !strings.Contains(ap.Diff, "-hello") || !strings.Contains(ap.Diff, "+HELLO") {
		t.Fatalf("approval should carry a unified diff, got %q", ap.Diff)
	}
}

func TestWriteFileApprovalCarriesDiff(t *testing.T) {
	w, bus, _ := newTestWorker(t, "a", nil)
	tb := w.toolbox
	tb.currentChat = "dm"
	done := make(chan *Approval, 1)
	go func() {
		for i := 0; i < 400; i++ {
			if aps := bus.PendingApprovals(); len(aps) > 0 {
				done <- aps[0]
				bus.Decide(aps[0].ID, true, "")
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		done <- nil
	}()
	if out, _, isErr := tb.Execute("write_file", map[string]any{"path": "new.txt", "content": "hi\n"}); isErr {
		t.Fatalf("write failed: %q", out)
	}
	ap := <-done
	if ap == nil || !strings.Contains(ap.Diff, "+hi") {
		t.Fatalf("new-file write should show additions, got %#v", ap)
	}
}

func TestGrepAndGlobStayInBounds(t *testing.T) {
	w, _, _ := newTestWorker(t, "a", nil)
	tb := w.toolbox
	inside := filepath.Join(w.workspace, "a.go")
	if err := os.WriteFile(inside, []byte("package inside\nfunc Target() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "b.go"), []byte("package outside\nfunc Target() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, _, isErr := tb.Execute("grep", map[string]any{"pattern": "Target"})
	if isErr || !strings.Contains(out, "a.go:2:") || strings.Contains(out, other) {
		t.Fatalf("grep should stay in the workspace: %q %v", out, isErr)
	}

	out, _, isErr = tb.Execute("glob", map[string]any{"pattern": "**/*.go"})
	if isErr || !strings.Contains(out, "a.go") || strings.Contains(out, filepath.Base(other)) {
		t.Fatalf("glob should stay in the workspace: %q %v", out, isErr)
	}

	if !w.roots.Add(other) {
		t.Fatal("grant failed")
	}
	out, _, isErr = tb.Execute("grep", map[string]any{"pattern": "package outside"})
	if isErr || !strings.Contains(out, "b.go") {
		t.Fatalf("grep should see a granted directory: %q %v", out, isErr)
	}
}

func TestGrepSkipsBinaryAndGit(t *testing.T) {
	w, _, _ := newTestWorker(t, "a", nil)
	tb := w.toolbox
	if err := os.WriteFile(filepath.Join(w.workspace, "ok.txt"), []byte("findme\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.workspace, "blob.bin"), []byte("findme\x00rest"), 0o644); err != nil {
		t.Fatal(err)
	}
	git := filepath.Join(w.workspace, ".git")
	if err := os.Mkdir(git, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(git, "obj"), []byte("findme\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, isErr := tb.Execute("grep", map[string]any{"pattern": "findme"})
	if isErr || !strings.Contains(out, "ok.txt") {
		t.Fatalf("text hit missing: %q %v", out, isErr)
	}
	if strings.Contains(out, "blob.bin") || strings.Contains(out, ".git") {
		t.Fatalf("binary or .git leaked: %q", out)
	}
}

func TestReadFileExtractsPDFAndOffice(t *testing.T) {
	w, _, _ := newTestWorker(t, "a", nil)
	tb := w.toolbox
	if err := os.WriteFile(filepath.Join(w.workspace, "quote.pdf"), docparse.FixturePDF("Invoice TOTAL-99"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, isErr := tb.Execute("read_file", map[string]any{"path": "quote.pdf"})
	if isErr || !strings.Contains(out, "Invoice TOTAL-99") {
		t.Fatalf("PDF text should be extracted: %q %v", out, isErr)
	}
	if !strings.Contains(out, "Extracted text from quote.pdf") {
		t.Fatalf("extraction should be labelled: %q", out)
	}

	if err := os.WriteFile(filepath.Join(w.workspace, "note.docx"), docparse.FixtureDOCX("Contract clause"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, isErr = tb.Execute("read_file", map[string]any{"path": "note.docx"})
	if isErr || !strings.Contains(out, "Contract clause") {
		t.Fatalf("docx text should be extracted: %q %v", out, isErr)
	}

	out, _, isErr = tb.Execute("read_file", map[string]any{"path": "quote.pdf", "offset": float64(1), "limit": float64(2)})
	if isErr || !strings.Contains(out, "quote.pdf: lines 1-2 of") {
		t.Fatalf("windowed PDF read: %q %v", out, isErr)
	}
}

func TestGrepSearchesExtractedPDF(t *testing.T) {
	w, _, _ := newTestWorker(t, "a", nil)
	tb := w.toolbox
	if err := os.WriteFile(filepath.Join(w.workspace, "quote.pdf"), docparse.FixturePDF("Invoice TOTAL-99"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, isErr := tb.Execute("grep", map[string]any{"pattern": "TOTAL-99"})
	if isErr || !strings.Contains(out, "quote.pdf") || !strings.Contains(out, "TOTAL-99") {
		t.Fatalf("grep should search extracted PDF text: %q %v", out, isErr)
	}
}

func TestReadFileStillRefusesOpaqueBinary(t *testing.T) {
	w, _, _ := newTestWorker(t, "a", nil)
	tb := w.toolbox
	if err := os.WriteFile(filepath.Join(w.workspace, "blob.bin"), []byte("x\x00y"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, isErr := tb.Execute("read_file", map[string]any{"path": "blob.bin"})
	if !isErr || !strings.Contains(out, "binary") {
		t.Fatalf("opaque binary should still be refused: %q %v", out, isErr)
	}
}

func TestParallelReadOnlyBatch(t *testing.T) {
	dir := t.TempDir()
	bus := NewBus()
	sched := NewScheduler(bus, filepath.Join(dir, "routines.json"))
	deps := newTestDeps(t, dir)
	w, err := NewBotWorker(config.BotConfig{Name: "a", Role: "test", Provider: "fake"}, bus, sched, dir, deps)
	if err != nil {
		t.Fatal(err)
	}
	bus.Register(w)
	if err := os.WriteFile(filepath.Join(w.workspace, "one.txt"), []byte("ONE"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.workspace, "two.txt"), []byte("TWO"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := []model.StepResult{
		{StopReason: "tool_use", ToolCalls: []model.ToolCall{
			{ID: "1", Name: "read_file", Input: map[string]any{"path": "one.txt"}},
			{ID: "2", Name: "read_file", Input: map[string]any{"path": "two.txt"}},
		}},
		{StopReason: "end_turn", Texts: []string{"done"}},
	}
	sess := &scriptedSession{script: &script}
	w.toolbox.currentChat = "dm"
	w.agentLoop(context.Background(), "dm", sess, Msg{Sender: "user", Chat: "dm"})
	if len(sess.toolResults) != 1 || len(sess.toolResults[0]) != 2 {
		t.Fatalf("both reads should return: %+v", sess.toolResults)
	}
	got := sess.toolResults[0][0].Content + sess.toolResults[0][1].Content
	if !strings.Contains(got, "ONE") || !strings.Contains(got, "TWO") {
		t.Fatalf("parallel reads lost a file: %+v", sess.toolResults[0])
	}
}

func TestSystemPromptNamesFileTools(t *testing.T) {
	w, _, _ := newTestWorker(t, "a", nil)
	prompt := w.systemPrompt("dm")
	if !strings.Contains(prompt, "prefer edit_file") || !strings.Contains(prompt, "prefer grep") {
		t.Fatalf("prompt should steer toward edit_file and grep:\n%s", prompt)
	}
	if !strings.Contains(prompt, "need user approval") {
		t.Fatal("the approval sentence must stay, tests and models both key on it")
	}
	names := map[string]bool{}
	for _, d := range w.toolbox.Defs() {
		names[d.Name] = true
	}
	for _, want := range []string{"edit_file", "grep", "glob", "read_file", "write_file"} {
		if !names[want] {
			t.Fatalf("defs missing %s: %v", want, names)
		}
	}
}
