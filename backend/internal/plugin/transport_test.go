package plugin

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"botbureau/backend/internal/secret"
)

func TestMCPHTTPTransport(t *testing.T) {

	// Minimal Streamable HTTP MCP server: replies with JSON directly
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		rw.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer secret-token" {
			rw.WriteHeader(401)
			return
		}
		switch req.Method {
		case "initialize":
			rw.Header().Set("Mcp-Session-Id", "sess-1")
			writeRPC(rw, req.ID, map[string]any{"protocolVersion": mcpProtocolVersion,
				"capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "h", "version": "1"}})
		case "tools/list":
			if r.Header.Get("Mcp-Session-Id") != "sess-1" {
				rw.WriteHeader(400)
				return
			}
			writeRPC(rw, req.ID, map[string]any{"tools": []map[string]any{
				{"name": "ping", "description": "pong",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
					"annotations": map[string]any{"readOnlyHint": true},
				},
			}})
		case "tools/call":
			writeRPC(rw, req.ID, map[string]any{"isError": false,
				"content": []map[string]any{{"type": "text", "text": "pong"}}})
		default:
			rw.WriteHeader(202)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	ks := secret.NewKeyStore(filepath.Join(dir, "keys.json"))
	_ = ks.Set("HTTP_MCP_TOKEN", "secret-token")
	mgr := NewMCPManager(filepath.Join(dir, "mcp.yaml"), ks)
	if err := mgr.Add(MCPServerConfig{Name: "remote", URL: srv.URL, BearerKey: "HTTP_MCP_TOKEN"}); err != nil {
		t.Fatal(err)
	}
	tools := mgr.Tools("remote")
	if len(tools) != 1 || tools[0].Name != "ping" {
		t.Fatalf("wrong http tool list: %+v", tools)
	}
	out, isErr, err := mgr.Call("remote", "ping", nil)
	if err != nil || isErr || out.Text != "pong" {
		t.Fatalf("http call failed: %q %v %v", out, isErr, err)
	}
}

// A remote connector must keep speaking the version the server settled on, and hand the session back
// when done. The initialize result used to be discarded and close() did nothing: the version was never
// negotiated and sessions were left dangling on the far side.
func TestHTTPVersionNegotiationAndSessionTermination(t *testing.T) {
	const serverPicks = "2025-03-26" // the server picks a different one
	var (
		mu          sync.Mutex
		laterHeader string
		deleted     string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			mu.Lock()
			deleted = r.Header.Get("Mcp-Session-Id")
			mu.Unlock()
			rw.WriteHeader(204)
			return
		}
		var req rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		rw.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			rw.Header().Set("Mcp-Session-Id", "sess-9")
			writeRPC(rw, req.ID, map[string]any{"protocolVersion": serverPicks,
				"capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "h", "version": "1"}})
		case "tools/list":
			mu.Lock()
			laterHeader = r.Header.Get("MCP-Protocol-Version")
			mu.Unlock()
			writeRPC(rw, req.ID, map[string]any{"tools": []map[string]any{
				{"name": "ping", "description": "pong",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}},
			}})
		default:
			rw.WriteHeader(202)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	ks := secret.NewKeyStore(filepath.Join(dir, "keys.json"))
	mgr := NewMCPManager(filepath.Join(dir, "mcp.yaml"), ks)
	if err := mgr.Add(MCPServerConfig{Name: "remote", URL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := laterHeader
	mu.Unlock()
	if got != serverPicks {
		t.Fatalf("later requests should carry the negotiated version %q, got %q", serverPicks, got)
	}

	if !mgr.Remove("remote") {
		t.Fatal("remove should succeed")
	}
	if !waitFor(2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return deleted == "sess-9"
	}) {
		t.Fatal("closing should terminate the remote session with a DELETE carrying its id")
	}
}

// TestMCPStdioTransport exercises a local plugin end to end: spawn, handshake, list tools, call one.
// The test binary doubles as the plugin process (it takes the server branch when BOTBUREAU_MCP_FAKE=1),
// so no external dependency is needed. The point is that this fake server only reads line-delimited
// JSON — exactly like the official SDK's stdio server. The client used to write LSP Content-Length
// frames, which no real plugin could parse, and the test at the time only covered the read direction.
func TestMCPStdioTransport(t *testing.T) {
	if os.Getenv("BOTBUREAU_MCP_FAKE") == "1" {
		runFakeStdioServer()
		return
	}
	dir := t.TempDir()
	ks := secret.NewKeyStore(filepath.Join(dir, "keys.json"))
	mgr := NewMCPManager(filepath.Join(dir, "mcp.yaml"), ks)
	err := mgr.Add(MCPServerConfig{
		Name:    "local",
		Command: os.Args[0],
		Args:    []string{"-test.run=TestMCPStdioTransport"},
		Env:     map[string]string{"BOTBUREAU_MCP_FAKE": "1"},
	})
	if err != nil {
		t.Fatalf("stdio plugin failed to connect: %v", err)
	}
	tools := mgr.Tools("local")
	if len(tools) != 1 || tools[0].Name != "echo" || !tools[0].ReadOnly {
		t.Fatalf("wrong stdio tool list: %+v", tools)
	}
	out, isErr, err := mgr.Call("local", "echo", map[string]any{"text": "hi"})
	if err != nil || isErr || out.Text != "hi" {
		t.Fatalf("stdio call failed: %q %v %v", out, isErr, err)
	}
}

// TestStdioConnectMissingCommand: a missing command must report an error rather than crash.
// It used to panic — the (*stdioConn)(nil) returned by newStdioConn became a non-nil mcpConn interface,
// which connect's conn != nil check did not stop, so close() dereferenced nil.
func TestStdioConnectMissingCommand(t *testing.T) {
	dir := t.TempDir()
	ks := secret.NewKeyStore(filepath.Join(dir, "keys.json"))
	mgr := NewMCPManager(filepath.Join(dir, "mcp.yaml"), ks)
	err := mgr.Add(MCPServerConfig{Name: "nope", Command: filepath.Join(dir, "definitely-not-here")})
	if err == nil {
		t.Fatal("expected an error for a command that does not exist")
	}
	st := mgr.Status()
	if len(st) != 1 || st[0]["status"] != "error" {
		t.Fatalf("plugin should be listed in the error state: %+v", st)
	}
}

// runFakeStdioServer is a minimal MCP server that reads and writes one line at a time.
func runFakeStdioServer() {
	r := bufio.NewReader(os.Stdin)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var req rpcRequest
		if json.Unmarshal(bytes.TrimSpace(line), &req) != nil || req.ID == nil {
			continue // a bad message or a notification
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": mcpProtocolVersion,
				"capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "fake", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{
				"name": "echo", "description": "echo the text back",
				"inputSchema": map[string]any{"type": "object",
					"properties": map[string]any{"text": map[string]any{"type": "string"}},
					"required":   []string{"text"}},
				"annotations": map[string]any{"readOnlyHint": true},
			}}}
		case "tools/call":
			var p struct {
				Arguments struct {
					Text string `json:"text"`
				} `json:"arguments"`
			}
			raw, _ := json.Marshal(req.Params)
			_ = json.Unmarshal(raw, &p)
			result = map[string]any{"isError": false,
				"content": []map[string]any{{"type": "text", "text": p.Arguments.Text}}}
		default:
			result = map[string]any{}
		}
		out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		_, _ = os.Stdout.Write(append(out, '\n'))
	}
}

