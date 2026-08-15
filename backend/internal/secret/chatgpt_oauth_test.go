package secret

import (
	"encoding/base64"
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

func testJWT(account string) string {
	payload, _ := json.Marshal(map[string]any{"chatgpt_account_id": account})
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(payload) + ".x"
}

func TestChatGPTDeviceLoginAndRefresh(t *testing.T) {
	var polls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/api/accounts/deviceauth/usercode"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_auth_id": "DA", "user_code": "ABCD-1234", "interval": "1",
			})
		case strings.HasSuffix(r.URL.Path, "/api/accounts/deviceauth/token"):
			polls++
			if polls < 2 {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"authorization_code": "AC", "code_verifier": "VER",
			})
		case strings.HasSuffix(r.URL.Path, "/oauth/token"):
			body, _ := io.ReadAll(r.Body)
			vals, _ := url.ParseQuery(string(body))
			if vals.Get("grant_type") == "refresh_token" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"access_token": testJWT("acct"), "refresh_token": "RT2", "expires_in": 3600,
				})
				return
			}
			if vals.Get("code") != "AC" || vals.Get("code_verifier") != "VER" {
				t.Errorf("exchange fields: %s", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": testJWT("acct"), "id_token": testJWT("acct"),
				"refresh_token": "RT1", "expires_in": 1,
			})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "chatgpt_oauth.json")
	c := NewChatGPTOAuth(path)
	c.issuer = srv.URL
	st, err := c.Start()
	if err != nil {
		t.Fatal(err)
	}
	if st["user_code"] != "ABCD-1234" {
		t.Fatalf("status: %v", st)
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && !c.Connected() {
		time.Sleep(50 * time.Millisecond)
	}
	if !c.Connected() || c.AccountID() != "acct" {
		t.Fatalf("not connected: %+v", c.Status())
	}
	c.mu.Lock()
	c.stored.Expires = time.Now().Add(-time.Minute).Unix()
	c.mu.Unlock()
	tok, err := c.Bearer()
	if err != nil || tok == "" {
		t.Fatalf("refresh: %q %v", tok, err)
	}
	if !NewChatGPTOAuth(path).Connected() {
		t.Fatal("tokens should persist")
	}
}
