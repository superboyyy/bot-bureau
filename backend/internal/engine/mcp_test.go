package engine

import (
	"botbureau/backend/internal/plugin"

	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A minimal stdio MCP server (python3): initialize / tools/list / tools/call.
// It speaks the MCP spec — one JSON object per line. That has to follow the spec rather than the client:
// this fake used to use LSP Content-Length framing just like the client of the day, both wrong together,
// which let the tests hide the fact that no real plugin could connect at all.
const fakeMCPServerPy = `#!/usr/bin/env python3
import sys, json
def send(o):
    sys.stdout.write(json.dumps(o) + "\n")
    sys.stdout.flush()
def read_one():
    line = sys.stdin.readline()
    if not line: return None
    line = line.strip()
    if not line: return read_one()
    return json.loads(line)
while True:
    m = read_one()
    if m is None: break
    mid, meth = m.get("id"), m.get("method")
    if meth == "initialize":
        send({"jsonrpc":"2.0","id":mid,"result":{"protocolVersion":m["params"]["protocolVersion"],
              "capabilities":{"tools":{}},"serverInfo":{"name":"fake","version":"1.0"}}})
    elif meth == "tools/list":
        send({"jsonrpc":"2.0","id":mid,"result":{"tools":[
            {"name":"echo","description":"回声","inputSchema":{"type":"object",
             "properties":{"text":{"type":"string"}},"required":["text"]},
             "annotations":{"readOnlyHint":True}},
            {"name":"write_thing","description":"有副作用","inputSchema":{"type":"object",
             "properties":{"text":{"type":"string"}}}}
        ]}})
    elif meth == "tools/call":
        p = m["params"]; args = p.get("arguments", {})
        send({"jsonrpc":"2.0","id":mid,"result":{"isError":False,
              "content":[{"type":"text","text":p["name"]+":"+args.get("text","")}]}})
    elif mid is not None:
        send({"jsonrpc":"2.0","id":mid,"error":{"code":-32601,"message":"nope"}})
`

func writeFakeMCPServer(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("this test needs python3")
	}
	p := filepath.Join(t.TempDir(), "fake_mcp.py")
	if err := os.WriteFile(p, []byte(fakeMCPServerPy), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMCPStdioEndToEnd(t *testing.T) {
	script := writeFakeMCPServer(t)
	w, bus, _ := newTestWorker(t, "a", nil)
	mgr := w.deps.MCP
	if err := mgr.Add(plugin.MCPServerConfig{Name: "fake", Command: "python3", Args: []string{script}}); err != nil {
		t.Fatal(err)
	}
	tools := mgr.Tools("fake")
	if len(tools) != 2 || tools[0].Name != "echo" || !tools[0].ReadOnly || tools[1].ReadOnly {
		t.Fatalf("wrong tool parsing: %+v", tools)
	}

	// After subscribing, prefixed plugin tools appear in defs
	w.toolbox.mcpServers = []string{"fake"}
	var names []string
	for _, d := range w.toolbox.Defs() {
		names = append(names, d.Name)
	}
	if !strings.Contains(strings.Join(names, ","), "mcp_fake_echo") {
		t.Fatalf("defs should include the plugin tools: %v", names)
	}

	// Read-only tools execute directly
	w.toolbox.currentChat = "dm"
	out, isErr := w.toolbox.Execute("mcp_fake_echo", map[string]any{"text": "hi"})
	if isErr || out != "echo:hi" {
		t.Fatalf("echo failed: %q %v", out, isErr)
	}

	// Non-read-only tools go through approval (rejected)
	go func() {
		for len(bus.PendingApprovals()) == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		bus.Decide(bus.PendingApprovals()[0].ID, false, "no")
	}()
	out, isErr = w.toolbox.Execute("mcp_fake_write_thing", map[string]any{"text": "x"})
	if !isErr || !strings.Contains(out, "rejected") {
		t.Fatalf("a non-read-only plugin must go through approval: %q %v", out, isErr)
	}

	// Actually invoked after approval
	go func() {
		for len(bus.PendingApprovals()) == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		bus.Decide(bus.PendingApprovals()[0].ID, true, "")
	}()
	out, isErr = w.toolbox.Execute("mcp_fake_write_thing", map[string]any{"text": "y"})
	if isErr || out != "write_thing:y" {
		t.Fatalf("should run after approval: %q %v", out, isErr)
	}

	// Unsubscribed bots cannot use it
	w.toolbox.mcpServers = nil
	if _, isErr := w.toolbox.Execute("mcp_fake_echo", map[string]any{"text": "z"}); !isErr {
		t.Fatal("an unsubscribed bot should error")
	}

	// Persistence + removal
	raw, _ := os.ReadFile(mgr.Path())
	if !strings.Contains(string(raw), "fake") {
		t.Fatal("mcp.yaml was not persisted")
	}
	if !mgr.Remove("fake") || mgr.Has("fake") {
		t.Fatal("removal failed")
	}
}

func TestMCPConfigValidation(t *testing.T) {
	dir := t.TempDir()
	mgr := plugin.NewMCPManager(filepath.Join(dir, "mcp.yaml"), nil)
	if err := mgr.Add(plugin.MCPServerConfig{Name: "Bad Name", Command: "x"}); err == nil {
		t.Fatal("an invalid name should error")
	}
	if err := mgr.Add(plugin.MCPServerConfig{Name: "both", Command: "x", URL: "http://x"}); err == nil {
		t.Fatal("giving both command and url should error")
	}
	if err := mgr.Add(plugin.MCPServerConfig{Name: "neither"}); err == nil {
		t.Fatal("giving neither should error")
	}
}
