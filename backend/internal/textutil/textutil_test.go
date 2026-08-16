package textutil

import (
	"botbureau/backend/internal/i18n"
	"strings"
	"testing"
)

func TestTruncate(t *testing.T) {
	i18n.SetLocale("en")
	if got := Truncate("short", 10); got != "short" {
		t.Fatalf("unchanged text = %q", got)
	}
	if got := Truncate("0123456789", 4); !strings.HasPrefix(got, "0123") || !strings.Contains(got, "truncated") {
		t.Fatalf("truncated text = %q", got)
	}
	if got := Truncate("abc", 0); !strings.Contains(got, "truncated") {
		t.Fatalf("zero-limit text = %q", got)
	}
}

func TestBrief(t *testing.T) {
	if got := Brief("short", 10); got != "short" {
		t.Fatalf("unchanged brief = %q", got)
	}
	if got := Brief("abcdef", 3); got != "abc…" {
		t.Fatalf("brief = %q, want abc…", got)
	}
}
