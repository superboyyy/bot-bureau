package secret

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestXaiDeviceLoginAndRefresh(t *testing.T) {
	var tokenCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form := string(body)
		vals, _ := url.ParseQuery(form)
		switch {
		case strings.HasSuffix(r.URL.Path, "/device/code"):
			if vals.Get("client_id") != xaiClientID || !strings.Contains(vals.Get("scope"), "api:access") {
				t.Errorf("device request missing fields: %s", form)
			}
			_ = json.NewEncoder(rw).Encode(map[string]any{
				"device_code": "DC", "user_code": "ABCD-1234",
				"verification_uri":          "https://x.ai/device",
				"verification_uri_complete": "https://x.ai/device?user_code=ABCD-1234",
				"expires_in":                60, "interval": 1,
			})
		case strings.HasSuffix(r.URL.Path, "/token"):
			tokenCalls++
			if strings.Contains(form, "refresh_token=") {
				_ = json.NewEncoder(rw).Encode(map[string]any{
					"access_token": "AT2", "refresh_token": "RT2", "expires_in": 3600,
				})
				return
			}
			if tokenCalls == 1 {
				rw.WriteHeader(400)
				_ = json.NewEncoder(rw).Encode(map[string]any{"error": "authorization_pending"})
				return
			}
			_ = json.NewEncoder(rw).Encode(map[string]any{
				"access_token": "AT1", "refresh_token": "RT1", "expires_in": 3600,
			})
		default:
			rw.WriteHeader(404)
		}
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "xai_oauth.json")
	x := NewXaiOAuth(path)
	x.deviceURL = srv.URL + "/oauth2/device/code"
	x.tokenURL = srv.URL + "/oauth2/token"

	st, err := x.Start()
	if err != nil {
		t.Fatal(err)
	}
	if st["user_code"] != "ABCD-1234" {
		t.Fatalf("user_code: %v", st)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if x.Connected() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !x.Connected() {
		t.Fatalf("should connect: %v", x.Status())
	}
	tok, err := x.Bearer()
	if err != nil || tok != "AT1" {
		t.Fatalf("bearer: %q %v", tok, err)
	}
	x.mu.Lock()
	x.stored.Expires = time.Now().Add(-time.Minute).Unix()
	x.mu.Unlock()
	tok, err = x.Bearer()
	if err != nil || tok != "AT2" {
		t.Fatalf("refreshed bearer: %q %v", tok, err)
	}
	if NewXaiOAuth(path).Connected() == false {
		t.Fatal("tokens should persist")
	}
}
