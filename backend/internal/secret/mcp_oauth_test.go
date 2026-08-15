package secret

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeAS 是一个最小的「受保护资源 + 授权服务器」：发现文档、动态注册、令牌端点齐活。
// 它按规范校验 PKCE——不校验的话，这个测试就只是在确认我们发出去的字段拼对了，
// 而不是确认这套流程真的能换到令牌。
//
// fakeAS is a minimal "protected resource plus authorization server": discovery documents, dynamic
// registration and a token endpoint. It verifies PKCE the way the spec requires — without that, the test
// would only confirm the fields we send are spelled right, not that the flow actually yields a token.
type fakeAS struct {
	srv        *httptest.Server
	challenge  string
	issuedCode string
	registered int
	tokenCalls int
}

func newFakeAS(t *testing.T) *fakeAS {
	t.Helper()
	f := &fakeAS{issuedCode: "the-code"}
	mux := http.NewServeMux()

	mux.HandleFunc("/mcp", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("WWW-Authenticate",
			fmt.Sprintf(`Bearer realm="mcp", resource_metadata="%s/.well-known/oauth-protected-resource"`, f.srv.URL))
		rw.WriteHeader(401)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(rw http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"resource":              f.srv.URL + "/mcp",
			"authorization_servers": []string{f.srv.URL},
			"scopes_supported":      []string{"read", "write"},
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(rw http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"issuer":                 f.srv.URL,
			"authorization_endpoint": f.srv.URL + "/authorize",
			"token_endpoint":         f.srv.URL + "/token",
			"registration_endpoint":  f.srv.URL + "/register",
		})
	})
	mux.HandleFunc("/register", func(rw http.ResponseWriter, r *http.Request) {
		f.registered++
		_ = json.NewEncoder(rw).Encode(map[string]any{"client_id": "client-123"})
	})
	mux.HandleFunc("/token", func(rw http.ResponseWriter, r *http.Request) {
		f.tokenCalls++
		_ = r.ParseForm()
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			if r.Form.Get("code") != f.issuedCode {
				http.Error(rw, `{"error":"invalid_grant"}`, 400)
				return
			}
			// PKCE：verifier 的 S256 摘要必须等于授权时提交的 challenge
			// PKCE: the S256 digest of the verifier must equal the challenge submitted at authorization
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge {
				http.Error(rw, `{"error":"invalid_grant","error_description":"pkce mismatch"}`, 400)
				return
			}
			_ = json.NewEncoder(rw).Encode(map[string]any{
				"access_token": "access-1", "refresh_token": "refresh-1",
				"token_type": "Bearer", "expires_in": 3600,
			})
		case "refresh_token":
			if r.Form.Get("refresh_token") != "refresh-1" {
				http.Error(rw, `{"error":"invalid_grant"}`, 400)
				return
			}
			_ = json.NewEncoder(rw).Encode(map[string]any{
				"access_token": "access-2", "token_type": "Bearer", "expires_in": 3600,
			})
		default:
			http.Error(rw, `{"error":"unsupported_grant_type"}`, 400)
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// approve 扮演用户在浏览器里点"同意"：授权服务器会带着 code 回调本地地址。
// approve plays the user approving in a browser: the authorization server redirects back to the local
// address with a code.
func (f *fakeAS) approve(t *testing.T, authURL string) {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	f.challenge = q.Get("code_challenge")
	if q.Get("code_challenge_method") != "S256" {
		t.Fatalf("PKCE must use S256, got %q", q.Get("code_challenge_method"))
	}
	if q.Get("resource") == "" {
		t.Fatal("the authorization request should carry the resource parameter (RFC 8707)")
	}
	cb, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		t.Fatal(err)
	}
	cb.RawQuery = url.Values{"code": {f.issuedCode}, "state": {q.Get("state")}}.Encode()
	resp, err := http.Get(cb.String())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func waitDone(t *testing.T, m *MCPOAuth, name string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st := m.Status(name)
		if st["status"] == "done" || st["status"] == "error" {
			return st
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the authorization never completed")
	return nil
}

func TestOAuthFullFlow(t *testing.T) {
	as := newFakeAS(t)
	m := NewMCPOAuth(filepath.Join(t.TempDir(), "mcp_oauth.json"))

	st, err := m.Start("linear", as.srv.URL+"/mcp")
	if err != nil {
		t.Fatal(err)
	}
	authURL, _ := st["url"].(string)
	if !strings.HasPrefix(authURL, as.srv.URL+"/authorize") {
		t.Fatalf("wrong authorization URL: %q", authURL)
	}
	if as.registered != 1 {
		t.Fatalf("the client should have been registered dynamically once, got %d", as.registered)
	}

	as.approve(t, authURL)
	if got := waitDone(t, m, "linear"); got["status"] != "done" {
		t.Fatalf("authorization failed: %+v", got)
	}

	tok, err := m.Bearer("linear")
	if err != nil || tok != "access-1" {
		t.Fatalf("Bearer should return the fresh token: %q %v", tok, err)
	}
	if !m.Connected("linear") {
		t.Fatal("it should report as connected")
	}
}

// 令牌过期要能自动刷新，而且刷新响应里没重发 refresh_token 时不能把旧的弄丢。
// An expired token must refresh itself, and a refresh response that omits refresh_token must not lose the
// one already held.
func TestOAuthRefresh(t *testing.T) {
	as := newFakeAS(t)
	path := filepath.Join(t.TempDir(), "mcp_oauth.json")
	m := NewMCPOAuth(path)
	st, err := m.Start("linear", as.srv.URL+"/mcp")
	if err != nil {
		t.Fatal(err)
	}
	as.approve(t, st["url"].(string))
	waitDone(t, m, "linear")

	m.mu.Lock()
	m.entries["linear"].Expires = time.Now().Add(-time.Minute)
	m.mu.Unlock()

	tok, err := m.Bearer("linear")
	if err != nil || tok != "access-2" {
		t.Fatalf("an expired token should be refreshed: %q %v", tok, err)
	}
	m.mu.Lock()
	kept := m.entries["linear"].RefreshToken
	m.mu.Unlock()
	if kept != "refresh-1" {
		t.Fatalf("the refresh token must survive a response that omits it, got %q", kept)
	}

	// 令牌要落盘，重启后不必重新授权
	// Tokens must persist so a restart does not mean authorizing again
	if reloaded := NewMCPOAuth(path); !reloaded.Connected("linear") {
		t.Fatal("the authorization should survive a reload")
	}
}

// state 对不上就必须拒绝：这是回调唯一的 CSRF 防线。
// A mismatched state must be rejected: it is the callback's only line of defence against CSRF.
func TestOAuthRejectsBadState(t *testing.T) {
	as := newFakeAS(t)
	m := NewMCPOAuth(filepath.Join(t.TempDir(), "mcp_oauth.json"))
	st, err := m.Start("linear", as.srv.URL+"/mcp")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(st["url"].(string))
	cb, _ := url.Parse(u.Query().Get("redirect_uri"))
	cb.RawQuery = url.Values{"code": {"the-code"}, "state": {"not-the-right-state"}}.Encode()
	resp, err := http.Get(cb.String())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	got := waitDone(t, m, "linear")
	if got["status"] != "error" {
		t.Fatalf("a mismatched state should fail the authorization: %+v", got)
	}
	if m.Connected("linear") {
		t.Fatal("nothing should be stored after a failed authorization")
	}
}

// 授权服务器不支持动态注册时，要给一句能照做的话，而不是把用户扔在一个走不通的授权页上。
// When the authorization server has no dynamic registration, say something actionable rather than
// stranding the user on an authorization page that cannot work.
func TestOAuthWithoutRegistrationEndpoint(t *testing.T) {
	mux := http.NewServeMux()
	var base string
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(rw http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"issuer":                 base,
			"authorization_endpoint": base + "/authorize",
			"token_endpoint":         base + "/token",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	base = srv.URL

	m := NewMCPOAuth(filepath.Join(t.TempDir(), "mcp_oauth.json"))
	_, err := m.Start("thing", srv.URL+"/mcp")
	if err == nil || !strings.Contains(err.Error(), "Bearer key") {
		t.Fatalf("the error should point at the token alternative, got %v", err)
	}
}
