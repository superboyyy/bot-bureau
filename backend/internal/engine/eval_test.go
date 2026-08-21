package engine

import (
	"botbureau/backend/internal/config"
	"botbureau/backend/internal/docparse"
	"botbureau/backend/internal/model"

	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Phase 6 eval suite (docs/agent-runtime.md). Unit tests prove parsers and the gate; these cases
// prove a scripted member actually takes the phase 1–4 path: edit_file not a whole-file rewrite,
// grep not bash, read_skill before an edit, background group lines stay silent, and memory survives
// a new conversation. The provider is the same scripted fake the rest of the engine tests use. There
// is no live API here; BOTBUREAU_LIVE_EVAL is not wired up.

type evalSkill struct {
	name, desc, body string
}

func newEval(t *testing.T, p *scriptedProvider, perm config.PermLevel, fixture string, skill *evalSkill) (*BotWorker, *Bus) {
	t.Helper()
	dataDir := t.TempDir()
	if skill != nil {
		writeTestSkill(t, dataDir, skill.name, skill.desc, skill.body)
	}
	bus := NewBus()
	sched := NewScheduler(bus, filepath.Join(dataDir, "routines.json"))
	deps := newTestDeps(t, dataDir)
	w, err := NewBotWorker(config.BotConfig{Name: "a", Role: "eval", Provider: "fake"}, bus, sched, dataDir, deps)
	if err != nil {
		t.Fatal(err)
	}
	if p != nil {
		w.provider = p
	}
	if perm != "" {
		w.toolbox.botPerm = string(perm)
	}
	bus.Register(w)
	bus.SetGroupMemberIn("group", w.Name(), true)
	if fixture != "" {
		copyEvalFixture(t, w.workspace, fixture)
	}
	return w, bus
}

func copyEvalFixture(t *testing.T, dest, name string) {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	src := filepath.Join(filepath.Dir(file), "testdata", "eval", name)
	if err := os.CopyFS(dest, os.DirFS(src)); err != nil {
		t.Fatalf("copy fixture %s: %v", name, err)
	}
}

func consumedTools(orig, remaining []model.StepResult) []string {
	n := len(orig) - len(remaining)
	if n < 0 {
		n = 0
	}
	var names []string
	for _, st := range orig[:n] {
		for _, c := range st.ToolCalls {
			names = append(names, c.Name)
		}
	}
	return names
}

func hasTool(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

func toolIndex(names []string, want string) int {
	for i, n := range names {
		if n == want {
			return i
		}
	}
	return -1
}

func botMsgTexts(bus *Bus, name string) []string {
	var out []string
	for _, ev := range bus.Recent(200) {
		if ev["kind"] != "msg" {
			continue
		}
		src, _ := ev["source"].(string)
		if src != name {
			continue
		}
		text, _ := ev["text"].(string)
		out = append(out, text)
	}
	return out
}

func defNames(w *BotWorker) map[string]bool {
	names := map[string]bool{}
	for _, d := range w.toolbox.Defs() {
		names[d.Name] = true
	}
	return names
}

func TestEvalEditFunctionUsesEditFile(t *testing.T) {
	orig := []model.StepResult{
		{StopReason: "tool_use", ToolCalls: []model.ToolCall{
			{ID: "1", Name: "grep", Input: map[string]any{"pattern": "func Sum"}},
		}},
		{StopReason: "tool_use", ToolCalls: []model.ToolCall{
			{ID: "2", Name: "read_file", Input: map[string]any{"path": "math.go", "offset": float64(1), "limit": float64(8)}},
		}},
		{StopReason: "tool_use", ToolCalls: []model.ToolCall{
			{ID: "3", Name: "edit_file", Input: map[string]any{
				"path":       "math.go",
				"old_string": "func Sum(a, b int) int {\n\treturn a + b\n}",
				"new_string": "func Sum(a, b int) int {\n\treturn a + b + 0\n}",
			}},
		}},
		{StopReason: "end_turn", Texts: []string{"patched Sum"}},
	}
	p := &scriptedProvider{script: append([]model.StepResult(nil), orig...)}
	w, bus := newEval(t, p, config.PermEdit, "edit-fn", nil)
	w.handle(Msg{Sender: "user", Content: "Change Sum so it returns a + b + 0", Chat: "dm", Respond: true})

	tools := consumedTools(orig, p.script)
	if !hasTool(tools, "edit_file") {
		t.Fatalf("the good path is edit_file, got %v", tools)
	}
	if hasTool(tools, "write_file") {
		t.Fatalf("rewriting the whole file is the failure mode this case exists to catch: %v", tools)
	}
	got, err := os.ReadFile(filepath.Join(w.workspace, "math.go"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(got)
	if !strings.Contains(body, "return a + b + 0") {
		t.Fatalf("Sum should have been patched in place:\n%s", body)
	}
	if !strings.Contains(body, "func Product") || !strings.Contains(body, "return a * b") {
		t.Fatalf("Product must still be the original function:\n%s", body)
	}
	if texts := botMsgTexts(bus, w.Name()); len(texts) == 0 || !strings.Contains(texts[len(texts)-1], "patched Sum") {
		t.Fatalf("the turn should finish with the scripted reply: %v", texts)
	}
}

func TestEvalFindSymbolUsesGrep(t *testing.T) {
	orig := []model.StepResult{
		{StopReason: "tool_use", ToolCalls: []model.ToolCall{
			{ID: "1", Name: "grep", Input: map[string]any{"pattern": "UniqueTokenQZ19"}},
		}},
		{StopReason: "end_turn", Texts: []string{"found it in pkg/beta.go"}},
	}
	p := &scriptedProvider{script: append([]model.StepResult(nil), orig...)}
	w, _ := newEval(t, p, "", "find-symbol", nil)
	w.handle(Msg{Sender: "user", Content: "Where is UniqueTokenQZ19 defined?", Chat: "dm", Respond: true})

	tools := consumedTools(orig, p.script)
	if !hasTool(tools, "grep") {
		t.Fatalf("finding a symbol should go through grep, got %v", tools)
	}
	if hasTool(tools, "bash") {
		t.Fatalf("bash grep is the path this case forbids: %v", tools)
	}
	sess := w.session("dm").(*scriptedSession)
	if len(sess.toolResults) == 0 || len(sess.toolResults[0]) == 0 {
		t.Fatalf("grep should have returned a result: %+v", sess.toolResults)
	}
	hit := sess.toolResults[0][0].Content
	if sess.toolResults[0][0].IsError || !strings.Contains(hit, "UniqueTokenQZ19") || !strings.Contains(hit, "beta.go") {
		t.Fatalf("grep should name the file that holds the symbol: %q", hit)
	}
	if strings.Contains(hit, "alpha.go") {
		t.Fatalf("the other file should not match: %q", hit)
	}
}

func TestEvalReadSkillBeforeEdit(t *testing.T) {
	skill := &evalSkill{
		name: "edit-code",
		desc: "Change existing code in a repository. Use when editing files that are already there.",
		body: "SECRET_SKILL_BODY: find the site, then edit_file, never write_file of the whole path.",
	}
	orig := []model.StepResult{
		{StopReason: "tool_use", ToolCalls: []model.ToolCall{
			{ID: "1", Name: "read_skill", Input: map[string]any{"name": "edit-code"}},
		}},
		{StopReason: "tool_use", ToolCalls: []model.ToolCall{
			{ID: "2", Name: "edit_file", Input: map[string]any{
				"path":       "math.go",
				"old_string": "func Sum(a, b int) int {\n\treturn a + b\n}",
				"new_string": "func Sum(a, b int) int {\n\treturn a + b + 0\n}",
			}},
		}},
		{StopReason: "end_turn", Texts: []string{"followed the skill"}},
	}
	p := &scriptedProvider{script: append([]model.StepResult(nil), orig...)}
	w, _ := newEval(t, p, config.PermEdit, "edit-fn", skill)

	prompt := w.systemPrompt("dm")
	if !strings.Contains(prompt, "edit-code") {
		t.Fatalf("the matching skill must be in the roster:\n%s", prompt)
	}
	if strings.Contains(prompt, "SECRET_SKILL_BODY") {
		t.Fatal("the skill body belongs behind read_skill, not in the prompt")
	}

	w.handle(Msg{Sender: "user", Content: "Patch Sum in math.go using the edit-code skill", Chat: "dm", Respond: true})

	tools := consumedTools(orig, p.script)
	ri, ei := toolIndex(tools, "read_skill"), toolIndex(tools, "edit_file")
	if ri < 0 || ei < 0 || ri > ei {
		t.Fatalf("read_skill must run before edit_file, got %v", tools)
	}
	sess := w.session("dm").(*scriptedSession)
	if len(sess.toolResults) == 0 || !strings.Contains(sess.toolResults[0][0].Content, "SECRET_SKILL_BODY") {
		t.Fatalf("read_skill should return the body: %+v", sess.toolResults)
	}
	got, _ := os.ReadFile(filepath.Join(w.workspace, "math.go"))
	if !strings.Contains(string(got), "return a + b + 0") {
		t.Fatalf("the edit after the skill should have landed:\n%s", got)
	}
}

func TestEvalGroupBackgroundDoesNotReply(t *testing.T) {
	orig := []model.StepResult{
		{StopReason: "end_turn", Texts: []string{"SHOULD_NOT_REPLY"}},
	}
	p := &scriptedProvider{script: append([]model.StepResult(nil), orig...)}
	w, bus := newEval(t, p, "", "", nil)
	w.handle(Msg{Sender: "user", Content: "what is this limit?", Chat: "group", Respond: false})

	if len(p.script) != len(orig) {
		t.Fatalf("a background line must not start a turn; script consumed %d of %d", len(orig)-len(p.script), len(orig))
	}
	if texts := botMsgTexts(bus, w.Name()); len(texts) != 0 {
		t.Fatalf("no end_turn text reply on a background group line, got %v", texts)
	}
	snap := string(w.session("group").Snapshot())
	if !strings.Contains(snap, "what is this limit?") {
		t.Fatalf("the line still belongs in context: %s", snap)
	}
	if !strings.Contains(snap, "was not addressed to you") {
		t.Fatalf("background must announce itself in the session: %s", snap)
	}
}

func TestEvalRememberSurvivesNewSession(t *testing.T) {
	note := "Prefer REST for the public API. SECRET_EVAL_BODY is the rest of the note."
	orig := []model.StepResult{
		{StopReason: "tool_use", ToolCalls: []model.ToolCall{
			{ID: "1", Name: "remember", Input: map[string]any{"note": note, "scope": "self"}},
		}},
		{StopReason: "end_turn", Texts: []string{"noted"}},
	}
	p := &scriptedProvider{script: append([]model.StepResult(nil), orig...)}
	w, _ := newEval(t, p, config.PermEdit, "", nil)
	w.handle(Msg{Sender: "user", Content: "Remember our API convention", Chat: "dm", Respond: true})

	if !hasTool(consumedTools(orig, p.script), "remember") {
		t.Fatal("the turn should have called remember")
	}

	w.ResetChat("dm")
	prompt := w.systemPrompt("dm")
	if strings.Contains(prompt, "SECRET_EVAL_BODY") {
		t.Fatal("a new session's roster must not carry the body")
	}
	if !strings.Contains(prompt, "Prefer REST for the public API.") {
		t.Fatalf("the first clause should still be listed:\n%s", prompt)
	}
	if strings.Contains(string(w.session("dm").Snapshot()), "Remember our API convention") {
		t.Fatal("ResetChat should drop the old user line from the live session")
	}

	out, _, isErr := w.toolbox.Execute("recall", map[string]any{"query": "SECRET_EVAL_BODY"})
	if isErr || !strings.Contains(out, "SECRET_EVAL_BODY") {
		t.Fatalf("recall should still see the note after a new session: %q %v", out, isErr)
	}
}

func TestEvalReadPDFUsesReadFile(t *testing.T) {
	skill := &evalSkill{
		name: "pdf",
		desc: "Read, summarise, extract from, or create PDF files.",
		body: "SECRET_PDF_SKILL: read_file extracts the text; do not bash.",
	}
	orig := []model.StepResult{
		{StopReason: "tool_use", ToolCalls: []model.ToolCall{
			{ID: "1", Name: "read_skill", Input: map[string]any{"name": "pdf"}},
		}},
		{StopReason: "tool_use", ToolCalls: []model.ToolCall{
			{ID: "2", Name: "read_file", Input: map[string]any{"path": "quote.pdf"}},
		}},
		{StopReason: "end_turn", Texts: []string{"the invoice says TOTAL-99"}},
	}
	p := &scriptedProvider{script: append([]model.StepResult(nil), orig...)}
	w, bus := newEval(t, p, config.PermEdit, "", skill)
	if err := os.WriteFile(filepath.Join(w.workspace, "quote.pdf"), docparse.FixturePDF("Invoice TOTAL-99"), 0o644); err != nil {
		t.Fatal(err)
	}

	w.handle(Msg{Sender: "user", Content: "What does quote.pdf say?", Chat: "dm", Respond: true})

	tools := consumedTools(orig, p.script)
	ri, fi := toolIndex(tools, "read_skill"), toolIndex(tools, "read_file")
	if ri < 0 || fi < 0 || ri > fi {
		t.Fatalf("read_skill must run before read_file, got %v", tools)
	}
	if hasTool(tools, "bash") {
		t.Fatalf("PDF text is extracted by read_file, not bash: %v", tools)
	}
	sess := w.session("dm").(*scriptedSession)
	if len(sess.toolResults) < 2 || !strings.Contains(sess.toolResults[1][0].Content, "TOTAL-99") {
		t.Fatalf("read_file should return extracted PDF text: %+v", sess.toolResults)
	}
	if texts := botMsgTexts(bus, w.Name()); len(texts) == 0 || !strings.Contains(texts[len(texts)-1], "TOTAL-99") {
		t.Fatalf("the turn should finish with the scripted reply: %v", texts)
	}
}

func TestEvalPolicyToolsTheModelCanSee(t *testing.T) {
	w, _, _ := newTestWorker(t, "a", nil)
	names := defNames(w)
	for _, want := range []string{
		"edit_file", "write_file", "read_file", "grep", "glob",
		"remember", "recall", "search_history",
		"web_search", "fetch_url",
	} {
		if !names[want] {
			t.Errorf("defs missing %s: %v", want, names)
		}
	}
	prompt := w.systemPrompt("dm")
	for _, want := range []string{"prefer edit_file", "prefer grep", "web_search", "todo_write", "recall", "extracts text from PDF"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt should steer with %q", want)
		}
	}
	if strings.Contains(prompt, "no search engine") {
		t.Error("nobody should be told they have no search engine")
	}
}
