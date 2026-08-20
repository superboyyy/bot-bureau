package api

import (
	"botbureau/backend/internal/bridge"
	"botbureau/backend/internal/config"
	"botbureau/backend/internal/engine"
	"botbureau/backend/internal/netx"
	"botbureau/backend/internal/sandbox"
	"botbureau/backend/internal/secret"

	"botbureau/backend/internal/i18n"

	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestApp(t *testing.T) (*App, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skills", ".keep"), []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "bots.yaml")
	cfgs := []config.BotConfig{
		{Name: "chief", Role: "Lead", Provider: "fake"},
		{Name: "scout", Role: "Researcher", Provider: "fake"},
	}
	if err := config.SaveBotConfigs(cfgPath, cfgs); err != nil {
		t.Fatal(err)
	}
	bus := engine.NewBus()
	sched := engine.NewScheduler(bus, filepath.Join(dir, "routines.json"))
	deps := engine.NewTeamDeps(dir, secret.NewKeyStore(filepath.Join(dir, "keys.json")), filepath.Join(dir, "mcp.yaml"))

	// The assertions read the English source text directly, so pin the locale to en (it would otherwise
	// follow the environment and every assertion would face translated output on a Chinese machine).
	// The tests that specifically exercise translation switch to zh themselves.
	settings := config.NewSettings(dir)
	settings.SetLocalePref("en")
	deps.Settings = settings // must be attached before bots are built; the toolbox captures it then
	deps.Sandbox = sandbox.Passthrough()
	for _, c := range cfgs {
		w, err := engine.NewBotWorker(c, bus, sched, dir, deps)
		if err != nil {
			t.Fatal(err)
		}
		bus.Register(w)
	}
	bus.LoadGroupMembers(filepath.Join(dir, "group.json"))
	for _, w := range bus.Bots() {
		w.Start()
	}
	tg := bridge.NewTGBridge(bus, deps.KS, filepath.Join(dir, "telegram.json"))
	app := NewApp(bus, sched, deps, tg, settings, cfgs, cfgPath, dir)
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close)
	return app, srv
}

func postJSON(t *testing.T, url string, body any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func waitForEvent(t *testing.T, srv *httptest.Server, pred func(engine.Event) bool) engine.Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(srv.URL + "/api/state")
		if err != nil {
			t.Fatal(err)
		}
		var st struct {
			Events []engine.Event `json:"events"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&st)
		resp.Body.Close()
		for _, ev := range st.Events {
			if pred(ev) {
				return ev
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the expected event never arrived")
	return nil
}

func TestHTTPGroupAndDMFlow(t *testing.T) {
	_, srv := newTestApp(t)

	// Group chat: no mention → chief responds by default (fake echo)
	code, _ := postJSON(t, srv.URL+"/api/send", map[string]any{"chat": "group", "text": "hello"})
	if code != 200 {
		t.Fatal("group send failed")
	}
	waitForEvent(t, srv, func(ev engine.Event) bool {
		return ev["kind"] == "msg" && ev["chat"] == "group" && ev["source"] == "chief" &&
			strings.Contains(ev["text"].(string), "hello")
	})

	// Group chat: mentioning @scout → scout responds
	postJSON(t, srv.URL+"/api/send", map[string]any{"chat": "group", "text": "@scout take a look"})
	waitForEvent(t, srv, func(ev engine.Event) bool {
		return ev["kind"] == "msg" && ev["chat"] == "group" && ev["source"] == "scout"
	})

	// DM scout
	postJSON(t, srv.URL+"/api/send", map[string]any{"chat": "dm:scout", "text": "dm test"})
	waitForEvent(t, srv, func(ev engine.Event) bool {
		return ev["kind"] == "msg" && ev["chat"] == "dm:scout" && ev["source"] == "scout" &&
			strings.Contains(ev["text"].(string), "dm test")
	})

	// Target does not exist
	if code, _ := postJSON(t, srv.URL+"/api/send", map[string]any{"chat": "dm:ghost", "text": "x"}); code != 404 {
		t.Fatalf("a nonexistent dm target should 404, got %d", code)
	}
}

func TestConversationPreviewsEndpointIsIndependentOfHistory(t *testing.T) {
	app, srv := newTestApp(t)
	app.bus.Emit("msg", "dm:chief", "user", "hello from the inbox", nil)

	resp, err := http.Get(srv.URL + "/api/conversations")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("conversation list failed with %d", resp.StatusCode)
	}
	var out struct {
		Conversations []engine.ConversationPreview `json:"conversations"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Conversations) != 1 || out.Conversations[0].Chat != "dm:chief" || out.Conversations[0].Text != "hello from the inbox" {
		t.Fatalf("unexpected conversation previews: %+v", out.Conversations)
	}
}

// Quoting a member's line in the group is addressing them: the name need not be typed again, and the
// message must not fall back to the default responder.
func TestQuoteReplyAddressesTheQuotedBot(t *testing.T) {
	_, srv := newTestApp(t)

	postJSON(t, srv.URL+"/api/send", map[string]any{"chat": "group", "text": "@scout take a look"})
	spoke := waitForEvent(t, srv, func(ev engine.Event) bool {
		return ev["kind"] == "msg" && ev["chat"] == "group" && ev["source"] == "scout"
	})

	id, _ := spoke["id"].(float64)
	postJSON(t, srv.URL+"/api/send", map[string]any{
		"chat": "group", "text": "carry on with that", "reply_to": int(id),
	})
	quoted := waitForEvent(t, srv, func(ev engine.Event) bool {
		return ev["kind"] == "msg" && ev["source"] == "user" && ev["text"] == "carry on with that"
	})
	if quoted["reply_to"] == nil || quoted["reply_src"] != "scout" {
		t.Fatalf("the sent message should carry the quotation: %+v", quoted)
	}

	// chief is the default responder, so scout answering this one is what proves the quotation aimed it
	waitForEvent(t, srv, func(ev engine.Event) bool {
		text, _ := ev["text"].(string)
		return ev["kind"] == "msg" && ev["source"] == "scout" && strings.Contains(text, "carry on with that")
	})
}

