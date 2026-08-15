package api

import (
	"botbureau/backend/internal/bridge"
	"botbureau/backend/internal/config"
	"botbureau/backend/internal/engine"
	"botbureau/backend/internal/netx"
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
	// 断言直接对着源码里的英文原文，所以把语言钉死为 en（否则会跟随系统环境，
	// 中文机器上跑出来的就是译文，断言全炸）。专门测翻译的用例自己再切回 zh。
	// The assertions read the English source text directly, so pin the locale to en (it would otherwise
	// follow the environment and every assertion would face translated output on a Chinese machine).
	// The tests that specifically exercise translation switch to zh themselves.
	settings := config.NewSettings(dir)
	settings.SetLocalePref("en")
	deps.Settings = settings // 必须在建 bot 之前挂上，工具箱是在那时抓走它的 / must be attached before bots are built; the toolbox captures it then
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

	// 群聊：不点名 → 默认 chief 回应（fake 回声）
	// Group chat: no mention → chief responds by default (fake echo)
	code, _ := postJSON(t, srv.URL+"/api/send", map[string]any{"chat": "group", "text": "hello"})
	if code != 200 {
		t.Fatal("group send failed")
	}
	waitForEvent(t, srv, func(ev engine.Event) bool {
		return ev["kind"] == "msg" && ev["chat"] == "group" && ev["source"] == "chief" &&
			strings.Contains(ev["text"].(string), "hello")
	})

	// 群聊：@scout 点名 → scout 回应
	// Group chat: mentioning @scout → scout responds
	postJSON(t, srv.URL+"/api/send", map[string]any{"chat": "group", "text": "@scout take a look"})
	waitForEvent(t, srv, func(ev engine.Event) bool {
		return ev["kind"] == "msg" && ev["chat"] == "group" && ev["source"] == "scout"
	})

	// 私聊 scout
	// DM scout
	postJSON(t, srv.URL+"/api/send", map[string]any{"chat": "dm:scout", "text": "dm test"})
	waitForEvent(t, srv, func(ev engine.Event) bool {
		return ev["kind"] == "msg" && ev["chat"] == "dm:scout" && ev["source"] == "scout" &&
			strings.Contains(ev["text"].(string), "dm test")
	})

	// 目标不存在
	// Target does not exist
	if code, _ := postJSON(t, srv.URL+"/api/send", map[string]any{"chat": "dm:ghost", "text": "x"}); code != 404 {
		t.Fatalf("a nonexistent dm target should 404, got %d", code)
	}
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
	// 新 bot 立即可用（私聊回声）
	// The new bot is usable immediately (DM echo)
	postJSON(t, srv.URL+"/api/send", map[string]any{"chat": "dm:coder", "text": "you there"})
	waitForEvent(t, srv, func(ev engine.Event) bool {
		return ev["kind"] == "msg" && ev["chat"] == "dm:coder" && ev["source"] == "coder"
	})

	// 非法参数
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

	// 删除
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
	// 手工造一个审批（等价于 bot 里 bash 走审批）
	// Manually create an approval (equivalent to bash going through approval inside a bot)
	go app.bus.RequestApproval("chief", "bash: touch x", "group")
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

func TestHTTPCancel(t *testing.T) {
	app, srv := newTestApp(t)
	go app.bus.RequestApproval("chief", "bash: touch x", "group")
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
	// 移出 scout：@scout 失效，默认接单人仍是 chief
	// Remove scout: @scout stops working, the default responder is still chief
	if code, _ := postJSON(t, srv.URL+"/api/group/set", map[string]any{"name": "scout", "in": false}); code != 200 {
		t.Fatal("removal failed")
	}
	if got := app.bus.GroupMembers(); len(got) != 1 || got[0] != "chief" {
		t.Fatalf("wrong member list: %v", got)
	}
	postJSON(t, srv.URL+"/api/send", map[string]any{"chat": "group", "text": "@scout take a look"})
	// @群外成员无效 → 落到默认 chief
	// @ on a non-member has no effect → falls back to the default chief
	waitForEvent(t, srv, func(ev engine.Event) bool {
		return ev["kind"] == "msg" && ev["chat"] == "group" && ev["source"] == "chief" &&
			strings.Contains(ev["text"].(string), "take a look")
	})
	// 拉回来
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
			return // 收到了 / received
		}
	}
	t.Fatal("SSE never pushed the event")
}

