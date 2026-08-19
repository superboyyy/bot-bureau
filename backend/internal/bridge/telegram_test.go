package bridge

import (
	"botbureau/backend/internal/engine"
	"botbureau/backend/internal/i18n"
	"botbureau/backend/internal/secret"

	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestTelegramBridgeRequiresTokenAndPersistsStatus(t *testing.T) {
	i18n.SetLocale("en")
	dir := t.TempDir()
	bridge := NewTGBridge(engine.NewBus(), secret.NewKeyStore(filepath.Join(dir, "keys.json")), filepath.Join(dir, "telegram.json"))
	if err := bridge.SetEnabled(true); err == nil {
		t.Fatal("enabling without a token should fail")
	}
	if got := bridge.Status(); got["enabled"] != false || got["running"] != false || got["has_token"] != false {
		t.Fatalf("status after rejected enable = %#v", got)
	}
}

func TestTelegramBridgeLifecycleAndAPI(t *testing.T) {
	i18n.SetLocale("en")
	dir := t.TempDir()
	ks := secret.NewKeyStore(filepath.Join(dir, "keys.json"))
	if err := ks.Set("TELEGRAM_BOT_TOKEN", "token"); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			_, _ = w.Write([]byte(`{"ok":true,"result":{"username":"bureau_bot"}}`))
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
		}
	}))
	defer srv.Close()

	bridge := NewTGBridge(engine.NewBus(), ks, filepath.Join(dir, "telegram.json"))
	bridge.SetAPIBase(srv.URL)
	if err := bridge.Start(); err != nil {
		t.Fatal(err)
	}
	status := bridge.Status()
	if status["running"] != true || status["bot"] != "bureau_bot" || status["has_token"] != true {
		t.Fatalf("running status = %#v", status)
	}
	if err := bridge.Start(); err != nil {
		t.Fatal("starting an already-running bridge should be idempotent: ", err)
	}
	bridge.send(42, "hello", nil)
	bridge.Stop()
	bridge.Stop()
	if bridge.Status()["running"] != false {
		t.Fatal("bridge did not stop")
	}
	if err := bridge.SetEnabled(false); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	callCount := len(calls)
	mu.Unlock()
	if callCount == 0 {
		t.Fatal("fake Telegram API received no calls")
	}

	reloaded := NewTGBridge(engine.NewBus(), ks, filepath.Join(dir, "telegram.json"))
	if got := reloaded.Status(); got["enabled"] != false || got["bind"] != "group" {
		t.Fatalf("reloaded status = %#v", got)
	}
}

func TestTelegramBridgeBindsOwnerAndRejectsOtherChats(t *testing.T) {
	i18n.SetLocale("en")
	dir := t.TempDir()
	ks := secret.NewKeyStore(filepath.Join(dir, "keys.json"))
	if err := ks.Set("TELEGRAM_BOT_TOKEN", "token"); err != nil {
		t.Fatal(err)
	}
	var messages []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			var payload struct {
				Text string `json:"text"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			messages = append(messages, payload.Text)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer srv.Close()
	bridge := NewTGBridge(engine.NewBus(), ks, filepath.Join(dir, "telegram.json"))
	bridge.SetAPIBase(srv.URL)

	var first TGUpdate
	if err := json.Unmarshal([]byte(`{"message":{"text":"/start","chat":{"id":7},"from":{"username":"alice"}}}`), &first); err != nil {
		t.Fatal(err)
	}
	bridge.handleUpdate(first)
	status := bridge.Status()
	if status["owner"] != "alice" {
		t.Fatalf("owner status = %#v", status)
	}

	var other TGUpdate
	if err := json.Unmarshal([]byte(`{"message":{"text":"hello","chat":{"id":8},"from":{"username":"bob"}}}`), &other); err != nil {
		t.Fatal(err)
	}
	bridge.handleUpdate(other)
	if len(messages) < 2 || !strings.Contains(messages[len(messages)-1], "already bound") {
		t.Fatalf("unauthorized response = %#v", messages)
	}
}

func TestTelegramCallbackCanDecideApproval(t *testing.T) {
	i18n.SetLocale("en")
	dir := t.TempDir()
	ks := secret.NewKeyStore(filepath.Join(dir, "keys.json"))
	if err := ks.Set("TELEGRAM_BOT_TOKEN", "token"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer srv.Close()
	bus := engine.NewBus()
	bridge := NewTGBridge(bus, ks, filepath.Join(dir, "telegram.json"))
	bridge.SetAPIBase(srv.URL)
	bridge.mu.Lock()
	bridge.cfg.OwnerChatID = 7
	bridge.mu.Unlock()
	approval := bus.RequestApproval("wren", "write", "group", "")
	var update TGUpdate
	if err := json.Unmarshal([]byte(`{"callback_query":{"id":"cb","data":"ap:1:1","message":{"chat":{"id":7}}}}`), &update); err != nil {
		t.Fatal(err)
	}
	// RequestApproval starts a timeout goroutine; make sure this test never waits for it.
	defer bus.Decide(approval.ID, false, "cleanup")
	bridge.handleUpdate(update)
	approved, reason := approval.Wait()
	if !approved || reason != "via Telegram" {
		t.Fatalf("approval = %v, %q", approved, reason)
	}
}

func TestApprovalDiffSuffix(t *testing.T) {
	if got := approvalDiffSuffix(engine.Event{}); got != "" {
		t.Fatalf("empty extra should add nothing, got %q", got)
	}
	got := approvalDiffSuffix(engine.Event{"approval_diff": "--- a/x\n+++ b/x\n+hi"})
	if !strings.Contains(got, "+hi") || !strings.HasPrefix(got, "\n") {
		t.Fatalf("diff should be appended: %q", got)
	}
}

func TestApprovalPreviewPrefersPlanBody(t *testing.T) {
	if got := approvalPreviewSuffix(engine.Event{}); got != "" {
		t.Fatalf("empty extra should add nothing, got %q", got)
	}
	got := approvalPreviewSuffix(engine.Event{"approval_body": "1. edit a.go"})
	if !strings.Contains(got, "edit a.go") || !strings.HasPrefix(got, "\n") {
		t.Fatalf("plan body should be appended: %q", got)
	}
	got = approvalPreviewSuffix(engine.Event{
		"approval_body": "the plan",
		"approval_diff": "--- a/x\n+hi",
	})
	if !strings.Contains(got, "the plan") || strings.Contains(got, "+hi") {
		t.Fatalf("body should win over a diff: %q", got)
	}
}
