package api

import (
	"botbureau/backend/internal/bridge"
	"botbureau/backend/internal/engine"
	"botbureau/backend/internal/netx"
	"botbureau/backend/internal/secret"

	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- 假 Telegram Bot API ----
// ---- Fake Telegram Bot API ----

type fakeTG struct {
	mu       sync.Mutex
	updates  []bridge.TGUpdate
	sent     []map[string]any // sendMessage 调用 / sendMessage calls
	answered []string         // answerCallbackQuery 文本 / answerCallbackQuery texts
}

func (f *fakeTG) push(u bridge.TGUpdate) {
	f.mu.Lock()
	f.updates = append(f.updates, u)
	f.mu.Unlock()
}

func (f *fakeTG) sentTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, m := range f.sent {
		if s, ok := m["text"].(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func (f *fakeTG) handler() http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		ok := func(result any) {
			raw, _ := json.Marshal(result)
			fmt.Fprintf(rw, `{"ok":true,"result":%s}`, raw)
		}
		switch method {
		case "getMe":
			ok(map[string]any{"username": "fakebot"})
		case "getUpdates":
			f.mu.Lock()
			ups := f.updates
			f.updates = nil
			f.mu.Unlock()
			if ups == nil {
				ups = []bridge.TGUpdate{}
			}
			ok(ups)
		case "sendMessage":
			var m map[string]any
			_ = json.Unmarshal(body, &m)
			f.mu.Lock()
			f.sent = append(f.sent, m)
			f.mu.Unlock()
			ok(map[string]any{"message_id": 1})
		case "answerCallbackQuery":
			var m map[string]any
			_ = json.Unmarshal(body, &m)
			f.mu.Lock()
			f.answered = append(f.answered, fmt.Sprint(m["text"]))
			f.mu.Unlock()
			ok(true)
		default:
			fmt.Fprint(rw, `{"ok":false,"description":"unknown"}`)
		}
	})
}

func waitFor(t *testing.T, what string, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("timed out waiting for: " + what)
}

func TestQuotaAlertThrottle(t *testing.T) {
	bus := engine.NewBus()
	if !bus.QuotaAlert("p1", "alert 1") {
		t.Fatal("the first alert should fire")
	}
	if bus.QuotaAlert("p1", "alert 2") {
		t.Fatal("the same provider should be throttled within ten minutes")
	}
	if !bus.QuotaAlert("p2", "another provider") {
		t.Fatal("different providers should not throttle each other")
	}
	n := 0
	for _, ev := range bus.Recent(10) {
		if ev["kind"] == "system" && ev["quota"] == true && ev["chat"] == "" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("expected 2 global quota alerts, got %d", n)
	}
}

func TestSelfSignedTLS(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, fp1, err := netx.EnsureSelfSignedCert(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(fp1) != 64 {
		t.Fatalf("the fingerprint should be sha256 hex: %q", fp1)
	}
	// 复用同一证书，指纹稳定（钉扎的前提）
	// Reuses the same certificate so the fingerprint stays stable (prerequisite for pinning)
	_, _, fp2, _ := netx.EnsureSelfSignedCert(dir)
	if fp1 != fp2 {
		t.Fatal("a repeated call should reuse the certificate")
	}

	// 起 HTTPS 服务，客户端按指纹校验（模拟 TOFU 钉扎）
	// Start an HTTPS server; the client verifies by fingerprint (simulating TOFU pinning)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		fmt.Fprint(rw, `{"app":"botbureau"}`)
	}))
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	defer srv.Close()

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		// 自签名：跳过 CA 校验，改为校验指纹
		// Self-signed: skip CA verification and verify the fingerprint instead
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			sum := sha256.Sum256(rawCerts[0])
			if hex.EncodeToString(sum[:]) != fp1 {
				return fmt.Errorf("fingerprint mismatch")
			}
			return nil
		},
	}}}
	resp, err := client.Get(srv.URL + "/api/ping")
	if err != nil {
		t.Fatal("a matching fingerprint should connect:", err)
	}
	resp.Body.Close()

	// 篡改场景：期望指纹不同 → 连接失败
	// Tampering scenario: the expected fingerprint differs → connection fails
	badClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			sum := sha256.Sum256(rawCerts[0])
			if hex.EncodeToString(sum[:]) != strings.Repeat("0", 64) {
				return fmt.Errorf("fingerprint mismatch")
			}
			return nil
		},
	}}}
	if _, err := badClient.Get(srv.URL + "/api/ping"); err == nil {
		t.Fatal("a mismatched fingerprint should refuse to connect")
	}
	_ = os.Remove(filepath.Join(dir, "tls"))
}

