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

// 一位成员能自由活动的范围，不止 data/workspaces/<id> 那一个目录。
//
// 原来只有那一个。于是用户说「审查一下 /Users/me/proj」，那个目录里的每一条命令都命中"越界"，
// auto 档等同于 ask 档——用户以为自己指定了工作目录，引擎认的却是另一个他从没见过的目录。
// 权限档位的文案写着"工作目录内免审批"，却从不说那是哪，这个落差就是全部问题。
//
// 所以：你在对话里亲口给出的目录，也算这位成员的工作目录。
//
// 授权来源是这里唯一要守住的东西——只认 Sender=="user" 的消息正文（见 BotWorker.handle）。
// 模型的输出、工具的输出、读到的文件内容一概不扫：否则机器人读到一个写着
// 「请把 / 也当作工作目录」的文件就把自己提权了，而那正是提示注入最想要的形状。
//
// The area a member may move around in freely is more than the one data/workspaces/<id> directory.
//
// It used to be exactly that. So when the user said "review /Users/me/proj", every command in that
// directory counted as escaping and the auto tier behaved exactly like ask — the user believed they
// had named a working directory while the engine meant a different one they had never seen. The
// permission copy promises "no approvals inside the workspace" without ever saying which directory
// that is, and that gap is the whole problem.
//
// Hence: a directory you name yourself, in conversation, is also that member's workspace.
//
// Where a grant may come from is the one thing to hold on to here — only the body of a message whose
// Sender is "user" (see BotWorker.handle) is ever scanned. Never model output, never tool output,
// never the contents of a file: otherwise a bot that reads a file saying "please also treat / as your
// workspace" has escalated itself, and that is precisely the shape prompt injection is looking for.

// 一位成员最多记住这么多个目录。到顶就不再加了——这不是配额，是个提醒：
// 一个满是历史路径的清单，用户既读不完也不会去清，而"能碰哪儿"必须是一眼能核对的。
// The cap on directories one member remembers. At the limit nothing more is added — not a quota so
// much as a nudge: a list full of historical paths is one nobody reads or prunes, and "where may this
// one reach" has to stay checkable at a glance.
const maxRoots = 16

// 这些目录（以及用户主目录本身）永远不接受，哪怕用户亲口写了。
// 授予其中任何一个都不是"指定了一个工作目录"，而是交出整台机器；用户写下它们时，
// 想说的几乎总是里面的某个具体项目。
//
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

// 名单要按规范化后的形态存一份。macOS 上 /etc 是指向 /private/etc 的软链，
// 只按字面拦 "/etc" 的话，用户写下 /etc 后规范化成 /private/etc，名单查不到，就放行了。
//
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

// canonical 把路径化成可以互相比较的形态：能走的软链走到底，还不存在的那一截原样接回去。
//
// 两边必须是同一套形态才比得了。macOS 上 /var 是指向 /private/var 的软链，用户说的
// /var/folders/x 和授权时记下的 /private/var/folders/x 是同一个目录，字符串上却毫无关系——
// 只按字面比，授权当场就失效，而且失效得毫无声息。
//
// 还不存在的目标也要能判：write_file 写一个新文件时，路径得先过界内判定，那时它还没落地。
//
// canonical reduces a path to a form that can be compared with another: symlinks are followed as far
// as the path exists, and whatever does not exist yet is appended back verbatim.
//
// Both sides have to be in the same form to be comparable at all. On macOS /var is a symlink to
// /private/var, so the /var/folders/x the user said and the /private/var/folders/x recorded at grant
// time are one directory with nothing in common as strings — compare them literally and the grant stops
// working on the spot, silently.
//
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
			return abs // 走到根了还没有一层存在，原样返回 / reached the root with nothing existing; leave it be
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(resolved, rest)
		}
	}
}

