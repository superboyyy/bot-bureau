package model

import (
	"encoding/json"
	"strings"
	"testing"

	"botbureau/backend/internal/secret"
)

// The image has to land in the tool_result's content blocks. Checking that a field was set is not
// enough: what matters is the shape that gets serialized, since that is what the model actually receives.
func TestAnthropicToolResultCarriesImage(t *testing.T) {
	ks := secret.NewKeyStore("")
	p := newAnthropicProvider("claude-opus-5", "ANTHROPIC_API_KEY", "", ks, "")
	s, ok := p.NewSession().(*anthropicSession)
	if !ok {
		t.Fatal("expected an anthropic session")
	}
	s.AddToolResults([]ToolResult{{
		ID:      "toolu_1",
		Content: "captured",
		Images:  []ResultImage{{MIME: "image/png", Base64: "iVBORw0KGgo="}},
	}})

	raw, err := json.Marshal(s.history)
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	for _, want := range []string{`"type":"tool_result"`, `"tool_use_id":"toolu_1"`, `"type":"image"`, `"media_type":"image/png"`, "iVBORw0KGgo=", "captured"} {
		if !strings.Contains(out, want) {
			t.Fatalf("serialized tool result is missing %s:\n%s", want, out)
		}
	}
}

// With no image the shape must be exactly what it was: supporting images should not reshape ordinary
// tool results.
func TestAnthropicToolResultWithoutImageUnchanged(t *testing.T) {
	ks := secret.NewKeyStore("")
	p := newAnthropicProvider("claude-opus-5", "ANTHROPIC_API_KEY", "", ks, "")
	s := p.NewSession().(*anthropicSession)
	s.AddToolResults([]ToolResult{{ID: "toolu_2", Content: "plain text"}})
	raw, _ := json.Marshal(s.history)
	out := string(raw)
	if strings.Contains(out, `"type":"image"`) {
		t.Fatalf("no image block should appear:\n%s", out)
	}
	if !strings.Contains(out, "plain text") {
		t.Fatalf("the text should be there:\n%s", out)
	}
}

// An OpenAI-compatible endpoint cannot carry the image, so it states how many came back, letting the
// model ask for a textual form instead of meeting silence and concluding the tool did nothing.
func TestOpenAIToolResultMentionsImages(t *testing.T) {
	ks := secret.NewKeyStore("")
	p := newOpenAIProvider("gpt-5.1", "https://api.example.com/v1", "OPENAI_API_KEY", "key", ks, nil, nil, "")
	s := p.NewSession().(*openAISession)
	s.AddToolResults([]ToolResult{{
		ID: "call_1", Content: "captured",
		Images: []ResultImage{{MIME: "image/png", Base64: "aaa"}},
	}})
	raw, _ := json.Marshal(s.history)
	out := string(raw)
	if strings.Contains(out, "aaa") {
		t.Fatalf("the base64 payload must not be smuggled into a text field:\n%s", out)
	}
	if !strings.Contains(out, "image(s) returned") {
		t.Fatalf("it should say images came back:\n%s", out)
	}
}
