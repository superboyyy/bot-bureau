package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"botbureau/backend/internal/secret"
)

// 造一个像样的插件包：清单 + 一个 MCP server + 一个技能 + 一个同事模板 + 两块我们不消费的东西。
// Build a realistic bundle: a manifest, one MCP server, one skill, one teammate template, and two parts
// we do not consume.
func writeBundle(t *testing.T, dir string) {
	t.Helper()
	write := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".claude-plugin/plugin.json", `{
  "name": "acme",
  "description": "Acme's house toolkit",
  "version": "1.2.0",
  "author": {"name": "Acme Corp"},
  "mcpServers": {
    "notes": {"command": "node", "args": ["${CLAUDE_PLUGIN_ROOT}/server.js"], "env": {"ACME_TOKEN": "$ACME_TOKEN"}}
  }
}`)
	write("server.js", "// pretend server\n")
	write("skills/release/SKILL.md", "---\nname: release\ndescription: Cut a release the way Acme does it.\n---\n\nTag, build, announce.\n")
	write("agents/reviewer.md", "---\nname: Code Reviewer\ndescription: Reviews diffs for correctness.\n---\n\nYou are picky about off-by-one errors.\n")
	write("commands/deploy.md", "deploy command\n")
	write("hooks/hooks.json", "{}\n")
}

func newBundleManager(t *testing.T) (*BundleManager, *MCPManager, string) {
	t.Helper()
	dir := t.TempDir()
	ks := secret.NewKeyStore(filepath.Join(dir, "keys.json"))
	mcp := NewMCPManager(filepath.Join(dir, "mcp.yaml"), ks)
	return NewBundleManager(filepath.Join(dir, "plugins"), mcp), mcp, dir
}

func TestInstallBundleFromDirectory(t *testing.T) {
	bm, mcp, dir := newBundleManager(t)
	src := filepath.Join(dir, "src")
	writeBundle(t, src)

	b, err := bm.Install(src)
	if err != nil {
		t.Fatal(err)
	}
	if b.Name != "acme" || b.Version != "1.2.0" || b.Author != "Acme Corp" {
		t.Fatalf("manifest not read: %+v", b)
	}
	if len(b.Skills) != 1 || b.Skills[0] != "release" {
		t.Fatalf("skills not discovered: %+v", b.Skills)
	}
	if len(b.Agents) != 1 || b.Agents[0].Name != "code-reviewer" {
		t.Fatalf("agent name should be squeezed into the bot alphabet: %+v", b.Agents)
	}
	if !strings.Contains(b.Agents[0].Prompt, "off-by-one") {
		t.Fatalf("the agent body is its prompt: %+v", b.Agents[0])
	}
	// 不支持的部分必须报出来，不能装完了悄悄少一半
	// Unsupported parts must be reported rather than silently dropped
	if len(b.Ignored) != 2 {
		t.Fatalf("commands and hooks should both be reported as ignored: %+v", b.Ignored)
	}

	// MCP server 名加了包名前缀，且 ${CLAUDE_PLUGIN_ROOT} 展开成了真实路径
	// The MCP server name is scoped by the bundle, and ${CLAUDE_PLUGIN_ROOT} expanded to a real path
	if len(b.MCPServers) != 1 || b.MCPServers[0] != "acme_notes" {
		t.Fatalf("MCP server should be registered as acme_notes: %+v", b.MCPServers)
	}
	if !mcp.Has("acme_notes") {
		t.Fatal("the MCP manager should know about acme_notes")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "mcp.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), pluginRootVar) {
		t.Fatalf("${CLAUDE_PLUGIN_ROOT} must be expanded before it is persisted:\n%s", raw)
	}
	if !strings.Contains(string(raw), filepath.Join(b.Dir, "server.js")) {
		t.Fatalf("the expanded path should point inside the installed bundle:\n%s", raw)
	}

	// 技能根目录要交给技能库
	// The skill root has to be handed to the skill library
	roots := bm.SkillRoots()
	if len(roots) != 1 || roots[0].Source != "acme" {
		t.Fatalf("wrong skill roots: %+v", roots)
	}
}