func TestHTTPBotCRUD(t *testing.T) {
	app, srv := newTestApp(t)

	code, out := postJSON(t, srv.URL+"/api/bots", map[string]any{
		"name": "coder", "role": "Engineer", "description": "writes code", "provider": "fake",
	})
	if code != 200 {
		t.Fatalf("creating the bot failed: %v", out)
	}
	raw, _ := os.ReadFile(app.cfgPath)
	if !strings.Contains(string(raw), "coder") {
		t.Fatal("the new bot was not persisted to bots.yaml")
	}

	// The new bot is usable immediately (DM echo)
	postJSON(t, srv.URL+"/api/send", map[string]any{"chat": "dm:coder", "text": "you there"})
	waitForEvent(t, srv, func(ev engine.Event) bool {
		return ev["kind"] == "msg" && ev["chat"] == "dm:coder" && ev["source"] == "coder"
	})

	// Invalid parameters
	if code, _ := postJSON(t, srv.URL+"/api/bots", map[string]any{"name": "Bad Name"}); code != 400 {
		t.Fatal("an invalid name should 400")
	}
	if code, _ := postJSON(t, srv.URL+"/api/bots", map[string]any{"name": "gpt", "provider": "openai"}); code != 400 {
		t.Fatal("openai without a model should 400")
	}
	if code, _ := postJSON(t, srv.URL+"/api/bots", map[string]any{"name": "coder", "provider": "fake"}); code != 400 {
		t.Fatal("a duplicate name should 400")
	}

	// Delete
	if code, _ := postJSON(t, srv.URL+"/api/bots/delete", map[string]any{"name": "coder"}); code != 200 {
		t.Fatal("delete failed")
	}
	raw, _ = os.ReadFile(app.cfgPath)
	if strings.Contains(string(raw), "coder") {
		t.Fatal("the delete was not synced to bots.yaml")
	}
}

func TestHTTPApprovalFlow(t *testing.T) {
	app, srv := newTestApp(t)

	// Manually create an approval (equivalent to bash going through approval inside a bot)
	go app.bus.RequestApproval("chief", "bash: touch x", "group", "")
	var id int
	waitForEvent(t, srv, func(ev engine.Event) bool {
		if ev["kind"] == "approval" {
			id = int(ev["approval_id"].(float64))
			return true
		}
		return false
	})
	if code, _ := postJSON(t, srv.URL+"/api/approve", map[string]any{"id": id, "approved": true}); code != 200 {
		t.Fatal("approval failed")
	}
	if code, _ := postJSON(t, srv.URL+"/api/approve", map[string]any{"id": id, "approved": true}); code != 404 {
		t.Fatal("a repeated approval should 404")
	}
}

