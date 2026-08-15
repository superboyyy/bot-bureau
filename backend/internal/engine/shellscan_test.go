package engine

import (
	"botbureau/backend/internal/config"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 判读一条命令：拆段、认引号、决定只读与否。
// 用例里那两条长命令是真实截图里的原文——一条 grep 管道本不该问，一条 curl 下载理应问。
//
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
		// 管道不是副作用。之前光凭这一根竖线，整条命令就得人点头。
		// A pipe is not a side effect. That one bar used to be enough to require a human.
		{"pipeline", "grep -n foo main.go | head -20", true},
		{"sequence", "ls; pwd; wc -l main.go", true},
		{"and-list", "ls && pwd", true},
		// 引号里的竖线是正则，不是管道；切错了这条常见命令就永远要问
		// The bar inside quotes is a regex, not a pipe; splitting on it would make this very ordinary
		// command ask forever
		{"regex alternation stays one segment",
			`grep -oE 'MJ[0-9A-Z]{4,}/A|"priceAmount":[0-9.]+|"fullPrice":[^,}]+' page.html | head -80`, true},
		{"quoted semicolon", `grep -n 'a; rm -rf /' notes.txt`, true},
		{"git read-only subcommand", "git log --oneline | head -5", true},
		{"sed without -i", "sed -n '1,20p' main.go", true},

		// 以下都必须落回"要问"
		// Everything below has to fall back to asking
		{"redirect writes", "grep foo main.go > out.txt", false},
		{"append writes", "echo hi >> notes.txt", false},
		{"git mutating subcommand", "git commit -m x", false},
		{"sed -i rewrites in place", "sed -i '' s/a/b/ main.go", false},
		// 首词只读救不了命令替换：它要碰什么，跑之前根本不知道
		// A read-only first word cannot rescue command substitution: what it touches is unknown until it runs
		{"command substitution", "ls $(cat target)", false},
		{"backticks", "cat `cat target`", false},
		{"one bad segment poisons the rest", "ls | rm -rf x", false},
		{"unknown command", "python3 script.py", false},
		{"find is never safe", "find . -name '*.go' -delete", false},
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

// 端到端：截图里那两条命令，在 auto 档下现在各自该是什么下场。
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

	// grep 管道翻 /tmp 里一个自己刚下下来的文件：纯读，落在草稿目录里，不该问
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
	// 真跑一遍：一条审批都不该攒出来
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

	// curl 下载：既发网络请求又写文件，首词不在白名单，照旧要问——这条问得对
	// The curl download makes a network request and writes a file, and its first word is not on the
	// whitelist: it still asks, and rightly so
	curlCmd := `curl -sL --max-time 30 "https://www.apple.com.cn/shop/buy-mac" -o /tmp/mbp.html; wc -c /tmp/mbp.html`
	segs, subst, ok = scanBash(curlCmd)
	if bashReadOnly(segs, subst, ok) {
		t.Fatal("curl 会写文件、会联网，不是只读 / curl writes a file and reaches the network; it is not read-only")
	}

	// /tmp 进界内，不等于机器上别处也进来了
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
