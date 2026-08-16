package engine

import "strings"

// Static reading of a bash command: split it into segments and pick out each segment's words and
// whether it carries an unquoted redirect.

// Why split at all. "Is this command read-only" used to be answered by "does it contain a shell
// metacharacter", so `grep -oE '…' f | head -80` counted as having side effects because of one pipe,
// and needed a human every single time. A pipe has no side effects of its own — redirects do, and so do
// the commands themselves. Claude Code splits a compound command on shell operators and judges each
// part; this does the same: the whole is read-only exactly when every segment is.

// Splitting badly can only be stricter, never looser. Spurious segments tend to start with a word that
// is not on the safe list, which puts the whole command back to "ask". Heredoc bodies go the same way:
// the body of `python3 - <<'PY' … PY` gets chopped up as though it were commands, none of the pieces is
// a safe command, and the answer stays "ask" — which is right for that command anyway.

// Quoting is the one thing that must be read correctly. The bar in `grep -oE 'a|b' f` is not a pipe,
// and splitting on it turns a perfectly ordinary read-only command into two fragments of nonsense that
// then get asked about regardless — which would defeat the point, since grep with a regex is the single
// most common command there is.

// /dev/null is a bit bucket rather than a place: writing there does nothing and reading there returns
// nothing, forever. Both "is this a write" and "is this in bounds" (see segEscapes in tools.go) have to
// know it.
const devNull = "/dev/null"

// eatRedirect handles whatever immediately follows a >, returning how many extra characters to skip.

// `2>&1` merely joins one file descriptor to another and writes no file at all — and like
// `2>/dev/null` it is everyday spelling for a model. The & used to read as a separator, cutting
// `cmd 2>&1 | head` into three segments one of which was a bare "1", not on the whitelist, so the whole
// command needed a human.

// Everything else (a target that is a real filename) is not decided here: the target has to go through
// the ordinary word path to be unquoted properly, so the judgement waits for flush (redirPending).
func eatRedirect(r []rune, i int, cur *shellSeg, redirPending *bool) int {
	if i+1 < len(r) && r[i+1] == '&' {
		j := i + 2
		for j < len(r) && (r[j] == '-' || (r[j] >= '0' && r[j] <= '9')) {
			j++
		}
		return j - 1 - i
	}
	*redirPending = true
	return 0
}

// shellSeg is one segment of a compound command: `a | b && c` has three.
type shellSeg struct {

	// The words with quoting removed. words[0] is the command this segment runs.
	words []string

	// Whether this segment has an unquoted > or >>. Input redirection with < does not count: that reads.
	redirect bool
}

// scanBash reads one command. ok is false when it could not be read (unbalanced quotes and the like),
// and the caller should assume the worst. subst reports command substitution, which makes what the
// command will actually touch undecidable before it runs.
func scanBash(command string) (segs []shellSeg, subst bool, ok bool) {
	var cur shellSeg
	var word strings.Builder
	started := false

	// A redirect counts as a write depending on its target, and the target only appears after the >, so
	// the fact is noted here and settled when the next word lands.
	redirPending := false

	flush := func() {
		if !started {
			return
		}
		w := word.String()
		if redirPending {
			redirPending = false

			// Writing to /dev/null writes nothing. Models reach for this on nearly every command
			// (`… 2>/dev/null` to drop errors nobody wants to read), and counting it as a side effect
			// means every command needs a human.
			if w != devNull {
				cur.redirect = true
			}
		}
		cur.words = append(cur.words, w)
		word.Reset()
		started = false
	}
	endSeg := func() {
		flush()

		// A > with nothing after it: unreadable, so assume the worst
		if redirPending {
			cur.redirect = true
			redirPending = false
		}
		if len(cur.words) > 0 || cur.redirect {
			segs = append(segs, cur)
		}
		cur = shellSeg{}
	}

	r := []rune(command)
	for i := 0; i < len(r); i++ {
		switch c := r[i]; c {
		case '\\':
			if i+1 < len(r) {
				i++
				word.WriteRune(r[i])
				started = true
			}
		case '\'':

			// Nothing is interpreted inside single quotes; the lot goes into the current word as-is
			j := i + 1
			for j < len(r) && r[j] != '\'' {
				j++
			}
			if j >= len(r) {
				return nil, subst, false
			}
			word.WriteString(string(r[i+1 : j]))
			started = true
			i = j
		case '"':
			j := i + 1
			for j < len(r) {
				if r[j] == '\\' {
					j += 2
					continue
				}
				if r[j] == '"' {
					break
				}
				j++
			}
			if j >= len(r) {
				return nil, subst, false
			}
			inner := string(r[i+1 : j])

			// Command substitution still runs inside double quotes
			if strings.Contains(inner, "$(") || strings.Contains(inner, "`") {
				subst = true
			}
			word.WriteString(inner)
			started = true
			i = j
		case '`':
			subst = true
			j := i + 1
			for j < len(r) && r[j] != '`' {
				j++
			}
			if j >= len(r) {
				return nil, subst, false
			}
			i = j
		case '$':
			if i+1 < len(r) && r[i+1] == '(' {
				subst = true
				depth, j := 0, i+1
				for ; j < len(r); j++ {
					if r[j] == '(' {
						depth++
					} else if r[j] == ')' {
						if depth--; depth == 0 {
							break
						}
					}
				}
				if j >= len(r) {
					return nil, subst, false
				}
				i = j
				continue
			}
			word.WriteRune(c)
			started = true
		case ';', '&', '|', '\n':

			// &> is a redirect, not "run in the background" followed by a new segment
			if c == '&' && i+1 < len(r) && r[i+1] == '>' {
				flush()
				i++
				i += eatRedirect(r, i, &cur, &redirPending)
				continue
			}
			endSeg()

			// && and || are single operators, not two splits
			if i+1 < len(r) && r[i+1] == c {
				i++
			}
		case '>':
			flush()
			if i+1 < len(r) && r[i+1] == '>' {
				i++
			}
			i += eatRedirect(r, i, &cur, &redirPending)
		case '<':

			// Reading input is not a side effect. << starts a heredoc, whose body is not parsed separately
			// (see the note at the top of this file).
			flush()
			if i+1 < len(r) && r[i+1] == '<' {
				i++
			}
		case ' ', '\t', '\r':
			flush()
		default:
			word.WriteRune(c)
			started = true
		}
	}
	endSeg()
	return segs, subst, true
}

