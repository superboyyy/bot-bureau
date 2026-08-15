package plugin

// 插件包（bundle）：把 Claude / Codex 那套 .claude-plugin/plugin.json 直接当成 Bot Bureau 的包格式。
//
// 为什么不自己定一套：自造格式意味着要从零招开发者；沿用既有格式意味着开发者写一次，
// Claude Code、Codex、Bot Bureau 都装得上——第一天就有存量生态。所以这里的工作不是"定义协议"，
// 而是"明确消费哪些字段"，并且把不消费的如实报出来（Ignored），让人一眼看见差在哪，
// 而不是装完发现有一半功能悄悄没生效。
//
// 映射关系：
//	mcpServers / .mcp.json  → MCP 插件（引擎已有的那一层）
//	skills/                 → 技能库（skill 包）
//	agents/                 → 团队成员模板（Bot Bureau 独有的红利：别人只能降级成子代理）
//	commands/ hooks/        → 暂不支持，如实列进 Ignored
//
// Plugin bundles: the .claude-plugin/plugin.json format from Claude / Codex, adopted as-is as Bot
// Bureau's package format.
//
// Why not invent one: a bespoke format means recruiting developers from zero, while an existing one
// means a developer writes a plugin once and Claude Code, Codex and Bot Bureau can all install it —
// an ecosystem on day one. So the job here is not "define a protocol" but "state which fields are
// consumed", and to report the ones that are not (Ignored) so the gap is visible immediately rather
// than discovered as half the plugin silently doing nothing.
//
// The mapping:
//	mcpServers / .mcp.json  → MCP plugins (the layer the engine already has)
//	skills/                 → the skill library (the skill package)
//	agents/                 → teammate templates (Bot Bureau's own dividend: elsewhere these degrade
//	                          into subagents)
//	commands/ hooks/        → not supported yet, reported honestly in Ignored

import (
	"botbureau/backend/internal/i18n"

	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// 插件包里引用自身路径用的占位符，Claude 生态的既有约定，必须支持——
// 不少插件的 mcpServers 就是靠它指向自己带的脚本。
// The placeholder a bundle uses to refer to its own location. It is an established convention in the
// Claude ecosystem and has to be supported: plenty of plugins point mcpServers at their own bundled
// scripts through it.
const pluginRootVar = "${CLAUDE_PLUGIN_ROOT}"

const gitCloneTimeout = 180 * time.Second

// Bundle 是一个已安装的插件包。
// Bundle is one installed plugin bundle.
type Bundle struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Version     string `json:"version,omitempty"`
	Author      string `json:"author,omitempty"`
	Dir         string `json:"dir"`
	Source      string `json:"source,omitempty"`
	// 非空表示它是从一个市场清单里挑出来装的，值是清单里那一条的名字。
	// 升级要照着这条重新取，因为装进来的是仓库的一个子目录，不是 git 检出。
	// Non-empty when it was picked out of a marketplace listing, holding that entry's name. An upgrade
	// must fetch by the entry again, since what is installed is a subdirectory rather than a git checkout.
	Marketplace string   `json:"marketplace,omitempty"`
	MCPServers  []string `json:"mcp_servers,omitempty"`
	SkillDir    string   `json:"-"`
	Skills      []string `json:"skills,omitempty"`
	Agents      []Agent  `json:"agents,omitempty"`
	// 这个包里存在、但 Bot Bureau 不消费的部分。列出来是刻意的：装完一言不发地少一半功能，
	// 比直接说"hooks 不支持"糟糕得多。
	// Parts present in this bundle that Bot Bureau does not consume. Listing them is deliberate:
	// silently dropping half a plugin is far worse than saying "hooks are not supported".
	Ignored []string `json:"ignored,omitempty"`
}

// Agent 是包里带的一个「同事模板」：Bot Bureau 可以据此建一个真正的团队成员。
// Agent is a "teammate template" shipped in a bundle, from which Bot Bureau can create a real member.
type Agent struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// 正文就是这个角色的系统提示词
	// The body is this role's system prompt
	Prompt string `json:"prompt"`
	Source string `json:"source"`
}

// manifest 是 .claude-plugin/plugin.json 里我们会读的字段。
// 其余字段一概忽略而不是报错——别人的清单里出现我们不认识的键是常态，
// 因此挑剔会让一堆现成插件装不上。
//
// manifest holds the fields we read out of .claude-plugin/plugin.json. Everything else is ignored
// rather than rejected: unknown keys are normal in someone else's manifest, and being fussy would make
// a pile of existing plugins refuse to install.
type manifest struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Version     string          `json:"version"`
	Author      json.RawMessage `json:"author"`
	MCPServers  json.RawMessage `json:"mcpServers"`
}

