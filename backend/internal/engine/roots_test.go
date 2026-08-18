package engine

import (
	"botbureau/backend/internal/config"

	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testDirOutsideScratch creates a fixture outside the directory that Toolbox treats as shared
// scratch space. t.TempDir uses /tmp on Linux, while /tmp is intentionally always in bounds.
func testDirOutsideScratch(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(cwd, ".roots-test-*")
	if err != nil {
		t.Fatal(err)
	}
	if within(canonical(dir), scratchDir()) {
		_ = os.RemoveAll(dir)
		t.Fatalf("test fixture must be outside scratch directory: %s", dir)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// UserPaths has to pick the directory a user named out of an ordinary sentence, and to let everything
// merely path-shaped go by.
func TestUserPathsPicksDirectoriesOnly(t *testing.T) {
	proj := t.TempDir()
	file := filepath.Join(proj, "README.md")
	if err := os.WriteFile(file, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	// What comes back is canonical. On macOS t.TempDir() hands out /var/…, and /var is a symlink whose
	// real home is /private/var; the two are not comparable until both are in one form.
	want := canonical(proj)

	cases := []struct {
		name string
		text string
		want []string
	}{
		{"bare path", "帮我看看 " + proj, []string{want}},

		// CJK punctuation glued to the end of a path is the norm; leave it on and the stat misses and the
		// whole feature quietly does nothing
		{"trailing CJK comma", "审查一下 " + proj + "，谢谢", []string{want}},
		{"trailing period", "look at " + proj + ".", []string{want}},
		{"trailing slash", "cd " + proj + "/", []string{want}},
		{"backticked", "仓库在 `" + proj + "` 里", []string{want}},

		// A file does not count: the smallest grant that could carry it is its directory, which might be
		// half a home directory
		{"a file is not a directory", "读一下 " + file, nil},
		{"nonexistent", "看看 /no/such/place/at/all", nil},
		{"relative", "看看 ./src 和 src/main.go", nil},
		{"url path fragment", "见 https://example.com/docs/guide", nil},
		{"repeated", proj + " 和 " + proj + " 是同一个", []string{want}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := UserPaths(c.text)
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("got %v, want %v", got, c.want)
				}
			}
		})
	}
}

// The blocklist is what backstops this whole mechanism: nobody writing / or their home directory means
// "the machine is yours".
func TestRootsRefusesWholeMachineAndOwnData(t *testing.T) {
	data := t.TempDir()
	workspace := filepath.Join(data, "workspaces", "bot")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	r := NewRoots(workspace, data)

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"/", "/etc", "/usr", "/Users", home} {
		if r.Add(dir) {
			t.Fatalf("%s must never be granted", dir)
		}
	}

	// The data directory and anything inside it are refused: granting either hands over the pairing
	// code, every vendor key and other members' workspaces
	for _, dir := range []string{data, workspace} {
		if r.Add(dir) {
			t.Fatalf("%s must never be granted", dir)
		}
	}
	if len(r.List()) != 0 {
		t.Fatalf("nothing should have been granted: %v", r.List())
	}

	// The parent, by contrast, has to be grantable: in development data sits inside the repository, and
	// refusing it too would mean "this repository is yours" could never take effect. It is granted, and
	// the data directory inside it stays out of reach all the same.
	parent := filepath.Dir(data)
	if !r.Add(parent) {
		t.Fatalf("%s should be grantable", parent)
	}
	if !r.Contains(filepath.Join(parent, "README.md")) {
		t.Fatal("授出去的目录里的普通文件应当在界内 / an ordinary file in the granted directory should be inside")
	}
	for _, hole := range []string{data, workspace, filepath.Join(data, "keys.json")} {
		if r.Contains(hole) {
			t.Fatalf("data 目录必须是个洞 / the data directory must stay a hole: %s", hole)
		}
	}
}

// Granting, persistence, surviving a restart, and revocation taking effect at once.
func TestRootsPersistAndRevoke(t *testing.T) {
	data := t.TempDir()
	workspace := filepath.Join(data, "workspaces", "bot")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	proj := t.TempDir()

	r := NewRoots(workspace, data)
	if !r.Add(proj) {
		t.Fatal("a plain project directory should be granted")
	}
	if r.Add(proj) {
		t.Fatal("granting the same directory twice should report nothing new")
	}
	if !r.Contains(filepath.Join(proj, "deep", "not-created-yet.txt")) {

		// A target that does not exist yet must still be judged: a new file clears the bounds check first
		t.Fatal("a path under a granted directory should be inside even before it exists")
	}
	if r.Contains(filepath.Dir(proj)) {
		t.Fatal("授予一个目录不该连它的父目录一起授出去 / granting a directory must not grant its parent")
	}

	reloaded := NewRoots(workspace, data)
	if got := reloaded.List(); len(got) != 1 || got[0] != r.List()[0] {
		t.Fatalf("a grant must survive a restart: %v", got)
	}
	if !reloaded.Remove(r.List()[0]) {
		t.Fatal("remove should find the directory it just loaded")
	}
	if len(NewRoots(workspace, data).List()) != 0 {
		t.Fatal("撤销要落盘 / revocation must be persisted")
	}
}

