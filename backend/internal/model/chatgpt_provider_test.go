package model

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"botbureau/backend/internal/secret"
)

func TestChatGPTResolveKeyPrefersAPIKey(t *testing.T) {
	dir := t.TempDir()
	ks := secret.NewKeyStore(filepath.Join(dir, "keys.json"))
	_ = ks.Set("OPENAI_API_KEY", "sk-from-store")
	c := secret.NewChatGPTOAuth(filepath.Join(dir, "chatgpt_oauth.json"))
	c.Restore("oauth-token", "acct", time.Now().Add(time.Hour))
	p := newOpenAIProvider("gpt-5.4", "https://api.openai.com/v1", "OPENAI_API_KEY", "", ks, nil, c, "")
	if got := p.resolveKey(); got != "sk-from-store" {
		t.Fatalf("API key should win: %s", got)
	}
	if p.usingCodex() {
		t.Fatal("API key should not use Codex")
	}
	_ = ks.Delete("OPENAI_API_KEY")
	if got := p.resolveKey(); got != "oauth-token" {
		t.Fatalf("oauth fallback: %s", got)
	}
	if !p.usingCodex() || p.responsesEndpoint() != secret.ChatGPTCodexURL {
		t.Fatalf("expected Codex endpoint, got %s", p.responsesEndpoint())
	}
}

func TestCodexRequestHeaders(t *testing.T) {
	var gotAuth, gotAcct, gotOrig string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAcct = r.Header.Get("ChatGPT-Account-Id")
		gotOrig = r.Header.Get("originator")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "completed",
			"output": []map[string]any{{
				"type":    "message",
				"content": []map[string]any{{"type": "output_text", "text": "hi"}},
			}},
		})
	}))
	defer srv.Close()
	c := secret.NewChatGPTOAuth("")
	c.SetAPIURL(srv.URL)
	c.Restore("oauth-token", "acct-9", time.Now().Add(time.Hour))
	p := newOpenAIProvider("gpt-5.4", "https://api.openai.com/v1", "OPENAI_API_KEY", "", nil, nil, c, "")
	sess := p.NewSession()
	sess.AddUser("hello")
	res, err := sess.Step(context.Background(), "sys", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Texts, "") != "hi" {
		t.Fatalf("got %+v", res)
	}
	if gotAuth != "Bearer oauth-token" || gotAcct != "acct-9" || gotOrig != "botbureau" {
		t.Fatalf("headers auth=%s acct=%s orig=%s", gotAuth, gotAcct, gotOrig)
	}
}