// The read-only whitelist. The test is "running it changes nothing on this machine" — writing a file,
// making a network request or starting another process each disqualify a command.

// find is not in this table and is judged separately in safeSegment: its first word cannot settle it,
// because -delete and -exec hide among the arguments. xargs, env and awk are refused outright — each can
// summon an arbitrary command out of its arguments, and what that command would be is invisible here.
var safeBashCommands = map[string]bool{
	"ls": true, "cat": true, "head": true, "tail": true, "grep": true,
	"rg": true, "wc": true, "pwd": true, "echo": true, "printf": true,
	"date": true, "du": true, "df": true, "file": true, "diff": true,
	"sort": true, "uniq": true, "cut": true, "tr": true, "nl": true,
	"comm": true, "paste": true, "rev": true, "fold": true, "expand": true,
	"basename": true, "dirname": true, "realpath": true, "readlink": true,
	"stat": true, "shasum": true, "md5": true, "cksum": true,
	"jq": true, "seq": true, "hexdump": true, "xxd": true, "strings": true,
	"uname": true, "whoami": true, "hostname": true, "id": true, "true": true, "false": true,
}

// git's read-only subcommands. git as a whole cannot go on the whitelist: commit, push, checkout and
// clean live right next to these.
var safeGitSubcommands = map[string]bool{
	"status": true, "log": true, "diff": true, "show": true,
	"ls-files": true, "rev-parse": true, "blame": true,
	"shortlog": true, "describe": true, "cat-file": true,
}

// These find arguments act: they delete files, run other commands, or write files. Every other find
// (-type, -name, -maxdepth, -path and friends) is listing a directory, and "what files are in here" is
// among the most common things to ask — cutting find out wholesale only pushes the model into more
// contorted spellings.

// The list names what acts rather than what is allowed: find has dozens of predicates, and an
// allow-list would misfire on every one not thought of, while the acting ones are few and have not
// changed in years.
var findActions = map[string]bool{
	"-delete": true, "-exec": true, "-execdir": true, "-ok": true, "-okdir": true,
	"-fls": true, "-fprint": true, "-fprint0": true, "-fprintf": true,
}

// safeSegment reports whether one segment is read-only.
func safeSegment(words []string) bool {
	if len(words) == 0 {
		return false
	}
	switch words[0] {
	case "sed":

		// sed -i rewrites files in place; without it the result only goes to stdout
		for _, w := range words[1:] {
			if strings.HasPrefix(w, "-i") {
				return false
			}
		}
		return true
	case "git":
		return len(words) > 1 && safeGitSubcommands[words[1]]
	case "find":
		for _, w := range words[1:] {
			if findActions[w] {
				return false
			}
		}
		return true
	}
	return safeBashCommands[words[0]]
}

// bashReadOnly reports whether a whole command merely reads: every segment read-only, no redirect
// anywhere, no command substitution.
func bashReadOnly(segs []shellSeg, subst, ok bool) bool {
	if !ok || subst || len(segs) == 0 {
		return false
	}
	for _, s := range segs {
		if s.redirect || !safeSegment(s.words) {
			return false
		}
	}
	return true
}
