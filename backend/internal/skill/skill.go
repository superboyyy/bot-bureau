package skill

// Agent Skills：MCP 之外的另一层。
// MCP 给的是「能做什么」（一批工具），skill 给的是「怎么做」（一套流程、约定、领域知识）。
// 一个 skill 就是一个目录：SKILL.md 带 YAML frontmatter（name/description），正文是写给模型看的
// 说明书，旁边可以放脚本和资料。
//
// 关键取舍是**两段式加载**：平时只把每个 skill 的一行 name + description 放进系统提示，模型判断
// 用得上时再调 read_skill 拉全文。装几十个 skill 也就多几十行提示词，而不是几十篇文档——
// 一次性全塞进去的话，上下文很快就没了，而且大部分内容当前任务根本用不上。
//
// Agent Skills: the layer next to MCP.
// MCP supplies "what can be done" (a set of tools); a skill supplies "how it is done" (a procedure, a
// convention, domain knowledge). A skill is a directory: SKILL.md with YAML frontmatter (name and
// description), a body written for the model, and optionally scripts and material beside it.
//
// The key decision is **two-stage loading**: only the one-line name + description of each skill goes
// into the system prompt, and the model calls read_skill for the full text when it judges one to
// apply. Fifty installed skills then cost fifty lines of prompt rather than fifty documents — loading
// everything up front burns the context window on material the task at hand mostly does not need.

import (
	"botbureau/backend/internal/i18n"
	"botbureau/backend/internal/textutil"

	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// skill 名沿用 bot / 插件那一套字母表：它要出现在提示词里被模型原样抄回来当参数，
// 短、无空格、无大小写歧义才不容易抄错。
// A skill name uses the same alphabet as bots and plugins: it appears in the prompt and comes back
// verbatim as a tool argument, so short, space-free and case-unambiguous is what keeps it from being
// mistyped.
var nameRe = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

// SKILL.md 正文的上限。技能说明书本来就该是给人读的长度；真要塞几万字，
// 该拆成旁边的资料文件按需再读。
// Cap on the SKILL.md body. A skill's instructions should be readable in one sitting; material that
// genuinely runs to tens of thousands of characters belongs in files beside it, read on demand.
const BodyLimit = 20000

type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// 来源：内置目录为 "local"，插件带来的是插件名——卸载插件时要按它回收
	// Origin: "local" for the built-in directory, otherwise the plugin's name — uninstalling a plugin
	// reclaims its skills by this field
	Source string `json:"source"`
	Dir    string `json:"dir"`
	Body   string `json:"-"`
	// 目录里除 SKILL.md 以外的文件（相对路径），列给模型看，让它知道手边有什么可用
	// Files other than SKILL.md in the directory (relative paths), listed for the model so it knows what
	// is at hand
	Files []string `json:"files,omitempty"`
}

// Root 是一处扫描根目录及其来源标签。
// Root is one directory to scan plus the label of where it came from.
type Root struct {
	Source string
	Dir    string
}

type Manager struct {
	mu     sync.Mutex
	roots  []Root
	skills map[string]*Skill
	order  []string
}

func NewManager(localDir string) *Manager {
	m := &Manager{skills: map[string]*Skill{}}
	m.roots = []Root{{Source: "local", Dir: localDir}}
	m.Rescan()
	return m
}

// SetRoots 覆盖「本地目录之外」的扫描根（插件装/卸后调用）。
// SetRoots replaces the scan roots beyond the local directory (called after a plugin is installed or
// removed).
func (m *Manager) SetRoots(extra []Root) {
	m.mu.Lock()
	local := m.roots[0]
	m.roots = append([]Root{local}, extra...)
	m.mu.Unlock()
	m.Rescan()
}

// Rescan 重扫全部根目录。技能是文件，用户可能直接在编辑器里改——每次装卸插件、
// 以及界面主动刷新时重扫，比缓存到进程退出要好用得多。
// Rescan re-reads every root. Skills are files a user may well edit in an editor, so rescanning on each
// install/uninstall and on an explicit refresh from the UI beats caching until the process exits.
func (m *Manager) Rescan() {
	m.mu.Lock()
	roots := append([]Root(nil), m.roots...)
	m.mu.Unlock()

	found := map[string]*Skill{}
	var order []string
	for _, root := range roots {
		entries, err := os.ReadDir(root.Dir)
		if err != nil {
			continue // 根目录不存在是正常的（还没建过任何技能）/ a missing root is normal (no skills yet)
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			s, err := loadSkill(filepath.Join(root.Dir, e.Name()), root.Source)
			if err != nil {
				if !errors.Is(err, os.ErrNotExist) {
					fmt.Fprintf(os.Stderr, i18n.T("Skipping skill %s: %v\n"), e.Name(), err)
				}
				continue
			}
			// 同名先到先得，并且说出来——静默覆盖会让人对着一个不生效的技能查半天
			// First one wins on a name clash, and it says so: silently shadowing one leaves someone
			// debugging a skill that was never in play
			if prev, dup := found[s.Name]; dup {
				fmt.Fprintf(os.Stderr, i18n.T("Skill name %s is taken by %s; ignoring the copy in %s\n"),
					s.Name, prev.Dir, s.Dir)
				continue
			}
			found[s.Name] = s
			order = append(order, s.Name)
		}
	}
	sort.Strings(order)

	m.mu.Lock()
	m.skills, m.order = found, order
	m.mu.Unlock()
}

