package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSkill(t *testing.T, root, dir, content string) string {
	t.Helper()
	p := filepath.Join(root, dir)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestScanAndRead(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "code-review", `---
name: code-review
description: Review a diff for correctness bugs before it is merged.
---

# Code review

1. Read the diff.
2. Look for off-by-one errors.
`)
	// frontmatter 缺 name：应当退回用目录名，而不是整个跳过
	// Missing name in the frontmatter: fall back to the directory name rather than skipping the skill
	writeSkill(t, root, "release-notes", "---\ndescription: Turn merged PRs into release notes.\n---\n\nBody here.\n")
	// 没有 description 就没法被模型选中，属于坏技能，要跳过
	// Without a description the model cannot select it, so it is a bad skill and gets skipped
	writeSkill(t, root, "broken", "---\nname: broken\n---\n\nNo description.\n")
	// 不是技能目录（没有 SKILL.md），静默忽略
	// Not a skill directory (no SKILL.md): ignored quietly
	if err := os.MkdirAll(filepath.Join(root, "not-a-skill"), 0o755); err != nil {
		t.Fatal(err)
	}

	m := NewManager(root)
	names := m.Names()
	if len(names) != 2 || names[0] != "code-review" || names[1] != "release-notes" {
		t.Fatalf("wrong skill list: %v", names)
	}

	s, ok := m.Get("code-review")
	if !ok {
		t.Fatal("code-review not found")
	}
	if s.Description != "Review a diff for correctness bugs before it is merged." {
		t.Fatalf("wrong description: %q", s.Description)
	}
	if !strings.HasPrefix(s.Body, "# Code review") || strings.Contains(s.Body, "description:") {
		t.Fatalf("body should start after the frontmatter: %q", s.Body)
	}

	roster := m.Roster()
	if !strings.Contains(roster, "- code-review: Review a diff") || strings.Contains(roster, "# Code review") {
		t.Fatalf("the roster carries name + description only: %q", roster)
	}
}

// 正文里的 --- 分割线不能被当成 frontmatter 结束标记，否则技能会被腰斩，
// 而且是静默的：前半段照常返回，谁也看不出丢了东西。
// A --- rule inside the body must not be taken for the frontmatter's closing fence: the skill would be
// cut in half silently, the first part coming back as normal with nothing to show something was lost.
func TestFrontmatterStopsAtFirstFenceOnly(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "rules", `---
name: rules
description: House rules.
---

Step one.

---

Step two, after a horizontal rule.
`)
	m := NewManager(root)
	s, ok := m.Get("rules")
	if !ok {
		t.Fatal("rules not found")
	}
	if !strings.Contains(s.Body, "Step one.") || !strings.Contains(s.Body, "Step two") {
		t.Fatalf("the body must survive a --- rule intact: %q", s.Body)
	}
}

func TestNoFrontmatterIsSkipped(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "plain", "Just a note with no frontmatter at all.\n")
	if names := NewManager(root).Names(); len(names) != 0 {
		t.Fatalf("a SKILL.md without a description cannot be selected, so it should be skipped: %v", names)
	}
}

// 插件带来的技能和本地技能同名时：先到先得，且两边都还在各自目录里。
// When a plugin's skill collides with a local one: first wins, and both remain in their own directories.
func TestRootsAndCollision(t *testing.T) {
	local := t.TempDir()
	fromPlugin := t.TempDir()
	writeSkill(t, local, "deploy", "---\nname: deploy\ndescription: Local deploy steps.\n---\nlocal body\n")
	writeSkill(t, fromPlugin, "deploy", "---\nname: deploy\ndescription: Plugin deploy steps.\n---\nplugin body\n")
	writeSkill(t, fromPlugin, "lint", "---\nname: lint\ndescription: Run the linters.\n---\nlint body\n")

	m := NewManager(local)
	m.SetRoots([]Root{{Source: "acme", Dir: fromPlugin}})

	names := m.Names()
	if len(names) != 2 {
		t.Fatalf("expected deploy + lint, got %v", names)
	}
	s, _ := m.Get("deploy")
	if s.Source != "local" || !strings.Contains(s.Body, "local body") {
		t.Fatalf("the local skill should win the name: %+v", s)
	}
	if s, _ := m.Get("lint"); s.Source != "acme" {
		t.Fatalf("lint should be attributed to the plugin: %+v", s)
	}
}

// 技能目录里的附带文件要列进 read_skill 的返回值——模型得知道手边有什么脚本可用。
// Files bundled beside SKILL.md must appear in what read_skill returns: the model has to know which
// scripts are at hand.
func TestRenderListsBundledFiles(t *testing.T) {
	root := t.TempDir()
	dir := writeSkill(t, root, "report", "---\nname: report\ndescription: Build the weekly report.\n---\nRun the script.\n")
	if err := os.WriteFile(filepath.Join(dir, "build.py"), []byte("print('hi')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := NewManager(root)
	s, _ := m.Get("report")
	out := s.Render()
	if !strings.Contains(out, "Run the script.") || !strings.Contains(out, "build.py") || !strings.Contains(out, dir) {
		t.Fatalf("render should carry the body, the bundled file and its location: %q", out)
	}
}