// authorName 兼容 author 写成字符串和写成对象两种形式。
// authorName copes with author written either as a string or as an object.
func authorName(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return obj.Name
	}
	return ""
}

type BundleManager struct {
	root string // data/plugins
	mcp  *MCPManager
	mu   sync.Mutex
	list map[string]*Bundle
	// 装卸插件后通知引擎重扫技能、刷新界面
	// Notifies the engine to rescan skills and refresh the UI after an install or uninstall
	onChange func()
}

func NewBundleManager(root string, mcp *MCPManager) *BundleManager {
	m := &BundleManager{root: root, mcp: mcp, list: map[string]*Bundle{}}
	m.Rescan()
	return m
}

func (m *BundleManager) SetOnChange(fn func()) {
	m.mu.Lock()
	m.onChange = fn
	m.mu.Unlock()
}

func (m *BundleManager) fireChange() {
	m.mu.Lock()
	fn := m.onChange
	m.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// Rescan 重扫已装目录。直接以文件系统为准（而不是另存一份索引），
// 于是用户手动把一个插件目录拷进来也能被认出来，索引和现实也不会对不上。
//
// Rescan re-reads the install directory. The filesystem is the source of truth rather than a separate
// index, so a bundle copied in by hand is recognised too, and an index can never disagree with reality.
func (m *BundleManager) Rescan() {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return
	}
	found := map[string]*Bundle{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(m.root, e.Name())
		b, err := readBundle(dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, i18n.T("Skipping plugin directory %s: %v\n"), e.Name(), err)
			continue
		}
		if raw, err := os.ReadFile(filepath.Join(dir, ".origin")); err == nil {
			b.Source = strings.TrimSpace(string(raw))
		}
		if raw, err := os.ReadFile(filepath.Join(dir, ".marketplace")); err == nil {
			b.Marketplace = strings.TrimSpace(string(raw))
		}
		found[b.Name] = b
	}
	m.mu.Lock()
	m.list = found
	m.mu.Unlock()
}

// ---- 市场清单 ----
//
// 生态里插件的分发单位常常不是「一个仓库一个插件」，而是「一个仓库一个市场，里面列着若干插件」——
// 那种仓库根目录放的是 .claude-plugin/marketplace.json 而不是 plugin.json。抽样二十个真实仓库，
// 约一半属于后者，只认 plugin.json 的话它们全部会以「找不到 plugin.json」告终，看上去像是我们不兼容。
//
// ---- marketplace manifests ----
//
// Plugins in the wild are often distributed not as "one repository, one plugin" but as "one repository,
// one marketplace listing several plugins" — those repositories carry .claude-plugin/marketplace.json at
// the root instead of plugin.json. Across a sample of twenty real repositories about half are of that
// kind, and recognising only plugin.json ends every one of them at "no plugin.json found", which reads as
// an incompatibility on our side.

type MarketplaceEntry struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// 仓库内的相对路径（"./"、"./plugins/foo"）。实测样本里全是这种形态。
	// A path relative to the repository ("./", "./plugins/foo"), which is what every sampled entry uses.
	Source string `json:"source,omitempty"`
}

type marketplaceFile struct {
	Name    string             `json:"name"`
	Plugins []MarketplaceEntry `json:"plugins"`
}

// MarketplaceError 不是失败，是「这个地址给的是一份清单，请先挑一个」。
// 做成 error 类型是为了让 Install 的签名保持单一返回值，同时让上层能用 errors.As 区分出来。
//
// MarketplaceError is not a failure but "this address gave a listing; pick one first".
// It is an error type so that Install keeps a single return value while the layer above can still tell
// the case apart with errors.As.
type MarketplaceError struct {
	Marketplace string
	Entries     []MarketplaceEntry
}

func (e *MarketplaceError) Error() string {
	return fmt.Sprintf(i18n.T("%s is a plugin marketplace listing %d plugins; choose one to install"),
		e.Marketplace, len(e.Entries))
}