// loadSkill 读一个技能目录。没有 SKILL.md 就不是技能目录，返回 os.ErrNotExist 让调用方静默跳过。
// loadSkill reads one skill directory. Without a SKILL.md it is not a skill directory, and os.ErrNotExist
// is returned so the caller can skip it quietly.
func loadSkill(dir, source string) (*Skill, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return nil, err
	}
	meta, body := splitFrontmatter(string(raw))
	var fm struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if meta != "" {
		if err := yaml.Unmarshal([]byte(meta), &fm); err != nil {
			return nil, fmt.Errorf(i18n.T("its frontmatter is not valid YAML: %w"), err)
		}
	}
	name := strings.TrimSpace(fm.Name)
	if name == "" {
		// frontmatter 没写 name 就用目录名——别人写的技能不一定规矩，能跑起来比挑剔好
		// A missing name falls back to the directory name: skills written elsewhere are not always tidy,
		// and running them beats being fussy
		name = filepath.Base(dir)
	}
	if !nameRe.MatchString(name) {
		return nil, errors.New(i18n.T("the name must be 1-64 characters of lowercase letters, digits, - or _"))
	}
	desc := strings.TrimSpace(fm.Description)
	if desc == "" {
		return nil, errors.New(i18n.T("SKILL.md needs a description in its frontmatter (that is what the model matches on)"))
	}
	s := &Skill{
		Name: name, Description: desc, Source: source, Dir: dir,
		Body: textutil.Truncate(strings.TrimSpace(body), BodyLimit),
	}
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if e.Name() != "SKILL.md" {
				s.Files = append(s.Files, e.Name())
			}
		}
	}
	return s, nil
}

// splitFrontmatter 切出 --- 包住的 YAML 头。没有头就整篇当正文。
// splitFrontmatter separates the YAML header fenced by ---. With no header the whole file is the body.
func splitFrontmatter(text string) (meta, body string) {
	// 编辑器保存出来的 SKILL.md 常带 BOM，不剥掉就识别不出开头的 ---
	// Editors routinely save SKILL.md with a BOM, which hides the opening --- unless it is stripped
	trimmed := strings.TrimLeft(text, "\ufeff \t\r\n")
	if !strings.HasPrefix(trimmed, "---") {
		return "", text
	}
	rest := strings.TrimPrefix(trimmed, "---")
	rest = strings.TrimLeft(rest, "\r\n")
	// 结束分隔符必须自成一行，否则正文里的 --- 分割线会把文件腰斩
	// The closing fence must be a line of its own, or a --- rule in the body would cut the file in half
	for _, sep := range []string{"\n---\n", "\n---\r\n"} {
		if i := strings.Index(rest, sep); i >= 0 {
			return rest[:i], rest[i+len(sep):]
		}
	}
	if strings.HasSuffix(rest, "\n---") {
		return strings.TrimSuffix(rest, "\n---"), ""
	}
	return "", text
}

func (m *Manager) List() []Skill {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Skill, 0, len(m.order))
	for _, n := range m.order {
		if s := m.skills[n]; s != nil {
			out = append(out, *s)
		}
	}
	return out
}

func (m *Manager) Get(name string) (Skill, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.skills[name]
	if !ok {
		return Skill{}, false
	}
	return *s, true
}

func (m *Manager) Names() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.order...)
}

// Roster 是放进系统提示的那几行：一个技能一行，只有名字和描述。
// 描述是模型唯一的判断依据，所以它必须写清楚「什么时候该用我」——这一点在
// 用户自己写技能时最容易忽略，提示词里也就顺带把这个约定讲明白了。
//
// Roster is the handful of lines that go into the system prompt: one line per skill, name and
// description only. The description is the model's only basis for choosing, so it has to say when the
// skill applies — the thing most easily missed when writing one's own skill, which is why the prompt
// spells the convention out.
func (m *Manager) Roster() string {
	list := m.List()
	if len(list) == 0 {
		return ""
	}
	var b strings.Builder
	for _, s := range list {
		fmt.Fprintf(&b, "- %s: %s\n", s.Name, textutil.Brief(s.Description, 300))
	}
	return b.String()
}

// Render 把一个技能渲染成 read_skill 的返回值：正文，加上一段「东西在哪儿」。
// 目录是绝对路径，且在 bot 工作目录之外——所以脚本要跑就得过审批门，这里如实说明，
// 免得模型以为可以直接执行然后困惑于被拦。
//
// Render turns a skill into what read_skill returns: the body, plus a note on where its files are.
// The directory is absolute and outside the bot's workspace, so running a script from it goes through
// the approval gate; saying so here keeps the model from expecting a free run and being puzzled by the
// prompt for approval.
func (s Skill) Render() string {
	var b strings.Builder
	b.WriteString(s.Body)
	if len(s.Files) > 0 {
		fmt.Fprintf(&b, i18n.T("\n\n---\nFiles bundled with this skill live in %s: %s\nRead or run them with bash using their full path; because that is outside your workspace, it goes through user approval."),
			s.Dir, strings.Join(s.Files, ", "))
	}
	return b.String()
}
