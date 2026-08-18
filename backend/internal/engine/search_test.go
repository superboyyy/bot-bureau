package engine

import (
	"strings"
	"testing"
)

func TestGlobRegexp(t *testing.T) {
	re, err := globRegexp("**/*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"foo.go", "a/foo.go", "a/b/foo.go"} {
		if !re.MatchString(name) {
			t.Errorf("%s should match **/*.go", name)
		}
	}
	if re.MatchString("foo.txt") {
		t.Error("foo.txt should not match **/*.go")
	}

	star, err := globRegexp("*.md")
	if err != nil {
		t.Fatal(err)
	}
	if !star.MatchString("README.md") || star.MatchString("docs/README.md") {
		t.Fatal("*.md should match only one path segment")
	}
}

func TestGrepInvalidPattern(t *testing.T) {
	w, _, _ := newTestWorker(t, "a", nil)
	out, _, isErr := w.toolbox.Execute("grep", map[string]any{"pattern": "("})
	if !isErr || !strings.Contains(out, "regular expression") {
		t.Fatalf("invalid regexp should error: %q %v", out, isErr)
	}
}
