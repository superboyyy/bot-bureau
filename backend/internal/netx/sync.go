package netx

// Multi-client (serverless) support:
// - The engine listens on the LAN and advertises via mDNS (_botbureau._tcp); clients on the same network auto-discover it and connect directly;
// - Pairing-code auth: every API endpoint except /api/ping requires the Authorization header; /api/events may instead use a short-lived SSE ticket;
// - Engine lock: prevents two devices from running the engine at once when the data directory lives on a sync drive (iCloud/Syncthing).

import (
	"botbureau/backend/internal/httpx"

	"botbureau/backend/internal/i18n"

	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	zeroconf "github.com/libp2p/zeroconf/v2"
)

// ---- Pairing code ----

func LoadOrCreateToken(dataDir string) (string, error) {
	p := filepath.Join(dataDir, "token")
	if raw, err := os.ReadFile(p); err == nil {
		if tok := strings.TrimSpace(string(raw)); tok != "" {
			return tok, nil
		}
	}
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(buf)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(p, []byte(tok+"\n"), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}

// SSETicketPath is where a ticket comes from: it demands the Authorization header, so a web page cannot get one.
const SSETicketPath = "/api/sse-ticket"

// EventsPath is the only path that accepts a ticket.
const EventsPath = "/api/events"

// RequireToken gates the whole API behind the pairing code; /, /api/ping and CORS preflight are exempt.

// The pairing code is accepted from the Authorization header only. The message stream cannot send
// headers (an EventSource limitation), so it uses a short-lived ticket: POST /api/sse-ticket to get
// one, and it works solely on /api/events. The pairing code itself therefore never appears in any URL
// and never lands in a reverse proxy's access log.
func RequireToken(token string, next http.Handler) http.Handler {
	tickets := newTicketStore()
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions || r.URL.Path == "/" || r.URL.Path == "/api/ping" {
			next.ServeHTTP(rw, r)
			return
		}

		// A ticket serves the message stream only; every other path still wants the pairing code
		if r.URL.Path == EventsPath && tickets.valid(r.URL.Query().Get("ticket")) {
			next.ServeHTTP(rw, r)
			return
		}

		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			rw.Header().Set("Access-Control-Allow-Origin", "*")
			httpx.WriteJSON(rw, 401, map[string]any{"error": i18n.T("Pairing code required")})
			return
		}

		if r.URL.Path == SSETicketPath {
			tok, err := tickets.issue()
			if err != nil {
				rw.Header().Set("Access-Control-Allow-Origin", "*")
				httpx.WriteJSON(rw, 500, map[string]any{"error": i18n.T("Failed to issue a stream ticket")})
				return
			}
			rw.Header().Set("Access-Control-Allow-Origin", "*")
			httpx.WriteJSON(rw, 200, map[string]any{"ticket": tok, "expires_in": int(TicketTTL.Seconds())})
			return
		}
		next.ServeHTTP(rw, r)
	})
}

// ---- mDNS advertising ----

func AdvertiseMDNS(port int) (func(), error) {
	host, _ := os.Hostname()

	// The advertised name carries the port: one machine can run several engines (different data
	// directories — see BOTBUREAU_DATA_DIR), and with the hostname alone they share one mDNS instance
	// name, so whichever registers last displaces the other and only one is ever discoverable.
	instance := fmt.Sprintf("botbureau@%s:%d", strings.TrimSuffix(host, ".local"), port)
	server, err := zeroconf.Register(instance, "_botbureau._tcp", "local.", port,
		[]string{"app=botbureau", "v=0.1.0"}, nil)
	if err != nil {
		return nil, err
	}
	return server.Shutdown, nil
}

// ---- Engine lock (prevents two engines from using the same data directory at once) ----

const (
	lockStaleAfter    = 30 * time.Second
	lockHeartbeatTick = 10 * time.Second
)

type EngineLock struct {
	path string
	stop chan struct{}
}

func AcquireEngineLock(dataDir string) (*EngineLock, error) {
	p := filepath.Join(dataDir, "engine.lock")
	if info, err := os.Stat(p); err == nil && time.Since(info.ModTime()) < lockStaleAfter {
		raw, _ := os.ReadFile(p)
		return nil, fmt.Errorf(
			i18n.T("Another engine is already using this data directory (%s). If it lives on a sync folder, run the engine on one device only and connect the others as clients; once you are sure no engine is running you can delete %s"),
			strings.TrimSpace(string(raw)), p)
	}
	host, _ := os.Hostname()
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(p, []byte(fmt.Sprintf("%s pid=%d", host, os.Getpid())), 0o644); err != nil {
		return nil, err
	}
	l := &EngineLock{path: p, stop: make(chan struct{})}
	go func() {
		t := time.NewTicker(lockHeartbeatTick)
		defer t.Stop()
		for {
			select {
			case <-l.stop:
				return
			case <-t.C:
				now := time.Now()
				_ = os.Chtimes(l.path, now, now)
			}
		}
	}()
	return l, nil
}

func (l *EngineLock) Release() {
	select {
	case <-l.stop:
	default:
		close(l.stop)
	}
	_ = os.Remove(l.path)
}
