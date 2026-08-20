package secret

// OAuth for remote MCP connectors.

// Why this is unavoidable: the remote connectors from Linear, Notion, Sentry and GitHub all speak OAuth,
// and a static Bearer connects to none of them. Linear / Notion / Sentry support dynamic client
// registration (RFC 7591): discover the authorization server, register a client on the spot, then
// authorization code + PKCE. GitHub's authorization server does not — there is no registration
// endpoint — so GitHub uses a baked-in public client id the same way ChatGPT and SuperGrok sign-in
// do, and GitHub's device-code flow (a browser page, no loopback callback).

// How this differs from the xai/chatgpt flows: those target one hardcoded vendor with known endpoints and
// use a device code. Here the vendor is whatever URL the user typed, so every endpoint must be discovered
// at run time, and the redirect needs a real local listener (the device flow is not universally offered).

import (
	"botbureau/backend/internal/i18n"

	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	mcpOAuthClientName = "Bot Bureau"

	// Public client id of the GitHub MCP OAuth app that official github-mcp-server builds ship —
	// the same pattern as chatgptClientID / xaiClientID: a vendor-published public client, not a
	// secret Bot Bureau registered. GitHub's device-code flow only needs this; no client secret
	// is stored.
	githubOAuthClientID = "Ov23ctZDe8AJr0QMDiIq"
	githubDeviceGrant   = "urn:ietf:params:oauth:grant-type:device_code"
	githubDeviceDefault = "https://github.com/login/device/code"
	githubTokenDefault  = "https://github.com/login/oauth/access_token"
	// Default scopes match the set github-mcp-server requests when none are discovered.
	githubOAuthScope = "repo read:org read:user user:email"

	// The user has to go and approve in a browser, so allow real time; the listener must be reclaimed
	// afterwards rather than holding a port forever.
	mcpAuthWindow  = 10 * time.Minute
	mcpHTTPTimeout = 20 * time.Second

	// Refresh a little early to avoid expiring midway through a request.
	mcpRefreshSkew = 60 * time.Second
)

type mcpOAuthEntry struct {
	ServerURL     string    `json:"server_url"`
	Issuer        string    `json:"issuer"`
	AuthEndpoint  string    `json:"auth_endpoint"`
	TokenEndpoint string    `json:"token_endpoint"`
	ClientID      string    `json:"client_id"`
	ClientSecret  string    `json:"client_secret,omitempty"`
	Scope         string    `json:"scope,omitempty"`
	AccessToken   string    `json:"access_token,omitempty"`
	RefreshToken  string    `json:"refresh_token,omitempty"`
	Expires       time.Time `json:"expires,omitempty"`
}

type mcpPending struct {
	status   string // pending | done | error
	msg      string
	authURL  string
	userCode string
	verifier string
	state    string
	redirect string
	listener net.Listener
	cancel   chan struct{}
}

type MCPOAuth struct {
	path    string
	mu      sync.Mutex
	entries map[string]*mcpOAuthEntry
	pending map[string]*mcpPending
	httpc   *http.Client
}

func NewMCPOAuth(path string) *MCPOAuth {
	m := &MCPOAuth{
		path:    path,
		entries: map[string]*mcpOAuthEntry{},
		pending: map[string]*mcpPending{},
		httpc:   &http.Client{Timeout: mcpHTTPTimeout},
	}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &m.entries)
	}
	return m
}

// Every call site already holds m.mu (encoding walks m.entries, so it has to); this only handles the
// atomic write.
func (m *MCPOAuth) save() error {
	raw, err := marshalSecret(m.entries)
	if err != nil {
		return err
	}
	return writeSecretFile(m.path, raw)
}

// Connected reports whether a connector has ever obtained a token.
func (m *MCPOAuth) Connected(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[name]
	return e != nil && e.AccessToken != ""
}

