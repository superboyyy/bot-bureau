package engine

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Chat history on disk.

// One rule: **a message disappears only when the user deletes the conversation**. The UI has to be able
// to scroll back to the start of a conversation however long ago it began and however much has piled up.
// How much is kept in memory is a separate question — that is about performance and must not decide what
// the user can see.

// It did not work this way. Events sat in one 4000-entry queue in memory, and every Emit serialised the
// whole array over events.json. Past 4000 the oldest 2000 were **overwritten for good**, with no backup
// and no archive — and only a quarter of those 4000 slots held actual messages (tool logs took half), so
// the rolling was steadily pushing what people said out of the way to make room for tool chatter.

// Now it is an append-only log, one event per line:

// - Appended, not rewritten. Every event used to re-marshal thousands of entries and write them all
// out; a few dozen Emits in one turn came to tens of megabytes. Now an event is a line.
// - Compaction keeps every msg and system event and only a recent window of the rest
// (tool/refresh/approval_done/status). What people said is the record; tool output is the process,
// and the process may be forgotten.
// - Scrolling back reads the file, not memory. Memory is only a cache of the tail.

const (

	// How many entries stay in memory. It only affects Recent and the SSE backfill — scrolling back
	// reads the file — so a smaller number loses nothing.
	memEvents = 2000

	// How many non-message events survive compaction. It runs only once the count reaches twice this,
	// so start-up does not rewrite the whole file every time.
	keepActivity = 4000
)

// Two kinds that do not survive a restart: busy/idle is a fact about right now, and a pending approval
// died with the process that was waiting on it.
func transientKind(kind string) bool { return kind == "status" || kind == "approval" }

// The two kinds a person reads back. Compaction never touches them.
func recordKind(kind string) bool { return kind == "msg" || kind == "system" }

// eventLog is the append-only log on disk.
type eventLog struct {
	path string
}

func newEventLog(path string) *eventLog { return &eventLog{path: path} }

// append writes one entry. A failure is not fatal: the event has already gone out to the UI, what is
// lost is one line of history, and breaking a conversation over one line of log would be worse.
func (l *eventLog) append(ev Event) {
	if l == nil || l.path == "" {
		return
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(raw, '\n'))
}

// scan walks the file start to end, handing each entry to fn. Unreadable lines are skipped: the last
// line of an append-only log can be torn (power lost mid-write), and condemning the whole history over
// that would make no sense.
func (l *eventLog) scan(fn func(Event)) error {
	if l == nil || l.path == "" {
		return nil
	}
	f, err := os.Open(l.path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)

	// One event can be long (someone pasted a wall of text), and the default 64KB line cap is not enough
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev Event
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		normalizeID(ev)
		fn(ev)
	}
	return sc.Err()
}

// normalizeID turns the float64 that JSON produces back into an int. Ids are ints everywhere in memory
// (EventsSince asserts e["id"].(int) outright), so anything read from the file has to be converted.
func normalizeID(ev Event) {
	if v, ok := ev["id"].(float64); ok {
		ev["id"] = int(v)
	}
}

// page returns the last limit entries of one conversation with an id below before, oldest first.
// A before of 0 means "from the newest". The second value reports whether anything older remains.

// It scans the whole file. That is slow, but it happens only when the user scrolls to the top, and it
// buys a log that can be appended to without maintaining an index. If the volume ever outgrows it, the
// answer is an index, not throwing history away.
func (l *eventLog) page(chat string, before, limit int) ([]Event, bool) {
	if limit <= 0 {
		limit = 100
	}
	var hits []Event
	_ = l.scan(func(ev Event) {
		if k, _ := ev["kind"].(string); transientKind(k) {
			return
		}
		if c, _ := ev["chat"].(string); c != chat {
			return
		}
		if before > 0 {
			if id, _ := ev["id"].(int); id >= before {
				return
			}
		}
		hits = append(hits, ev)
	})
	sort.SliceStable(hits, func(i, j int) bool {
		a, _ := hits[i]["id"].(int)
		b, _ := hits[j]["id"].(int)
		return a < b
	})
	if len(hits) > limit {
		return hits[len(hits)-limit:], true
	}
	return hits, false
}

// deleteChat removes one conversation's record from the log and reports how many entries went.

// This is the only path by which a message disappears. If keeping history is a promise, then deleting
// has to mean deleting — the old "messages in this group are kept" line only made sense back when
// messages were going to roll away regardless.
func (l *eventLog) deleteChat(chat string) int {
	if l == nil || l.path == "" || chat == "" {
		return 0
	}
	var kept []Event
	removed := 0
	if l.scan(func(ev Event) {
		if c, _ := ev["chat"].(string); c == chat {
			removed++
			return
		}
		kept = append(kept, ev)
	}) != nil {
		return 0
	}
	if removed == 0 {
		return 0
	}
	l.rewrite(kept)
	return removed
}

// compact runs only once the activity log has grown too far: messages and system notices are left
// alone and the rest keeps its most recent keepActivity entries. It reports whether it rewrote anything.
func (l *eventLog) compact() bool {
	if l == nil || l.path == "" {
		return false
	}
	var all []Event
	activity := 0
	if l.scan(func(ev Event) {
		all = append(all, ev)
		if k, _ := ev["kind"].(string); !recordKind(k) {
			activity++
		}
	}) != nil {
		return false
	}
	if activity <= keepActivity*2 {
		return false
	}
	drop := activity - keepActivity
	kept := make([]Event, 0, len(all))
	for _, ev := range all {
		if k, _ := ev["kind"].(string); !recordKind(k) && drop > 0 {
			drop--
			continue
		}
		kept = append(kept, ev)
	}
	l.rewrite(kept)
	return true
}

// rewrite replaces the file wholesale, via a temporary file and a rename, so losing power midway leaves
// a stray .tmp at worst and the original untouched.
func (l *eventLog) rewrite(evs []Event) {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return
	}
	tmp := l.path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return
	}
	w := bufio.NewWriter(f)
	for _, ev := range evs {
		raw, err := json.Marshal(ev)
		if err != nil {
			continue
		}
		w.Write(append(raw, '\n'))
	}
	if w.Flush() != nil || f.Close() != nil {
		os.Remove(tmp)
		return
	}
	_ = os.Rename(tmp, l.path)
}

// migrate moves the old events.json — one big array — into the append-only log, once.
// Somebody upgrading should not lose the history they already had just because the format changed.
func (l *eventLog) migrate(oldPath string) {
	if l == nil || l.path == "" {
		return
	}
	if _, err := os.Stat(l.path); err == nil {
		return // the new log is already there; leave it alone
	}
	raw, err := os.ReadFile(oldPath)
	if err != nil {
		return
	}
	var evs []Event
	if json.Unmarshal(raw, &evs) != nil {
		return
	}
	for _, ev := range evs {
		normalizeID(ev)
	}
	l.rewrite(evs)

	// The old file is renamed rather than deleted, so the original is still there if the move went wrong.
	_ = os.Rename(oldPath, oldPath+".migrated")
}
