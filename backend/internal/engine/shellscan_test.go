package engine

import (
	"botbureau/backend/internal/config"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Reading one command: splitting it, honouring quotes, deciding whether it only reads.
// The two long cases are verbatim from real screenshots — the grep pipeline should never have been
// asked about, the curl download rightly is.
func TestBashReadOnlyJudgesEachSegment(t *testing.T) {
	cases := []struct {
		name     string
		command  string
		readOnly bool
	}{
		{"plain read", "cat notes.txt", true},

		// A pipe is not a side effect. That one bar used to be enough to require a human.
		{"pipeline", "grep -n foo main.go | head -20", true},
		{"sequence", "ls; pwd; wc -l main.go", true},
		{"and-list", "ls && pwd", true},

		// The bar inside quotes is a regex, not a pipe; splitting on it would make this very ordinary
		// command ask forever
		{"regex alternation stays one segment",
			`grep -oE 'MJ[0-9A-Z]{4,}/A|"priceAmount":[0-9.]+|"fullPrice":[^,}]+' page.html | head -80`, true},
		{"quoted semicolon", `grep -n 'a; rm -rf /' notes.txt`, true},
		{"git read-only subcommand", "git log --oneline | head -5", true},
		{"sed without -i", "sed -n '1,20p' main.go", true},

		// Everything below has to fall back to asking
		{"redirect writes", "grep foo main.go > out.txt", false},
		{"append writes", "echo hi >> notes.txt", false},
		{"git mutating subcommand", "git commit -m x", false},
		{"sed -i rewrites in place", "sed -i '' s/a/b/ main.go", false},

		// A read-only first word cannot rescue command substitution: what it touches is unknown until it runs
		{"command substitution", "ls $(cat target)", false},
		{"backticks", "cat `cat target`", false},
		{"one bad segment poisons the rest", "ls | rm -rf x", false},
		{"unknown command", "python3 script.py", false},

		// find is refused when it acts; a find that only lists is among the commonest commands there is
		{"find that only lists", `find . -type f -name '*.go' -maxdepth 2`, true},
		{"find -delete acts", "find . -name '*.go' -delete", false},
		{"find -exec acts", "find . -name '*.go' -exec rm {} ;", false},
		{"find -fprintf writes", "find . -fprintf out.txt %p", false},

		// Discarding error output writes nothing, and models reach for it on nearly every command
		{"stderr to the bit bucket", "ls -la /etc 2>/dev/null", true},
		{"stdout to the bit bucket", "grep -r x . >/dev/null", true},
		{"file descriptor duplication", "grep x notes.txt 2>&1 | head -5", true},
		{"both streams to the bit bucket", "ls foo &>/dev/null", true},

		// A real file as the target is still a write
		{"redirect to a real file", "ls -la > listing.txt", false},
		{"append to a real file", "ls >> listing.txt", false},
		{"xargs can summon anything", "ls | xargs rm", false},
		{"unbalanced quotes are unreadable", `grep -n 'unclosed main.go`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			segs, subst, ok := scanBash(c.command)
			if got := bashReadOnly(segs, subst, ok); got != c.readOnly {
				t.Fatalf("readOnly = %v, want %v (segments %+v, subst=%v, ok=%v)", got, c.readOnly, segs, subst, ok)
			}
		})
	}
}