// When a plugin process goes away on its own, the state must turn unavailable at once and reconnect —
// rather than leaving a green dot up while the model keeps calling a tool list that no longer exists.
func TestStdioReconnectsAfterProcessDies(t *testing.T) {
	if os.Getenv("BOTBUREAU_MCP_FAKE") == "1" {
		runFakeStdioServer()
		return
	}
	dir := t.TempDir()
	ks := secret.NewKeyStore(filepath.Join(dir, "keys.json"))
	mgr := NewMCPManager(filepath.Join(dir, "mcp.yaml"), ks)
	err := mgr.Add(MCPServerConfig{
		Name:    "local",
		Command: os.Args[0],
		Args:    []string{"-test.run=TestStdioReconnectsAfterProcessDies"},
		Env:     map[string]string{"BOTBUREAU_MCP_FAKE": "1"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Kill the process from outside, standing in for a plugin crash
	mgr.mu.Lock()
	client := mgr.servers["local"]
	mgr.mu.Unlock()
	client.mu.Lock()
	sc := client.conn.(*stdioConn)
	client.mu.Unlock()
	if sc.cmd.Process == nil {
		t.Fatal("the plugin process should be running")
	}
	_ = sc.cmd.Process.Kill()

	// first it must fall out of the connected state
	if !waitFor(2*time.Second, func() bool { return statusOf(mgr, "local") != "connected" }) {
		t.Fatalf("a dead plugin must not keep reporting connected, got %q", statusOf(mgr, "local"))
	}
	// then it must come back on its own
	if !waitFor(20*time.Second, func() bool { return statusOf(mgr, "local") == "connected" }) {
		t.Fatalf("it should reconnect by itself, still %q", statusOf(mgr, "local"))
	}
	if tools := mgr.Tools("local"); len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools should be back after reconnecting: %+v", tools)
	}
}

// A deliberate removal must not trigger a reconnect, or a deleted plugin crawls back on its own.
func TestRemoveDoesNotReconnect(t *testing.T) {
	if os.Getenv("BOTBUREAU_MCP_FAKE") == "1" {
		runFakeStdioServer()
		return
	}
	dir := t.TempDir()
	ks := secret.NewKeyStore(filepath.Join(dir, "keys.json"))
	mgr := NewMCPManager(filepath.Join(dir, "mcp.yaml"), ks)
	if err := mgr.Add(MCPServerConfig{
		Name:    "local",
		Command: os.Args[0],
		Args:    []string{"-test.run=TestRemoveDoesNotReconnect"},
		Env:     map[string]string{"BOTBUREAU_MCP_FAKE": "1"},
	}); err != nil {
		t.Fatal(err)
	}
	if !mgr.Remove("local") {
		t.Fatal("remove should succeed")
	}
	time.Sleep(3 * reconnectBackoffMin)
	if mgr.Has("local") || len(mgr.Names()) != 0 {
		t.Fatal("a removed plugin must stay removed")
	}
}

func statusOf(m *MCPManager, name string) string {
	for _, entry := range m.Status() {
		if entry["name"] == name {
			s, _ := entry["status"].(string)
			return s
		}
	}
	return ""
}

func waitFor(limit time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func writeRPC(rw http.ResponseWriter, id *int64, result any) {
	_ = json.NewEncoder(rw).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

// A plugin process must receive only the allowlisted environment. This guards a security boundary:
// something installed by one click from a panel has no business also receiving SSH_AUTH_SOCK and
// whatever tokens live in your shell.
func TestPluginEnvIsAllowlisted(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "/tmp/agent.sock")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "super-secret")
	t.Setenv("HTTPS_PROXY", "http://proxy.corp:8080")

	dir := t.TempDir()
	ks := secret.NewKeyStore(filepath.Join(dir, "keys.json"))
	if err := ks.Set("ACME_TOKEN", "tok-123"); err != nil {
		t.Fatal(err)
	}
	env := pluginEnv(MCPServerConfig{
		Name: "acme",
		Env:  map[string]string{"ACME_TOKEN": "$ACME_TOKEN", "PLAIN": "value"},
	}, ks)

	got := map[string]string{}
	for _, kv := range env {
		if i := strings.Index(kv, "="); i > 0 {
			got[kv[:i]] = kv[i+1:]
		}
	}
	for _, leaked := range []string{"SSH_AUTH_SOCK", "AWS_SECRET_ACCESS_KEY"} {
		if _, ok := got[leaked]; ok {
			t.Fatalf("%s must not reach a plugin process", leaked)
		}
	}

	// The proxy has to get through, or every plugin behind a corporate network fails with nothing but a
	// timeout to go on
	if got["HTTPS_PROXY"] != "http://proxy.corp:8080" {
		t.Fatalf("HTTPS_PROXY should pass through, got %q", got["HTTPS_PROXY"])
	}
	if got["PATH"] == "" {
		t.Fatal("PATH should pass through, or nothing can be executed")
	}

	// What the plugin declares still arrives, and a $ value still resolves from the key store
	if got["ACME_TOKEN"] != "tok-123" || got["PLAIN"] != "value" {
		t.Fatalf("declared env should survive: %+v", got)
	}
}

// The write direction must be line-delimited JSON. This assertion is faster than the end-to-end test and
// points straight at the cause when it breaks.
func TestWriteMCPFrameIsLineDelimited(t *testing.T) {
	var buf bytes.Buffer
	if err := writeMCPFrame(&buf, []byte(`{"jsonrpc":"2.0","id":1}`)); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != `{"jsonrpc":"2.0","id":1}`+"\n" {
		t.Fatalf("frame must be one line of JSON plus \\n, got %q", got)
	}
}

func TestReadMCPFrame(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"result":{}}`
	framed := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
	got, err := readMCPFrame(bufio.NewReader(strings.NewReader(framed)))
	if err != nil || string(got) != body {
		t.Fatalf("Content-Length framing: %q %v", got, err)
	}
	got, err = readMCPFrame(bufio.NewReader(strings.NewReader(body + "\n")))
	if err != nil || string(got) != body {
		t.Fatalf("NDJSON framing: %q %v", got, err)
	}
}
