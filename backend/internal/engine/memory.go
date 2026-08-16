package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Memory is each bot's long-term memory: MEMORY.md in its working directory, preserved across sessions.
type Memory struct {
	path string
	mu   sync.Mutex
}

func NewMemory(path string) *Memory { return &Memory{path: path} }

func (m *Memory) Load() string {
	raw, err := os.ReadFile(m.path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func (m *Memory) Append(note string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(m.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "- [%s] %s\n", time.Now().Format("2006-01-02"), strings.TrimSpace(note))
	return err
}