func readMarketplace(dir string) (*marketplaceFile, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, ".claude-plugin", "marketplace.json"))
	if err != nil {
		return nil, false
	}
	var mk marketplaceFile
	if json.Unmarshal(raw, &mk) != nil || len(mk.Plugins) == 0 {
		return nil, false
	}
	if mk.Name == "" {
		mk.Name = filepath.Base(dir)
	}
	return &mk, true
}

// readBundle 读一个插件包目录，不做任何注册动作。
// readBundle reads a bundle directory without registering anything.
func readBundle(dir string) (*Bundle, error) {
	raw, err := os.ReadFile(filepath.Join(dir, ".claude-plugin", "plugin.json"))
	if err != nil {
		return nil, errors.New(i18n.T("no .claude-plugin/plugin.json found"))
	}
	var mf manifest
	if err := json.Unmarshal(raw, &mf); err != nil {
		return nil, fmt.Errorf(i18n.T("plugin.json is not valid JSON: %w"), err)
	}
	return scanBundle(dir, mf.Name, mf.Description, mf.Version, authorName(mf.Author))
}

// scanBundle 按目录内容拼出一个 Bundle。清单里的元数据由调用方给，
// 因为它可能来自 plugin.json，也可能来自市场清单里的那一条（有些插件目录压根没有 plugin.json，
// 比如 source 指向仓库根的那种）。
//
// scanBundle assembles a Bundle from a directory's contents. The manifest metadata is supplied by the
// caller because it may come from plugin.json or from the marketplace entry instead — some plugin
// directories carry no plugin.json at all, such as those whose source points at the repository root.
func scanBundle(dir, name, description, version, author string) (*Bundle, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = filepath.Base(dir)
	}
	name = sanitizeBotName(name)
	if !pluginNameRe.MatchString(name) {
		return nil, fmt.Errorf(i18n.T("plugin name %q must be 1-24 characters of lowercase letters, digits, - or _"), name)
	}
	b := &Bundle{
		Name: name, Description: description, Version: version,
		Author: author, Dir: dir,
	}

	if sub := filepath.Join(dir, "skills"); isDir(sub) {
		b.SkillDir = sub
		if entries, err := os.ReadDir(sub); err == nil {
			for _, e := range entries {
				if e.IsDir() {
					b.Skills = append(b.Skills, e.Name())
				}
			}
		}
	}
	b.Agents = readAgents(filepath.Join(dir, "agents"), b.Name)

	// 不消费的部分如实登记 / record what is not consumed
	if isDir(filepath.Join(dir, "commands")) {
		b.Ignored = append(b.Ignored, i18n.T("commands (Bot Bureau has no slash commands; ask the bot in chat instead)"))
	}
	if isDir(filepath.Join(dir, "hooks")) || fileExists(filepath.Join(dir, "hooks", "hooks.json")) {
		b.Ignored = append(b.Ignored, i18n.T("hooks (no hook system; the permission tiers cover the safety side)"))
	}
	return b, nil
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// readAgents 解析 agents/*.md：YAML frontmatter 给名字和描述，正文是系统提示词。
// readAgents parses agents/*.md: YAML frontmatter supplies the name and description, the body is the
// system prompt.
func readAgents(dir, source string) []Agent {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Agent
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		meta, body := splitFrontmatter(string(raw))
		var fm struct {
			Name        string `yaml:"name"`
			Description string `yaml:"description"`
		}
		if meta != "" {
			_ = yaml.Unmarshal([]byte(meta), &fm)
		}
		name := strings.TrimSpace(fm.Name)
		if name == "" {
			name = strings.TrimSuffix(e.Name(), ".md")
		}
		out = append(out, Agent{
			Name:        sanitizeBotName(name),
			Description: strings.TrimSpace(fm.Description),
			Prompt:      strings.TrimSpace(body),
			Source:      source,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// sanitizeBotName 把别处的代理名压进 bot 名的字母表（小写、限长 24）。
// sanitizeBotName squeezes an agent name from elsewhere into the bot-name alphabet (lowercase, ≤24).
func sanitizeBotName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ' || r == '.':
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-_")
	if len(out) > 24 {
		out = out[:24]
	}
	return out
}

// splitFrontmatter 与 skill 包里那份同义；此处重复一小段而不是反向依赖 skill 包，
// 是为了不让 plugin → skill 产生依赖（skill 是更上层的概念，engine 才是它们的汇合点）。
//
// splitFrontmatter mirrors the one in the skill package. The few lines are repeated rather than
// depended upon so that plugin does not import skill: skills sit at a higher level, and engine is where
// the two meet.
func splitFrontmatter(text string) (meta, body string) {
	trimmed := strings.TrimLeft(text, "\ufeff \t\r\n")
	if !strings.HasPrefix(trimmed, "---") {
		return "", text
	}
	rest := strings.TrimLeft(strings.TrimPrefix(trimmed, "---"), "\r\n")
	for _, sep := range []string{"\n---\n", "\n---\r\n"} {
		if i := strings.Index(rest, sep); i >= 0 {
			return rest[:i], rest[i+len(sep):]
		}
	}
	return "", text
}

// mcpServersOf 从清单的 mcpServers 字段或根目录的 .mcp.json 里取 MCP 定义。
// 两种写法生态里都有，都认。
// mcpServersOf pulls MCP definitions from the manifest's mcpServers field or from .mcp.json at the
// bundle root. Both spellings exist in the wild and both are accepted.
func mcpServersOf(dir string, mf json.RawMessage) map[string]rawMCPEntry {
	out := map[string]rawMCPEntry{}
	parse := func(raw json.RawMessage) {
		var m map[string]rawMCPEntry
		if json.Unmarshal(raw, &m) != nil {
			return
		}
		for k, v := range m {
			out[k] = v
		}
	}
	if len(mf) > 0 {
		parse(mf)
	}
	if raw, err := os.ReadFile(filepath.Join(dir, ".mcp.json")); err == nil {
		var f struct {
			MCPServers json.RawMessage `json:"mcpServers"`
		}
		if json.Unmarshal(raw, &f) == nil && len(f.MCPServers) > 0 {
			parse(f.MCPServers)
		}
	}
	return out
}

type rawMCPEntry struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	URL     string            `json:"url"`
	Type    string            `json:"type"`
}

// fetchInto 把一个来源（本地目录或 git 地址）取到指定目录里。
//
// git 判断放在目录判断之前：一个本地的裸仓库路径（/srv/plugins/foo.git）**同时**是一个目录，
// 按目录拷贝会把 HEAD/objects/refs 原样搬过来，然后报"找不到 plugin.json"——错误信息完全指不到
// 真正的原因。而 looksLikeGit 只匹配 http/https/git@/.git 结尾，普通的插件文件夹不会长这样，
// 所以这个顺序不会误伤本地目录安装。
//
// fetchInto brings a source (a local directory or a git address) into the given directory.
//
// The git check comes before the directory check: a local bare repository path (/srv/plugins/foo.git) is
// *also* a directory, and copying it as one moves HEAD/objects/refs across verbatim and then reports "no
// plugin.json found" — an error pointing nowhere near the cause. looksLikeGit only matches http/https/git@
// or a .git suffix, none of which an ordinary plugin folder looks like, so this order cannot misfire on a
// local directory install.
func fetchInto(source, dest string) error {
	if looksLikeGit(source) {
		return gitClone(source, dest)
	}
	if isDir(source) {
		return copyTree(source, dest)
	}
	return fmt.Errorf(i18n.T("%s is neither an existing directory nor a git URL"), source)
}

// Install 从本地目录或 git 地址装一个插件包。
// Install installs a bundle from a local directory or a git URL.
func (m *BundleManager) Install(source string) (*Bundle, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return nil, errors.New(i18n.T("Enter a local directory or a git URL"))
	}
	if err := os.MkdirAll(m.root, 0o755); err != nil {
		return nil, err
	}

	staging, err := os.MkdirTemp(m.root, ".staging-")
	if err != nil {
		return nil, err
	}
	// 先装进临时目录，验完再挪到正式位置：半个插件留在目录里比装不上更难查
	// Stage first and move into place only once it validates: half a plugin left in the directory is
	// harder to diagnose than an install that simply failed
	defer os.RemoveAll(staging)

	src := staging
	if err := fetchInto(source, staging); err != nil {
		return nil, err
	}

	// 根目录没有 plugin.json，但有市场清单：把清单交回上层让用户挑一个，而不是报「找不到 plugin.json」
	// No plugin.json at the root but a marketplace listing: hand the listing back so the user can pick,
	// rather than reporting "no plugin.json found"
	b, err := readBundle(src)
	if err != nil {
		if mk, ok := readMarketplace(src); ok {
			return nil, &MarketplaceError{Marketplace: mk.Name, Entries: mk.Plugins}
		}
		return nil, err
	}
	m.mu.Lock()
	_, dup := m.list[b.Name]
	m.mu.Unlock()
	if dup {
		return nil, fmt.Errorf(i18n.T("A plugin named %s is already installed"), b.Name)
	}
	dest := filepath.Join(m.root, b.Name)
	if _, err := os.Stat(dest); err == nil {
		return nil, fmt.Errorf(i18n.T("%s already exists"), dest)
	}
	if err := os.Rename(src, dest); err != nil {
		return nil, err
	}
	_ = os.WriteFile(filepath.Join(dest, ".origin"), []byte(source), 0o644)

	b, err = readBundle(dest)
	if err != nil {
		return nil, err
	}
	b.Source = source
	b.MCPServers = m.registerMCP(b, dest)

	m.mu.Lock()
	m.list[b.Name] = b
	m.mu.Unlock()
	m.fireChange()
	return b, nil
}

