package engine

import (
	"botbureau/backend/internal/i18n"
	"botbureau/backend/internal/textutil"

	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Memory is each bot's long-term memory: MEMORY.md in its working directory (or TEAM_MEMORY.md for
// the shared file). The prompt only lists id + first clause; recall loads a body.
type Memory struct {
	path string
	mu   sync.Mutex
}

func NewMemory(path string) *Memory { return &Memory{path: path} }

const (
	maxMemoryRoster = 40
	maxMemoryNote   = 2000
	maxRecallHits   = 8
	memoryIDLen     = 4
)

// MemEntry is one bullet in MEMORY.md.
type MemEntry struct {
	ID   string
	Date string
	Text string
}

var (
	memDateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	memIDRe   = regexp.MustCompile(`^[a-z0-9]{4,8}$`)
	memLineRe = regexp.MustCompile(`^-\s*(?:\[([^\]]+)\]\s*)?(?:\[([^\]]+)\]\s*)?(.*)$`)
)

func (m *Memory) Load() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	raw, err := os.ReadFile(m.path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// Roster is what belongs in the system prompt: id and a first clause, not the bodies.
func (m *Memory) Roster() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	entries, _, err := m.readLocked()
	if err != nil || len(entries) == 0 {
		return ""
	}
	start := 0
	var b strings.Builder
	if len(entries) > maxMemoryRoster {
		extra := len(entries) - maxMemoryRoster
		start = extra
		fmt.Fprintf(&b, i18n.T("… %d older notes; call recall to search.\n"), extra)
	}
	for _, e := range entries[start:] {
		fmt.Fprintf(&b, "%s: %s\n", e.ID, firstClause(e.Text))
	}
	return b.String()
}

// Append is remember without an id: keep a note, or no-op if it is already there.
func (m *Memory) Append(note string) error {
	_, _, err := m.Remember(note, "")
	return err
}

// Remember inserts note, or replaces the entry named by id. existed is true when the same body was
// already stored and nothing was written.
func (m *Memory) Remember(note, id string) (assigned string, existed bool, err error) {
	note = strings.TrimSpace(note)
	if note == "" {
		return "", false, fmt.Errorf("%s", i18n.T("note cannot be empty"))
	}
	note = textutil.Brief(note, maxMemoryNote)
	id = strings.ToLower(strings.TrimSpace(id))
	if id != "" && !memIDRe.MatchString(id) {
		return "", false, fmt.Errorf(i18n.T("memory id %q must be 4-8 letters or digits"), id)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	entries, _, err := m.readLocked()
	if err != nil {
		return "", false, err
	}
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.ID] = true
	}

	if id != "" {
		for i, e := range entries {
			if e.ID == id {
				entries[i].Text = note
				entries[i].Date = today()
				if err := m.writeLocked(entries); err != nil {
					return "", false, err
				}
				return id, false, nil
			}
		}
		entries = append(entries, MemEntry{ID: id, Date: today(), Text: note})
		if err := m.writeLocked(entries); err != nil {
			return "", false, err
		}
		return id, false, nil
	}

	for _, e := range entries {
		if strings.EqualFold(strings.TrimSpace(e.Text), note) {
			return e.ID, true, nil
		}
	}
	assigned = newMemoryID(seen)
	entries = append(entries, MemEntry{ID: assigned, Date: today(), Text: note})
	if err := m.writeLocked(entries); err != nil {
		return "", false, err
	}
	return assigned, false, nil
}

