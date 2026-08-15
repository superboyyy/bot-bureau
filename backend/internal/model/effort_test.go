package model

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"botbureau/backend/internal/config"
)

// 思考强度在三条路径上的旋钮名字不同，但都必须"选了才发"。
// 留空还硬塞一个字段，会把不支持思考的模型直接打成 400——这是最要紧的一条。
//
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

	// chat/completions：顶层 reasoning_effort
	if b := step(srv.URL+"/v1", config.EffortHigh); b["reasoning_effort"] != "high" {
		t.Fatalf("chat/completions should carry reasoning_effort, got %+v", b)
	}
	if b := step(srv.URL+"/v1", ""); b["reasoning_effort"] != nil {
		t.Fatalf("an unset effort must send no reasoning_effort, got %+v", b["reasoning_effort"])
	}

	// Responses：reasoning.effort
	b := step(srv.URL+"/v1/responses", config.EffortLow)
	r, ok := b["reasoning"].(map[string]any)
	if !ok || r["effort"] != "low" {
		t.Fatalf("responses should carry reasoning.effort, got %+v", b["reasoning"])
	}
	if b := step(srv.URL+"/v1/responses", ""); b["reasoning"] != nil {
		t.Fatalf("an unset effort must send no reasoning block, got %+v", b["reasoning"])
	}
}

// 档位到 Anthropic 思考预算的换算：必须单调递增，且始终低于总输出上限
// （预算比 max_tokens 还大的请求会被直接拒）。
//
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
	for _, bad := range []string{"HIGH", "max", "yes", "2"} {
		if config.ValidEffort(bad) {
			t.Fatalf("%q should not be a valid effort", bad)
		}
		if config.ThinkingBudget(bad) != 0 {
			t.Fatalf("%q should map to no thinking", bad)
		}
	}
}