// registerMCP 把包里的 MCP 定义注册进插件管理器，名字加包名前缀避免撞车。
// 单个 server 连不上不算安装失败——插件的其它部分（技能、代理）照样有用，
// 用户可以事后在插件面板里补密钥再重连。
//
// registerMCP registers the bundle's MCP definitions with the plugin manager, prefixing names with the
// bundle's to avoid collisions. One server failing to connect is not an install failure: the rest of the
// bundle (skills, agents) is still useful, and the user can supply a key and reconnect from the panel.
func (m *BundleManager) registerMCP(b *Bundle, dir string) []string {
	var added []string
	for name, entry := range mcpServersOf(dir, rawManifestMCP(dir)) {
		full := scopedMCPName(b.Name, name)
		if m.mcp.Has(full) {
			continue
		}
		cfg := mcpConfigFrom(full, entry, dir)
		if cfg.Command == "" && cfg.URL == "" {
			continue
		}
		if err := m.mcp.Add(cfg); err != nil {
			// 连接失败也已经存进 mcp.yaml 了，照样登记，让它以 error 状态出现在面板里
			// A failed connection is still saved in mcp.yaml, so record it either way and let it show up
			// in the panel in the error state
			fmt.Fprintf(os.Stderr, i18n.T("Plugin %s: MCP server %s did not connect: %v\n"), b.Name, full, err)
		}
		added = append(added, full)
	}
	sort.Strings(added)
	return added
}

