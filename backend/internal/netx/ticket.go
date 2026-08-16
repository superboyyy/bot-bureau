package netx

// SSE tickets.

// The pairing code should only ever travel in the Authorization header — headers are rarely logged,
// URLs are logged everywhere. But the message stream is a browser EventSource, and the EventSource API
// cannot set custom headers (switching to WebSocket does not help; the browser API cannot set them
// either). That one connection is therefore forced to carry its credential in the query string, which
// a reverse proxy writes verbatim into its access log — logs that then rotate, get backed up and ship
// to central logging. On a public deployment behind a proxy that leaves the pairing code sitting in
// plain text in a log file.

// So the pairing code is no longer accepted from the query at all. A client first exchanges it, over a
// POST that can set headers, for a short-lived ticket; only the ticket ever appears in a URL. A ticket
// works solely on /api/events and expires in ten minutes, so one that reaches a log is long dead.

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// TicketTTL is how long a ticket stays valid.

// Ten minutes: long enough to survive EventSource's own reconnection attempts (it retries internally
// before our code ever runs, where a single-use ticket would simply 401), and short enough that a
// ticket found in a log is useless by the time anyone reads it.
const TicketTTL = 10 * time.Minute

type ticketStore struct {
	mu      sync.Mutex
	expires map[string]time.Time
}

func newTicketStore() *ticketStore {
	return &ticketStore{expires: map[string]time.Time{}}
}

// issue mints a ticket and sweeps expired ones on the way.
func (s *ticketStore) issue() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(buf[:])

	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for t, exp := range s.expires {
		if now.After(exp) {
			delete(s.expires, t)
		}
	}
	s.expires[tok] = now.Add(TicketTTL)
	return tok, nil
}

// valid reports whether a ticket is still live. Tickets are reusable within their window:
// EventSource's internal reconnects never pass through our code, and a single-use ticket would break
// the connection right there.
func (s *ticketStore) valid(tok string) bool {
	if tok == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.expires[tok]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.expires, tok)
		return false
	}
	return true
}
