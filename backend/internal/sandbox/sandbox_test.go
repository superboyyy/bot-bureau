package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGitDirsSkipsWorktreeFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, ".git"), []byte("gitdir: /somewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := GitDirs([]string{dir, work, "", dir})
	if len(got) != 1 || got[0] != filepath.Join(dir, ".git") {
		t.Fatalf("GitDirs = %#v", got)
	}
}

func TestSeatbeltProfileDeniesGitAndNetwork(t *testing.T) {
	ws := "/Users/aiden/proj"
	p := seatbeltProfile(Spec{
		Dir:      ws,
		Writable: []string{ws},
		ReadOnly: []string{ws + "/.git"},
		TmpDir:   "/tmp/sbx",
		Command:  "true",
	})
	for _, needle := range []string{
		`(allow file-write* (subpath "` + ws + `"))`,
		`(deny file-write* (subpath "` + ws + `/.git"))`,
		`(deny network*)`,
		`(allow file-read*)`,
	} {
		if !strings.Contains(p, needle) {
			t.Fatalf("profile missing %s\n%s", needle, p)
		}
	}
}

func TestBwrapArgsBindThenRoBindGit(t *testing.T) {
	ws := "/home/aiden/proj"
	git := ws + "/.git"
	args := bwrapArgs(Spec{Dir: ws, Writable: []string{ws}, ReadOnly: []string{git}, TmpDir: "/tmp/sbx", Command: "echo hi"})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--unshare-net") || !strings.Contains(joined, "--bind "+ws+" "+ws) {
		t.Fatalf("bwrap args: %v", args)
	}
	bindAt, roAt := -1, -1
	for i := 0; i < len(args)-2; i++ {
		if args[i] == "--bind" && args[i+1] == ws {
			bindAt = i
		}
		if args[i] == "--ro-bind" && args[i+1] == git {
			roAt = i
		}
	}
	if bindAt < 0 || roAt < 0 || roAt < bindAt {
		t.Fatalf("writable bind must precede .git ro-bind: %v", args)
	}
}

func TestPassthroughIsNotAvailable(t *testing.T) {
	r := Passthrough()
	if r.Available() || r.IsolatesNetwork() || r.Name() != "none" {
		t.Fatalf("%s available=%v net=%v", r.Name(), r.Available(), r.IsolatesNetwork())
	}
}

func TestOSEnvironWithTmpRewrites(t *testing.T) {
	t.Setenv("TMPDIR", "/old")
	t.Setenv("KEEP", "1")
	got := osEnvironWithTmp("/new")
	var tmp, keep string
	for _, e := range got {
		if strings.HasPrefix(e, "TMPDIR=") {
			tmp = e
		}
		if e == "KEEP=1" {
			keep = e
		}
	}
	if tmp != "TMPDIR=/new" || keep != "KEEP=1" {
		t.Fatalf("env rewrite tmp=%q keep=%q", tmp, keep)
	}
}

func TestOSSandboxWriteBoundary(t *testing.T) {
	r := Detect()
	if !r.Available() {
		t.Skip("no OS sandbox backend: " + r.Name())
	}
	ws := t.TempDir()
	tmp := t.TempDir()
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "pwned.txt")
	inside := filepath.Join(ws, "ok.txt")

	run := func(command string) (string, error) {
		cmd, err := r.Command(context.Background(), Spec{
			Command: command, Dir: ws, Writable: []string{ws}, TmpDir: tmp,
		})
		if err != nil {
			return "", err
		}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	if _, err := run("echo ok > ok.txt"); err != nil {
		t.Fatalf("write inside workspace: %v", err)
	}
	if _, err := os.Stat(inside); err != nil {
		t.Fatal("workspace file missing")
	}
	out, err := run("echo pwned > " + outside)
	if err == nil {
		t.Fatalf("write outside should fail, output %q", out)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatal("sandbox leaked a write outside the workspace")
	}
}

func TestOSSandboxNetwork(t *testing.T) {
	r := Detect()
	if !r.Available() || !r.IsolatesNetwork() {
		t.Skip("backend does not isolate network")
	}
	ws := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	cmd, err := r.Command(ctx, Spec{
		Command: `python3 -c "import socket; socket.create_connection(('1.1.1.1', 443), 2)"`,
		Dir:     ws, Writable: []string{ws}, TmpDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Run(); err == nil {
		t.Fatal("outbound TCP should fail inside the sandbox")
	}
}
