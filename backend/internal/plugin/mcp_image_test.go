package plugin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"botbureau/backend/internal/secret"
)

// 插件返回的图片必须一路带出来，而不是在协议边界上被换成一句"[image content omitted]"。
// Playwright 的截图就走这条路——它在一键安装目录里，之前等于半残。
//
// An image returned by a plugin has to make it all the way out rather than being swapped for
// "[image content omitted]" at the protocol boundary. Playwright's screenshots take this path, and it
// sits in the one-click catalog, so it was half-broken before.
func TestCallCarriesImages(t *testing.T) {
	const tiny = "iVBORw0KGgoAAAANSUhEUg=="
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		rw.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			writeRPC(rw, req.ID, map[string]any{"protocolVersion": mcpProtocolVersion,
				"capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "shot", "version": "1"}})
		case "tools/list":
			writeRPC(rw, req.ID, map[string]any{"tools": []map[string]any{
				{"name": "screenshot", "description": "take one",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}},
			}})
		case "tools/call":
			writeRPC(rw, req.ID, map[string]any{"isError": false, "content": []map[string]any{
				{"type": "text", "text": "captured"},
				{"type": "image", "data": tiny, "mimeType": "image/png"},
				{"type": "audio", "data": "zzz", "mimeType": "audio/wav"},
			}})
		default:
			rw.WriteHeader(202)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	ks := secret.NewKeyStore(filepath.Join(dir, "keys.json"))
	mgr := NewMCPManager(filepath.Join(dir, "mcp.yaml"), ks)
	if err := mgr.Add(MCPServerConfig{Name: "shot", URL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	res, isErr, err := mgr.Call("shot", "screenshot", nil)
	if err != nil || isErr {
		t.Fatalf("call failed: %v %v", isErr, err)
	}
	if len(res.Images) != 1 || res.Images[0].Base64 != tiny || res.Images[0].MIME != "image/png" {
		t.Fatalf("the image should survive: %+v", res.Images)
	}
	if !strings.Contains(res.Text, "captured") {
		t.Fatalf("text should still come through: %q", res.Text)
	}
	// 还不支持的类型仍然如实说明，不静默丢弃
	// A type still unsupported is stated rather than silently dropped
	if !strings.Contains(res.Text, "audio") {
		t.Fatalf("an unsupported block should say so: %q", res.Text)
	}
}

// 超大的图要丢掉但说清楚——几 MB 的 base64 塞进上下文既贵又能把这一轮撑爆。
// An oversized image is dropped with a note: several megabytes of base64 in the context is both
// expensive and enough to blow up the turn.
func TestOversizedImageIsReportedNotEmbedded(t *testing.T) {
	huge := strings.Repeat("A", ImageDataLimit+1)
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		rw.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			writeRPC(rw, req.ID, map[string]any{"protocolVersion": mcpProtocolVersion,
				"capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "big", "version": "1"}})
		case "tools/list":
			writeRPC(rw, req.ID, map[string]any{"tools": []map[string]any{
				{"name": "shot", "description": "d",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}},
			}})
		case "tools/call":
			writeRPC(rw, req.ID, map[string]any{"isError": false, "content": []map[string]any{
				{"type": "image", "data": huge, "mimeType": "image/png"},
			}})
		default:
			rw.WriteHeader(202)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	ks := secret.NewKeyStore(filepath.Join(dir, "keys.json"))
	mgr := NewMCPManager(filepath.Join(dir, "mcp.yaml"), ks)
	if err := mgr.Add(MCPServerConfig{Name: "big", URL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	res, _, err := mgr.Call("big", "shot", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Images) != 0 {
		t.Fatal("an oversized image must not be embedded")
	}
	if !strings.Contains(res.Text, "too large") {
		t.Fatalf("it should say why nothing came back: %q", res.Text)
	}
}
