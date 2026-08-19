package model

import (
	"botbureau/backend/internal/i18n"
	"botbureau/backend/internal/textutil"

	"encoding/json"
	"strings"
)

const compactBodyLimit = 2400
const compactSketchLimit = 160
const compactSketchMax = 24

const compactNoteKey = "[Earlier turns were dropped to free context. What was dropped:\n%s]"

// pickTrim returns the index to keep history from, or 0 when nothing should be dropped.
// Cuts are user-turn starts. The current turn (mark) is never split: a cut after mark is ignored.
func (t *cutTracker) pickTrim(n, maxMessages, maxChars int, charsFrom func(int) int) int {
	if n == 0 {
		return 0
	}
	overMsg := maxMessages > 0 && n > maxMessages
	overChars := maxChars > 0 && charsFrom != nil && charsFrom(0) > maxChars
	if !overMsg && !overChars {
		return 0
	}
	chosen := 0
	for _, c := range t.cuts {
		if c <= 0 || c >= n {
			continue
		}
		// mark == 0 means no current-turn protection (tests, or a session that never called MarkTurn).
		if t.mark > 0 && c > t.mark {
			break
		}
		chosen = c
		fitsMsg := maxMessages <= 0 || n-c <= maxMessages
		fitsChars := maxChars <= 0 || charsFrom == nil || charsFrom(c) <= maxChars
		if fitsMsg && fitsChars {
			break
		}
	}
	return chosen
}

func (t *cutTracker) shift(delta int) {
	if delta == 0 {
		return
	}
	for i := range t.cuts {
		t.cuts[i] += delta
	}
	t.mark += delta
}

func compactNoteBody(sketches []string) string {
	if len(sketches) > compactSketchMax {
		sketches = sketches[:compactSketchMax]
	}
	lines := make([]string, 0, len(sketches))
	for _, s := range sketches {
		if brief := textutil.Brief(strings.TrimSpace(s), compactSketchLimit); brief != "" {
			lines = append(lines, brief)
		}
	}
	body := strings.TrimSpace(strings.Join(lines, "\n"))
	if body == "" {
		body = i18n.T("(no extractable text)")
	}
	return textutil.Brief(body, compactBodyLimit)
}

func renderCompactNote(sketches []string) string {
	body := compactNoteBody(sketches)
	// The dropped text is user-controlled and may contain % verbs; Sprintf would eat them.
	return strings.Replace(i18n.T(compactNoteKey), "%s", body, 1)
}

func jsonChars(v any) int {
	raw, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(raw)
}

func charsFromJSON[T any](items []T) func(int) int {
	return func(from int) int {
		if from < 0 {
			from = 0
		}
		if from >= len(items) {
			return 0
		}
		return jsonChars(items[from:])
	}
}

func charsFromStrings(items []string) func(int) int {
	return func(from int) int {
		if from < 0 {
			from = 0
		}
		n := 0
		for _, s := range items[from:] {
			n += len(s)
		}
		return n
	}
}

func sketchJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	var parts []string
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case map[string]any:
			if s, ok := t["name"].(string); ok && s != "" {
				parts = append(parts, s)
			}
			if s, ok := t["text"].(string); ok && s != "" {
				parts = append(parts, s)
			}
			if s, ok := t["content"].(string); ok && s != "" {
				parts = append(parts, s)
			}
			for k, val := range t {
				if k == "name" || k == "text" {
					continue
				}
				walk(val)
			}
		case []any:
			for _, item := range t {
				walk(item)
			}
		}
	}
	var tree any
	if json.Unmarshal(raw, &tree) != nil {
		return ""
	}
	walk(tree)
	return strings.Join(parts, " · ")
}

func sketchesJSON[T any](dropped []T) []string {
	out := make([]string, 0, len(dropped))
	for _, m := range dropped {
		if s := sketchJSON(m); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func sketchStrings(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		out = append(out, textutil.Brief(strings.TrimSpace(line), compactSketchLimit))
	}
	return out
}

func applyTrim[T any](history *[]T, t *cutTracker, maxMessages, maxChars int, noteOf func(string) T, sketch func([]T) []string, charsFrom func([]T) func(int) int) {
	h := *history
	var nChars func(int) int
	if charsFrom != nil {
		nChars = charsFrom(h)
	} else {
		nChars = charsFromJSON(h)
	}
	p := t.pickTrim(len(h), maxMessages, maxChars, nChars)
	if p <= 0 {
		return
	}
	sketches := sketch(h[:p])
	note := renderCompactNote(sketches)
	t.note = note
	rest := append([]T(nil), h[p:]...)
	*history = append([]T{noteOf(note)}, rest...)
	t.rebase(p)
	t.shift(1)
}

func applyStringTrim(history *[]string, t *cutTracker, maxMessages, maxChars int) {
	applyTrim(history, t, maxMessages, maxChars, func(note string) string {
		return "user: " + note
	}, sketchStrings, charsFromStrings)
}