func TestInstallRejectsDuplicateAndNonBundle(t *testing.T) {
	bm, _, dir := newBundleManager(t)
	src := filepath.Join(dir, "src")
	writeBundle(t, src)
	if _, err := bm.Install(src); err != nil {
		t.Fatal(err)
	}
	if _, err := bm.Install(src); err == nil {
		t.Fatal("installing the same plugin twice should fail")
	}
	plain := filepath.Join(dir, "plain")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := bm.Install(plain); err == nil {
		t.Fatal("a directory without plugin.json is not a bundle")
	}
	// 失败的安装不能留下垃圾目录 / a failed install must not leave debris behind
	entries, err := os.ReadDir(filepath.Join(dir, "plugins"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "acme" {
		t.Fatalf("only the one good install should remain: %+v", entries)
	}
}

// 卸载要把它带来的 MCP 插件一并摘掉，否则 mcp.yaml 里会留下指向已删目录的死条目。
// Uninstalling must detach the MCP plugins it brought, or mcp.yaml keeps dead entries pointing at a
// directory that no longer exists.
func TestRemoveBundleDetachesMCP(t *testing.T) {
	bm, mcp, dir := newBundleManager(t)
	src := filepath.Join(dir, "src")
	writeBundle(t, src)
	b, err := bm.Install(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := bm.Remove("acme"); err != nil {
		t.Fatal(err)
	}
	if mcp.Has("acme_notes") {
		t.Fatal("the MCP server should be gone with the plugin")
	}
	if _, err := os.Stat(b.Dir); !os.IsNotExist(err) {
		t.Fatal("the plugin directory should be deleted")
	}
	if len(bm.List()) != 0 || len(bm.SkillRoots()) != 0 {
		t.Fatal("nothing should be left listed")
	}
}

// 直接把插件目录拷进 data/plugins 也要认——文件系统是唯一事实，不额外维护索引。
// A bundle copied straight into data/plugins is recognised too: the filesystem is the only source of
// truth, with no separate index to fall out of step.
func TestRescanPicksUpHandPlacedBundle(t *testing.T) {
	bm, _, dir := newBundleManager(t)
	writeBundle(t, filepath.Join(dir, "plugins", "acme"))
	bm.Rescan()
	list := bm.List()
	if len(list) != 1 || list[0].Name != "acme" {
		t.Fatalf("a hand-placed bundle should be found: %+v", list)
	}
}

func TestReadMCPServersFromDotMcpJSON(t *testing.T) {
	bm, mcp, dir := newBundleManager(t)
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(filepath.Join(src, ".claude-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 清单里不写 mcpServers，改用根目录的 .mcp.json —— 生态里两种写法都有
	// No mcpServers in the manifest, a root .mcp.json instead — both spellings exist in the wild
	if err := os.WriteFile(filepath.Join(src, ".claude-plugin", "plugin.json"),
		[]byte(`{"name":"beta","description":"b"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, ".mcp.json"),
		[]byte(`{"mcpServers":{"remote":{"url":"https://mcp.example.com/mcp"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := bm.Install(src); err != nil {
		t.Fatal(err)
	}
	if !mcp.Has("beta_remote") {
		t.Fatalf("a server from .mcp.json should be registered: %+v", mcp.Names())
	}
}

// 升级必须保住用户在那个插件上做过的设置。卸了重装最省事，但那会把工具勾选和授权一起清掉——
// 挑过一遍工具的人升级一次就得重挑，这正是要避免的。
//
// An upgrade has to preserve what the user configured on that plugin. Remove-and-reinstall would be
// simpler but wipes the tool selection and the authorization along with it — anyone who narrowed the
// tool list once would have to do it again after every upgrade, which is exactly what this avoids.
func TestUpdateKeepsUserSettingsAndReconciles(t *testing.T) {
	bm, mcp, dir := newBundleManager(t)
	src := filepath.Join(dir, "src")
	writeBundle(t, src)
	if _, err := bm.Install(src); err != nil {
		t.Fatal(err)
	}
	// 用户挑了工具子集 / the user narrows the tools
	if err := mcp.SetTools("acme_notes", []string{"read_note"}); err != nil {
		t.Fatal(err)
	}

	// 上游发了新版本：加了一个 server，技能也多了一个
	// Upstream ships a new version: one more server, one more skill
	if err := os.WriteFile(filepath.Join(src, ".claude-plugin", "plugin.json"), []byte(`{
  "name": "acme",
  "description": "Acme's house toolkit",
  "version": "2.0.0",
  "mcpServers": {
    "notes": {"command": "node", "args": ["${CLAUDE_PLUGIN_ROOT}/server.js"]},
    "search": {"command": "node", "args": ["${CLAUDE_PLUGIN_ROOT}/search.js"]}
  }
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "skills", "changelog"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "skills", "changelog", "SKILL.md"),
		[]byte("---\nname: changelog\ndescription: Write the changelog.\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fresh, err := bm.Update("acme")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Version != "2.0.0" {
		t.Fatalf("the manifest should be re-read: %+v", fresh)
	}
	if len(fresh.Skills) != 2 {
		t.Fatalf("the new skill should be picked up: %+v", fresh.Skills)
	}
	// 新 server 加进来了 / the new server arrived
	if !mcp.Has("acme_search") {
		t.Fatalf("a newly shipped server should be registered: %v", mcp.Names())
	}
	// 老 server 的用户设置原样还在——这是这条测试的重点
	// The existing server's user settings survived — the whole point of this test
	if got := mcp.Tools("acme_notes"); len(got) != 0 {
		// 这个假 server 连不上，所以工具列表是空的；关键看配置里的勾选没被抹掉
		// The fake server never connects so the tool list is empty; what matters is the selection in config
		_ = got
	}
	raw, err := os.ReadFile(filepath.Join(dir, "mcp.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "read_note") {
		t.Fatalf("the user's tool selection must survive an upgrade:\n%s", raw)
	}
}

// 上游删掉的 server 要跟着消失，否则 mcp.yaml 里会留下指向已不存在脚本的死条目。
// A server dropped upstream must disappear too, or mcp.yaml keeps a dead entry pointing at a script that
// is no longer there.
func TestUpdateRemovesVanishedServers(t *testing.T) {
	bm, mcp, dir := newBundleManager(t)
	src := filepath.Join(dir, "src")
	writeBundle(t, src)
	if _, err := bm.Install(src); err != nil {
		t.Fatal(err)
	}
	if !mcp.Has("acme_notes") {
		t.Fatal("setup: acme_notes should exist")
	}
	if err := os.WriteFile(filepath.Join(src, ".claude-plugin", "plugin.json"),
		[]byte(`{"name":"acme","description":"d","version":"3.0.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := bm.Update("acme"); err != nil {
		t.Fatal(err)
	}
	if mcp.Has("acme_notes") {
		t.Fatal("a server the new version no longer ships should be gone")
	}
}
