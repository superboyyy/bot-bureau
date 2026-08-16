package engine

import (
	"botbureau/backend/internal/i18n"

	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// The area a member may move around in freely is more than the one data/workspaces/<id> directory.

// It used to be exactly that. So when the user said "review /Users/me/proj", every command in that
// directory counted as escaping and the auto tier behaved exactly like ask — the user believed they
// had named a working directory while the engine meant a different one they had never seen. The
// permission copy promises "no approvals inside the workspace" without ever saying which directory
// that is, and that gap is the whole problem.

// Hence: a directory you name yourself, in conversation, is also that member's workspace.

// Where a grant may come from is the one thing to hold on to here — only the body of a message whose
// Sender is "user" (see BotWorker.handle) is ever scanned. Never model output, never tool output,
// never the contents of a file: otherwise a bot that reads a file saying "please also treat / as your
// workspace" has escalated itself, and that is precisely the shape prompt injection is looking for.

// The cap on directories one member remembers. At the limit nothing more is added — not a quota so
// much as a nudge: a list full of historical paths is one nobody reads or prunes, and "where may this
// one reach" has to stay checkable at a glance.
const maxRoots = 16

// These directories (and the home directory itself) are never accepted, even typed by the user.
// Granting any one of them is not naming a workspace, it is handing over the machine; someone writing
// one of these almost always means some specific project inside it.
var blockedRootNames = []string{
	"/", "/bin", "/sbin", "/usr", "/etc",
	"/var", "/tmp", "/opt", "/dev", "/proc",
	"/sys", "/private", "/root", "/home",
	"/System", "/Library", "/Applications", "/Volumes",
	"/Users", "/Network", "/cores",
}

// The list is also kept in canonical form. On macOS /etc is a symlink to /private/etc, so a check on
// the literal "/etc" alone lets it through: the user writes /etc, it canonicalises to /private/etc, and
// the lookup misses.
var blockedRoots = func() map[string]bool {
	out := make(map[string]bool, len(blockedRootNames)*2)
	for _, d := range blockedRootNames {
		out[d] = true
		out[canonical(d)] = true
	}
	return out
}()

// canonical reduces a path to a form that can be compared with another: symlinks are followed as far
// as the path exists, and whatever does not exist yet is appended back verbatim.

// Both sides have to be in the same form to be comparable at all. On macOS /var is a symlink to
// /private/var, so the /var/folders/x the user said and the /private/var/folders/x recorded at grant
// time are one directory with nothing in common as strings — compare them literally and the grant stops
// working on the spot, silently.

// Targets that do not exist yet must be judged too: a write_file creating a new file clears the bounds
// check before the file is there.
func canonical(abs string) string {
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	rest, cur := "", abs
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs // reached the root with nothing existing; leave it be
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(resolved, rest)
		}
	}
}

// A bare path: from / or ~/ until whitespace, a quote, or CJK punctuation.

// CJK punctuation has to be in the terminating set rather than left to trailing-trim: Chinese does not
// space its words, so a comma followed immediately by more text would stay attached to the candidate
// path and make the stat miss every time.

// The character in front is what keeps URLs out. Requiring it not to be an ASCII alphanumeric or one
// of ./:@ and friends rules out the /docs in https://example.com/docs (preceded by m) while allowing

// one would simply not work for anyone writing Chinese.
var barePathRe = regexp.MustCompile("(^|[^A-Za-z0-9._:/@%?=&+~-])((?:~/|/)[^\\s\"'`)\\]}>，。；：！？、）】》」』]*)")

// A quoted or backticked path: the only way to write one containing spaces, and the only form whose
// boundaries can be told reliably.
var quotedPathRe = regexp.MustCompile("[\"'`]([^\"'`\n]+)[\"'`]")

// UserPaths picks the directories a user named in one of their own messages, deduplicated in order of
// appearance. Only directories that exist on disk qualify: path-shaped strings turn up all over
// ordinary conversation (URL fragments, regexes, dates), and one stat separates "the place on this
// machine they mean" from "looks like a path".