// Forget removes the note with this id. Unknown ids are an error so the model can tell a typo from success.
func (m *Memory) Forget(id string) error {
	id = strings.ToLower(strings.TrimSpace(id))
	if !memIDRe.MatchString(id) {
		return fmt.Errorf(i18n.T("memory id %q must be 4-8 letters or digits"), id)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entries, _, err := m.readLocked()
	if err != nil {
		return err
	}
	kept := make([]MemEntry, 0, len(entries))
	found := false
	for _, e := range entries {
		if e.ID == id {
			found = true
			continue
		}
		kept = append(kept, e)
	}
	if !found {
		return fmt.Errorf(i18n.T("there is no note %s"), id)
	}
	return m.writeLocked(kept)
}

// Recall returns notes whose id equals query, or whose body contains it. Empty query is an error at the tool.
func (m *Memory) Recall(query string, max int) []MemEntry {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil
	}
	if max <= 0 {
		max = maxRecallHits
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entries, _, err := m.readLocked()
	if err != nil {
		return nil
	}
	ql := strings.ToLower(query)
	var hits []MemEntry
	for _, e := range entries {
		if strings.EqualFold(e.ID, query) || strings.Contains(strings.ToLower(e.Text), ql) {
			hits = append(hits, e)
			if len(hits) >= max {
				break
			}
		}
	}
	return hits
}

func (m *Memory) readLocked() ([]MemEntry, bool, error) {
	raw, err := os.ReadFile(m.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	entries, assigned := parseMemory(string(raw))
	return entries, assigned, nil
}

func (m *Memory) writeLocked(entries []MemEntry) error {
	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
		return err
	}
	if len(entries) == 0 {
		_ = os.Remove(m.path)
		return nil
	}
	return os.WriteFile(m.path, []byte(renderMemory(entries)), 0o644)
}

func parseMemory(raw string) ([]MemEntry, bool) {
	seen := map[string]bool{}
	var entries []MemEntry
	assigned := false
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "-") {
			continue
		}
		e, ok := parseMemoryLine(line)
		if !ok || strings.TrimSpace(e.Text) == "" {
			continue
		}
		if e.ID == "" || seen[e.ID] || !memIDRe.MatchString(e.ID) {
			e.ID = legacyMemoryID(e.Text, seen)
			assigned = true
		}
		seen[e.ID] = true
		entries = append(entries, e)
	}
	return entries, assigned
}

func parseMemoryLine(line string) (MemEntry, bool) {
	m := memLineRe.FindStringSubmatch(line)
	if m == nil {
		return MemEntry{}, false
	}
	g1, g2, rest := strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), strings.TrimSpace(m[3])
	var e MemEntry
	switch {
	case memDateRe.MatchString(g1):
		e.Date = g1
		if g2 != "" {
			rest = "[" + g2 + "] " + rest
		}
	case g1 != "":
		e.ID = strings.ToLower(g1)
		if memDateRe.MatchString(g2) {
			e.Date = g2
		} else if g2 != "" {
			rest = "[" + g2 + "] " + rest
		}
	}
	e.Text = strings.TrimSpace(rest)
	return e, true
}

func renderMemory(entries []MemEntry) string {
	var b strings.Builder
	for _, e := range entries {
		date := e.Date
		if date == "" {
			date = today()
		}
		fmt.Fprintf(&b, "- [%s] [%s] %s\n", e.ID, date, e.Text)
	}
	return b.String()
}

func firstClause(s string) string {
	s = strings.TrimSpace(s)
	cut := -1
	for i, r := range s {
		if r == '\n' {
			cut = i
			break
		}
		if i >= 12 && (r == '.' || r == '。' || r == ';' || r == '；') {
			cut = i
			if r == '.' || r == '。' {
				cut = i + 1
			}
			break
		}
	}
	if cut > 0 {
		s = strings.TrimSpace(s[:cut])
	}
	return textutil.Brief(s, 80)
}

func legacyMemoryID(text string, seen map[string]bool) string {
	sum := sha256.Sum256([]byte(text))
	hexed := hex.EncodeToString(sum[:])
	for n := 4; n <= 8; n++ {
		id := hexed[:n]
		if !seen[id] {
			return id
		}
	}
	return hexed[:8]
}

func newMemoryID(seen map[string]bool) string {
	for n := 0; n < 32; n++ {
		id := randomHex(memoryIDLen)
		if !seen[id] {
			return id
		}
	}
	return randomHex(8)
}

func randomHex(chars int) string {
	raw := make([]byte, (chars+1)/2)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())[:chars]
	}
	s := hex.EncodeToString(raw)
	if len(s) > chars {
		s = s[:chars]
	}
	return s
}

func today() string { return time.Now().Format("2006-01-02") }