// End to end: what the two commands from the screenshots should now meet under the auto tier.
func TestScreenshotCommandsUnderAutoTier(t *testing.T) {
	dir := t.TempDir()
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

	// A grep pipeline over a file it just downloaded into /tmp: pure reading, inside the scratch
	// directory, and not something to ask about
	grepPipeline := `grep -oE 'MJ[0-9A-Z]{4,}/A|"priceAmount":[0-9.]+' /tmp/mba_au.html | head -80; echo '==='; grep -oE '"partNumber":"[^"]+"' /tmp/mba_au.html | head -40`
	segs, subst, ok := scanBash(grepPipeline)
	if !bashReadOnly(segs, subst, ok) {
		t.Fatal("这条 grep 管道是纯读 / the grep pipeline only reads")
	}
	if tb.bashEscapes(segs) {
		t.Fatal("/tmp 应当算界内 / /tmp should count as inside")
	}

	// Actually run it: not one approval should pile up
	if err := os.WriteFile("/tmp/mba_au.html", []byte(`{"partNumber":"MJ3E4X/A","priceAmount":1999.0}`), 0o644); err != nil {
		t.Skipf("cannot write to /tmp on this machine: %v", err)
	}
	defer os.Remove("/tmp/mba_au.html")
	out, isErr := tb.Execute("bash", map[string]any{"command": grepPipeline})
	if isErr {
		t.Fatalf("the pipeline should have run: %s", out)
	}
	if !strings.Contains(out, "MJ3E4X/A") {
		t.Fatalf("the pipeline never read the file: %q", out)
	}
	if n := len(bus.PendingApprovals()); n != 0 {
		t.Fatalf("这条命令一次都不该问 / this command must not ask even once, got %d", n)
	}

	// The curl download makes a network request and writes a file, and its first word is not on the
	// whitelist: it still asks, and rightly so
	curlCmd := `curl -sL --max-time 30 "https://www.apple.com.cn/shop/buy-mac" -o /tmp/mbp.html; wc -c /tmp/mbp.html`
	segs, subst, ok = scanBash(curlCmd)
	if bashReadOnly(segs, subst, ok) {
		t.Fatal("curl 会写文件、会联网，不是只读 / curl writes a file and reaches the network; it is not read-only")
	}

	// /tmp being inside does not bring anywhere else along with it
	segs, _, _ = scanBash("cat /etc/hosts")
	if !tb.bashEscapes(segs) {
		t.Fatal("/etc 仍然在界外 / /etc is still out of bounds")
	}
	segs, _, _ = scanBash("ls ~/Documents")
	if !tb.bashEscapes(segs) {
		t.Fatal("主目录仍然在界外 / the home directory is still out of bounds")
	}
}

// Verbatim from the screenshot: list the workspace, /tmp and the inbox, then find a few kinds of file.
// It tripped on three separate counts, every one a false positive — not one path out of bounds, and not
// one byte written.
func TestListingAndFindNeedsNoApproval(t *testing.T) {
	dir := t.TempDir()
	bus := NewBus()
	sched := NewScheduler(bus, filepath.Join(dir, "routines.json"))
	deps := newTestDeps(t, dir)
	deps.Settings.SetPermission(string(config.PermAsk)) // the strictest tier

	w, err := NewBotWorker(config.BotConfig{Name: "bot", Role: "test", Provider: "fake"}, bus, sched, dir, deps)
	if err != nil {
		t.Fatal(err)
	}
	bus.Register(w)
	w.toolbox.currentChat = "group"

	cmd := "ls -la " + w.workspace + ` /tmp 2>/dev/null; ls -la inbox 2>/dev/null; find ` + w.workspace +
		` -type f \( -name '*.txt' -o -name '*.md' -o -name '*.json' -o -name '*.html' \) 2>/dev/null | head -50`

	segs, subst, ok := scanBash(cmd)
	if !bashReadOnly(segs, subst, ok) {
		for i, s := range segs {
			t.Logf("[%d] redirect=%v safe=%v words=%q", i, s.redirect, safeSegment(s.words), s.words)
		}
		t.Fatal("列目录 + find 是纯读 / listing plus find only reads")
	}
	if w.toolbox.bashEscapes(segs) {
		t.Fatal("工作目录、/tmp、相对路径、/dev/null 都在界内 / the workspace, /tmp, a relative path and /dev/null are all inside")
	}

	// ask is the most conservative tier, and a read-only action passes even there
	if out, isErr := w.toolbox.Execute("bash", map[string]any{"command": cmd}); isErr {
		t.Fatalf("it should have run: %s", out)
	}
	if n := len(bus.PendingApprovals()); n != 0 {
		t.Fatalf("这条不该问 / this must not ask, got %d approvals", n)
	}
}
