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
	// 最小 Streamable HTTP MCP server：直接回 JSON
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

// 远程连接器要按服务端说定的版本继续说话，并且用完把会话还回去。
// 以前 initialize 的返回值被丢掉、close() 是空函数：版本从不协商，会话一路挂在对方那里。
//
// A remote connector must keep speaking the version the server settled on, and hand the session back
// when done. The initialize result used to be discarded and close() did nothing: the version was never
// negotiated and sessions were left dangling on the far side.
func TestHTTPVersionNegotiationAndSessionTermination(t *testing.T) {
	const serverPicks = "2025-03-26" // 服务端挑了个和我们提议的不同的版本 / the server picks a different one
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

// TestMCPStdioTransport 端到端跑一遍本地插件：起进程、握手、列工具、调用。
// 用测试二进制自己当插件进程（BOTBUREAU_MCP_FAKE=1 时进入 server 分支），不引入外部依赖。
// 关键在于这个假 server 只按行读 JSON——和官方 SDK 的 stdio server 一样。之前客户端写的是
// LSP 的 Content-Length 帧，真插件一个都握不上手，而当时的测试只测了读方向，没测出来。
//
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

// TestStdioConnectMissingCommand 命令不存在时要老实报错，不能崩。
// 曾经会 panic：newStdioConn 返回的 (*stdioConn)(nil) 被装进 mcpConn 接口后，
// connect 里的 conn != nil 判断拦不住它，close() 一调就空指针。
//
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

// runFakeStdioServer 是个最小 MCP server：只按行读、按行写。
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
			continue // 坏消息或通知 / a bad message or a notification
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

// 插件进程自己没了的时候，状态必须立刻变成不可用并自动重连——而不是留着一颗绿点，
// 让模型对着一份已经不存在的工具列表接着调。
//
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

	// 从外面杀掉进程，模拟插件崩溃
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

	// 先掉到不可用 / first it must fall out of the connected state
	if !waitFor(2*time.Second, func() bool { return statusOf(mgr, "local") != "connected" }) {
		t.Fatalf("a dead plugin must not keep reporting connected, got %q", statusOf(mgr, "local"))
	}
	// 再自己回来 / then it must come back on its own
	if !waitFor(20*time.Second, func() bool { return statusOf(mgr, "local") == "connected" }) {
		t.Fatalf("it should reconnect by itself, still %q", statusOf(mgr, "local"))
	}
	if tools := mgr.Tools("local"); len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools should be back after reconnecting: %+v", tools)
	}
}

// 主动删除不该触发重连——否则删掉的插件会自己爬回来。
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

// 插件进程只该拿到白名单里的环境变量。这条守的是一个安全边界：面板上点一下就能装的东西，
// 不该顺手拿到 SSH_AUTH_SOCK 和你 shell 里的各种令牌。
//
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
	// 代理必须过去，否则公司网里所有插件都连不出去，而症状只是"超时"
	// The proxy has to get through, or every plugin behind a corporate network fails with nothing but a
	// timeout to go on
	if got["HTTPS_PROXY"] != "http://proxy.corp:8080" {
		t.Fatalf("HTTPS_PROXY should pass through, got %q", got["HTTPS_PROXY"])
	}
	if got["PATH"] == "" {
		t.Fatal("PATH should pass through, or nothing can be executed")
	}
	// 插件自己声明的照旧，$ 开头的仍从密钥仓库解析
	// What the plugin declares still arrives, and a $ value still resolves from the key store
	if got["ACME_TOKEN"] != "tok-123" || got["PLAIN"] != "value" {
		t.Fatalf("declared env should survive: %+v", got)
	}
}

// 写方向必须是按行 JSON。这条断言比端到端那条快，坏了也更直指原因。
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