func TestHTTPApproveReplacesBashCommand(t *testing.T) {
	app, srv := newTestApp(t)
	w := app.bus.Bot("chief")
	tb := w.Toolbox()

	done := make(chan string, 1)
	go func() {
		out, _, isErr := tb.Execute("bash", map[string]any{"command": "touch original-flag.txt"})
		if isErr {
			done <- "err:" + out
			return
		}
		done <- out
	}()
	deadline := time.After(2 * time.Second)
	for len(app.bus.PendingApprovals()) == 0 {
		select {
		case <-deadline:
			t.Fatal("bash should wait for approval")
		case out := <-done:
			t.Fatalf("bash returned without waiting: %s", out)
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	ap := app.bus.PendingApprovals()[0]
	code, _ := postJSON(t, srv.URL+"/api/approve", map[string]any{
		"id": ap.ID, "approved": true, "command": "touch edited-flag.txt",
	})
	if code != 200 {
		t.Fatalf("approve with command should succeed, got %d", code)
	}
	if res := <-done; strings.HasPrefix(res, "err:") {
		t.Fatalf("edited command failed: %s", res)
	}
	if _, err := os.Stat(filepath.Join(w.Workspace(), "edited-flag.txt")); err != nil {
		t.Fatalf("the edited command should have run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(w.Workspace(), "original-flag.txt")); !os.IsNotExist(err) {
		t.Fatal("the original command must not run")
	}
}

func TestHTTPSettingsFetchHosts(t *testing.T) {
	app, srv := newTestApp(t)
	code, out := postJSON(t, srv.URL+"/api/settings", map[string]any{
		"fetch_hosts": []string{"https://GitHub.com/foo", "golang.org"},
	})
	if code != 200 {
		t.Fatalf("settings: %d %v", code, out)
	}
	hosts, _ := out["fetch_hosts"].([]any)
	if len(hosts) != 2 || hosts[0] != "github.com" || hosts[1] != "golang.org" {
		t.Fatalf("normalized hosts: %v", hosts)
	}
	if got := app.settings.FetchHosts(); len(got) != 2 || got[0] != "github.com" {
		t.Fatalf("store: %v", got)
	}
	code, out = postJSON(t, srv.URL+"/api/settings", map[string]any{"locale": "en"})
	if code != 200 {
		t.Fatal(out)
	}
	hosts, _ = out["fetch_hosts"].([]any)
	if len(hosts) != 2 {
		t.Fatalf("omitting fetch_hosts must not wipe the list: %v", hosts)
	}
}

func TestHTTPSettingsSandbox(t *testing.T) {
	app, srv := newTestApp(t)
	code, out := postJSON(t, srv.URL+"/api/settings", map[string]any{
		"sandbox": map[string]any{"enabled": false, "auto_allow_bash": true, "allow_unsandboxed": false},
	})
	if code != 200 {
		t.Fatalf("settings: %d %v", code, out)
	}
	sb, _ := out["sandbox"].(map[string]any)
	if sb["enabled"] != false || sb["auto_allow_bash"] != true || sb["allow_unsandboxed"] != false {
		t.Fatalf("sandbox: %v", sb)
	}
	if sb["available"] != false || sb["backend"] != "none" {
		t.Fatalf("test runner must stay passthrough: %v", sb)
	}
	if app.settings.SandboxEnabled() || !app.settings.SandboxAutoAllowBash() || app.settings.SandboxAllowUnsandboxed() {
		t.Fatal("store did not keep sandbox prefs")
	}
	code, out = postJSON(t, srv.URL+"/api/settings", map[string]any{"locale": "en"})
	if code != 200 {
		t.Fatal(out)
	}
	sb, _ = out["sandbox"].(map[string]any)
	if sb["enabled"] != false || sb["auto_allow_bash"] != true {
		t.Fatalf("omitting sandbox must not wipe prefs: %v", sb)
	}
	code, out = postJSON(t, srv.URL+"/api/settings", map[string]any{
		"sandbox": map[string]any{"enabled": true},
	})
	if code != 200 {
		t.Fatal(out)
	}
	sb, _ = out["sandbox"].(map[string]any)
	if sb["enabled"] != true || sb["auto_allow_bash"] != true || sb["allow_unsandboxed"] != false {
		t.Fatalf("partial sandbox update wiped siblings: %v", sb)
	}
}

func TestHTTPCancel(t *testing.T) {
	app, srv := newTestApp(t)
	go app.bus.RequestApproval("chief", "bash: touch x", "group", "")
	waitForEvent(t, srv, func(ev engine.Event) bool { return ev["kind"] == "approval" })
	if code, _ := postJSON(t, srv.URL+"/api/cancel", map[string]any{"name": "chief"}); code != 200 {
		t.Fatal("cancel should succeed")
	}
	if n := len(app.bus.PendingApprovals()); n != 0 {
		t.Fatalf("no approval should remain after cancelling: %d", n)
	}
	if code, _ := postJSON(t, srv.URL+"/api/cancel", map[string]any{"name": "ghost"}); code != 404 {
		t.Fatal("a nonexistent bot should 404")
	}
}

func TestHTTPSessionReset(t *testing.T) {
	app, srv := newTestApp(t)
	postJSON(t, srv.URL+"/api/send", map[string]any{"chat": "dm:chief", "text": "reset-me-please"})
	waitForEvent(t, srv, func(ev engine.Event) bool {
		return ev["kind"] == "msg" && ev["chat"] == "dm:chief" && ev["source"] == "chief" &&
			strings.Contains(ev["text"].(string), "reset-me-please")
	})

	ws := engine.WorkspaceDir(app.dataDir, "chief")
	memPath := filepath.Join(ws, "MEMORY.md")
	if err := os.WriteFile(memPath, []byte("- keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if code, out := postJSON(t, srv.URL+"/api/session/reset", map[string]any{"chat": "dm:chief"}); code != 200 {
		t.Fatalf("reset failed: %v", out)
	}
	waitForEvent(t, srv, func(ev engine.Event) bool {
		text, _ := ev["text"].(string)
		return ev["kind"] == "system" && ev["chat"] == "dm:chief" &&
			strings.Contains(text, "New conversation started")
	})

	matches, err := filepath.Glob(filepath.Join(ws, "sessions-*.json"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected one sessions archive, got %v (%v)", matches, err)
	}
	archived, _ := os.ReadFile(matches[0])
	if !strings.Contains(string(archived), "reset-me-please") {
		t.Fatalf("the archive should keep the old session: %s", archived)
	}
	live, _ := os.ReadFile(filepath.Join(ws, "sessions.json"))
	if strings.Contains(string(live), "reset-me-please") {
		t.Fatalf("the live session should be empty of the old text: %s", live)
	}
	got, err := os.ReadFile(memPath)
	if err != nil || !strings.Contains(string(got), "keep me") {
		t.Fatalf("MEMORY.md must stay: %s", got)
	}

	if code, _ := postJSON(t, srv.URL+"/api/session/reset", map[string]any{}); code != 400 {
		t.Fatal("reset without a conversation should 400")
	}
	if code, _ := postJSON(t, srv.URL+"/api/session/reset", map[string]any{"chat": "dm:ghost"}); code != 404 {
		t.Fatal("reset of a missing bot should 404")
	}
	if code, out := postJSON(t, srv.URL+"/api/session/reset", map[string]any{"chat": "group"}); code != 200 {
		t.Fatalf("group reset should succeed: %v", out)
	}
}

func TestHTTPKeysAPI(t *testing.T) {
	_, srv := newTestApp(t)
	if code, out := postJSON(t, srv.URL+"/api/keys", map[string]any{"name": "XAI_API_KEY", "value": "xai-secret-12345678"}); code != 200 {
		t.Fatalf("saving the key failed: %v", out)
	}
	resp, _ := http.Get(srv.URL + "/api/keys")
	var got struct {
		Keys []map[string]string `json:"keys"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if len(got.Keys) != 1 || got.Keys[0]["name"] != "XAI_API_KEY" {
		t.Fatalf("wrong key list: %v", got.Keys)
	}
	if strings.Contains(got.Keys[0]["masked"], "secret") {
		t.Fatalf("the API must not return plaintext: %v", got.Keys[0])
	}
	if code, _ := postJSON(t, srv.URL+"/api/keys", map[string]any{"name": "bad name", "value": "x"}); code != 400 {
		t.Fatal("an invalid name should 400")
	}
	if code, _ := postJSON(t, srv.URL+"/api/keys/delete", map[string]any{"name": "XAI_API_KEY"}); code != 200 {
		t.Fatal("deleting the key failed")
	}
}

func TestHTTPGroupMembersAPI(t *testing.T) {
	app, srv := newTestApp(t)

	// Remove scout: @scout stops working, the default responder is still chief
	if code, _ := postJSON(t, srv.URL+"/api/group/set", map[string]any{"name": "scout", "in": false}); code != 200 {
		t.Fatal("removal failed")
	}
	if got := app.bus.GroupMembers(); len(got) != 1 || got[0] != "chief" {
		t.Fatalf("wrong member list: %v", got)
	}
	postJSON(t, srv.URL+"/api/send", map[string]any{"chat": "group", "text": "@scout take a look"})

	// @ on a non-member has no effect → falls back to the default chief
	waitForEvent(t, srv, func(ev engine.Event) bool {
		return ev["kind"] == "msg" && ev["chat"] == "group" && ev["source"] == "chief" &&
			strings.Contains(ev["text"].(string), "take a look")
	})

	// Add back
	if code, _ := postJSON(t, srv.URL+"/api/group/set", map[string]any{"name": "scout", "in": true}); code != 200 {
		t.Fatal("re-adding failed")
	}
	if code, _ := postJSON(t, srv.URL+"/api/group/set", map[string]any{"name": "ghost", "in": true}); code != 404 {
		t.Fatal("a nonexistent bot should 404")
	}
}

func TestSSEStream(t *testing.T) {
	app, srv := newTestApp(t)
	resp, err := http.Get(srv.URL + "/api/events?after=0")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("wrong Content-Type: %s", ct)
	}
	go app.bus.Emit("msg", "group", "user", "sse test", nil)
	reader := bufio.NewReader(resp.Body)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, "sse test") {
			return // received
		}
	}
	t.Fatal("SSE never pushed the event")
}

// Editing a bot: changing its avatar or display name must not shift its position (the group's default
// recipient is whoever is first) and must not drop it from the group.
func TestUpdateBotKeepsOrderAndMembership(t *testing.T) {
	app, srv := newTestApp(t)
	defer srv.Close()

	before := app.bus.DefaultGroupMember()
	if before != "chief" {
		t.Fatalf("the default recipient should be chief, got %q", before)
	}
	app.bus.SetGroupMember("chief", true)

	cfg := config.BotConfig{
		Name: "chief", Role: "Lead", Provider: "fake",
		DisplayName: "Chief of Staff", Avatar: "#c9b9a6",
	}
	if err := app.UpdateBot(cfg); err != nil {
		t.Fatalf("UpdateBot: %v", err)
	}
	if got := app.bus.DefaultGroupMember(); got != "chief" {
		t.Fatalf("after editing the default recipient became %q", got)
	}
	if !app.bus.IsGroupMember("chief") {
		t.Fatal("chief dropped out of the group after editing")
	}
	w := app.bus.Bot("chief")
	if w == nil || w.Cfg.Title() != "Chief of Staff" || w.Cfg.Avatar != "#c9b9a6" {
		t.Fatalf("the new config did not take effect: %+v", w)
	}
	// the new fields must survive to bots.yaml
	saved, err := config.LoadBotConfigs(app.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if saved[0].Name != "chief" || saved[0].DisplayName != "Chief of Staff" {
		t.Fatalf("bots.yaml was written wrong: %+v", saved)
	}
}

func TestUpdateBotRejectsBadInput(t *testing.T) {
	app, srv := newTestApp(t)
	defer srv.Close()

	if err := app.UpdateBot(config.BotConfig{Name: "nobody", Provider: "fake"}); err == nil {
		t.Fatal("editing a nonexistent bot should error")
	}
	if err := app.UpdateBot(config.BotConfig{Name: "scout", Provider: "fake", Avatar: "javascript:alert(1)"}); err == nil {
		t.Fatal("an invalid avatar should be rejected")
	}
	if err := app.UpdateBot(config.BotConfig{Name: "scout", Provider: "fake", Avatar: "data:image/png;base64," + strings.Repeat("A", config.AvatarMaxBytes)}); err == nil {
		t.Fatal("an oversized avatar should be rejected")
	}
	long := strings.Repeat("x", config.DisplayNameMax+1)
	if err := app.UpdateBot(config.BotConfig{Name: "scout", Provider: "fake", DisplayName: long}); err == nil {
		t.Fatal("an overlong display name should be rejected")
	}
	// the original bot must still be alive after a rejection
	if app.bus.Bot("scout") == nil {
		t.Fatal("scout vanished after a failed validation")
	}
}

// The group's name and avatar live in settings; changing the language must not wipe them.
func TestGroupMetaSurvivesLocaleChange(t *testing.T) {
	dir := t.TempDir()
	s := config.NewSettings(dir)
	title, avatar := "War Room", "#a8bccf"
	s.SetGroupMeta(&title, &avatar)
	s.SetLocalePref("en")

	reloaded := config.NewSettings(dir)
	if reloaded.GroupTitle != "War Room" || reloaded.GroupAvatar != "#a8bccf" {
		t.Fatalf("the group metadata was not kept: %+v", reloaded)
	}
	if reloaded.LocalePref != "en" {
		t.Fatalf("the locale preference was not kept: %q", reloaded.LocalePref)
	}
	i18n.SetLocale("zh") // restore so other tests are unaffected
}

// Pinning: group chats and DMs go through the same endpoint and live in the engine (connect from
// another device and the same ones are on top). A pin leaves with its conversation, or settings.json
// would collect ids that are invisible in the UI and impossible to take back.
func TestPinnedConversations(t *testing.T) {
	app, srv := newTestApp(t)

	code, out := postJSON(t, srv.URL+"/api/pins", map[string]any{"chat": "dm:scout", "pinned": true})
	if code != 200 {
		t.Fatalf("pinning a DM failed: %d %v", code, out)
	}
	if code, out = postJSON(t, srv.URL+"/api/pins", map[string]any{"chat": "group", "pinned": true}); code != 200 {
		t.Fatalf("pinning the group chat failed: %d %v", code, out)
	}
	if got := pinsIn(t, out); len(got) != 2 || got[0] != "dm:scout" || got[1] != "group" {
		t.Fatalf("wrong pins in the reply: %v", got)
	}
	if got := statePins(t, srv); len(got) != 2 {
		t.Fatalf("the state should carry both pins, got %v", got)
	}

	// A conversation that is not there cannot be pinned: a mistyped id would stay in settings forever,
	// with no row in the UI to take it back from
	if code, _ := postJSON(t, srv.URL+"/api/pins", map[string]any{"chat": "dm:ghost", "pinned": true}); code != 404 {
		t.Fatalf("pinning a nonexistent conversation should 404, got %d", code)
	}
	if code, _ := postJSON(t, srv.URL+"/api/pins", map[string]any{"chat": "", "pinned": true}); code != 400 {
		t.Fatalf("an empty conversation id should 400, got %d", code)
	}

	// Still there after a restart
	if got := config.NewSettings(app.dataDir).Pins(); len(got) != 2 {
		t.Fatalf("the pins did not survive a reload: %v", got)
	}

	// Unpin
	_, out = postJSON(t, srv.URL+"/api/pins", map[string]any{"chat": "group", "pinned": false})
	if got := pinsIn(t, out); len(got) != 1 || got[0] != "dm:scout" {
		t.Fatalf("unpinning left the wrong set: %v", got)
	}

	// The group goes, and its pin with it
	_, made := postJSON(t, srv.URL+"/api/groups", map[string]any{"title": "War Room", "members": []string{"chief"}})
	id, _ := made["id"].(string)
	if id == "" {
		t.Fatalf("creating a group failed: %v", made)
	}
	postJSON(t, srv.URL+"/api/pins", map[string]any{"chat": id, "pinned": true})
	postJSON(t, srv.URL+"/api/groups/delete", map[string]any{"id": id})
	for _, c := range statePins(t, srv) {
		if c == id {
			t.Fatal("the deleted group is still pinned")
		}
	}

	// A member leaves, and the pin on their DM goes too
	if _, err := app.RemoveBot("scout", true); err != nil {
		t.Fatalf("removing scout failed: %v", err)
	}
	if got := statePins(t, srv); len(got) != 0 {
		t.Fatalf("a removed bot left its pin behind: %v", got)
	}
}

func pinsIn(t *testing.T, out map[string]any) []string {
	t.Helper()
	raw, _ := out["pins"].([]any)
	got := make([]string, 0, len(raw))
	for _, v := range raw {
		s, _ := v.(string)
		got = append(got, s)
	}
	return got
}

func statePins(t *testing.T, srv *httptest.Server) []string {
	t.Helper()
	resp, err := http.Get(srv.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st struct {
		Pins []string `json:"pins"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	return st.Pins
}

// The team may be emptied: that is the initial state for a fresh install, where the client shows
// onboarding. This used to assert the opposite, back when the engine refused to start on an empty file.
func TestRemoveBotCanEmptyTheTeam(t *testing.T) {
	app, srv := newTestApp(t)
	defer srv.Close()

	for _, name := range []string{"scout", "chief"} {
		if _, err := app.RemoveBot(name, false); err != nil {
			t.Fatalf("removing %s failed: %v", name, err)
		}
	}
	if n := len(app.bus.Bots()); n != 0 {
		t.Fatalf("should be empty, %d left", n)
	}

	// An empty bots.yaml must still load, or the next start is broken
	saved, err := config.LoadBotConfigs(app.cfgPath)
	if err != nil {
		t.Fatalf("an empty bots.yaml failed to load: %v", err)
	}
	if len(saved) != 0 {
		t.Fatalf("should be empty, got %+v", saved)
	}
	if _, err := app.RemoveBot("nobody", false); err == nil {
		t.Fatal("removing a nonexistent bot should error")
	}
}

// A removal keeps the files by default: the workspace is renamed into an archive that turns up under
// "Former members". Also confirms routines leave with their owner — those are behaviour, not records.
func TestRemoveBotArchivesWorkspaceByDefault(t *testing.T) {
	app, srv := newTestApp(t)
	defer srv.Close()

	ws := engine.WorkspaceDir(app.dataDir, "scout")
	if err := os.WriteFile(filepath.Join(ws, "MEMORY.md"), []byte("- 老板讨厌 emoji\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "notes.md"), []byte("draft"), 0o644); err != nil {
		t.Fatal(err)
	}
	app.sched.Add("daily", "scout", "check the news", 60)
	app.sched.Add("weekly", "chief", "write the summary", 600)

	if _, err := app.RemoveBot("scout", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ws); !os.IsNotExist(err) {
		t.Fatalf("the live workspace should be gone after archiving, stat gave %v", err)
	}
	departed := engine.ListDeparted(app.dataDir)
	if len(departed) != 1 {
		t.Fatalf("want one archive, got %+v", departed)
	}
	d := departed[0]
	if d.ID != "scout" || !d.HasMemory || d.Files < 2 || d.RemovedAt == 0 {
		t.Fatalf("the archive does not describe what was kept: %+v", d)
	}
	// only the other bot's routine survives
	left := app.sched.List()
	if len(left) != 1 || left[0].Bot != "chief" {
		t.Fatalf("scout's routines should be gone, left: %+v", left)
	}
}

// Choosing "delete the files too" really deletes them, leaving no archive behind.
func TestRemoveBotPurgesWorkspaceWhenAsked(t *testing.T) {
	app, srv := newTestApp(t)
	defer srv.Close()

	ws := engine.WorkspaceDir(app.dataDir, "scout")
	if err := os.WriteFile(filepath.Join(ws, "MEMORY.md"), []byte("secrets"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := app.RemoveBot("scout", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ws); !os.IsNotExist(err) {
		t.Fatalf("the workspace should be deleted, stat gave %v", err)
	}
	if d := engine.ListDeparted(app.dataDir); len(d) != 0 {
		t.Fatalf("nothing should have been archived, got %+v", d)
	}
}

// Archives can be listed, viewed and deleted; the directory name arrives over HTTP, so this also
// pins down that it cannot be used to climb out of the workspaces directory.
func TestDepartedArchivesAreListedViewedAndDeleted(t *testing.T) {
	app, srv := newTestApp(t)
	defer srv.Close()

	ws := engine.WorkspaceDir(app.dataDir, "scout")
	if err := os.WriteFile(filepath.Join(ws, "MEMORY.md"), []byte("- 周报周五交\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := app.RemoveBot("scout", false); err != nil {
		t.Fatal(err)
	}

	code, body := postJSON(t, srv.URL+"/api/bots/departed", nil)
	if code != 200 {
		t.Fatalf("listing archives gave %d: %v", code, body)
	}
	list, _ := body["departed"].([]any)
	if len(list) != 1 {
		t.Fatalf("want one archive, got %v", body["departed"])
	}
	dir, _ := list[0].(map[string]any)["dir"].(string)
	if !strings.HasPrefix(dir, "scout.removed-") {
		t.Fatalf("unexpected archive name %q", dir)
	}

	code, body = postJSON(t, srv.URL+"/api/bots/departed/detail", map[string]any{"dir": dir})
	if code != 200 {
		t.Fatalf("viewing the archive gave %d: %v", code, body)
	}
	if mem, _ := body["memory"].(string); !strings.Contains(mem, "周报周五交") {
		t.Fatalf("the memory should be readable, got %q", body["memory"])
	}

	// a climbing name is refused before any RemoveAll
	for _, bad := range []string{"..", "../..", "scout", "scout.removed-x", filepath.Join("..", "keys.json")} {
		if code, _ := postJSON(t, srv.URL+"/api/bots/departed/delete", map[string]any{"dir": bad}); code != 400 {
			t.Fatalf("deleting %q should have been refused, got %d", bad, code)
		}
	}
	if _, err := os.Stat(filepath.Join(app.dataDir, "workspaces")); err != nil {
		t.Fatalf("the workspaces directory should still be there: %v", err)
	}

	if code, body := postJSON(t, srv.URL+"/api/bots/departed/delete", map[string]any{"dir": dir}); code != 200 {
		t.Fatalf("deleting the archive gave %d: %v", code, body)
	}
	if d := engine.ListDeparted(app.dataDir); len(d) != 0 {
		t.Fatalf("the archive should be gone, got %+v", d)
	}
}

// A routine can be handed to someone else in place, without the clock restarting.
func TestRoutineCanBeReassigned(t *testing.T) {
	app, srv := newTestApp(t)
	defer srv.Close()

	r := app.sched.Add("daily", "scout", "check the news", 60)
	due := r.NextRun

	if code, body := postJSON(t, srv.URL+"/api/routines/update", map[string]any{"name": "daily", "bot": "chief"}); code != 200 {
		t.Fatalf("reassigning gave %d: %v", code, body)
	}
	got := app.sched.List()
	if len(got) != 1 || got[0].Bot != "chief" {
		t.Fatalf("the routine should belong to chief now: %+v", got)
	}
	if got[0].NextRun != due {
		t.Fatalf("reassigning must not reschedule: was %d, now %d", due, got[0].NextRun)
	}

	// Handing it to nobody must be refused, or it would only ever be skipped when due
	if code, _ := postJSON(t, srv.URL+"/api/routines/update", map[string]any{"name": "daily", "bot": "ghost"}); code != 400 {
		t.Fatal("reassigning to a nonexistent bot should 400")
	}
	if code, _ := postJSON(t, srv.URL+"/api/routines/update", map[string]any{"name": "nope", "bot": "chief"}); code != 404 {
		t.Fatal("reassigning a nonexistent routine should 404")
	}
}

// Two members cannot share a name: a group call matches ids and display names alike, so a duplicate
// hands the same job out twice.
func TestDuplicateDisplayNameIsRejected(t *testing.T) {
	app, srv := newTestApp(t)
	defer srv.Close()

	if err := app.UpdateBot(config.BotConfig{Name: "chief", Role: "Lead", Provider: "fake", DisplayName: "吴敏"}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.AddBot(config.BotConfig{Name: "wumin-k3f9a", Role: "r", Provider: "fake", DisplayName: "吴敏"}); err == nil {
		t.Fatal("a second 吴敏 should have been refused")
	}
	// colliding with someone's id calls on both, too
	if _, err := app.AddBot(config.BotConfig{Name: "newbie", Role: "r", Provider: "fake", DisplayName: "scout"}); err == nil {
		t.Fatal("a display name equal to another member's id should have been refused")
	}
	// a bot keeping its own name is not a clash
	if err := app.UpdateBot(config.BotConfig{Name: "chief", Role: "Boss", Provider: "fake", DisplayName: "吴敏"}); err != nil {
		t.Fatalf("editing a bot without changing its display name should be fine: %v", err)
	}
}

// A newly created bot must not join any group: who belongs in a room is the user's call, not the system's.
func TestNewBotDoesNotAutoJoinGroups(t *testing.T) {
	app, srv := newTestApp(t)
	defer srv.Close()

	if _, err := app.AddBot(config.BotConfig{Name: "newbie", Role: "r", Provider: "fake"}); err != nil {
		t.Fatal(err)
	}
	for _, g := range app.bus.Groups() {
		for _, m := range g.Members {
			if m == "newbie" {
				t.Fatalf("newbie was auto-added to group %q", g.ID)
			}
		}
	}

	// The DM still works: staying out of a group affects the group only, never one-on-one
	if app.bus.Bot("newbie") == nil {
		t.Fatal("the bot should still exist and be reachable in a DM")
	}

	// Adding it deliberately must work
	if ok, changed := app.bus.SetGroupMemberIn("group", "newbie", true); !ok || !changed {
		t.Fatal("adding it to the group should succeed")
	}
	if !app.bus.IsGroupMemberOf("group", "newbie") {
		t.Fatal("it should be a member after being added")
	}
}

// The preflight must allow Authorization: the pairing code travels in that header, and without it a
// cross-origin client cannot connect at all.
func TestCORSAllowsAuthorizationHeader(t *testing.T) {
	_, srv := newTestApp(t)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodOptions, srv.URL+"/api/state", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "http://example.invalid")
	req.Header.Set("Access-Control-Request-Method", "GET")
	req.Header.Set("Access-Control-Request-Headers", "authorization")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	allowed := strings.ToLower(res.Header.Get("Access-Control-Allow-Headers"))
	if !strings.Contains(allowed, "authorization") {
		t.Fatalf("the preflight must allow Authorization, got %q", allowed)
	}
}

// Local mode is authenticated too.

// "Bound to 127.0.0.1" is not a security boundary: every page open on the machine can reach localhost.
// This hole was real — unauthenticated, any site could read the team and its history, create bots, set
// the permission tier to full and send messages, which chains into arbitrary command execution on the
// user's machine from a web page.
func TestLocalModeStillRequiresToken(t *testing.T) {
	app, srv := newTestApp(t)
	defer srv.Close()

	guarded := httptest.NewServer(netx.RequireToken("the-secret", app.Handler()))
	defer guarded.Close()

	// Every request a foreign page could make must be refused
	attacks := []struct{ method, path, body string }{
		{http.MethodGet, "/api/state", ""},
		{http.MethodPost, "/api/bots", `{"name":"pwned","role":"x","provider":"fake"}`},
		{http.MethodPost, "/api/settings", `{"permission":"full"}`},
		{http.MethodPost, "/api/send", `{"chat":"group","text":"hi"}`},
		{http.MethodPost, "/api/approve", `{"id":1,"approved":true}`},
	}
	for _, a := range attacks {
		req, err := http.NewRequest(a.method, guarded.URL+a.path, strings.NewReader(a.body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", "https://evil.example")
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != 401 {
			t.Fatalf("%s %s without a token should be 401, got %d", a.method, a.path, res.StatusCode)
		}
	}
	// and not one of them may have taken effect
	if app.bus.Bot("pwned") != nil {
		t.Fatal("an unauthenticated request created a bot")
	}
	if got := app.settings.Perm(); got == string(config.PermFull) {
		t.Fatalf("an unauthenticated request raised the permission tier to %q", got)
	}

	// with the pairing code it goes through as usual
	req, err := http.NewRequest(http.MethodGet, guarded.URL+"/api/state", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer the-secret")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("a request carrying the pairing code should be 200, got %d", res.StatusCode)
	}
}

// The working-directory list has to surface in state and revocation has to actually revoke: those two
// are the user's only means of checking, and taking back, what "no approvals inside the workspace"
// grants.
func TestBotRootsSurfaceInStateAndCanBeRevoked(t *testing.T) {
	app, srv := newTestApp(t)
	proj := t.TempDir()

	w := app.bus.Bot("chief")
	if !w.Roots().Add(proj) {
		t.Fatal("a plain project directory should be granted")
	}
	granted := w.Roots().List()[0]

	fetchState := func(t *testing.T, srv *httptest.Server) map[string]any {
		t.Helper()
		resp, err := http.Get(srv.URL + "/api/state")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return out
	}
	botIn := func(state map[string]any) map[string]any {
		for _, raw := range state["bots"].([]any) {
			b := raw.(map[string]any)
			if b["name"] == "chief" {
				return b
			}
		}
		t.Fatal("chief missing from state")
		return nil
	}

	b := botIn(fetchState(t, srv))
	if ws, _ := b["workspace"].(string); ws == "" {
		t.Fatal("每位成员自己的工作目录也要印出来 / a member's own workspace must be printed too")
	}
	roots := b["roots"].([]any)
	if len(roots) != 1 || roots[0] != granted {
		t.Fatalf("granted directory missing from state: %v", roots)
	}

	// Another member's list is untouched and an unknown directory errors: this endpoint takes HTTP input
	if code, _ := postJSON(t, srv.URL+"/api/bots/roots/remove", map[string]any{"name": "scout", "dir": granted}); code != 404 {
		t.Fatalf("removing from a member who was never granted it should 404, got %d", code)
	}
	if code, _ := postJSON(t, srv.URL+"/api/bots/roots/remove", map[string]any{"name": "chief", "dir": "/nope"}); code != 404 {
		t.Fatalf("removing an unknown directory should 404, got %d", code)
	}

	if code, _ := postJSON(t, srv.URL+"/api/bots/roots/remove", map[string]any{"name": "chief", "dir": granted}); code != 200 {
		t.Fatalf("removing a granted directory should succeed, got %d", code)
	}
	if n := len(w.Roots().List()); n != 0 {
		t.Fatalf("撤销之后清单该空了 / the list should be empty after revocation: %d", n)
	}
	if n := len(botIn(fetchState(t, srv))["roots"].([]any)); n != 0 {
		t.Fatalf("state should reflect the revocation: %d", n)
	}
}

// "Approve and make this a working directory" on the approval card: one click both lets this command
// through and stops the questions about that directory. It is the only place a phrasing like "treat
// bot-bureau as your working directory" can land reliably — by approval time the command spells the
// directory out and nothing has to be guessed.
func TestApproveCanGrantTheDirectoryOnTheCard(t *testing.T) {
	app, srv := newTestApp(t)
	proj := t.TempDir()

	w := app.bus.Bot("chief")
	ap := app.bus.RequestApproval("chief", "bash: ls "+proj, "group", proj)

	// An approval with no grantable directory cannot be used as one
	bare := app.bus.RequestApproval("chief", "bash: touch x", "group", "")
	if code, _ := postJSON(t, srv.URL+"/api/approve", map[string]any{"id": bare.ID, "approved": true, "grant": true}); code != 400 {
		t.Fatalf("granting from an approval with no directory should 400, got %d", code)
	}
	app.bus.Decide(bare.ID, false, "")

	code, out := postJSON(t, srv.URL+"/api/approve", map[string]any{"id": ap.ID, "approved": true, "grant": true})
	if code != 200 {
		t.Fatalf("approve+grant should succeed: %d %v", code, out)
	}
	if got, _ := out["granted"].(string); got == "" {
		t.Fatalf("the response should name the directory it granted: %v", out)
	}
	if approved, _, _ := ap.WaitCtx(nil); !approved {
		t.Fatal("the command itself should still be approved")
	}
	if !w.Roots().Contains(proj) {
		t.Fatalf("the granted directory should be in bounds now: %v", w.Roots().List())
	}
}

func TestStateTodosAreArraysAndPlanApprovalsCarryKind(t *testing.T) {
	config.SetApprovalTimeout(2 * time.Second)
	t.Cleanup(func() { config.SetApprovalTimeout(0) })
	app, srv := newTestApp(t)
	fetch := func() map[string]any {
		t.Helper()
		resp, err := http.Get(srv.URL + "/api/state")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	botNamed := func(state map[string]any, name string) map[string]any {
		t.Helper()
		for _, raw := range state["bots"].([]any) {
			b := raw.(map[string]any)
			if b["name"] == name {
				return b
			}
		}
		t.Fatalf("%s missing from state", name)
		return nil
	}

	chief := botNamed(fetch(), "chief")
	todos, ok := chief["todos"].([]any)
	if !ok {
		t.Fatalf("todos must be an array, got %T %v", chief["todos"], chief["todos"])
	}
	if len(todos) != 0 {
		t.Fatalf("empty personal list should be [], got %v", todos)
	}

	w := app.bus.Bot("chief")
	out, _, isErr := w.Toolbox().Execute("todo_write", map[string]any{
		"items": []any{map[string]any{"id": "ship", "content": "open the PR", "status": "pending"}},
	})
	if isErr {
		t.Fatalf("todo_write: %q", out)
	}
	listed := botNamed(fetch(), "chief")["todos"].([]any)
	if len(listed) != 1 {
		t.Fatalf("state should show the personal list, got %v", listed)
	}
	item := listed[0].(map[string]any)
	if item["id"] != "ship" || item["content"] != "open the PR" || item["status"] != "pending" {
		t.Fatalf("todo row: %v", item)
	}

	done := make(chan string, 1)
	go func() {
		text, _, err := w.Toolbox().Execute("submit_plan", map[string]any{
			"title": "Split auth",
			"body":  "edit two files",
		})
		if err {
			done <- "err:" + text
			return
		}
		done <- text
	}()
	var ap *engine.Approval
	for i := 0; i < 400; i++ {
		aps := app.bus.PendingApprovals()
		if len(aps) > 0 {
			ap = aps[0]
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if ap == nil {
		t.Fatal("submit_plan should raise an approval")
	}
	state := fetch()
	found := false
	for _, raw := range state["approvals"].([]any) {
		row := raw.(map[string]any)
		if row["kind"] == "plan" && row["title"] == "Split auth" && row["body"] == "edit two files" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("state should carry the plan card, got %v", state["approvals"])
	}
	app.bus.Decide(ap.ID, true, "")
	select {
	case got := <-done:
		if !strings.Contains(got, "accepted") {
			t.Fatalf("approve should continue: %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("submit_plan did not return after approve")
	}
}

// Catalog OAuth installs persist the connector before a token exists; /api/mcp/add must still return
// ok so the client can open the browser Authorize flow.
func TestMCPAddOAuthNeedsAuth(t *testing.T) {
	_, srv := newTestApp(t)
	code, out := postJSON(t, srv.URL+"/api/mcp/add", map[string]any{
		"name": "atlassian",
		"url":  "https://mcp.atlassian.com/v1/mcp/authv2",
		"auth": "oauth",
	})
	if code != 200 {
		t.Fatalf("status %d body %v", code, out)
	}
	if out["ok"] != true || out["needs_auth"] != true {
		t.Fatalf("expected needs_auth success, got %v", out)
	}
	code, out = postJSON(t, srv.URL+"/api/mcp", nil)
	// GET /api/mcp ignores body; use http.Get
	resp, err := http.Get(srv.URL + "/api/mcp")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var status map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&status)
	list, _ := status["mcp"].([]any)
	found := false
	for _, raw := range list {
		row := raw.(map[string]any)
		if row["name"] == "atlassian" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("atlassian should be listed after oauth add: %v", status)
	}
	_ = code
	_ = out
}
