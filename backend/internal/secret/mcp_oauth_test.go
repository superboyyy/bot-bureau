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

	// Tokens must persist so a restart does not mean authorizing again
	if reloaded := NewMCPOAuth(path); !reloaded.Connected("linear") {
		t.Fatal("the authorization should survive a reload")
	}
}

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

func TestIsGitHubOAuth(t *testing.T) {
	if !isGitHubOAuth("https://api.githubcopilot.com/mcp/", "") {
		t.Fatal("copilot MCP host should use the baked-in GitHub client")
	}
	if !isGitHubOAuth("https://example.com/mcp", "https://github.com/login/oauth") {
		t.Fatal("GitHub's authorization issuer should use the baked-in client")
	}
	if isGitHubOAuth("https://mcp.linear.app/mcp", "https://mcp.linear.app") {
		t.Fatal("a normal MCP host must keep dynamic registration")
	}
}

func TestGitHubDeviceEndpointsRewriteGitHubTokenPath(t *testing.T) {
	device, token := githubDeviceEndpoints(asMetadata{})
	if device != githubDeviceDefault || token != githubTokenDefault {
		t.Fatalf("empty metadata: device=%q token=%q", device, token)
	}
	device, token = githubDeviceEndpoints(asMetadata{
		DeviceEndpoint: "https://github.com/login/device/code",
		TokenEndpoint:  "https://github.com/login/oauth/token",
	})
	if token != githubTokenDefault {
		t.Fatalf("github.com /token fallback should become access_token, got %q", token)
	}
	if device != "https://github.com/login/device/code" {
		t.Fatalf("device URL: %q", device)
	}
	const localToken = "http://127.0.0.1:9/token"
	_, token = githubDeviceEndpoints(asMetadata{TokenEndpoint: localToken})
	if token != localToken {
		t.Fatalf("httptest token URLs must be left alone, got %q", token)
	}
}

// GitHub has no RFC 7591 registration. Start must skip DCR, send the baked-in public client id,
// open a device-code page (not a loopback callback), and poll until a token arrives.
func TestGitHubDeviceOAuthUsesBakedClientID(t *testing.T) {
	var gotDeviceForm, gotTokenForm url.Values
	pollN := 0
	mux := http.NewServeMux()
	var srv *httptest.Server
	mcpAuth := func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("WWW-Authenticate",
			fmt.Sprintf(`Bearer realm="mcp", resource_metadata="%s/.well-known/oauth-protected-resource"`, srv.URL))
		rw.WriteHeader(401)
	}
	mux.HandleFunc("/mcp", mcpAuth)
	mux.HandleFunc("/mcp/", mcpAuth)
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(rw http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"resource":              srv.URL + "/mcp",
			"authorization_servers": []string{srv.URL},
			"scopes_supported":      []string{"repo", "read:org"},
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(rw http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"issuer":                        srv.URL,
			"authorization_endpoint":        srv.URL + "/authorize",
			"token_endpoint":                srv.URL + "/login/oauth/access_token",
			"device_authorization_endpoint": srv.URL + "/login/device/code",
		})
	})
	mux.HandleFunc("/login/device/code", func(rw http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotDeviceForm = r.PostForm
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"device_code": "DC-1", "user_code": "ABCD-1234",
			"verification_uri":          srv.URL + "/login/device",
			"verification_uri_complete": srv.URL + "/login/device?user_code=ABCD-1234",
			"expires_in":                60, "interval": 1,
		})
	})
	mux.HandleFunc("/login/oauth/access_token", func(rw http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotTokenForm = r.PostForm
		pollN++
		if pollN < 2 {
			rw.WriteHeader(400)
			_ = json.NewEncoder(rw).Encode(map[string]any{"error": "authorization_pending"})
			return
		}
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"access_token": "gho-test", "refresh_token": "refresh-gh",
			"token_type": "bearer", "expires_in": 3600,
		})
	})
	mux.HandleFunc("/register", func(rw http.ResponseWriter, r *http.Request) {
		t.Error("GitHub must not attempt dynamic client registration")
		http.Error(rw, "no registration", 404)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	m := NewMCPOAuth(filepath.Join(t.TempDir(), "mcp_oauth.json"))
	m.httpc = &http.Client{
		Timeout: 5 * time.Second,
		Transport: rewriteHostTransport(map[string]string{
			"api.githubcopilot.com": srv.URL,
		}),
	}

	st, err := m.Start("github", "https://api.githubcopilot.com/mcp/")
	if err != nil {
		t.Fatal(err)
	}
	if gotDeviceForm.Get("client_id") != githubOAuthClientID {
		t.Fatalf("device request client_id=%q, want baked-in %q", gotDeviceForm.Get("client_id"), githubOAuthClientID)
	}
	if gotDeviceForm.Get("client_secret") != "" {
		t.Fatal("device flow must not send a client secret")
	}
	if st["user_code"] != "ABCD-1234" {
		t.Fatalf("user_code: %v", st["user_code"])
	}
	authURL, _ := st["url"].(string)
	if !strings.Contains(authURL, "user_code=ABCD-1234") {
		t.Fatalf("authorization URL should carry the user code, got %q", authURL)
	}
	if strings.Contains(authURL, "127.0.0.1") && strings.Contains(authURL, "/callback") {
		t.Fatalf("device flow must not use a loopback callback, got %q", authURL)
	}

	got := waitDone(t, m, "github")
	if got["status"] != "done" {
		t.Fatalf("authorization failed: %+v", got)
	}
	if gotTokenForm.Get("client_id") != githubOAuthClientID {
		t.Fatalf("token poll client_id=%q", gotTokenForm.Get("client_id"))
	}
	if gotTokenForm.Get("grant_type") != githubDeviceGrant {
		t.Fatalf("grant_type=%q", gotTokenForm.Get("grant_type"))
	}
	if gotTokenForm.Get("client_secret") != "" {
		t.Fatal("token poll must not send a client secret")
	}
	tok, err := m.Bearer("github")
	if err != nil || tok != "gho-test" {
		t.Fatalf("Bearer: %q %v", tok, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func rewriteHostTransport(hosts map[string]string) http.RoundTripper {
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		nr := req.Clone(req.Context())
		if dest, ok := hosts[strings.ToLower(nr.URL.Hostname())]; ok {
			u, err := url.Parse(dest)
			if err != nil {
				return nil, err
			}
			nr.URL.Scheme, nr.URL.Host, nr.Host = u.Scheme, u.Host, u.Host
		}
		return http.DefaultTransport.RoundTrip(nr)
	})
}
