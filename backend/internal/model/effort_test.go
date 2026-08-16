package model

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"botbureau/backend/internal/config"
)

// The knob has a different name on each path, but all three must send it only when a tier was chosen.
// Forcing the field on an unset bot is a plain 400 from any model that does not support thinking,
// which is the case that matters most.
func TestEffortReachesEachPathOnlyWhenSet(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		rw.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/responses" {
			_, _ = rw.Write([]byte(`{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`))
			return
		}
		_, _ = rw.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	step := func(base, effort string) map[string]any {
		got = nil
		p := newOpenAIProvider("m", base, "K", AuthKey, nil, nil, nil, effort)
		sess := p.NewSession()
		sess.AddUser("hi")
		if _, err := sess.Step(context.Background(), "sys", nil, false); err != nil {
			t.Fatalf("step failed: %v", err)
		}
		return got
	}

	if b := step(srv.URL+"/v1", config.EffortHigh); b["reasoning_effort"] != "high" {
		t.Fatalf("chat/completions should carry reasoning_effort, got %+v", b)
	}
	if b := step(srv.URL+"/v1", ""); b["reasoning_effort"] != nil {
		t.Fatalf("an unset effort must send no reasoning_effort, got %+v", b["reasoning_effort"])
	}

	// Responses API: reasoning.effort
	b := step(srv.URL+"/v1/responses", config.EffortLow)
	r, ok := b["reasoning"].(map[string]any)
	if !ok || r["effort"] != "low" {
		t.Fatalf("responses should carry reasoning.effort, got %+v", b["reasoning"])
	}
	if b := step(srv.URL+"/v1/responses", ""); b["reasoning"] != nil {
		t.Fatalf("an unset effort must send no reasoning block, got %+v", b["reasoning"])
	}
}

// The tier-to-budget mapping for Anthropic must increase monotonically and stay under the total output
// cap — a budget larger than max_tokens is rejected outright.
func TestThinkingBudgetShape(t *testing.T) {
	if config.ThinkingBudget("") != 0 {
		t.Fatal("an unset effort must leave extended thinking off")
	}
	lo, mid, hi := config.ThinkingBudget(config.EffortLow), config.ThinkingBudget(config.EffortMedium), config.ThinkingBudget(config.EffortHigh)
	if !(0 < lo && lo < mid && mid < hi) {
		t.Fatalf("budgets should increase: %d %d %d", lo, mid, hi)
	}
	if hi >= config.MaxTokens {
		t.Fatalf("the highest budget %d must stay below MaxTokens %d", hi, config.MaxTokens)
	}
	for _, bad := range []string{"HIGH", "MAX", "yes", "2"} {
		if config.ValidEffort(bad) {
			t.Fatalf("%q should not be a valid effort", bad)
		}
		if config.ThinkingBudget(bad) != 0 {
			t.Fatalf("%q should map to no thinking", bad)
		}
	}
	for _, notAnthropicBudget := range []string{config.EffortNone, config.EffortXHigh, config.EffortMax} {
		if config.ThinkingBudget(notAnthropicBudget) != 0 {
			t.Fatalf("%q should not be converted to an Anthropic budget", notAnthropicBudget)
		}
	}
}

func TestDeepSeekEffortUsesBetaEndpoint(t *testing.T) {
	if got := chatCompletionsURL("https://api.deepseek.com/v1", config.EffortHigh); got != "https://api.deepseek.com/beta/chat/completions" {
		t.Fatalf("DeepSeek effort should use beta endpoint, got %s", got)
	}
	if got := chatCompletionsURL("https://api.deepseek.com/v1", ""); got != "https://api.deepseek.com/v1/chat/completions" {
		t.Fatalf("default DeepSeek request should keep the normal endpoint, got %s", got)
	}
}

// Tiers are served per concrete model: two models under one provider can accept different values.
func TestEffortOptionsFollowTheModel(t *testing.T) {
	ids := func(provider, model string) []string {
		var out []string
		for _, o := range config.EffortOptionsForModel(provider, model) {
			out = append(out, o["id"].(string))
		}
		return out
	}

	tests := []struct {
		provider string
		model    string
		want     []string
	}{
		{"anthropic", "claude-opus-5", []string{"", "low", "medium", "high"}},
		{"openai", "gpt-5", []string{"", "minimal", "low", "medium", "high"}},
		{"openai", "gpt-5.6-luna", []string{"", "none", "low", "medium", "high", "xhigh", "max"}},
		{"openai", "gpt-4.1", []string{""}},
		{"xai", "grok-4.6", []string{"", "low", "medium", "high", "xhigh"}},
		{"xai", "grok-4.5", []string{"", "low", "medium", "high"}},
		{"xai", "grok-3", []string{""}},
		{"deepseek", "deepseek-v4-flash", []string{"", "high", "max"}},
		{"deepseek", "deepseek-v4-pro", []string{"", "high", "max"}},
		{"deepseek", "deepseek-reasoner", []string{"", "high", "max"}},
		{"deepseek", "deepseek-chat", []string{""}},
		{"custom", "gpt-5", []string{"", "minimal", "low", "medium", "high"}},
		{"fake", "fake", []string{""}},
		{"custom", "qwen3", []string{""}},
	}
	for _, tt := range tests {
		if got := ids(tt.provider, tt.model); !equalStrings(got, tt.want) {
			t.Fatalf("%s/%s: got %v, want %v", tt.provider, tt.model, got, tt.want)
		}
	}

	if !config.EffortSupportedForModel("xai", "grok-4.6", config.EffortXHigh) {
		t.Fatal("grok-4.6 should offer xhigh")
	}
	if config.EffortSupportedForModel("xai", "grok-4.6", config.EffortNone) {
		t.Fatal("grok-4.6 should not offer none")
	}
	if !config.EffortSupportedForModel("openai", "gpt-5.6-luna", config.EffortMax) {
		t.Fatal("gpt-5.6-luna should offer max")
	}
	if config.EffortSupportedForModel("openai", "gpt-5.6-luna", config.EffortMinimal) {
		t.Fatal("gpt-5.6-luna should not offer minimal")
	}
	if !config.EffortSupportedForModel("openai", "gpt-5", config.EffortMinimal) {
		t.Fatal("gpt-5 should offer minimal")
	}
	if config.EffortSupportedForModel("openai", "gpt-4.1", config.EffortHigh) {
		t.Fatal("gpt-4.1 should not offer a reasoning effort")
	}
	if config.EffortSupportedForModel("some-self-hosted-thing", "local-model", config.EffortMedium) {
		t.Fatal("unknown models should not guess a reasoning parameter")
	}
	// empty is accepted everywhere, including unknown models
	for _, p := range []string{"anthropic", "openai", "xai", "fake"} {
		if !config.EffortSupportedForModel(p, "unknown", "") {
			t.Fatalf("%s should accept the vendor default", p)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
