package skill

// Agent Skills: the layer next to MCP.
// MCP supplies "what can be done" (a set of tools); a skill supplies "how it is done" (a procedure, a
// convention, domain knowledge). A skill is a directory: SKILL.md with YAML frontmatter (name and
// description), a body written for the model, and optionally scripts and material beside it.

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

// A skill name uses the same alphabet as bots and plugins: it appears in the prompt and comes back
// verbatim as a tool argument, so short, space-free and case-unambiguous is what keeps it from being
// mistyped.
var nameRe = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

// Cap on the SKILL.md body. A skill's instructions should be readable in one sitting; material that
// genuinely runs to tens of thousands of characters belongs in files beside it, read on demand.
const BodyLimit = 20000

type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`

	// Origin: "local" for the built-in directory, otherwise the plugin's name — uninstalling a plugin
	// reclaims its skills by this field
	Source string `json:"source"`
	Dir    string `json:"dir"`
	Body   string `json:"-"`

	// Files other than SKILL.md in the directory (relative paths), listed for the model so it knows what
	// is at hand
	Files []string `json:"files,omitempty"`
}

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

// SetRoots replaces the scan roots beyond the local directory (called after a plugin is installed or
// removed).
func (m *Manager) SetRoots(extra []Root) {
	m.mu.Lock()
	local := m.roots[0]
	m.roots = append([]Root{local}, extra...)
	m.mu.Unlock()
	m.Rescan()
}

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
			continue
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

// splitFrontmatter separates the YAML header fenced by ---. With no header the whole file is the body.
func splitFrontmatter(text string) (meta, body string) {

	// Editors routinely save SKILL.md with a BOM, which hides the opening --- unless it is stripped
	trimmed := strings.TrimLeft(text, "\ufeff \t\r\n")
	if !strings.HasPrefix(trimmed, "---") {
		return "", text
	}
	rest := strings.TrimPrefix(trimmed, "---")
	rest = strings.TrimLeft(rest, "\r\n")

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