// 编辑 bot：改头像/显示名不能动列表位次（群聊默认收件人就是第一个），也不能掉出群。
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
	// 落盘的 bots.yaml 也要带上新字段 / the new fields must survive to bots.yaml
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
	// 被拒之后原 bot 必须还活着 / the original bot must still be alive after a rejection
	if app.bus.Bot("scout") == nil {
		t.Fatal("scout vanished after a failed validation")
	}
}

// 群聊的名字和头像存在 settings 里，改语言不能把它们清掉。
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
	i18n.SetLocale("zh") // 复原，别影响其他用例 / restore so other tests are unaffected
}

// 团队可以删空：空团队是新装用户的初始状态，客户端会显示引导。
// 早先这里是"最后一个不许删"，那是因为引擎当时拒绝空 bots.yaml 启动。
//
// The team may be emptied: that is the initial state for a fresh install, where the client shows
// onboarding. This used to assert the opposite, back when the engine refused to start on an empty file.
func TestRemoveBotCanEmptyTheTeam(t *testing.T) {
	app, srv := newTestApp(t)
	defer srv.Close()

	for _, name := range []string{"scout", "chief"} {
		if err := app.RemoveBot(name); err != nil {
			t.Fatalf("removing %s failed: %v", name, err)
		}
	}
	if n := len(app.bus.Bots()); n != 0 {
		t.Fatalf("should be empty, %d left", n)
	}
	// 空的 bots.yaml 必须还能加载，否则下次启动就废了
	// An empty bots.yaml must still load, or the next start is broken
	saved, err := config.LoadBotConfigs(app.cfgPath)
	if err != nil {
		t.Fatalf("an empty bots.yaml failed to load: %v", err)
	}
	if len(saved) != 0 {
		t.Fatalf("should be empty, got %+v", saved)
	}
	if err := app.RemoveBot("nobody"); err == nil {
		t.Fatal("removing a nonexistent bot should error")
	}
}

// 新建的 bot 不该自动进群：谁在群里是用户的决定，不是系统替他决定的。
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
	// 私聊照常可用：不在群里只影响群，不影响一对一
	// The DM still works: staying out of a group affects the group only, never one-on-one
	if app.bus.Bot("newbie") == nil {
		t.Fatal("the bot should still exist and be reachable in a DM")
	}
	// 用户主动拉进去就该生效
	// Adding it deliberately must work
	if !app.bus.SetGroupMemberIn("group", "newbie", true) {
		t.Fatal("adding it to the group should succeed")
	}
	if !app.bus.IsGroupMemberOf("group", "newbie") {
		t.Fatal("it should be a member after being added")
	}
}

// 预检必须放行 Authorization：配对码就走这个头，少了它跨源客户端连不上。
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

// 本机模式同样要认证。
//
// "只绑 127.0.0.1" 不是安全边界：本机上每个网页都能访问 localhost。这个洞真实存在过——
// 免认证时任何站点都能读走团队和聊天记录，还能建 bot、把权限改成 full、给 bot 发消息，
// 串起来就是从一个网页在用户机器上执行任意命令。
//
// Local mode is authenticated too.
//
// "Bound to 127.0.0.1" is not a security boundary: every page open on the machine can reach localhost.
// This hole was real — unauthenticated, any site could read the team and its history, create bots, set
// the permission tier to full and send messages, which chains into arbitrary command execution on the
// user's machine from a web page.
func TestLocalModeStillRequiresToken(t *testing.T) {
	app, srv := newTestApp(t)
	defer srv.Close()

	guarded := httptest.NewServer(netx.RequireToken("the-secret", app.Handler()))
	defer guarded.Close()

	// 外站页面能发出的每一种请求都必须被挡住
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
	// 副作用一个都不能发生 / and not one of them may have taken effect
	if app.bus.Bot("pwned") != nil {
		t.Fatal("an unauthenticated request created a bot")
	}
	if got := app.settings.Perm(); got == string(config.PermFull) {
		t.Fatalf("an unauthenticated request raised the permission tier to %q", got)
	}

	// 带上配对码则照常放行 / with the pairing code it goes through as usual
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
