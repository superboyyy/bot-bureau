package plugin

// Plugin bundles: the .claude-plugin/plugin.json format from Claude / Codex, adopted as-is as Bot
// Bureau's package format.

// Why not invent one: a bespoke format means recruiting developers from zero, while an existing one
// means a developer writes a plugin once and Claude Code, Codex and Bot Bureau can all install it —
// an ecosystem on day one. So the job here is not "define a protocol" but "state which fields are
// consumed", and to report the ones that are not (Ignored) so the gap is visible immediately rather
// than discovered as half the plugin silently doing nothing.

// The mapping:
// mcpServers / .mcp.json  → MCP plugins (the layer the engine already has)
// skills/                 → the skill library (the skill package)
// agents/                 → member templates (Bot Bureau's own dividend: elsewhere these degrade
// into subagents)
// commands/ hooks/        → not supported yet, reported honestly in Ignored

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

// The placeholder a bundle uses to refer to its own location. It is an established convention in the
// Claude ecosystem and has to be supported: plenty of plugins point mcpServers at their own bundled
// scripts through it.
const pluginRootVar = "${CLAUDE_PLUGIN_ROOT}"

const gitCloneTimeout = 180 * time.Second

// Bundle is one installed plugin bundle.
type Bundle struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Version     string `json:"version,omitempty"`
	Author      string `json:"author,omitempty"`
	Dir         string `json:"dir"`
	Source      string `json:"source,omitempty"`

	// Non-empty when it was picked out of a marketplace listing, holding that entry's name. An upgrade
	// must fetch by the entry again, since what is installed is a subdirectory rather than a git checkout.
	Marketplace string   `json:"marketplace,omitempty"`
	MCPServers  []string `json:"mcp_servers,omitempty"`
	SkillDir    string   `json:"-"`
	Skills      []string `json:"skills,omitempty"`
	Agents      []Agent  `json:"agents,omitempty"`

	// Parts present in this bundle that Bot Bureau does not consume. Listing them is deliberate:
	// silently dropping half a plugin is far worse than saying "hooks are not supported".
	Ignored []string `json:"ignored,omitempty"`
}

// Agent is a "member template" shipped in a bundle, from which Bot Bureau can create a real member.
type Agent struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`

	// The body is this role's system prompt
	Prompt string `json:"prompt"`
	Source string `json:"source"`
}

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

// ---- marketplace manifests ----

// Plugins in the wild are often distributed not as "one repository, one plugin" but as "one repository,
// one marketplace listing several plugins" — those repositories carry .claude-plugin/marketplace.json at
// the root instead of plugin.json. Across a sample of twenty real repositories about half are of that
// kind, and recognising only plugin.json ends every one of them at "no plugin.json found", which reads as
// an incompatibility on our side.

type MarketplaceEntry struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`

	// A path relative to the repository ("./", "./plugins/foo"), which is what every sampled entry uses.
	Source string `json:"source,omitempty"`
}

type marketplaceFile struct {
	Name    string             `json:"name"`
	Plugins []MarketplaceEntry `json:"plugins"`
}

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

	// record what is not consumed
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

// fetchInto brings a source (a local directory or a git address) into the given directory.

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

	// Stage first and move into place only once it validates: half a plugin left in the directory is
	// harder to diagnose than an install that simply failed
	defer os.RemoveAll(staging)

	src := staging
	if err := fetchInto(source, staging); err != nil {
		return nil, err
	}

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

			// A failed connection is still saved in mcp.yaml, so record it either way and let it show up
			// in the panel in the error state
			fmt.Fprintf(os.Stderr, i18n.T("Plugin %s: MCP server %s did not connect: %v\n"), b.Name, full, err)
		}
		added = append(added, full)
	}
	sort.Strings(added)
	return added
}

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
			return nil // do not follow symlinks and friends
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), raw, info.Mode().Perm())
	})
}

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

// bundleAt reads a plugin directory, falling back to the marketplace entry's metadata when there is no
// plugin.json. Marketplaces do point source at the repository root in practice, where only
// marketplace.json lives — being strict would reject every one of those.
func bundleAt(dir string, entry MarketplaceEntry) (*Bundle, error) {
	if b, err := readBundle(dir); err == nil {
		return b, nil
	}
	return scanBundle(dir, entry.Name, entry.Description, "", "")
}

// Update upgrades an installed bundle in place.

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

	// The manifest renamed it: the directory keeps the old name, but the record follows the new one, or
	// the list ends up showing both
	if fresh.Name != name {
		delete(m.list, name)
	}
	m.mu.Unlock()
	m.fireChange()
	return fresh, nil
}

// refreshFromMarketplace re-fetches the marketplace repository and swaps in the subdirectory that entry
// refers to. A git pull is not an option: what is installed is a subdirectory of the repository, not a
// git checkout in its own right.

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
		_ = os.Rename(aside, b.Dir) // put it back rather than leave a gap
		return err
	}
	// the origin markers follow the new directory
	_ = os.WriteFile(filepath.Join(b.Dir, ".origin"), []byte(b.Source), 0o644)
	_ = os.WriteFile(filepath.Join(b.Dir, ".marketplace"), []byte(b.Marketplace), 0o644)
	_ = os.RemoveAll(aside)
	return nil
}

// refreshSource pulls the latest content into the installed directory.
func refreshSource(source, dir string) error {
	if !looksLikeGit(source) && isDir(source) {

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

// reconcileMCP handles only the difference; already-registered entries are left untouched, since the
// user's tool selection and authorization live in them.
func (m *BundleManager) reconcileMCP(old, fresh *Bundle) []string {
	want := map[string]MCPServerConfig{}
	for name, entry := range mcpServersOf(fresh.Dir, rawManifestMCP(fresh.Dir)) {
		full := scopedMCPName(fresh.Name, name)
		want[full] = mcpConfigFrom(full, entry, fresh.Dir)
	}

	// drop what this version no longer ships
	for _, prev := range old.MCPServers {
		if _, still := want[prev]; !still {
			m.mcp.Remove(prev)
		}
	}
	var out []string
	for full, cfg := range want {
		out = append(out, full)
		if m.mcp.Has(full) {
			continue // already there: kept along with the user's settings
		}
		if err := m.mcp.Add(cfg); err != nil {
			fmt.Fprintf(os.Stderr, i18n.T("Plugin %s: MCP server %s did not connect: %v\n"), fresh.Name, full, err)
		}
	}
	sort.Strings(out)
	return out
}

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

// Agents gathers the member templates from every bundle.
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