// mcpConfigFrom 把清单里的一条 MCP 定义翻成引擎的配置，顺手展开 ${CLAUDE_PLUGIN_ROOT}。
// mcpConfigFrom turns one manifest MCP entry into the engine's config, expanding ${CLAUDE_PLUGIN_ROOT}.
func mcpConfigFrom(name string, entry rawMCPEntry, dir string) MCPServerConfig {
	cfg := MCPServerConfig{Name: name}
	if entry.URL != "" {
		cfg.URL = expandRoot(entry.URL, dir)
		return cfg
	}
	cfg.Command = expandRoot(entry.Command, dir)
	for _, a := range entry.Args {
		cfg.Args = append(cfg.Args, expandRoot(a, dir))
	}
	if len(entry.Env) > 0 {
		cfg.Env = map[string]string{}
		for k, v := range entry.Env {
			cfg.Env[k] = expandRoot(v, dir)
		}
	}
	return cfg
}

func rawManifestMCP(dir string) json.RawMessage {
	raw, err := os.ReadFile(filepath.Join(dir, ".claude-plugin", "plugin.json"))
	if err != nil {
		return nil
	}
	var mf manifest
	if json.Unmarshal(raw, &mf) != nil {
		return nil
	}
	return mf.MCPServers
}

// scopedMCPName 生成 <包>_<server>，压进 MCP 名字的字母表和长度上限。
// scopedMCPName builds <bundle>_<server>, squeezed into the MCP name alphabet and length cap.
func scopedMCPName(bundle, server string) string {
	name := sanitizeBotName(bundle + "_" + server)
	if name == "" {
		name = bundle
	}
	if len(name) > 24 {
		name = name[:24]
	}
	return name
}

func expandRoot(s, dir string) string { return strings.ReplaceAll(s, pluginRootVar, dir) }

func looksLikeGit(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") ||
		strings.HasPrefix(s, "git@") || strings.HasSuffix(s, ".git")
}