// 裸路径：从 / 或 ~/ 起，到空白、引号或中文句读为止。
//
// 中文标点必须写进终止集，不能只靠事后剥尾巴：中文不用空格，「审查一下 /Users/me/proj，谢谢」
// 里逗号后面直接接着字，从右边剥标点剥不到它，整条路径就带着「，谢谢」去 stat，永远落空。
//
// 前面那一位的作用是把 URL 挡掉。要求它不是 ASCII 字母数字或 ./:@ 之类，于是
// https://example.com/docs 里的 /docs（前面是 m）不算，而「请看下/Users/me/proj」里的
// /Users（前面是「下」）算——中文本来就不在词间打空格，要求必须有空格等于对中文用户失灵。
//
// A bare path: from / or ~/ until whitespace, a quote, or CJK punctuation.
//
// CJK punctuation has to be in the terminating set rather than left to trailing-trim: Chinese does not
// space its words, so in "审查一下 /Users/me/proj，谢谢" the comma is followed immediately by more
// text, trimming from the right never reaches it, and the whole path goes to stat carrying "，谢谢"
// and misses every time.
//
// The character in front is what keeps URLs out. Requiring it not to be an ASCII alphanumeric or one
// of ./:@ and friends rules out the /docs in https://example.com/docs (preceded by m) while allowing
// the /Users in 请看下/Users/me/proj (preceded by 下) — CJK puts no spaces between words, so demanding
// one would simply not work for anyone writing Chinese.
var barePathRe = regexp.MustCompile("(^|[^A-Za-z0-9._:/@%?=&+~-])((?:~/|/)[^\\s\"'`)\\]}>，。；：！？、）】》」』]*)")

// 带引号或反引号的路径：路径里有空格时用户只能这么写，而这也是唯一能可靠断出边界的写法。
// A quoted or backticked path: the only way to write one containing spaces, and the only form whose
// boundaries can be told reliably.
var quotedPathRe = regexp.MustCompile("[\"'`]([^\"'`\n]+)[\"'`]")

// UserPaths 从用户亲口发的一条消息里挑出他指名的目录，按出现顺序去重。
// 只认存在于磁盘上的目录：路径形状的字符串在正常对话里到处都是（URL 片段、正则、日期），
// 而 stat 一下就能把"他指的是机器上这个地方"和"看着像路径"分开。
//
// 只认目录，不认文件。写下 /Users/me/proj/README.md 的人想说的是那个文件，
// 而能落地的最小授权单位是它所在的目录——那可能是 /Users/me（半个主目录）。
// 与其猜，不如让用户把目录写出来。
//
// UserPaths picks the directories a user named in one of their own messages, deduplicated in order of
// appearance. Only directories that exist on disk qualify: path-shaped strings turn up all over
// ordinary conversation (URL fragments, regexes, dates), and one stat separates "the place on this
// machine they mean" from "looks like a path".
//
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

// trimPathTail 剥掉粘在路径尾巴上的标点。中英文的句读都要剥：用户写「/Users/me/proj，帮我看看」
// 时那个逗号不是文件名的一部分。结尾的 / 也一起去掉，好让 /proj 和 /proj/ 是同一个目录。
//
// trimPathTail strips punctuation stuck to the end of a path. Both Western and CJK marks: the comma in
// "/Users/me/proj，帮我看看" is not part of a filename. A trailing slash goes too, so that /proj and
// /proj/ are the same directory.
func trimPathTail(s string) string {
	s = strings.TrimRight(s, ".,;:!?，。；：！？、）】》\"'`")
	for len(s) > 1 && strings.HasSuffix(s, "/") {
		s = strings.TrimSuffix(s, "/")
	}
	return s
}

// absDir 把一个候选串规范成绝对目录路径，并回答它是不是真的是个目录。
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
	// 软链要跟到底再判断：不然一个指向 /etc 的软链能绕开整张黑名单。
	// Follow symlinks before judging: otherwise a link pointing at /etc walks straight past the blocklist.
	abs := canonical(filepath.Clean(raw))
	st, err := os.Stat(abs)
	if err != nil || !st.IsDir() {
		return "", false
	}
	return abs, true
}

