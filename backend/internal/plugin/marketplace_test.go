package plugin

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 造一个市场仓库：根上只有 marketplace.json，插件在子目录里。
// 抽样的真实仓库里约一半是这个形状。
//
// Build a marketplace repository: only marketplace.json at the root, the plugins in subdirectories.
// About half of the sampled real repositories look exactly like this.
func writeMarketplace(t *testing.T, dir string) {
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
	write(".claude-plugin/marketplace.json", `{
  "name": "acme-market",
  "owner": {"name": "Acme"},
  "plugins": [
    {"name": "alpha", "source": "./plugins/alpha", "description": "The alpha plugin"},
    {"name": "beta",  "source": "./plugins/beta",  "description": "No manifest of its own"}
  ]
}`)
	// alpha 自带 plugin.json / alpha ships its own plugin.json
	write("plugins/alpha/.claude-plugin/plugin.json", `{"name":"alpha","description":"a","version":"1.0.0"}`)
	write("plugins/alpha/skills/deploy/SKILL.md", "---\nname: deploy\ndescription: Deploy it.\n---\nbody\n")
	// beta 没有 plugin.json，只有技能——实测里 source 指向仓库根的那类就是这样
	// beta has no plugin.json, only skills — which is what an entry pointing at the repository root looks like
	write("plugins/beta/skills/lint/SKILL.md", "---\nname: lint\ndescription: Lint it.\n---\nbody\n")
}

func TestInstallDetectsMarketplace(t *testing.T) {
	bm, _, dir := newBundleManager(t)
	src := filepath.Join(dir, "market")
	writeMarketplace(t, src)

	_, err := bm.Install(src)
	var mk *MarketplaceError
	if !errors.As(err, &mk) {
		t.Fatalf("a marketplace should be reported as a choice, not a failure: %v", err)
	}
	if mk.Marketplace != "acme-market" || len(mk.Entries) != 2 {
		t.Fatalf("wrong listing: %+v", mk)
	}
	if mk.Entries[0].Name != "alpha" || mk.Entries[0].Description == "" {
		t.Fatalf("entries should carry name and description: %+v", mk.Entries)
	}
	// 什么都不该被装上 / nothing should have been installed
	if len(bm.List()) != 0 {
		t.Fatal("detecting a marketplace must not install anything")
	}
}

func TestInstallFromMarketplace(t *testing.T) {
	bm, _, dir := newBundleManager(t)
	src := filepath.Join(dir, "market")
	writeMarketplace(t, src)

	b, err := bm.InstallFromMarketplace(src, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if b.Name != "alpha" || b.Version != "1.0.0" {
		t.Fatalf("its own plugin.json should win: %+v", b)
	}
	if len(b.Skills) != 1 || b.Skills[0] != "deploy" {
		t.Fatalf("only its own subdirectory should be installed: %+v", b.Skills)
	}
	// 装进来的是那个子目录，不是整个仓库
	// What landed is that subdirectory, not the whole repository
	if _, err := os.Stat(filepath.Join(b.Dir, ".claude-plugin", "marketplace.json")); err == nil {
		t.Fatal("the marketplace manifest should not come along")
	}
	if b.Marketplace != "alpha" {
		t.Fatalf("it should remember which entry it came from: %+v", b)
	}
}

// 条目指向的目录没有 plugin.json 时，用清单里那条的元数据兜底——
// 实测有市场把 source 指向仓库根，而根上只有 marketplace.json。挑剔的话这类一个也装不上。
//
// When the entry's directory has no plugin.json, the listing's own metadata stands in — marketplaces do
// point source at the repository root, where only marketplace.json lives. Being strict rejects all of them.
func TestInstallFromMarketplaceWithoutOwnManifest(t *testing.T) {
	bm, _, dir := newBundleManager(t)
	src := filepath.Join(dir, "market")
	writeMarketplace(t, src)

	b, err := bm.InstallFromMarketplace(src, "beta")
	if err != nil {
		t.Fatalf("an entry without its own manifest should still install: %v", err)
	}
	if b.Name != "beta" || b.Description != "No manifest of its own" {
		t.Fatalf("metadata should fall back to the listing: %+v", b)
	}
	if len(b.Skills) != 1 || b.Skills[0] != "lint" {
		t.Fatalf("its contents should still be scanned: %+v", b.Skills)
	}
}

// 清单是别人写的文件，source 里的 ../ 不能把安装目标指到仓库外面去。
// The listing is someone else's file, and a ../ in source must not aim the install outside the repository.
func TestMarketplaceSourceCannotEscape(t *testing.T) {
	bm, _, dir := newBundleManager(t)
	src := filepath.Join(dir, "market")
	writeMarketplace(t, src)
	if err := os.WriteFile(filepath.Join(src, ".claude-plugin", "marketplace.json"),
		[]byte(`{"name":"evil","plugins":[{"name":"esc","source":"../../../etc"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := bm.InstallFromMarketplace(src, "esc")
	if err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("a path escaping the repository must be refused, got %v", err)
	}
}

// 市场装的插件也要能升级：它不是 git 检出，得照着清单那条重新取一遍。
// A marketplace-installed plugin must upgrade too: it is no git checkout, so it is re-fetched by entry.
func TestUpdateMarketplacePlugin(t *testing.T) {
	bm, _, dir := newBundleManager(t)
	src := filepath.Join(dir, "market")
	writeMarketplace(t, src)
	if _, err := bm.InstallFromMarketplace(src, "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "plugins", "alpha", ".claude-plugin", "plugin.json"),
		[]byte(`{"name":"alpha","description":"a","version":"2.0.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	fresh, err := bm.Update("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Version != "2.0.0" {
		t.Fatalf("expected v2.0.0 after re-fetch, got %q", fresh.Version)
	}
	if fresh.Marketplace != "alpha" {
		t.Fatalf("the marketplace marker should survive: %+v", fresh)
	}
}
