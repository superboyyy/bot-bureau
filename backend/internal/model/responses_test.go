package model

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUsesResponsesAPI(t *testing.T) {
	if !usesResponsesAPI("https://opencode.ai/zen/v1", "gpt-5.4") {
		t.Fatal("zen gpt should use responses")
	}
	if !usesResponsesAPI("https://opencode.ai/zen/go/v1", "grok-4.5") {
		t.Fatal("go grok should use responses")
	}
	if usesResponsesAPI("https://opencode.ai/zen/v1", "kimi-k2.6") {
		t.Fatal("zen kimi should stay on chat completions")
	}
	if !usesResponsesAPI("https://api.openai.com/v1", "gpt-4.1") {
		t.Fatal("official openai should use responses")
	}
	if !usesResponsesAPI("https://api.openai.com/v1/responses", "gpt-5.4") {
		t.Fatal("explicit /responses should win")
	}
}

func TestResponsesTextAndTools(t *testing.T) {
	var paths []string
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		bodies = append(bodies, body)
		if len(paths) == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "completed",
				"output": []map[string]any{{
					"type": "function_call", "call_id": "call_1",
					"name": "echo", "arguments": `{"q":"hi"}`,
				}},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "completed",
			"output": []map[string]any{{
				"type":    "message",
				"content": []map[string]any{{"type": "output_text", "text": "ok"}},
			}},
		})
	}))
	defer srv.Close()

	p := newOpenAIProvider("gpt-5.4", srv.URL+"/v1/responses", "OPENCODE_API_KEY", "", nil, nil, nil, "")
	sess := p.NewSession()
	sess.AddUser("hello")
	res, err := sess.Step(context.Background(), "sys", []ToolDef{{
		Name: "echo", Description: "echo", Properties: map[string]any{"q": map[string]any{"type": "string"}}, Required: []string{"q"},
	}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != "tool_use" || len(res.ToolCalls) != 1 || res.ToolCalls[0].ID != "call_1" {
		t.Fatalf("tool step: %+v", res)
	}
	sess.AddToolResults([]ToolResult{{ID: "call_1", Content: "hi"}})
	res, err = sess.Step(context.Background(), "sys", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != "end_turn" || strings.Join(res.Texts, "") != "ok" {
		t.Fatalf("text step: %+v", res)
	}
	if len(paths) != 2 || !strings.HasSuffix(paths[0], "/responses") || !strings.HasSuffix(paths[1], "/responses") {
		t.Fatalf("paths: %v", paths)
	}
	if bodies[0]["instructions"] != "sys" {
		t.Fatalf("missing instructions: %v", bodies[0])
	}
	in, _ := bodies[1]["input"].([]any)
	foundOut := false
	for _, item := range in {
		m, _ := item.(map[string]any)
		if m["type"] == "function_call_output" && m["call_id"] == "call_1" {
			foundOut = true
		}
	}
	if !foundOut {
		t.Fatalf("second request missing tool output: %v", bodies[1])
	}
}

func TestZenKimiStaysOnChatCompletions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"finish_reason": "stop",
				"message":       map[string]any{"role": "assistant", "content": "kimi"},
			}},
		})
	}))
	defer srv.Close()
	p := newOpenAIProvider("kimi-k2.6", srv.URL+"/zen/v1", "OPENCODE_API_KEY", "", nil, nil, nil, "")
	sess := p.NewSession()
	sess.AddUser("hi")
	res, err := sess.Step(context.Background(), "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Texts, "") != "kimi" {
		t.Fatalf("got %+v", res)
	}
}
