package textutil

import (
	"botbureau/backend/internal/i18n"
	"strings"
	"testing"
)

func TestUnifiedReplaceLine(t *testing.T) {
	i18n.SetLocale("en")
	old := "alpha\nbeta\ngamma\n"
	new := "alpha\nBETA\ngamma\n"
	got := Unified("n.go", old, new, 0)
	if !strings.Contains(got, "--- a/n.go") || !strings.Contains(got, "+++ b/n.go") {
		t.Fatalf("missing headers:\n%s", got)
	}
	if !strings.Contains(got, "-beta") || !strings.Contains(got, "+BETA") {
		t.Fatalf("missing the changed line:\n%s", got)
	}
	if !strings.Contains(got, " alpha") || !strings.Contains(got, " gamma") {
		t.Fatalf("context lines should be present:\n%s", got)
	}
}

func TestUnifiedNewFile(t *testing.T) {
	got := Unified("new.txt", "", "hello\nworld\n", 0)
	if !strings.Contains(got, "@@ -0,0 +1,2 @@") {
		t.Fatalf("new file hunk = %q", got)
	}
	if !strings.Contains(got, "+hello") || !strings.Contains(got, "+world") {
		t.Fatalf("additions missing:\n%s", got)
	}
}

func TestUnifiedIdenticalIsEmpty(t *testing.T) {
	if got := Unified("x", "a\nb\n", "a\nb\n", 0); got != "" {
		t.Fatalf("identical files should produce no diff, got %q", got)
	}
}

func TestUnifiedTruncates(t *testing.T) {
	i18n.SetLocale("en")
	var old, new strings.Builder
	for i := 0; i < 200; i++ {
		old.WriteString("line\n")
		new.WriteString("LINE\n")
	}
	got := Unified("big.txt", old.String(), new.String(), 80)
	if !strings.Contains(got, "truncated") {
		t.Fatalf("a capped diff should say so: %q", got)
	}
	if len(got) > 200 {
		t.Fatalf("capped diff still too long: %d", len(got))
	}
}

func TestUnifiedDeleteFile(t *testing.T) {
	got := Unified("gone.txt", "only\n", "", 0)
	if !strings.Contains(got, "-only") || !strings.Contains(got, "@@ -1,1 +0,0 @@") {
		t.Fatalf("delete hunk = %q", got)
	}
}