func gitClone(url, dest string) error {
	if _, err := exec.LookPath("git"); err != nil {
		return errors.New(i18n.T("git is not installed, so a plugin cannot be fetched from a URL"))
	}
	cmd := exec.Command("git", "clone", "--depth", "1", "--quiet", url, dest)
	// 禁掉凭据提示：否则一个私有仓库地址会让 git 挂在那儿等输入，而这里没有终端可输
	// Disable credential prompts: otherwise a private repository URL leaves git waiting for input at a
	// terminal that does not exist here
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "SSH_ASKPASS=")
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf(i18n.T("git clone failed: %s"), strings.TrimSpace(errBuf.String()))
		}
		return nil
	case <-time.After(gitCloneTimeout):
		_ = cmd.Process.Kill()
		return errors.New(i18n.T("git clone timed out"))
	}
}

// copyTree 拷贝目录树，跳过 .git（本地目录装插件时没必要连历史一起拷）。
// copyTree copies a directory tree, skipping .git (installing from a local directory has no need of its
// history).
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == ".git" && rel != "." {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		if !info.Mode().IsRegular() {
			return nil // 符号链接等一律不跟 / do not follow symlinks and friends
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), raw, info.Mode().Perm())
	})
}

// InstallFromMarketplace 从一个市场地址里装其中一个插件。
// InstallFromMarketplace installs one plugin out of a marketplace address.
func (m *BundleManager) InstallFromMarketplace(source, pluginName string) (*Bundle, error) {
	if err := os.MkdirAll(m.root, 0o755); err != nil {
		return nil, err
	}
	staging, err := os.MkdirTemp(m.root, ".staging-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(staging)
	if err := fetchInto(source, staging); err != nil {
		return nil, err
	}

	sub, entry, err := locateMarketplacePlugin(staging, pluginName)
	if err != nil {
		return nil, err
	}
	b, err := bundleAt(sub, entry)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	_, dup := m.list[b.Name]
	m.mu.Unlock()
	if dup {
		return nil, fmt.Errorf(i18n.T("A plugin named %s is already installed"), b.Name)
	}
	dest := filepath.Join(m.root, b.Name)
	if _, err := os.Stat(dest); err == nil {
		return nil, fmt.Errorf(i18n.T("%s already exists"), dest)
	}
	if err := os.Rename(sub, dest); err != nil {
		return nil, err
	}
	_ = os.WriteFile(filepath.Join(dest, ".origin"), []byte(source), 0o644)
	// 记下它来自哪个市场的哪一条：升级时要照着这条重新取一次，
	// 因为装进来的是仓库的一个子目录，不是 git 检出，pull 不了。
	// Record which entry of which marketplace it came from: an upgrade has to fetch by that entry again,
	// since what was installed is a subdirectory of the repository rather than a git checkout that could
	// be pulled.
	_ = os.WriteFile(filepath.Join(dest, ".marketplace"), []byte(entry.Name), 0o644)

	b, err = bundleAt(dest, entry)
	if err != nil {
		return nil, err
	}
	b.Source, b.Marketplace = source, entry.Name
	b.MCPServers = m.registerMCP(b, dest)

	m.mu.Lock()
	m.list[b.Name] = b
	m.mu.Unlock()
	m.fireChange()
	return b, nil
}

// locateMarketplacePlugin 在已取下来的仓库里找到某一条对应的目录。
// locateMarketplacePlugin finds the directory one entry refers to inside the fetched repository.
func locateMarketplacePlugin(repo, pluginName string) (string, MarketplaceEntry, error) {
	mk, ok := readMarketplace(repo)
	if !ok {
		return "", MarketplaceEntry{}, errors.New(i18n.T("no .claude-plugin/marketplace.json found"))
	}
	for _, e := range mk.Plugins {
		if e.Name != pluginName {
			continue
		}
		rel := strings.TrimSpace(e.Source)
		if rel == "" {
			rel = "."
		}
		sub := filepath.Clean(filepath.Join(repo, rel))
		// 清单是别人写的，source 里出现 ../ 就会把安装目标指到仓库外面去。
		// The listing is someone else's file, and a ../ in source would aim the install outside the
		// repository entirely.
		if r, err := filepath.Rel(repo, sub); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			return "", e, fmt.Errorf(i18n.T("the source path %q of %s points outside the repository"), e.Source, e.Name)
		}
		if !isDir(sub) {
			return "", e, fmt.Errorf(i18n.T("%s points at %s, which the repository does not contain"), e.Name, e.Source)
		}
		return sub, e, nil
	}
	return "", MarketplaceEntry{}, fmt.Errorf(i18n.T("the marketplace has no plugin named %s"), pluginName)
}

