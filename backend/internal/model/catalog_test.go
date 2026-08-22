package model

import (
	"path/filepath"
	"testing"
	"time"

	"botbureau/backend/internal/secret"
)

func TestExplicitSubscriptionBeatsStoredKey(t *testing.T) {
	dir := t.TempDir()
	ks := secret.NewKeyStore(filepath.Join(dir, "keys.json"))
	if err := ks.Set("OPENAI_API_KEY", "sk-unrelated"); err != nil {
		t.Fatal(err)
	}
	if err := ks.Set("XAI_API_KEY", "sk-unrelated-xai"); err != nil {
		t.Fatal(err)
	}
	c := secret.NewChatGPTOAuth(filepath.Join(dir, "chatgpt_oauth.json"))
	c.Restore("chatgpt-token", "acct", time.Now().Add(time.Hour))
	x := secret.NewXaiOAuth(filepath.Join(dir, "xai_oauth.json"))
	x.Restore("xai-token", time.Now().Add(time.Hour))

	gpt := newOpenAIProvider("gpt-x", "https://api.openai.com/v1", "OPENAI_API_KEY", AuthChatGPT, ks, x, c, "")
	if got := gpt.resolveKey(); got != "chatgpt-token" {
		t.Fatalf("chose the ChatGPT subscription but used %q", got)
	}
	if !gpt.usingCodex() || gpt.responsesEndpoint() != secret.ChatGPTCodexURL {
		t.Fatalf("a subscription should use the Codex endpoint, got %s", gpt.responsesEndpoint())
	}

	grok := newOpenAIProvider("grok-x", "https://api.x.ai/v1", "XAI_API_KEY", AuthXai, ks, x, c, "")
	if got := grok.resolveKey(); got != "xai-token" {
		t.Fatalf("chose the SuperGrok subscription but used %q", got)
	}

	// And the reverse: an explicitly chosen key must not be jumped by a connected subscription
	byKey := newOpenAIProvider("gpt-x", "https://api.openai.com/v1", "OPENAI_API_KEY", AuthKey, ks, x, c, "")
	if got := byKey.resolveKey(); got != "sk-unrelated" {
		t.Fatalf("chose the API key but used %q", got)
	}
	if byKey.usingCodex() {
		t.Fatal("choosing the API key must not use the Codex endpoint")
	}
}

// A bot set to a subscription must not silently fall back to an API key when not signed in.
func TestSubscriptionWithoutLoginYieldsNoCredential(t *testing.T) {
	dir := t.TempDir()
	ks := secret.NewKeyStore(filepath.Join(dir, "keys.json"))
	if err := ks.Set("OPENAI_API_KEY", "sk-unrelated"); err != nil {
		t.Fatal(err)
	}
	c := secret.NewChatGPTOAuth(filepath.Join(dir, "chatgpt_oauth.json"))
	p := newOpenAIProvider("gpt-x", "https://api.openai.com/v1", "OPENAI_API_KEY", AuthChatGPT, ks, nil, c, "")
	if got := p.resolveKey(); got != "" {
		t.Fatalf("got a credential %q while not signed in", got)
	}
}

func TestProviderCatalogShape(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range ProviderCatalog() {
		if p.ID == "" || p.Label == "" || p.Provider == "" {
			t.Fatalf("incomplete entry: %+v", p)
		}
		if seen[p.ID] {
			t.Fatalf("duplicate vendor id %q", p.ID)
		}
		seen[p.ID] = true
		if len(p.Auth) == 0 {
			t.Fatalf("%s has no auth mode", p.ID)
		}
		if !p.Custom && p.Provider == "openai" && p.BaseURL == "" {
			t.Fatalf("%s is not custom and should carry a base_url", p.ID)
		}
	}
	for _, want := range []string{"anthropic", "openai", "xai", "custom", "fake"} {
		if !seen[want] {
			t.Fatalf("the catalog is missing %s", want)
		}
	}

	// The subscription modes must hang off the two vendors that support them, or the UI never offers a login
	if opt := providerOption("openai"); opt == nil || opt.Auth[0] != AuthChatGPT {
		t.Fatal("the OpenAI entry should default to the ChatGPT subscription")
	}
	if opt := providerOption("xai"); opt == nil || opt.Auth[0] != AuthXai {
		t.Fatal("the xAI entry should default to the SuperGrok subscription")
	}
}

func TestKimiCodeIsAMembershipKeyNotOpenPlatform(t *testing.T) {
	code := providerOption("kimi-code")
	if code == nil {
		t.Fatal("missing kimi-code vendor")
	}
	if code.BaseURL != "https://api.kimi.com/coding/v1" || code.KeyEnv != "KIMI_CODE_API_KEY" {
		t.Fatalf("kimi-code should hit the coding endpoint with its own key name: %+v", code)
	}
	if len(code.Auth) != 1 || code.Auth[0] != AuthKey {
		t.Fatalf("official third-party Kimi Code is a console key, not an OAuth login: %+v", code.Auth)
	}

	// The Open Platform entry must stay: the two keys and endpoints are not interchangeable
	payg := providerOption("moonshot")
	if payg == nil || payg.BaseURL != "https://api.moonshot.cn/v1" || payg.KeyEnv != "MOONSHOT_API_KEY" {
		t.Fatalf("the pay-as-you-go Kimi entry should stay: %+v", payg)
	}
	if payg.KeyEnv == code.KeyEnv || payg.BaseURL == code.BaseURL {
		t.Fatal("the two Kimi products must not share a key or a URL")
	}
}
