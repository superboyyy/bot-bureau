package netx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.WriteHeader(200)
		_, _ = rw.Write([]byte("reached"))
	})
}

// 配对码不再从 query 走。这才是这次改动的要点：URL 会被反代原样记进日志，
// 长期凭据落在那里就是泄漏。
//
// The pairing code is no longer accepted from the query. That is the point of this change: URLs land
// verbatim in proxy logs, and a long-lived credential sitting there is a leak.
func TestPairingCodeNotAcceptedInQuery(t *testing.T) {
	h := RequireToken("master-secret", okHandler())
	srv := httptest.NewServer(h)
	defer srv.Close()

	for _, path := range []string{"/api/state", EventsPath} {
		res, err := http.Get(srv.URL + path + "?token=master-secret")
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != 401 {
			t.Fatalf("%s?token=<master> should be refused, got %d", path, res.StatusCode)
		}
	}

	// 头里带着照常放行 / it still passes in the header
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/state", nil)
	req.Header.Set("Authorization", "Bearer master-secret")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("the header form should pass, got %d", res.StatusCode)
	}
}

// 换票要认证，票据只对消息流有效，别的路径一概不认。
// Minting requires auth, and a ticket works on the message stream alone.
func TestTicketScopeAndAuth(t *testing.T) {
	srv := httptest.NewServer(RequireToken("master-secret", okHandler()))
	defer srv.Close()

	// 没有配对码换不到票 / no pairing code, no ticket
	res, err := http.Post(srv.URL+SSETicketPath, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatalf("minting without the pairing code should be 401, got %d", res.StatusCode)
	}

	ticket := mint(t, srv.URL, "master-secret")
	if ticket == "" || ticket == "master-secret" {
		t.Fatalf("a ticket must be its own secret, got %q", ticket)
	}

	// 票据在消息流上有效 / valid on the stream
	res, err = http.Get(srv.URL + EventsPath + "?ticket=" + ticket)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("a ticket should open the stream, got %d", res.StatusCode)
	}

	// 票据换不来别的路径 / and buys nothing else
	for _, path := range []string{"/api/state", "/api/send", "/api/bots"} {
		res, err := http.Get(srv.URL + path + "?ticket=" + ticket)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != 401 {
			t.Fatalf("a ticket must not open %s, got %d", path, res.StatusCode)
		}
	}

	// 伪造的票据不认 / a made-up ticket is refused
	res, err = http.Get(srv.URL + EventsPath + "?ticket=deadbeefdeadbeef")
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatalf("a forged ticket should be 401, got %d", res.StatusCode)
	}
}

// 票据会过期；有效期内可重复使用（EventSource 的内部重连不经过我们的代码）。
// Tickets expire, and stay reusable inside their window (EventSource's internal reconnects never reach our code).
func TestTicketExpiryAndReuse(t *testing.T) {
	s := newTicketStore()
	tok, err := s.issue()
	if err != nil {
		t.Fatal(err)
	}
	if !s.valid(tok) || !s.valid(tok) {
		t.Fatal("a ticket should be reusable within its window")
	}
	s.mu.Lock()
	s.expires[tok] = time.Now().Add(-time.Second)
	s.mu.Unlock()
	if s.valid(tok) {
		t.Fatal("an expired ticket must be refused")
	}
	if s.valid("") {
		t.Fatal("an empty ticket must be refused")
	}
	if TicketTTL > 15*time.Minute {
		t.Fatalf("the TTL should stay short; it is %s", TicketTTL)
	}
}

func mint(t *testing.T, base, token string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+SSETicketPath, strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("minting should succeed, got %d", res.StatusCode)
	}
	var body struct {
		Ticket    string `json:"ticket"`
		ExpiresIn int    `json:"expires_in"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.ExpiresIn <= 0 {
		t.Fatalf("expires_in should be reported, got %d", body.ExpiresIn)
	}
	return body.Ticket
}
