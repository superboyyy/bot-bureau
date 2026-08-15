package model

import (
	"path/filepath"
	"testing"
	"time"

	"botbureau/backend/internal/secret"
)

func TestXaiResolveKeyPrefersAPIKey(t *testing.T) {
	dir := t.TempDir()
	ks := secret.NewKeyStore(filepath.Join(dir, "keys.json"))
	_ = ks.Set("XAI_API_KEY", "sk-from-store")
	x := secret.NewXaiOAuth(filepath.Join(dir, "xai_oauth.json"))
	x.Restore("oauth-token", time.Now().Add(time.Hour))
	p := newOpenAIProvider("grok-4", "https://api.x.ai/v1", "XAI_API_KEY", "", ks, x, nil, "")
	if got := p.resolveKey(); got != "sk-from-store" {
		t.Fatalf("API key should win: %s", got)
	}
	_ = ks.Delete("XAI_API_KEY")
	if got := p.resolveKey(); got != "oauth-token" {
		t.Fatalf("oauth fallback: %s", got)
	}
}