// bundleAt 读一个插件目录；没有 plugin.json 就用市场清单里那条的元数据兜底。
// 实测有市场把 source 指向仓库根，而根目录只有 marketplace.json——挑剔的话这类一个也装不上。
//
// bundleAt reads a plugin directory, falling back to the marketplace entry's metadata when there is no
// plugin.json. Marketplaces do point source at the repository root in practice, where only
// marketplace.json lives — being strict would reject every one of those.
func bundleAt(dir string, entry MarketplaceEntry) (*Bundle, error) {
	if b, err := readBundle(dir); err == nil {
		return b, nil
	}
	return scanBundle(dir, entry.Name, entry.Description, "", "")
}

// Update 就地升级一个已装插件包。
//
// 关键在于**调和而不是重建**：直接卸了重装最省事，但 Remove 会把它注册的 MCP 条目从 mcp.yaml 里
// 删掉，你为那个插件挑好的工具子集、跑过的 OAuth 授权也就一起没了。装了 GitHub 插件、从九十多个
// 工具里挑出六个、授权完，升级一次全部重来——那不叫升级。
// 所以这里只动差集：新增的 server 加进来，消失的删掉，已经存在的一律不碰。
//
// Update upgrades an installed bundle in place.
//
// The point is to **reconcile rather than rebuild**: removing and reinstalling would be simpler, but
// Remove deletes the bundle's MCP entries from mcp.yaml, taking the tool subset you picked and the OAuth
// authorization you completed with them. Install the GitHub plugin, narrow ninety-odd tools down to six,
// authorize — and one upgrade puts you back at the start. That is not an upgrade.
// So only the difference is touched: new servers are added, vanished ones removed, existing ones left
// exactly as they are.
func (m *BundleManager) Update(name string) (*Bundle, error) {
	m.mu.Lock()
	old, ok := m.list[name]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf(i18n.T("No plugin named %s"), name)
	}
	if old.Source == "" {
		return nil, fmt.Errorf(i18n.T("%s has no recorded source, so it cannot be updated automatically"), name)
	}

	if old.Marketplace != "" {
		if err := m.refreshFromMarketplace(old); err != nil {
			return nil, err
		}
	} else if err := refreshSource(old.Source, old.Dir); err != nil {
		return nil, err
	}
	fresh, err := bundleAt(old.Dir, MarketplaceEntry{Name: old.Name, Description: old.Description})
	if err != nil {
		return nil, err
	}
	fresh.Source, fresh.Marketplace = old.Source, old.Marketplace
	fresh.MCPServers = m.reconcileMCP(old, fresh)

	m.mu.Lock()
	m.list[fresh.Name] = fresh
	// 清单里改了名字：目录名还是旧的，但登记要按新名字来，否则列表里会同时出现两条
	// The manifest renamed it: the directory keeps the old name, but the record follows the new one, or
	// the list ends up showing both
	if fresh.Name != name {
		delete(m.list, name)
	}
	m.mu.Unlock()
	m.fireChange()
	return fresh, nil
}

// refreshFromMarketplace 重新取一遍市场仓库，把那一条对应的子目录换进已装目录。
// 不能用 git pull：装进来的是仓库的一个子目录，本身不是 git 检出。
//
// 换目录用「先挪走旧的、再把新的挪进来、最后删旧的」：中途任何一步失败都能把旧的挪回去，
// 而不是先删后写留下一个空目录。
//
// refreshFromMarketplace re-fetches the marketplace repository and swaps in the subdirectory that entry
// refers to. A git pull is not an option: what is installed is a subdirectory of the repository, not a
// git checkout in its own right.
//
// The swap moves the old aside, moves the new in, and only then deletes the old, so a failure at any
// step can put the old one back rather than leaving an empty directory behind.
func (m *BundleManager) refreshFromMarketplace(b *Bundle) error {
	staging, err := os.MkdirTemp(m.root, ".staging-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err := fetchInto(b.Source, staging); err != nil {
		return err
	}
	sub, _, err := locateMarketplacePlugin(staging, b.Marketplace)
	if err != nil {
		return err
	}

	aside := b.Dir + ".old"
	_ = os.RemoveAll(aside)
	if err := os.Rename(b.Dir, aside); err != nil {
		return err
	}
	if err := os.Rename(sub, b.Dir); err != nil {
		_ = os.Rename(aside, b.Dir) // 放回去，别留下一个空目录 / put it back rather than leave a gap
		return err
	}
	// 来源标记跟着新目录走 / the origin markers follow the new directory
	_ = os.WriteFile(filepath.Join(b.Dir, ".origin"), []byte(b.Source), 0o644)
	_ = os.WriteFile(filepath.Join(b.Dir, ".marketplace"), []byte(b.Marketplace), 0o644)
	_ = os.RemoveAll(aside)
	return nil
}

