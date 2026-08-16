package httpx

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteJSON(t *testing.T) {
	rw := httptest.NewRecorder()
	WriteJSON(rw, 201, map[string]any{"ok": true, "n": 2})
	if rw.Code != 201 {
		t.Fatalf("status = %d, want 201", rw.Code)
	}
	if got := rw.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q", got)
	}
	if got := strings.TrimSpace(rw.Body.String()); got != `{"n":2,"ok":true}` && got != `{"ok":true,"n":2}` {
		t.Fatalf("body = %q", got)
	}
}

func TestReadJSON(t *testing.T) {
	req := httptest.NewRequest("POST", "/", strings.NewReader(`{"name":"wren","enabled":true}`))
	var got struct {
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
	}
	if err := ReadJSON(req, &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "wren" || !got.Enabled {
		t.Fatalf("decoded = %#v", got)
	}
	bad := httptest.NewRequest("POST", "/", strings.NewReader("not-json"))
	if err := ReadJSON(bad, &got); err == nil {
		t.Fatal("invalid JSON should fail")
	}
}