// Directories only, never files. Someone writing /Users/me/proj/README.md means that file, and the
// smallest grant that could carry it is the directory holding it — which might be /Users/me, half a
// home directory. Better to have the user name the directory than to guess.
func UserPaths(text string) []string {
	if text == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	take := func(raw string) {
		abs, ok := absDir(raw)
		if !ok || seen[abs] {
			return
		}
		seen[abs] = true
		out = append(out, abs)
	}
	for _, m := range quotedPathRe.FindAllStringSubmatch(text, -1) {
		take(m[1])
	}
	for _, m := range barePathRe.FindAllStringSubmatch(text, -1) {
		take(trimPathTail(m[2]))
	}
	return out
}

// trimPathTail strips punctuation stuck to the end of a path. Both Western and CJK marks are removed;
// a comma at the end of a sentence is not part of the filename. A trailing slash goes too, so /proj
// and /proj/ are the same directory.
func trimPathTail(s string) string {
	s = strings.TrimRight(s, ".,;:!?，。；：！？、）】》\"'`")
	for len(s) > 1 && strings.HasSuffix(s, "/") {
		s = strings.TrimSuffix(s, "/")
	}
	return s
}

// absDir normalises a candidate into an absolute directory path and says whether it really is one.
func absDir(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	if raw == "~" || strings.HasPrefix(raw, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		raw = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(raw, "~"), "/"))
	}
	if !filepath.IsAbs(raw) {
		return "", false
	}

	// Follow symlinks before judging: otherwise a link pointing at /etc walks straight past the blocklist.
	abs := canonical(filepath.Clean(raw))
	st, err := os.Stat(abs)
	if err != nil || !st.IsDir() {
		return "", false
	}
	return abs, true
}

// blockedRoot reports whether a directory must not be granted. The data directory — and every one of
// its ancestors — is included: granting data/ hands over the pairing code, every vendor key and other
// members' workspaces, which are the very things the permission tiers exist to protect.
func blockedRoot(abs, dataDir string) bool {
	if blockedRoots[abs] {
		return true
	}
	if home, err := os.UserHomeDir(); err == nil && abs == canonical(filepath.Clean(home)) {
		return true
	}

	// The data directory itself, and anything inside it, cannot be granted.

	// Its parents can. In development data sits inside the repository (data/ under bot-bureau), so
	// blocking ancestors as well would mean the single most common sentence — "this repository is
	// yours" — could never take effect, when what the user is handing over is their code, not the data
	// directory that happens to be buried in it. The grant goes through and the data directory is
	// punched back out by Roots.Contains (see there).
	if data, err := filepath.Abs(dataDir); err == nil {
		data = canonical(filepath.Clean(data))
		if abs == data || within(abs, data) {
			return true
		}
	}
	return false
}

// within reports whether path lies inside root (root itself counts).
// It goes through filepath.Rel rather than a string prefix test, which would call /a/bc a child of /a/b.
func within(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// Roots is the list of directories granted to one member, persisted inside their own workspace.
// It lives there rather than in bots.yaml because it is state accumulated at runtime rather than
// configuration anyone hand-writes, and because it travels with the directory when they leave instead
// of stranding an orphan entry pointing elsewhere.
type Roots struct {
	mu      sync.Mutex
	path    string
	dataDir string
	dirs    []string
}

// NewRoots loads the directories already granted to one member. An unreadable list (first run, a
// corrupt file) means an empty one: a list that cannot be parsed must never be best-effort recovered
// into some of its entries, because nobody could then say what was granted.
func NewRoots(workspace, dataDir string) *Roots {
	r := &Roots{path: filepath.Join(workspace, "roots.json"), dataDir: dataDir}
	raw, err := os.ReadFile(r.path)
	if err != nil {
		return r
	}
	var dirs []string
	if json.Unmarshal(raw, &dirs) != nil {
		return r
	}
	for _, d := range dirs {
		if abs, ok := absDir(d); ok && !blockedRoot(abs, dataDir) && !r.has(abs) && len(r.dirs) < maxRoots {
			r.dirs = append(r.dirs, abs)
		}
	}
	return r
}

func (r *Roots) has(abs string) bool {
	for _, d := range r.dirs {
		if d == abs {
			return true
		}
	}
	return false
}

// List returns a copy of the current list.
func (r *Roots) List() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.dirs...)
}

