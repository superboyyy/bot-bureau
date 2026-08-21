package httpx

import (
	"errors"
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

func TestReadJSONRejectsOversize(t *testing.T) {
	// One extra byte past the cap used to be truncated and then fail as invalid JSON. The caller
	// needs to tell those two cases apart — a screenshot is oversize, a truncated object is garbage.
	body := `{"name":"` + strings.Repeat("a", defaultJSONMax) + `"}`
	req := httptest.NewRequest("POST", "/", strings.NewReader(body))
	var got struct {
		Name string `json:"name"`
	}
	if err := ReadJSON(req, &got); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize body: %v", err)
	}
}

func TestReadJSONMaxAcceptsBodyTheDefaultCapWouldRefuse(t *testing.T) {
	body := `{"name":"` + strings.Repeat("a", defaultJSONMax) + `"}`
	req := httptest.NewRequest("POST", "/", strings.NewReader(body))
	var got struct {
		Name string `json:"name"`
	}
	if err := ReadJSONMax(req, &got, int64(len(body))); err != nil {
		t.Fatal(err)
	}
	if got.Name != strings.Repeat("a", defaultJSONMax) {
		t.Fatalf("decoded name length = %d", len(got.Name))
	}
}