func (m *MCPOAuth) Status(name string) map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]any{"name": name, "connected": false, "pending": false}
	if e := m.entries[name]; e != nil && e.AccessToken != "" {
		out["connected"] = true
		out["issuer"] = e.Issuer
	}
	if p := m.pending[name]; p != nil {
		out["pending"] = p.status == "pending"
		out["status"] = p.status
		out["url"] = p.authURL
		if p.userCode != "" {
			out["user_code"] = p.userCode
		}
		if p.msg != "" {
			out["error"] = p.msg
		}
	}
	return out
}

// ClearPending drops a finished authorization's state so the UI does not sit on "just completed", and so
// the next authorization starts clean.
func (m *MCPOAuth) ClearPending(name string) {
	m.mu.Lock()
	delete(m.pending, name)
	m.mu.Unlock()
}

func (m *MCPOAuth) Logout(name string) {
	m.mu.Lock()
	delete(m.entries, name)
	if p := m.pending[name]; p != nil {
		if p.cancel != nil {
			select {
			case <-p.cancel:
			default:
				close(p.cancel)
			}
		}
		if p.listener != nil {
			_ = p.listener.Close()
		}
	}
	delete(m.pending, name)
	_ = m.save()
	m.mu.Unlock()
}

// Start begins an authorization: discover the endpoints, register a client, open a local callback and
// return the URL for the user to approve. Authorization is not complete when this returns; the UI polls
// Status.
func (m *MCPOAuth) Start(name, serverURL string) (map[string]any, error) {
	if strings.TrimSpace(serverURL) == "" {
		return nil, errors.New(i18n.T("This connector has no URL"))
	}
	meta, err := m.discover(serverURL)
	if err != nil {
		return nil, err
	}

	// GitHub has no dynamic client registration. Use the baked-in public client id and the
	// device-code flow (a browser page, same shape as ChatGPT / SuperGrok sign-in).
	if isGitHubOAuth(serverURL, meta.Issuer) {
		return m.startGitHubDevice(name, serverURL, meta)
	}

	// Reuse a previously registered client: there is no reason to register a fresh one with the same
	// authorization server on every authorization.
	m.mu.Lock()
	prev := m.entries[name]
	m.mu.Unlock()
	clientID, clientSecret := "", ""
	if prev != nil && prev.Issuer == meta.Issuer {
		clientID, clientSecret = prev.ClientID, prev.ClientSecret
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf(i18n.T("Could not open a local callback port: %w"), err)
	}
	redirect := fmt.Sprintf("http://127.0.0.1:%d/callback", ln.Addr().(*net.TCPAddr).Port)

	if clientID == "" {
		clientID, clientSecret, err = m.register(meta, redirect)
		if err != nil {
			_ = ln.Close()
			return nil, err
		}
	}

	verifier := randomString(64)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	state := randomString(24)

	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirect},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},

		// RFC 8707: tells the authorization server which resource the token is for. Servers that support
		// it scope the token accordingly and those that do not ignore the parameter, so sending it costs
		// nothing.
		"resource": {canonicalResource(serverURL)},
	}
	if meta.Scope != "" {
		q.Set("scope", meta.Scope)
	}
	authURL := meta.AuthEndpoint + "?" + q.Encode()

	entry := &mcpOAuthEntry{
		ServerURL: serverURL, Issuer: meta.Issuer,
		AuthEndpoint: meta.AuthEndpoint, TokenEndpoint: meta.TokenEndpoint,
		ClientID: clientID, ClientSecret: clientSecret, Scope: meta.Scope,
	}
	p := &mcpPending{
		status: "pending", authURL: authURL, verifier: verifier,
		state: state, redirect: redirect, listener: ln,
	}
	m.mu.Lock()
	if old := m.pending[name]; old != nil {
		if old.cancel != nil {
			select {
			case <-old.cancel:
			default:
				close(old.cancel)
			}
		}
		if old.listener != nil {
			_ = old.listener.Close()
		}
	}
	m.pending[name] = p
	m.entries[name] = entry
	m.mu.Unlock()

	go m.awaitCallback(name, p, entry, canonicalResource(serverURL))
	return m.Status(name), nil
}

