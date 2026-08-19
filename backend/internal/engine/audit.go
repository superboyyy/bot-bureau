package engine

import (
	"botbureau/backend/internal/config"

	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"
)

// AuditLog is the append-only record in data/audit.jsonl. events.jsonl may drop old tool chatter;
// this file does not. One line per bash, write, edit, non-read-only plugin call, and the approval
// decision that went with it (or allowed:true when the tier did not ask).

type AuditLog struct {
	path string
	mu   sync.Mutex
}

type auditLine struct {
	TS       int64  `json:"ts"`
	ID       int    `json:"id,omitempty"`
	Bot      string `json:"bot"`
	Act      string `json:"act"`
	Path     string `json:"path,omitempty"`
	Command  string `json:"command,omitempty"`
	Original string `json:"original,omitempty"`
	Allowed  bool   `json:"allowed"`
	Reason   string `json:"reason,omitempty"`
}

func NewAuditLog(path string) *AuditLog {
	if path == "" {
		return nil
	}
	return &AuditLog{path: path}
}

func (l *AuditLog) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

func (l *AuditLog) Record(rec auditLine) {
	if l == nil || l.path == "" {
		return
	}
	if rec.TS == 0 {
		rec.TS = time.Now().Unix()
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(raw, '\n'))
}

func auditKind(act config.ToolAct, action string) string {
	switch act.Kind {
	case config.ActBash:
		return "bash"
	case config.ActPlugin:
		return "plugin"
	case config.ActWrite:
		if strings.HasPrefix(action, "edit_file:") {
			return "edit"
		}
		return "write"
	}
	return string(act.Kind)
}

func (t *Toolbox) shouldAudit(act config.ToolAct) bool {
	switch act.Kind {
	case config.ActBash, config.ActWrite:
		return true
	case config.ActPlugin:
		return !act.ReadOnly
	}
	return false
}
