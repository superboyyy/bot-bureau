package model

import (
	"context"
	"encoding/json"
	"testing"
)

// ---- Scripted provider for tests: returns StepResult from a preset queue ----

type scriptedProvider struct{ script []StepResult }

func (p *scriptedProvider) SupportsWebTools() bool { return false }
func (p *scriptedProvider) Label() string          { return "scripted" }
func (p *scriptedProvider) NewSession() Session    { return &scriptedSession{script: &p.script} }

type scriptedSession struct {
	script      *[]StepResult
	history     []string
	tracker     cutTracker
	toolResults [][]ToolResult
}

func (s *scriptedSession) MarkTurn() { s.tracker.markTurn(len(s.history)) }
func (s *scriptedSession) Rollback() { s.history = s.history[:s.tracker.rollbackTo()] }
func (s *scriptedSession) AddUser(text string, _ ...ResultImage) {
	s.tracker.addCut(len(s.history))
	s.history = append(s.history, "user:"+text)
}
func (s *scriptedSession) AddToolResults(rs []ToolResult) {
	s.toolResults = append(s.toolResults, rs)
	s.history = append(s.history, "toolresults")
}
func (s *scriptedSession) Trim(limit int) {
	if p := s.tracker.trimPoint(len(s.history), limit); p > 0 {
		s.history = append([]string(nil), s.history[p:]...)
	}
}
func (s *scriptedSession) Snapshot() json.RawMessage {
	return packSession("scripted", s.history, s.tracker)
}
func (s *scriptedSession) Restore(raw json.RawMessage) bool {
	var hist []string
	t, ok := unpackSession(raw, "scripted", &hist)
	if !ok {
		return false
	}
	s.history = hist
	s.tracker = t
	return true
}
func (s *scriptedSession) Step(ctx context.Context, system string, tools []ToolDef, includeWeb bool) (StepResult, error) {
	if len(*s.script) == 0 {
		return StepResult{StopReason: "end_turn"}, nil
	}
	res := (*s.script)[0]
	*s.script = (*s.script)[1:]
	s.history = append(s.history, "assistant")
	return res, nil
}

func TestCutTrackerTrim(t *testing.T) {
	tr := &cutTracker{}
	n := 0
	// toolresults
	// 3 entries per turn: user / assistant / toolresults
	for i := 0; i < 30; i++ {
		tr.addCut(n)
		n += 3
	}
	p := tr.trimPoint(n, 10)
	if p == 0 || p%3 != 0 {
		t.Fatalf("the trim point should land on a turn boundary: %d", p)
	}
	if n-p > 12 {
		t.Fatalf("after trimming it should not exceed limit + turn size: %d left", n-p)
	}
}

// ---- Routine robustness ----