func msgUpdate(id int64, chatID int64, text string) bridge.TGUpdate {
	u := bridge.TGUpdate{UpdateID: id}
	u.Message = &struct {
		Text string `json:"text"`
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		From struct {
			ID        int64  `json:"id"`
			FirstName string `json:"first_name"`
			Username  string `json:"username"`
		} `json:"from"`
	}{}
	u.Message.Text = text
	u.Message.Chat.ID = chatID
	u.Message.From.Username = "aiden"
	return u
}
func TestTelegramBridgeEndToEnd(t *testing.T) {
	app, _ := newTestApp(t)
	fake := &fakeTG{}
	tgSrv := httptest.NewServer(fake.handler())
	defer tgSrv.Close()

	_ = app.deps.KS.Set("TELEGRAM_BOT_TOKEN", "tok123")
	app.tg.SetAPIBase(tgSrv.URL)
	if err := app.tg.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	defer app.tg.Stop()
	waitFor(t, "the bridge to be ready", func() bool { return app.tg.Status()["running"] == true })

	// 1. /start 绑定
	// 1. /start binding
	fake.push(msgUpdate(1, 777, "/start"))
	waitFor(t, "the binding welcome message", func() bool {
		return len(fake.sentTexts()) > 0 && strings.Contains(strings.Join(fake.sentTexts(), ""), "Bound to your account")
	})

	// 2. 其他账号被拒
	// 2. Other accounts are rejected
	fake.push(msgUpdate(2, 888, "hello"))
	waitFor(t, "the stranger to be rejected", func() bool {
		return strings.Contains(strings.Join(fake.sentTexts(), ""), "already bound to another user")
	})

	// 3. 普通消息 → 群聊，chief（fake 回声）回复被转发回 TG
	// 3. Plain message → group chat; chief's reply (fake echo) is forwarded back to TG
	fake.push(msgUpdate(3, 777, "good morning"))
	waitFor(t, "the group reply to be forwarded", func() bool {
		for _, s := range fake.sentTexts() {
			if strings.Contains(s, "[Group] chief") && strings.Contains(s, "good morning") {
				return true
			}
		}
		return false
	})

	// 4. /dm 私聊路由
	// 4. /dm DM routing
	fake.push(msgUpdate(4, 777, "/dm scout dm test"))
	waitFor(t, "the dm reply to be forwarded", func() bool {
		for _, s := range fake.sentTexts() {
			if strings.Contains(s, "[DM·scout] scout") && strings.Contains(s, "dm test") {
				return true
			}
		}
		return false
	})

	// 5. 审批 → 内联按钮推送 → 回调批准
	// 5. engine.Approval → inline button pushed → callback approves
	ap := app.bus.RequestApproval("chief", "bash: touch x", "group")
	waitFor(t, "the approval push", func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		for _, m := range fake.sent {
			if s, _ := m["text"].(string); strings.Contains(s, "approval") && m["reply_markup"] != nil {
				return true
			}
		}
		return false
	})
	cb := bridge.TGUpdate{UpdateID: 5}
	cb.Callback = &struct {
		ID      string `json:"id"`
		Data    string `json:"data"`
		Message struct {
			Chat struct {
				ID int64 `json:"id"`
			} `json:"chat"`
		} `json:"message"`
	}{}
	cb.Callback.ID = "cb1"
	cb.Callback.Data = fmt.Sprintf("ap:%d:1", ap.ID)
	cb.Callback.Message.Chat.ID = 777
	fake.push(cb)
	waitFor(t, "the approval to be granted", func() bool { return len(app.bus.PendingApprovals()) == 0 })
	approved, _ := ap.Wait()
	if !approved {
		t.Fatal("the callback should grant the approval")
	}

	// 6. /bind scout：普通消息直达 scout 私聊；群聊消息不再转发
	// 6. /bind scout: plain messages go straight to scout's DM; group chat messages are no longer forwarded
	fake.push(msgUpdate(6, 777, "/bind scout"))
	waitFor(t, "the bind confirmation", func() bool {
		return strings.Contains(strings.Join(fake.sentTexts(), ""), "Connected to the DM with scout")
	})
	fake.push(msgUpdate(7, 777, "after binding"))
	waitFor(t, "the dm reply (via binding)", func() bool {
		for _, s := range fake.sentTexts() {
			if strings.Contains(s, "[DM·scout] scout") && strings.Contains(s, "after binding") {
				return true
			}
		}
		return false
	})
	// 群聊里发生的消息此时不应转发
	// Group chat messages should not be forwarded at this point
	before := len(fake.sentTexts())
	app.bus.PostGroup("user", "group message from the web client", []string{"chief"})
	waitFor(t, "chief replying in the group (on the bus)", func() bool {
		for _, ev := range app.bus.Recent(50) {
			if ev["kind"] == "msg" && ev["chat"] == "group" && ev["source"] == "chief" &&
				strings.Contains(ev["text"].(string), "group message from the web client") {
				return true
			}
		}
		return false
	})
	// 给转发循环机会（不应转发）
	// Give the forwarding loop a chance (it should not forward)
	time.Sleep(300 * time.Millisecond)
	for _, s := range fake.sentTexts()[before:] {
		if strings.Contains(s, "[Group]") {
			t.Fatalf("group messages must not be forwarded while bound to scout: %q", s)
		}
	}
	// /bind group 切回
	// /bind group switches back
	fake.push(msgUpdate(8, 777, "/bind group"))
	waitFor(t, "switch back to the group", func() bool {
		return strings.Contains(strings.Join(fake.sentTexts(), ""), "Connected to the team group chat")
	})
	// /bind 不存在的 bot
	// /bind a nonexistent bot
	fake.push(msgUpdate(9, 777, "/bind ghost"))
	waitFor(t, "the invalid bind error", func() bool {
		return strings.Contains(strings.Join(fake.sentTexts(), ""), "No bot named ghost")
	})

	// 7. 全局额度告警（chat 为空的 system）必转发
	// 7. Global quota alert (a system event with empty chat) must be forwarded
	app.bus.QuotaAlert("openai:gpt-x", "test quota alert")
	waitFor(t, "the quota alert to be forwarded", func() bool {
		return strings.Contains(strings.Join(fake.sentTexts(), ""), "test quota alert")
	})

	// 8. 未存 token 时开启应报错
	// 8. Enabling without a stored token should fail
	tg2 := bridge.NewTGBridge(app.bus, secret.NewKeyStore(filepath.Join(t.TempDir(), "k.json")), filepath.Join(t.TempDir(), "t.json"))
	if err := tg2.SetEnabled(true); err == nil {
		t.Fatal("enabling without a token should error")
	}
}
