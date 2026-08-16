package engine

import (
	"botbureau/backend/internal/i18n"

	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// BoardTask is one item on the task board: exactly one owner, so multiple bots don't duplicate the same work.
type BoardTask struct {
	ID        int    `json:"id"`
	Title     string `json:"title"`
	Detail    string `json:"detail"`
	Owner     string `json:"owner"`
	Status    string `json:"status"` // todo | doing | done
	Note      string `json:"note"`
	CreatedBy string `json:"created_by"`
	UpdatedAt int64  `json:"updated_at"`
}

var validStatus = map[string]bool{"todo": true, "doing": true, "done": true}

type TaskBoard struct {
	path   string
	mu     sync.Mutex
	tasks  map[int]*BoardTask
	nextID int
}

func NewTaskBoard(path string) *TaskBoard {
	b := &TaskBoard{path: path, tasks: map[int]*BoardTask{}, nextID: 1}
	raw, err := os.ReadFile(path)
	if err != nil {
		return b
	}
	var items []BoardTask
	if json.Unmarshal(raw, &items) != nil {
		return b
	}
	for i := range items {
		t := items[i]
		if t.ID <= 0 || t.Title == "" || !validStatus[t.Status] {
			continue
		}
		b.tasks[t.ID] = &t
		if t.ID >= b.nextID {
			b.nextID = t.ID + 1
		}
	}
	return b
}

func (b *TaskBoard) save() {
	_ = os.MkdirAll(filepath.Dir(b.path), 0o755)
	out, err := json.MarshalIndent(b.list(), "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(b.path, out, 0o644)
}

func (b *TaskBoard) list() []*BoardTask {
	items := make([]*BoardTask, 0, len(b.tasks))
	for _, t := range b.tasks {
		items = append(items, t)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items
}

func (b *TaskBoard) Add(createdBy, owner, title, detail string) *BoardTask {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := &BoardTask{
		ID: b.nextID, Title: strings.TrimSpace(title), Detail: strings.TrimSpace(detail),
		Owner: owner, Status: "todo", CreatedBy: createdBy, UpdatedAt: time.Now().Unix(),
	}
	b.nextID++
	b.tasks[t.ID] = t
	b.save()
	return t
}

func (b *TaskBoard) Update(id int, status, note string) (*BoardTask, error) {
	if status != "" && !validStatus[status] {
		return nil, errors.New(i18n.T("status must be todo/doing/done"))
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.tasks[id]
	if !ok {
		return nil, fmt.Errorf(i18n.T("There is no task #%d"), id)
	}
	if status != "" {
		t.Status = status
	}
	if note != "" {
		t.Note = note
	}
	t.UpdatedAt = time.Now().Unix()
	b.save()
	cp := *t
	return &cp, nil
}

func (b *TaskBoard) List() []BoardTask {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]BoardTask, 0, len(b.tasks))
	for _, t := range b.list() {
		out = append(out, *t)
	}
	return out
}

func (b *TaskBoard) ClearDone() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for id, t := range b.tasks {
		if t.Status == "done" {
			delete(b.tasks, id)
			n++
		}
	}
	if n > 0 {
		b.save()
	}
	return n
}

// Render renders the board as text for the model to read.
func (b *TaskBoard) Render() string {
	tasks := b.List()
	if len(tasks) == 0 {
		return i18n.T("(the board is empty)")
	}
	var sb strings.Builder
	for _, t := range tasks {
		fmt.Fprintf(&sb, i18n.T("#%d [%s] %s — owner: %s"), t.ID, t.Status, t.Title, t.Owner)
		if t.Detail != "" {
			fmt.Fprintf(&sb, i18n.T("; %s"), t.Detail)
		}
		if t.Note != "" {
			fmt.Fprintf(&sb, i18n.T(" (note: %s)"), t.Note)
		}
		sb.WriteString("\n")
	}
	return strings.TrimSpace(sb.String())
}