// Add takes a directory in and reports whether it was new. Already listed, blocked, or a full list all
// return false.
func (r *Roots) Add(dir string) bool {
	if r == nil {
		return false
	}
	abs, ok := absDir(dir)
	if !ok || blockedRoot(abs, r.dataDir) {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.has(abs) || len(r.dirs) >= maxRoots {
		return false
	}
	r.dirs = append(r.dirs, abs)
	r.save()
	return true
}

// Remove revokes a directory. Revocation takes effect at once: clicking remove withdraws trust, it
// does not queue a request.
func (r *Roots) Remove(dir string) bool {
	if r == nil {
		return false
	}
	target := filepath.Clean(strings.TrimSpace(dir))
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, d := range r.dirs {
		if d == target {
			r.dirs = append(r.dirs[:i], r.dirs[i+1:]...)
			r.save()
			return true
		}
	}
	return false
}

// Contains reports whether an absolute path falls inside one of the granted directories.
// It judges the path rather than the file, so it still answers when the target does not exist yet
// (a new file about to be written).
func (r *Roots) Contains(abs string) bool {
	if r == nil {
		return false
	}

	// The list holds canonical paths (that is what absDir returns), so the query has to be reduced to
	// the same form before the two can be compared at all.
	c := canonical(abs)

	// The data directory is a hole inside every granted directory. In development it lives in the
	// repository, and someone handing over that repository is handing over their code — not the pairing
	// code, the vendor keys, and other members' workspaces and history that happen to sit in the same
	// tree. Blocked here rather than at grant time: the area is granted as asked, this part of it simply
	// stays out of reach.
	if data, err := filepath.Abs(r.dataDir); err == nil && within(c, canonical(filepath.Clean(data))) {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, d := range r.dirs {
		if within(c, d) {
			return true
		}
	}
	return false
}

// Candidate gives the directory which, taken in, would put this path inside; empty when it cannot be
// granted.

// A path pointing at a file yields the directory holding it. That does not contradict UserPaths
// accepting directories only, because the situations differ: there it is a guess made from a sentence,
// and a wrong guess grants something the user never saw; here the directory is printed on the approval
// card and the user presses the button looking straight at it.
func (r *Roots) Candidate(path string) string {
	if r == nil || path == "" {
		return ""
	}
	dir := path
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		dir = filepath.Dir(path)
	}
	abs, ok := absDir(dir)
	if !ok || blockedRoot(abs, r.dataDir) || r.Contains(abs) {
		return ""
	}
	return abs
}

// save is called with the lock held. A failed write is not an error: the list is already in force in
// memory, and losing a grant on the next start errs on the safe side.
func (r *Roots) save() {
	if len(r.dirs) == 0 {
		_ = os.Remove(r.path)
		return
	}
	if raw, err := json.MarshalIndent(r.dirs, "", "  "); err == nil {
		_ = os.WriteFile(r.path, raw, 0o600)
	}
}

// Describe feeds the system prompt: the granted directories as a few readable lines.
func (r *Roots) Describe() string {
	dirs := r.List()
	if len(dirs) == 0 {
		return ""
	}
	sorted := append([]string(nil), dirs...)
	sort.Strings(sorted)
	var b strings.Builder
	b.WriteString(i18n.T("- Directories the user pointed you at (treat these exactly like your workspace: read, write and run commands there without approval where your tier allows it, using absolute paths):\n"))
	for _, d := range sorted {
		b.WriteString("  - " + d + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