func isGitHubOAuth(serverURL, issuer string) bool {
	if u, err := url.Parse(serverURL); err == nil {
		if strings.EqualFold(u.Host, "api.githubcopilot.com") {
			return true
		}
	}
	iss := strings.TrimRight(strings.ToLower(issuer), "/")
	return iss == "https://github.com/login/oauth"
}

type githubDeviceCode struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
	Error                   string `json:"error"`
	ErrorDescription        string `json:"error_description"`
}

// githubDeviceEndpoints prefers discovered URLs (so tests can point at httptest) and only rewrites
// GitHub's RFC-8414 fallback `/token` to the real `/login/oauth/access_token`.
func githubDeviceEndpoints(meta asMetadata) (deviceURL, tokenURL string) {
	deviceURL = strings.TrimSpace(meta.DeviceEndpoint)
	if deviceURL == "" {
		deviceURL = githubDeviceDefault
	}
	tokenURL = strings.TrimSpace(meta.TokenEndpoint)
	if tokenURL == "" {
		return deviceURL, githubTokenDefault
	}
	if u, err := url.Parse(tokenURL); err == nil && strings.EqualFold(u.Host, "github.com") && !strings.Contains(u.Path, "access_token") {
		return deviceURL, githubTokenDefault
	}
	return deviceURL, tokenURL
}

// startGitHubDevice is the ChatGPT-style path: baked-in public client id, device code, browser page.
func (m *MCPOAuth) startGitHubDevice(name, serverURL string, meta asMetadata) (map[string]any, error) {
	deviceURL, tokenURL := githubDeviceEndpoints(meta)
	scope := meta.Scope
	if scope == "" {
		scope = githubOAuthScope
	}

	form := url.Values{"client_id": {githubOAuthClientID}, "scope": {scope}}
	var device githubDeviceCode
	if err := m.postFormJSON(deviceURL, form, &device); err != nil {
		return nil, fmt.Errorf(i18n.T("Failed to request a GitHub login code: %w"), err)
	}
	if device.Error != "" {
		msg := device.Error
		if device.ErrorDescription != "" {
			msg = device.ErrorDescription
		}
		return nil, fmt.Errorf(i18n.T("Failed to request a GitHub login code: %s"), msg)
	}
	if device.DeviceCode == "" || device.UserCode == "" || device.VerificationURI == "" {
		return nil, errors.New(i18n.T("GitHub returned an incomplete login code"))
	}
	authURL := device.VerificationURIComplete
	if authURL == "" {
		authURL = device.VerificationURI + "?user_code=" + url.QueryEscape(device.UserCode)
	}

	entry := &mcpOAuthEntry{
		ServerURL: serverURL, Issuer: firstNonEmpty(meta.Issuer, "https://github.com/login/oauth"),
		AuthEndpoint: meta.AuthEndpoint, TokenEndpoint: tokenURL,
		ClientID: githubOAuthClientID, Scope: scope,
	}
	cancel := make(chan struct{})
	p := &mcpPending{
		status: "pending", authURL: authURL, userCode: device.UserCode, cancel: cancel,
	}
	m.mu.Lock()
	if old := m.pending[name]; old != nil {
		if old.cancel != nil {
			select {
			case <-old.cancel:
			default:
				close(old.cancel)
			}
		}
		if old.listener != nil {
			_ = old.listener.Close()
		}
	}
	m.pending[name] = p
	m.entries[name] = entry
	m.mu.Unlock()
	go m.pollGitHubDevice(name, p, entry, device)
	return m.Status(name), nil
}