// refreshSource 把远端的新内容取到已装目录里。
// refreshSource pulls the latest content into the installed directory.
func refreshSource(source, dir string) error {
	if !looksLikeGit(source) && isDir(source) {
		// 本地目录装的：重新拷一遍。拷贝是覆盖式的，源里删掉的文件在这里会留下——
		// 对本地开发插件来说这反而方便（不会把你正在调试的东西清掉）。
		// Installed from a local directory: copy it again. The copy overlays, so a file deleted at the
		// source lingers here — which is the friendlier behaviour for a plugin you are developing locally
		// (nothing you are mid-debug gets wiped).
		return copyTree(source, dir)
	}
	if !isDir(filepath.Join(dir, ".git")) {
		return fmt.Errorf(i18n.T("%s is not a git checkout, so it cannot be updated; remove and reinstall it"), dir)
	}
	cmd := exec.Command("git", "-C", dir, "pull", "--ff-only", "--quiet")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "SSH_ASKPASS=")
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf(i18n.T("git pull failed: %s"), strings.TrimSpace(errBuf.String()))
		}
		return nil
	case <-time.After(gitCloneTimeout):
		_ = cmd.Process.Kill()
		return errors.New(i18n.T("git pull timed out"))
	}
}

// reconcileMCP 只处理差集，已注册的条目原样保留（用户的工具勾选和授权都在里面）。
// reconcileMCP handles only the difference; already-registered entries are left untouched, since the
// user's tool selection and authorization live in them.
func (m *BundleManager) reconcileMCP(old, fresh *Bundle) []string {
	want := map[string]MCPServerConfig{}
	for name, entry := range mcpServersOf(fresh.Dir, rawManifestMCP(fresh.Dir)) {
		full := scopedMCPName(fresh.Name, name)
		want[full] = mcpConfigFrom(full, entry, fresh.Dir)
	}

	// 这一版没有了的，摘掉 / drop what this version no longer ships
	for _, prev := range old.MCPServers {
		if _, still := want[prev]; !still {
			m.mcp.Remove(prev)
		}
	}
	var out []string
	for full, cfg := range want {
		out = append(out, full)
		if m.mcp.Has(full) {
			continue // 已在：连同用户的设置一起留着 / already there: kept along with the user's settings
		}
		if err := m.mcp.Add(cfg); err != nil {
			fmt.Fprintf(os.Stderr, i18n.T("Plugin %s: MCP server %s did not connect: %v\n"), fresh.Name, full, err)
		}
	}
	sort.Strings(out)
	return out
}

// Remove 卸载：先摘掉它注册的 MCP 插件，再删目录。
// Remove uninstalls: detach the MCP plugins it registered first, then delete the directory.
func (m *BundleManager) Remove(name string) error {
	m.mu.Lock()
	b, ok := m.list[name]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf(i18n.T("No plugin named %s"), name)
	}
	for _, srv := range b.MCPServers {
		m.mcp.Remove(srv)
	}
	if err := os.RemoveAll(b.Dir); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.list, name)
	m.mu.Unlock()
	m.fireChange()
	return nil
}

func (m *BundleManager) List() []Bundle {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Bundle, 0, len(m.list))
	for _, b := range m.list {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// SkillRoots 给技能管理器用：每个包的 skills/ 目录一处根。
// SkillRoots feeds the skill manager: one root per bundle's skills/ directory.
func (m *BundleManager) SkillRoots() []struct{ Source, Dir string } {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []struct{ Source, Dir string }
	names := make([]string, 0, len(m.list))
	for n := range m.list {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if b := m.list[n]; b != nil && b.SkillDir != "" {
			out = append(out, struct{ Source, Dir string }{b.Name, b.SkillDir})
		}
	}
	return out
}

// Agents 汇总全部包里的同事模板。
// Agents gathers the teammate templates from every bundle.
func (m *BundleManager) Agents() []Agent {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Agent
	names := make([]string, 0, len(m.list))
	for n := range m.list {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if b := m.list[n]; b != nil {
			out = append(out, b.Agents...)
		}
	}
	return out
}
