package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSchedulerCRUDClampAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routines.json")
	s := NewScheduler(NewBus(), path)
	short := s.Add("short", "wren", "check", 0)
	if short.EveryMinutes != 1 {
		t.Fatalf("minimum interval = %d, want 1", short.EveryMinutes)
	}
	long := s.Add("long", "coder", "ship", maxEveryMinutes+1)
	if long.EveryMinutes != maxEveryMinutes {
		t.Fatalf("maximum interval = %d, want %d", long.EveryMinutes, maxEveryMinutes)
	}
	if !s.Reassign("short", "coder") || s.Reassign("missing", "coder") {
		t.Fatal("Reassign behavior is wrong")
	}
	if !s.Remove("long") || s.Remove("long") {
		t.Fatal("Remove behavior is wrong")
	}
	s.Add("other", "other-bot", "other", 5)
	if got := s.RemoveByBot("coder"); got != 1 {
		t.Fatalf("RemoveByBot() = %d, want 1", got)
	}
	if got := len(s.List()); got != 1 {
		t.Fatalf("remaining routines = %d, want 1", got)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	reloaded := NewScheduler(NewBus(), path)
	items := reloaded.List()
	if len(items) != 1 || items[0].Name != "other" {
		t.Fatalf("reloaded routines = %#v", items)
	}
}

func TestSchedulerTickAdvancesAndReportsMissingAssignee(t *testing.T) {
	bus := NewBus()
	s := NewScheduler(bus, filepath.Join(t.TempDir(), "routines.json"))
	s.routines["due"] = &Routine{Name: "due", Bot: "missing", Prompt: "run", EveryMinutes: 5, NextRun: 100}
	s.tick(1000)
	items := s.List()
	if len(items) != 1 || items[0].NextRun <= 1000 {
		t.Fatalf("due routine was not advanced: %#v", items)
	}
	seenSkipped := false
	for _, ev := range bus.Recent(10) {
		if ev["kind"] == "system" && ev["source"] == "scheduler" {
			seenSkipped = true
		}
	}
	if !seenSkipped {
		t.Fatal("missing assignee did not produce scheduler event")
	}
}