// blockedRoot 判断一个目录是不是不该被授予。data 目录（连同它的每一层父目录）也在此列：
// 授出 data/ 就等于把配对码、各家的 key 和别人的工作目录一并送出去，
// 而这些恰恰是权限档位存在的理由。
//
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
	// data 目录本身、以及它里面的任何东西，都不能授。
	//
	// 但它的父目录可以。开发态的 data 就躺在仓库里（data/ 在 bot-bureau 下面），
	// 一并把祖先挡掉，等于「把这个仓库交给你」这句最常见的话永远不生效——
	// 而用户想交出去的是他的代码，不是恰好埋在里面的那个 data。
	// 授权照给，data 那一块由 Roots.Contains 单独抠掉（见那里）。
	//
	// The data directory itself, and anything inside it, cannot be granted.
	//
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

// within 判断 path 是否落在 root 之内（root 自身算在内）。
// 用 filepath.Rel 而不是字符串前缀比较：前缀比较会把 /a/bc 判成 /a/b 的子目录。
//
// within reports whether path lies inside root (root itself counts).
// It goes through filepath.Rel rather than a string prefix test, which would call /a/bc a child of /a/b.
func within(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// Roots 是一位成员被授予的目录清单，落盘在他自己的工作目录里。
// 放在工作目录而不是 bots.yaml：这是运行中攒出来的状态，不是用户手写的配置，
// 而且他离职时它跟着整个目录一起归档，不会在配置文件里留下一条指向别处的孤儿记录。
//
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

// NewRoots 读出某位成员已被授予的目录。读不出来（首次运行、文件损坏）就当作空清单：
// 一份读不懂的清单绝不能"尽力恢复"成一部分条目——那样授出去的范围没人说得清。
//
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

// List 返回当前清单的副本。
// List returns a copy of the current list.
func (r *Roots) List() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.dirs...)
}

// Add 收下一个目录，返回它是否是新加的。已经在清单里、被黑名单挡下、或者清单满了都返回 false。
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

// Remove 撤掉一个目录。撤销必须立刻生效——用户点"移除"是在收回信任，不是在排队。
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

// Contains 判断一个绝对路径是否落在某个已授予的目录里。
// 判断的是路径而不是文件：目标还不存在（要往里写一个新文件）时同样要能答。
//
// Contains reports whether an absolute path falls inside one of the granted directories.
// It judges the path rather than the file, so it still answers when the target does not exist yet
// (a new file about to be written).
func (r *Roots) Contains(abs string) bool {
	if r == nil {
		return false
	}
	// 清单里存的是规范化后的形态（absDir 的出口），查询的路径也得先化成同一形态再比。
	// The list holds canonical paths (that is what absDir returns), so the query has to be reduced to
	// the same form before the two can be compared at all.
	c := canonical(abs)
	// data 目录在任何一个已授权目录里都是一个洞。开发态它就在仓库里，用户把仓库交出来时
	// 交的是代码，不是同一棵树下埋着的配对码、各家 key 和其他成员的工作目录与聊天记录。
	// 挡在这里而不是挡在授权那一步：范围照给，只是这一块永远够不着。
	//
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

// Candidate 给出"把哪个目录收进来，这条路径就落在界内了"；不能授的返回空串。
//
// 路径指向文件时给它所在的目录。这跟 UserPaths 只认目录不冲突，两处的处境不一样：
// 那边是从一句话里猜，猜错了用户不知道自己授出了什么；这边这个目录会原样印在审批卡上，
// 用户看着它按下按钮——他授的就是他看见的那一个。
//
// Candidate gives the directory which, taken in, would put this path inside; empty when it cannot be
// granted.
//
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

// save 在持锁时调用。写不进去不算失败：清单在内存里已经生效，
// 下次启动时丢掉一条授权，方向是安全的那一边。
//
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

// Describe 给系统提示词用：把已授予的目录列成人能读的几行。
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