func (m *MCPOAuth) pollGitHubDevice(name string, p *mcpPending, entry *mcpOAuthEntry, device githubDeviceCode) {
	interval := time.Duration(device.Interval) * time.Second
	if interval < time.Second {
		interval = 5 * time.Second
	}
	deadline := time.Now().Add(mcpAuthWindow)
	if device.ExpiresIn > 0 {
		deadline = time.Now().Add(time.Duration(device.ExpiresIn) * time.Second)
	}
	form := url.Values{
		"grant_type":  {githubDeviceGrant},
		"client_id":   {githubOAuthClientID},
		"device_code": {device.DeviceCode},
	}
	for time.Now().Before(deadline) {
		select {
		case <-p.cancel:
			return
		default:
		}
		var tok oauthToken
		err := m.postFormJSON(entry.TokenEndpoint, form, &tok)
		if err == nil && tok.AccessToken != "" {
			m.mu.Lock()
			if m.pending[name] == p {
				applyToken(entry, tok)
				m.entries[name] = entry
				p.status, p.msg = "done", ""
				_ = m.save()
			}
			m.mu.Unlock()
			return
		}
		code := ""
		if tok.Error != "" {
			code = tok.Error
		}
		switch code {
		case "authorization_pending", "":
			if err != nil && code == "" {
				m.finish(name, p, err.Error())
				return
			}
		case "slow_down":
			interval += 5 * time.Second
		case "access_denied", "authorization_denied":
			m.finish(name, p, i18n.T("Login was denied"))
			return
		case "expired_token":
			m.finish(name, p, i18n.T("The login code expired — try again"))
			return
		default:
			msg := tok.ErrorDescription
			if msg == "" {
				msg = code
			}
			m.finish(name, p, msg)
			return
		}
		select {
		case <-p.cancel:
			return
		case <-time.After(interval):
		}
	}
	m.finish(name, p, i18n.T("Login timed out — try again"))
}

// awaitCallback receives the authorization code, redeems it, and closes the listener either way.
func (m *MCPOAuth) awaitCallback(name string, p *mcpPending, entry *mcpOAuthEntry, resource string) {
	defer p.listener.Close()

	type result struct{ code, errMsg string }
	ch := make(chan result, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		rw.Header().Set("Content-Type", "text/html; charset=utf-8")
		if errStr := q.Get("error"); errStr != "" {
			detail := q.Get("error_description")
			if detail == "" {
				detail = errStr
			}
			fmt.Fprint(rw, callbackPage(i18n.T("Authorization was declined."), detail))
			ch <- result{errMsg: detail}
			return
		}

		// A mismatched state counts as nothing received: it is the only CSRF check the callback can make
		if q.Get("state") != p.state {
			fmt.Fprint(rw, callbackPage(i18n.T("Authorization failed."), i18n.T("The state parameter did not match.")))
			ch <- result{errMsg: i18n.T("the state parameter did not match")}
			return
		}
		code := q.Get("code")
		if code == "" {
			fmt.Fprint(rw, callbackPage(i18n.T("Authorization failed."), i18n.T("No authorization code was returned.")))
			ch <- result{errMsg: i18n.T("no authorization code was returned")}
			return
		}
		fmt.Fprint(rw, callbackPage(i18n.T("Connected."), i18n.T("You can close this tab and go back to Bot Bureau.")))
		ch <- result{code: code}
	})}
	go srv.Serve(p.listener)
	defer srv.Close()

	select {
	case res := <-ch:
		if res.errMsg != "" {
			m.finish(name, p, res.errMsg)
			return
		}
		tok, err := m.exchange(entry, res.code, p.verifier, p.redirect, resource)
		if err != nil {
			m.finish(name, p, err.Error())
			return
		}
		m.mu.Lock()
		applyToken(entry, tok)
		m.entries[name] = entry
		p.status, p.msg = "done", ""
		_ = m.save()
		m.mu.Unlock()
	case <-time.After(mcpAuthWindow):
		m.finish(name, p, i18n.T("the authorization window expired"))
	}
}

func (m *MCPOAuth) finish(name string, p *mcpPending, msg string) {
	m.mu.Lock()
	if p.cancel != nil {
		select {
		case <-p.cancel:
		default:
			close(p.cancel)
		}
	}
	p.status, p.msg = "error", msg

	// Do not leave a half-finished entry behind, or the UI reads it as connected
	if e := m.entries[name]; e != nil && e.AccessToken == "" {
		delete(m.entries, name)
	}
	m.mu.Unlock()
}