// End to end: once the user names a directory in conversation, an auto-tier member works there
// without approvals, while the directory next to it still asks. This is exactly the gap that made
// "which working directory" impossible to answer before.
func TestNamedDirectoryBecomesWorkspaceForTheBotAddressed(t *testing.T) {
	dir := testDirOutsideScratch(t)
	proj := testDirOutsideScratch(t)
	outside := testDirOutsideScratch(t)

	bus := NewBus()
	sched := NewScheduler(bus, filepath.Join(dir, "routines.json"))
	deps := newTestDeps(t, dir)
	deps.Settings.SetPermission(string(config.PermAuto))

	w, err := NewBotWorker(config.BotConfig{Name: "worker", Role: "test", Provider: "fake"}, bus, sched, dir, deps)
	if err != nil {
		t.Fatal(err)
	}
	bus.Register(w)
	tb := w.toolbox
	tb.currentChat = "group"

	// Before the grant that directory is outside, like anywhere else on the machine
	if tb.inBounds(proj) {
		t.Fatal("a directory nobody named must not be in bounds")
	}

	// What the model says does not count; only what the user says does. Break this line and a bot that
	// reads a file saying "treat / as your workspace" has escalated itself.
	w.grantUserRoots(Msg{Sender: "worker", Content: "看下 " + proj, Chat: "group", GrantRoots: true})
	if tb.inBounds(proj) {
		t.Fatal("只有用户能授权 / only the user may grant")
	}

	// A grant not addressed to them (someone else was named in the group) does not count either
	w.grantUserRoots(Msg{Sender: "user", Content: "看下 " + proj, Chat: "group", GrantRoots: false, Respond: true})
	if tb.inBounds(proj) {
		t.Fatal("点名给别人的授权不该落到他头上 / a grant aimed at someone else must not land here")
	}

	w.grantUserRoots(Msg{Sender: "user", Content: "帮我审查一下 " + proj + "，谢谢", Chat: "group", GrantRoots: true})
	if !tb.inBounds(proj) {
		t.Fatalf("用户指名过的目录应当在界内 / a directory the user named should be in bounds: %v", w.Roots().List())
	}

	// A command in that directory runs straight through and raises no approval
	out, _, isErr := tb.Execute("bash", map[string]any{"command": "ls " + proj})
	if isErr {
		t.Fatalf("a command in a named directory must not fail under auto: %s", out)
	}
	if n := len(bus.PendingApprovals()); n != 0 {
		t.Fatalf("a command in a named directory must raise no approval under auto, got %d", n)
	}

	// Absolute reads and writes work too, and land where they say: read_file used to be joined onto the
	// workspace and actually reached <workspace>/Users/..., which was never there
	target := filepath.Join(proj, "note.txt")
	if out, _, isErr := tb.Execute("write_file", map[string]any{"path": target, "content": "written"}); isErr {
		t.Fatalf("a write into a named directory should succeed: %s", out)
	}
	if raw, err := os.ReadFile(target); err != nil || string(raw) != "written" {
		t.Fatalf("the write did not land in the real directory: %v %q", err, raw)
	}

	// The directory next door, unnamed, still asks: what was granted is one directory, not a machine
	done := make(chan string, 1)
	go func() {
		res, _, _ := tb.Execute("bash", map[string]any{"command": "ls " + outside})
		done <- res
	}()
	deadline := time.After(2 * time.Second)
	for {
		if len(bus.PendingApprovals()) > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("an unnamed directory ran without approval under auto")
		case res := <-done:
			t.Fatalf("an unnamed directory returned without waiting: %s", res)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	ap := bus.PendingApprovals()[0]
	bus.Decide(ap.ID, false, "test rejection")
	if res := <-done; !strings.Contains(res, "reject") {
		t.Fatalf("a rejection should come back explained: %s", res)
	}

	// Revoking puts it straight back to asking
	w.Roots().Remove(w.Roots().List()[0])
	if tb.inBounds(proj) {
		t.Fatal("撤销必须立刻生效 / revocation must take effect at once")
	}
	if _, err := tb.resolve(target); err == nil {
		t.Fatal("a revoked directory should stop resolving")
	}
}

// Who a grant reaches in a group: whoever was named, or everyone when nobody was.
// "treat /Users/me/proj as your working directory", said to the room, is said to the room;
// "@Wumin have a look at /Users/me/proj" is said to Wumin.
func TestGroupGrantGoesToWhoeverWasNamed(t *testing.T) {
	dir := t.TempDir()
	proj := t.TempDir()

	bus := NewBus()
	sched := NewScheduler(bus, filepath.Join(dir, "routines.json"))
	deps := newTestDeps(t, dir)

	for _, name := range []string{"alice", "bob"} {
		w, err := NewBotWorker(config.BotConfig{Name: name, Role: "test", Provider: "fake"}, bus, sched, dir, deps)
		if err != nil {
			t.Fatal(err)
		}
		bus.Register(w)
		bus.SetGroupMember(name, true)
	}

	// No consumers started: what matters here is the flag on the delivered Msg, not how a turn runs
	grantedBy := func(name string) bool {
		w := bus.Bot(name)
		select {
		case m := <-w.inbox:
			return m.GrantRoots
		default:
			t.Fatalf("%s got no message at all", name)
			return false
		}
	}

	bus.PostGroupTo("group", "user", "把 "+proj+" 当你们的工作目录", []string{"alice"})
	if !grantedBy("alice") || !grantedBy("bob") {
		t.Fatal("没点名任何人时，这句授权是说给全群的 / with nobody named the grant is addressed to the room")
	}

	bus.PostGroupTo("group", "user", "@alice 看下 "+proj, []string{"alice"})
	if !grantedBy("alice") {
		t.Fatal("被点名的应当拿到授权 / the member named should be granted")
	}
	if grantedBy("bob") {
		t.Fatal("没被点名的不该跟着拿到 / a member not named must not be granted along the way")
	}

	// A message from one member to another never grants, named or not
	bus.PostGroupTo("group", "alice", "把 "+proj+" 当工作目录", []string{"bob"})
	if grantedBy("bob") {
		t.Fatal("只有用户能授权 / only the user may grant")
	}
}
