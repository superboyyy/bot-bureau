package engine

import (
	"botbureau/backend/internal/i18n"
	"botbureau/backend/internal/textutil"

	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const maxTodos = 40

// TodoItem is one line on a member's personal checklist. It is not a group-board task:
// there is no owner other than this member, and it never assigns work to someone else.
type TodoItem struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Status  string `json:"status"` // pending | done
}

var (
	todoLineRe = regexp.MustCompile(`^-\s*\[([ xX])\]\s+(?:([a-z0-9_-]{1,32}):\s*)?(.+)$`)
	todoIDRe   = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)
)

func todoPath(workspace string) string {
	return filepath.Join(workspace, "TODO.md")
}

func LoadTodos(workspace string) []TodoItem {
	raw, err := os.ReadFile(todoPath(workspace))
	if err != nil {
		return []TodoItem{}
	}
	items := make([]TodoItem, 0)
	seen := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		m := todoLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		status := "pending"
		if m[1] == "x" || m[1] == "X" {
			status = "done"
		}
		content := strings.TrimSpace(m[3])
		id := strings.TrimSpace(m[2])
		if id == "" {
			id = slugTodoID(content, seen)
		}
		if seen[id] {
			id = slugTodoID(id+"-2", seen)
		}
		seen[id] = true
		items = append(items, TodoItem{ID: id, Content: content, Status: status})
		if len(items) >= maxTodos {
			break
		}
	}
	return items
}

func SaveTodos(workspace string, items []TodoItem) error {
	path := todoPath(workspace)
	if len(items) == 0 {
		_ = os.Remove(path)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# Todos\n")
	for _, it := range items {
		mark := " "
		if it.Status == "done" {
			mark = "x"
		}
		fmt.Fprintf(&b, "- [%s] %s: %s\n", mark, it.ID, it.Content)
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func slugTodoID(content string, seen map[string]bool) string {
	s := strings.ToLower(strings.TrimSpace(content))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_':
			if b.Len() > 0 && b.String()[b.Len()-1] != '-' {
				b.WriteByte('-')
			}
		}
		if b.Len() >= 24 {
			break
		}
	}
	id := strings.Trim(b.String(), "-")
	if id == "" {
		id = "item"
	}
	base := id
	for i := 2; seen[id]; i++ {
		id = fmt.Sprintf("%s-%d", base, i)
	}
	return id
}

func parseTodoItems(v any) ([]TodoItem, error) {
	raw, ok := v.([]any)
	if !ok {
		return nil, errors.New(i18n.T("todo_write items must be an array"))
	}
	if len(raw) > maxTodos {
		return nil, fmt.Errorf(i18n.T("todo_write accepts at most %d items"), maxTodos)
	}
	seen := map[string]bool{}
	items := make([]TodoItem, 0, len(raw))
	for i, row := range raw {
		m, ok := row.(map[string]any)
		if !ok {
			return nil, fmt.Errorf(i18n.T("todo_write item %d is not an object"), i+1)
		}
		content, _ := m["content"].(string)
		content = strings.TrimSpace(content)
		if content == "" {
			return nil, fmt.Errorf(i18n.T("todo_write item %d needs content"), i+1)
		}
		id, _ := m["id"].(string)
		id = strings.ToLower(strings.TrimSpace(id))
		if id == "" {
			id = slugTodoID(content, seen)
		}
		if !todoIDRe.MatchString(id) {
			return nil, fmt.Errorf(i18n.T("todo_write id %q must be 1-32 letters, digits, - or _"), id)
		}
		if seen[id] {
			return nil, fmt.Errorf(i18n.T("todo_write id %q is used twice"), id)
		}
		seen[id] = true
		status, _ := m["status"].(string)
		status = strings.ToLower(strings.TrimSpace(status))
		switch status {
		case "", "pending":
			status = "pending"
		case "done":
		default:
			return nil, fmt.Errorf(i18n.T("todo_write status must be pending or done, not %q"), status)
		}
		items = append(items, TodoItem{
			ID:      id,
			Content: textutil.Brief(content, 400),
			Status:  status,
		})
	}
	return items, nil
}

func RenderTodoSection(items []TodoItem) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(i18n.T(`
# Todos
Your current personal checklist — not the group task board. Call todo_write to replace the whole list. Mark an item done when you finish it.
`))
	for _, it := range items {
		mark := " "
		if it.Status == "done" {
			mark = "x"
		}
		fmt.Fprintf(&b, "- [%s] %s: %s\n", mark, it.ID, it.Content)
	}
	return b.String()
}

func (w *BotWorker) Todos() []TodoItem {
	return LoadTodos(w.workspace)
}