// Bearer returns a usable token, refreshing first when it has expired.
func (m *MCPOAuth) Bearer(name string) (string, error) {
	m.mu.Lock()
	e := m.entries[name]
	if e == nil || e.AccessToken == "" {
		m.mu.Unlock()
		return "", errors.New(i18n.T("This connector has not been authorized yet"))
	}
	if e.Expires.IsZero() || time.Now().Add(mcpRefreshSkew).Before(e.Expires) {
		tok := e.AccessToken
		m.mu.Unlock()
		return tok, nil
	}
	snapshot := *e
	m.mu.Unlock()

	if snapshot.RefreshToken == "" {
		return "", errors.New(i18n.T("The authorization has expired; authorize this connector again"))
	}
	tok, err := m.refresh(&snapshot)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur := m.entries[name]; cur != nil {
		applyToken(cur, tok)
		_ = m.save()
		return cur.AccessToken, nil
	}
	return tok.AccessToken, nil
}

type oauthToken struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int64  `json:"expires_in"`
	TokenType        string `json:"token_type"`
	Scope            string `json:"scope"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func applyToken(e *mcpOAuthEntry, tok oauthToken) {
	e.AccessToken = tok.AccessToken

	// A refresh response does not always resend the refresh_token; keeping the old one matters, or the
	// next refresh has nothing to work with
	if tok.RefreshToken != "" {
		e.RefreshToken = tok.RefreshToken
	}
	if tok.ExpiresIn > 0 {
		e.Expires = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	} else {
		e.Expires = time.Time{}
	}
	if tok.Scope != "" {
		e.Scope = tok.Scope
	}
}

func (m *MCPOAuth) exchange(e *mcpOAuthEntry, code, verifier, redirect, resource string) (oauthToken, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirect},
		"client_id":     {e.ClientID},
		"code_verifier": {verifier},
		"resource":      {resource},
	}
	if e.ClientSecret != "" {
		form.Set("client_secret", e.ClientSecret)
	}
	return m.postToken(e.TokenEndpoint, form)
}

func (m *MCPOAuth) refresh(e *mcpOAuthEntry) (oauthToken, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {e.RefreshToken},
		"client_id":     {e.ClientID},
	}
	if e.ClientSecret != "" {
		form.Set("client_secret", e.ClientSecret)
	}
	return m.postToken(e.TokenEndpoint, form)
}

func (m *MCPOAuth) postToken(endpoint string, form url.Values) (oauthToken, error) {
	var tok oauthToken
	req, err := http.NewRequest("POST", endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return tok, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := m.httpc.Do(req)
	if err != nil {
		return tok, fmt.Errorf(i18n.T("Could not reach the token endpoint: %w"), err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return tok, fmt.Errorf(i18n.T("The token endpoint returned HTTP %d: %.200s"), resp.StatusCode, string(body))
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return tok, fmt.Errorf(i18n.T("The token endpoint returned something unreadable: %w"), err)
	}
	if tok.AccessToken == "" {
		return tok, errors.New(i18n.T("The token endpoint returned no access token"))
	}
	return tok, nil
}

// ---- endpoint discovery ----

type asMetadata struct {
	Issuer               string
	AuthEndpoint         string
	TokenEndpoint        string
	RegistrationEndpoint string
	DeviceEndpoint       string
	Scope                string
}

// discover follows the MCP flow: ask the protected resource's metadata for its authorization server,
// then ask that server for its endpoints. Every step has a fallback — plenty of servers in the wild
// implement only half of this, and failing outright on one missing document is too brittle.
func (m *MCPOAuth) discover(serverURL string) (asMetadata, error) {
	var meta asMetadata
	u, err := url.Parse(serverURL)
	if err != nil {
		return meta, fmt.Errorf(i18n.T("The connector URL is not valid: %w"), err)
	}
	origin := u.Scheme + "://" + u.Host

	issuer := ""
	scope := ""
	if prm := m.fetchResourceMetadata(serverURL, origin, u.Path); prm != nil {
		if len(prm.AuthorizationServers) > 0 {
			issuer = strings.TrimRight(prm.AuthorizationServers[0], "/")
		}
		scope = strings.Join(prm.ScopesSupported, " ")
	}
	if issuer == "" {
		issuer = origin
	}

	for _, candidate := range wellKnownURLs(issuer, u.Path) {
		var doc struct {
			Issuer                      string   `json:"issuer"`
			AuthorizationEndpoint       string   `json:"authorization_endpoint"`
			TokenEndpoint               string   `json:"token_endpoint"`
			RegistrationEndpoint        string   `json:"registration_endpoint"`
			DeviceAuthorizationEndpoint string   `json:"device_authorization_endpoint"`
			ScopesSupported             []string `json:"scopes_supported"`
		}
		if err := m.getJSON(candidate, &doc); err != nil || doc.AuthorizationEndpoint == "" || doc.TokenEndpoint == "" {
			continue
		}
		meta = asMetadata{
			Issuer:               firstNonEmpty(doc.Issuer, issuer),
			AuthEndpoint:         doc.AuthorizationEndpoint,
			TokenEndpoint:        doc.TokenEndpoint,
			RegistrationEndpoint: doc.RegistrationEndpoint,
			DeviceEndpoint:       doc.DeviceAuthorizationEndpoint,
			Scope:                firstNonEmpty(scope, strings.Join(doc.ScopesSupported, " ")),
		}
		return meta, nil
	}

	// No metadata document at all: fall back to the default paths from RFC 8414. Either it works, or the
	// registration step fails with something legible.
	return asMetadata{
		Issuer:               issuer,
		AuthEndpoint:         issuer + "/authorize",
		TokenEndpoint:        issuer + "/token",
		RegistrationEndpoint: issuer + "/register",
		Scope:                scope,
	}, nil
}

type resourceMetadata struct {
	AuthorizationServers []string `json:"authorization_servers"`
	ScopesSupported      []string `json:"scopes_supported"`
}

// fetchResourceMetadata first follows wherever the 401's WWW-Authenticate points, then tries the
// standard paths.
func (m *MCPOAuth) fetchResourceMetadata(serverURL, origin, path string) *resourceMetadata {
	var candidates []string
	if u := m.probeResourceMetadataURL(serverURL); u != "" {
		candidates = append(candidates, u)
	}
	candidates = append(candidates,
		origin+"/.well-known/oauth-protected-resource"+strings.TrimSuffix(path, "/"),
		origin+"/.well-known/oauth-protected-resource",
	)
	for _, c := range candidates {
		var prm resourceMetadata
		if err := m.getJSON(c, &prm); err == nil && len(prm.AuthorizationServers) > 0 {
			return &prm
		}
	}
	return nil
}

// probeResourceMetadataURL deliberately sends an unauthenticated request and reads the location of
// resource_metadata out of the 401's WWW-Authenticate header — the way the spec points a client at it.
func (m *MCPOAuth) probeResourceMetadataURL(serverURL string) string {
	req, err := http.NewRequest("POST", serverURL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := m.httpc.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	for _, h := range resp.Header.Values("WWW-Authenticate") {
		for _, part := range strings.Split(h, ",") {
			part = strings.TrimSpace(part)
			const key = "resource_metadata="
			if i := strings.Index(strings.ToLower(part), key); i >= 0 {
				return strings.Trim(part[i+len(key):], `"' `)
			}
		}
	}
	return ""
}

