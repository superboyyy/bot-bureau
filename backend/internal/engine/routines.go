package engine

import (
	"botbureau/backend/internal/i18n"

	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Cap at one year: blocks absurd input and also guards against time overflow
const maxEveryMinutes = 60 * 24 * 365

// Routine is a scheduled task: recurring work dispatched to a bot when it comes due.
type Routine struct {
	Name         string `json:"name"`
	Bot          string `json:"bot"`
	Prompt       string `json:"prompt"`
	EveryMinutes int    `json:"every_minutes"`
	NextRun      int64  `json:"next_run"` // epoch seconds
}

// Scheduler checks every few seconds; each routine that comes due is delivered to its bot as a group-chat message.
type Scheduler struct {
	bus      *Bus
	path     string
	mu       sync.Mutex
	routines map[string]*Routine
	stop     chan struct{}
}

func NewScheduler(bus *Bus, path string) *Scheduler {
	s := &Scheduler{bus: bus, path: path, routines: map[string]*Routine{}, stop: make(chan struct{})}
	s.load()
	return s
}

func clampEvery(v int) int {
	if v < 1 {
		return 1
	}
	if v > maxEveryMinutes {
		return maxEveryMinutes
	}
	return v
}

// ---- Persistence (the file may have been hand-edited: validate each entry, skip bad ones) ----

func (s *Scheduler) load() {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		fmt.Fprintf(os.Stderr, i18n.T("%s is not a valid routine list, ignoring its contents\n"), s.path)
		return
	}
	for _, it := range items {
		name, _ := it["name"].(string)
		bot, _ := it["bot"].(string)
		prompt, _ := it["prompt"].(string)
		every, okE := it["every_minutes"].(float64)
		next, okN := it["next_run"].(float64)
		if name == "" || bot == "" || prompt == "" || !okE || !okN {
			fmt.Fprintf(os.Stderr, i18n.T("Skipping a bad entry in routines.json: %v\n"), it)
			continue
		}
		s.routines[name] = &Routine{
			Name: name, Bot: bot, Prompt: prompt,
			EveryMinutes: clampEvery(int(every)), NextRun: int64(next),
		}
	}
}

func (s *Scheduler) save() {
	_ = os.MkdirAll(filepath.Dir(s.path), 0o755)
	out, err := json.MarshalIndent(s.list(), "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(s.path, out, 0o644)
}

func (s *Scheduler) list() []*Routine {
	items := make([]*Routine, 0, len(s.routines))
	for _, r := range s.routines {
		items = append(items, r)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].NextRun < items[j].NextRun })
	return items
}

// ---- Add / remove / list ----

func (s *Scheduler) Add(name, bot, prompt string, everyMinutes int) *Routine {
	everyMinutes = clampEvery(everyMinutes)
	r := &Routine{
		Name: name, Bot: bot, Prompt: prompt,
		EveryMinutes: everyMinutes,
		NextRun:      time.Now().Unix() + int64(everyMinutes)*60,
	}
	s.mu.Lock()
	s.routines[name] = r
	s.save()
	s.mu.Unlock()
	return r
}

func (s *Scheduler) Remove(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.routines[name]; !ok {
		return false
	}
	delete(s.routines, name)
	s.save()
	return true
}

// RemoveByBot deletes every routine assigned to a bot and returns how many went.

// When someone leaves, the schedules pointing at them should stop. Keeping them waits for a successor
// who can never arrive — ids are one-shot random strings and no new member will ever carry this one
// again — and only drips "routine skipped: its assignee does not exist" into the group forever. Work
// files are what they produced and are kept; a routine is live behaviour and is revoked.
func (s *Scheduler) RemoveByBot(bot string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for name, r := range s.routines {
		if r.Bot == bot {
			delete(s.routines, name)
			n++
		}
	}
	if n > 0 {
		s.save()
	}
	return n
}

// Reassign hands a routine to a different bot.

// Until now the only way to "transfer" one was sideways: have the other member save it again under
// the same name and let Add overwrite the entry wholesale (the "same name is overwritten" line in the
// tool description). That asked the user to know an implementation detail and to restate the prompt
// word for word — impossible from a UI that truncates it at 90 characters.
func (s *Scheduler) Reassign(name, bot string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.routines[name]
	if !ok {
		return false
	}

	// Reassigning does not reset the clock: it changes whose job this is, not when it happens
	r.Bot = bot
	s.save()
	return true
}

func (s *Scheduler) List() []*Routine {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Routine, 0, len(s.routines))
	for _, r := range s.routines {
		cp := *r
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NextRun < out[j].NextRun })
	return out
}

// ---- Scheduling loop ----

// tick runs one round: delivers due routines and advances NextRun.
func (s *Scheduler) tick(now int64) {
	var due []*Routine
	s.mu.Lock()
	for _, r := range s.routines {
		if r.NextRun <= now {
			cp := *r
			due = append(due, &cp)

			// Advance arithmetically (rather than accumulating in a loop): this never loops forever regardless of the data,
			// and a backlogged trigger point jumps straight to the next future moment
			step := int64(clampEvery(r.EveryMinutes)) * 60
			r.NextRun += ((now-r.NextRun)/step + 1) * step
		}
	}
	if len(due) > 0 {
		s.save()
	}
	s.mu.Unlock()
	for _, r := range due {
		if s.bus.Deliver("routine:"+r.Name, r.Bot, "group", r.Prompt, true) {
			s.bus.Emit("system", "group", "scheduler",
				fmt.Sprintf(i18n.T("Routine \"%s\" fired, assigned to %s"), r.Name, r.Bot), nil)
		} else {
			s.bus.Emit("system", "group", "scheduler",
				fmt.Sprintf(i18n.T("Routine \"%s\" was skipped: its assignee %s does not exist"), r.Name, r.Bot), nil)
		}
	}
}

func (s *Scheduler) Start() {
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-ticker.C:
				func() {
					defer func() { // the scheduling loop must never die
						if r := recover(); r != nil {
							s.bus.Emit("system", "", "scheduler",
								fmt.Sprintf(i18n.T("Scheduler error: %v"), r), nil)
						}
					}()
					s.tick(time.Now().Unix())
				}()
			}
		}
	}()
}
