package engine

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestSearchHistoryFindsMessageDroppedFromMemoryWindow(t *testing.T) {
	w, bus, _ := newTestWorker(t, "a", nil)
	dir := t.TempDir()
	bus.EnableEventLog(filepath.Join(dir, "events.json"))
	tb := w.toolbox
	tb.currentChat = "dm"

	bus.Emit("msg", "dm:a", "user", "we decided the API is REST after all", nil)
	for i := 0; i < memEvents*2+50; i++ {
		bus.Emit("tool", "dm:a", "a", fmt.Sprintf("step %d", i), nil)
	}
	found := false
	for _, ev := range bus.Recent(5000) {
		if text, _ := ev["text"].(string); strings.Contains(text, "API is REST") {
			found = true
			break
		}
	}
	if found {
		t.Fatal("the distinctive line should have fallen out of the in-memory tail")
	}

	out, _, isErr := tb.Execute("search_history", map[string]any{"query": "API is REST"})
	if isErr || !strings.Contains(out, "API is REST") || !strings.Contains(out, "from=user") {
		t.Fatalf("search_history should read the log: %q %v", out, isErr)
	}
}

func TestSearchHistoryStaysInThisChat(t *testing.T) {
	w, bus, _ := newTestWorker(t, "a", nil)
	tb := w.toolbox
	tb.currentChat = "dm"
	bus.Emit("msg", "dm:a", "user", "the dm secret token ALPHA", nil)
	bus.Emit("msg", "group", "user", "the group secret token BETA", nil)

	out, _, isErr := tb.Execute("search_history", map[string]any{"query": "secret token"})
	if isErr || !strings.Contains(out, "ALPHA") {
		t.Fatalf("dm search should find the dm line: %q %v", out, isErr)
	}
	if strings.Contains(out, "BETA") {
		t.Fatalf("dm search must not leak the group chat: %q", out)
	}

	tb.currentChat = "group"
	out, _, isErr = tb.Execute("search_history", map[string]any{"query": "secret token"})
	if isErr || !strings.Contains(out, "BETA") || strings.Contains(out, "ALPHA") {
		t.Fatalf("group search should stay in the group: %q %v", out, isErr)
	}
}

func TestSearchHistoryEmptyQuery(t *testing.T) {
	w, _, _ := newTestWorker(t, "a", nil)
	out, _, isErr := w.toolbox.Execute("search_history", map[string]any{"query": "  "})
	if !isErr || !strings.Contains(out, "query") {
		t.Fatalf("empty query should fail: %q %v", out, isErr)
	}
}

func TestRecallAndSearchAreParallelizable(t *testing.T) {
	w, _, _ := newTestWorker(t, "a", nil)
	if !w.toolbox.parallelizable("recall") || !w.toolbox.parallelizable("search_history") {
		t.Fatal("reads should batch")
	}
	if w.toolbox.parallelizable("remember") || w.toolbox.parallelizable("forget") {
		t.Fatal("writes must stay in order")
	}
}