// wellKnownURLs lists the candidate locations for the authorization server metadata, in spec order.
func wellKnownURLs(issuer, path string) []string {
	issuer = strings.TrimRight(issuer, "/")
	trimmed := strings.TrimSuffix(path, "/")
	out := []string{}
	if trimmed != "" && trimmed != "/" {
		out = append(out,
			issuer+"/.well-known/oauth-authorization-server"+trimmed,
			issuer+"/.well-known/openid-configuration"+trimmed,
		)
	}
	return append(out,
		issuer+"/.well-known/oauth-authorization-server",
		issuer+"/.well-known/openid-configuration",
	)
}

// register performs dynamic client registration. Without a registration_endpoint there is no automatic
// path in, and saying so plainly tells the user this connector needs a hand-entered token rather than
// leaving them on an authorization page that goes nowhere.
func (m *MCPOAuth) register(meta asMetadata, redirect string) (clientID, clientSecret string, err error) {
	if meta.RegistrationEndpoint == "" {
		return "", "", errors.New(i18n.T("This connector's authorization server does not support automatic client registration; use a token instead (fill in Bearer key name)"))
	}
	body := map[string]any{
		"client_name":                mcpOAuthClientName,
		"redirect_uris":              []string{redirect},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	}
	if meta.Scope != "" {
		body["scope"] = meta.Scope
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", meta.RegistrationEndpoint, strings.NewReader(string(raw)))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := m.httpc.Do(req)
	if err != nil {
		return "", "", fmt.Errorf(i18n.T("Could not reach the registration endpoint: %w"), err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", "", fmt.Errorf(i18n.T("Client registration failed (HTTP %d): %.200s"), resp.StatusCode, string(payload))
	}
	var reg struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := json.Unmarshal(payload, &reg); err != nil || reg.ClientID == "" {
		return "", "", errors.New(i18n.T("Client registration returned no client_id"))
	}
	return reg.ClientID, reg.ClientSecret, nil
}

// postFormJSON POSTs application/x-www-form-urlencoded and decodes JSON (or form-urlencoded, which
// GitHub still returns if Accept is ignored). A 4xx body with an OAuth `error` field is decoded
// rather than treated as a transport failure — device-code polling uses HTTP 400 for
// authorization_pending.
func (m *MCPOAuth) postFormJSON(endpoint string, form url.Values, dest any) error {
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "botbureau/0.1.0")
	resp, err := m.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if dest != nil && len(strings.TrimSpace(string(body))) > 0 {
		if err := decodeOAuthJSONOrForm(body, dest); err != nil {
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
			}
			return err
		}
	}
	if oauthReplyHasError(dest) {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func oauthReplyHasError(dest any) bool {
	switch v := dest.(type) {
	case *oauthToken:
		return v != nil && v.Error != ""
	case *githubDeviceCode:
		return v != nil && v.Error != ""
	default:
		return false
	}
}

// decodeOAuthJSONOrForm accepts JSON or form-urlencoded (GitHub's default when Accept is missing).
func decodeOAuthJSONOrForm(body []byte, dest any) error {
	trim := strings.TrimSpace(string(body))
	if trim == "" {
		return io.ErrUnexpectedEOF
	}
	if trim[0] == '{' {
		return json.Unmarshal([]byte(trim), dest)
	}
	vals, err := url.ParseQuery(trim)
	if err != nil {
		return json.Unmarshal([]byte(trim), dest)
	}
	obj := map[string]any{}
	for k, vs := range vals {
		if len(vs) == 0 {
			continue
		}
		if n, convErr := strconv.ParseInt(vs[0], 10, 64); convErr == nil && vs[0] != "" {
			obj[k] = n
			continue
		}
		obj[k] = vs[0]
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, dest)
}

func (m *MCPOAuth) getJSON(endpoint string, into any) error {
	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := m.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(into)
}

// canonicalResource strips the query and fragment, leaving the resource identifier itself.
func canonicalResource(serverURL string) string {
	u, err := url.Parse(serverURL)
	if err != nil {
		return serverURL
	}
	u.RawQuery, u.Fragment = "", ""
	return u.String()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func randomString(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {

		// A crypto/rand failure means something is badly wrong with the system, and there is no safe
		// fallback to pick here
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(buf)[:n]
}

// callbackPage is the page the user lands on in the browser. Deliberately minimal: all it has to convey
// is whether this worked and that the tab can be closed.
func callbackPage(title, detail string) string {
	return `<!doctype html><meta charset="utf-8"><title>` + title + `</title>` +
		`<div style="font:15px/1.6 -apple-system,system-ui,sans-serif;max-width:32em;margin:18vh auto;padding:0 6vw;color:#222">` +
		`<h2 style="font-size:17px;margin:0 0 8px">` + title + `</h2><p style="margin:0;color:#666">` + detail + `</p></div>`
}
