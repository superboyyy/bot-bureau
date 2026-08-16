package model

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"botbureau/backend/internal/secret"
)

// codexSSE replies with one stream in the Responses API's event format.
func codexSSE(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	for _, e := range events {
		io.WriteString(w, "data: "+e+"\n\n")
	}
}

// codexSSEWithoutContentType mimics a proxy that forwards the body but rewrites the SSE header.
func codexSSEWithoutContentType(w http.ResponseWriter, events ...string) {
	w.WriteHeader(200)
	for _, e := range events {
		io.WriteString(w, "data: "+e+"\n\n")
	}
}

// The Codex backend accepts only store=false plus stream=true — omitting either is a 400 — and answers
// with an event stream that has to fold back into the non-streaming object, or no reply is read at all.
func TestCodexStreamsAndFoldsBackToOneResponse(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		if r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("a streaming request should say so: Accept=%q", r.Header.Get("Accept"))
		}
		codexSSE(w,
			`{"type":"response.created","response":{"status":"in_progress","output":[]}}`,
			`{"type":"response.output_text.delta","delta":"O"}`,
			`{"type":"response.output_text.delta","delta":"K"}`,
			`{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"OK"}]}]}}`,
		)
	}))
	defer srv.Close()

	sess := codexSessionAt(srv.URL)
	sess.AddUser("hello")
	res, err := sess.Step(context.Background(), "sys", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Texts, "") != "OK" {
		t.Fatalf("the reply did not survive the stream: %+v", res)
	}
	if body["store"] != false {
		t.Fatalf("store must be sent as false, got %v", body["store"])
	}
	if body["stream"] != true {
		t.Fatalf("stream must be sent as true, got %v", body["stream"])
	}
}

func TestCodexStreamSurvivesMissingContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		codexSSEWithoutContentType(w,
			`{"type":"response.created","response":{"status":"in_progress","output":[]}}`,
			`{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"OK"}]}]}}`,
		)
	}))
	defer srv.Close()

	sess := codexSessionAt(srv.URL)
	sess.AddUser("hello")
	res, err := sess.Step(context.Background(), "sys", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Texts, "") != "OK" {
		t.Fatalf("the reply did not survive the missing Content-Type: %+v", res)
	}
}

// Some Codex gateways leave response.output empty in response.completed and put the result only in
// prior events. Both common event shapes must fold back for the regular Responses parser.
func TestCodexStreamRestoresOutputOmittedFromCompletedResponse(t *testing.T) {
	for name, events := range map[string][]string{
		"completed item": {
			`{"type":"response.output_item.done","item":{"type":"message","content":[{"type":"output_text","text":"from item"}]}}`,
		},
		"text deltas": {
			`{"type":"response.output_text.delta","delta":"from "}`,
			`{"type":"response.output_text.delta","delta":"delta"}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				all := append([]string{}, events...)
				all = append(all, `{"type":"response.completed","response":{"status":"completed","output":[]}}`)
				codexSSE(w, all...)
			}))
			defer srv.Close()

			sess := codexSessionAt(srv.URL)
			sess.AddUser("hello")
			res, err := sess.Step(context.Background(), "sys", nil, false)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"completed item": "from item", "text deltas": "from delta"}[name]
			if got := strings.Join(res.Texts, ""); got != want {
				t.Fatalf("stream output = %q, want %q", got, want)
			}
		})
	}
}

// Tool calls likewise come from the terminal event, not from the deltas in between.
func TestCodexStreamCarriesToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		codexSSE(w,
			`{"type":"response.output_item.added","item":{"type":"function_call"}}`,
			`{"type":"response.completed","response":{"status":"completed","output":[{"type":"function_call","call_id":"call_7","name":"echo","arguments":"{\"q\":\"hi\"}"}]}}`,
		)
	}))
	defer srv.Close()

	sess := codexSessionAt(srv.URL)
	sess.AddUser("use the tool")
	res, err := sess.Step(context.Background(), "sys", []ToolDef{{Name: "echo", Description: "echo"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != "tool_use" || len(res.ToolCalls) != 1 || res.ToolCalls[0].ID != "call_7" {
		t.Fatalf("tool call lost in the stream: %+v", res)
	}
}

// A mid-stream error must not read as "no reply" — whatever the vendor said has to reach the user.
func TestCodexStreamSurfacesMidStreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		codexSSE(w,
			`{"type":"response.created","response":{"status":"in_progress","output":[]}}`,
			`{"type":"error","error":{"message":"the model went away"}}`,
		)
	}))
	defer srv.Close()

	sess := codexSessionAt(srv.URL)
	sess.AddUser("hello")
	_, err := sess.Step(context.Background(), "sys", nil, false)
	if err == nil || !strings.Contains(err.Error(), "the model went away") {
		t.Fatalf("the vendor's reason should reach the caller, got %v", err)
	}
}

func codexSessionAt(url string) Session {
	c := secret.NewChatGPTOAuth("")
	c.SetAPIURL(url)
	c.Restore("oauth-token", "acct-9", time.Now().Add(time.Hour))
	return newOpenAIProvider("gpt-5.4", "https://api.openai.com/v1", "OPENAI_API_KEY", AuthChatGPT, nil, nil, c, "").NewSession()
}
