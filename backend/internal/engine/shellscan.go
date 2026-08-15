package engine

import "strings"

// bash 命令的静态判读：把一条命令拆成若干段，认出每段的词、以及有没有未加引号的重定向。
//
// 为什么要拆。原来判"这条命令是不是只读"看的是"含不含 shell 元字符"，于是
// `grep -oE '…' f | head -80` 因为有个管道就被判成有副作用，每跑一次都要人点一次头。
// 可管道本身不产生副作用——产生副作用的是重定向，和命令自己。Claude Code 那边是先按
// shell 操作符把复合命令拆开、每一段各自判定，这里照做：整条命令只读，当且仅当每一段都只读。
//
// 拆不准只会更严，不会更松。多切出来的段，段首词多半不在白名单里，整条就落回"要问"。
// heredoc 的正文也是这样：`python3 - <<'PY' … PY` 的正文会被当成命令乱切一气，
// 而切出来的每一段都不是白名单命令，结论仍然是"要问"——这条命令本来也该问。
//
// 引号是唯一必须认准的东西。`grep -oE 'a|b' f` 里那个竖线不是管道，按它切开会把一条正常的
// 只读命令切成两段乱码、然后照样要问，这个功能就白做了——而带正则的 grep 恰恰是最常见的命令。
//
// Static reading of a bash command: split it into segments and pick out each segment's words and
// whether it carries an unquoted redirect.
//
// Why split at all. "Is this command read-only" used to be answered by "does it contain a shell
// metacharacter", so `grep -oE '…' f | head -80` counted as having side effects because of one pipe,
// and needed a human every single time. A pipe has no side effects of its own — redirects do, and so do
// the commands themselves. Claude Code splits a compound command on shell operators and judges each
// part; this does the same: the whole is read-only exactly when every segment is.
//
// Splitting badly can only be stricter, never looser. Spurious segments tend to start with a word that
// is not on the safe list, which puts the whole command back to "ask". Heredoc bodies go the same way:
// the body of `python3 - <<'PY' … PY` gets chopped up as though it were commands, none of the pieces is
// a safe command, and the answer stays "ask" — which is right for that command anyway.
//
// Quoting is the one thing that must be read correctly. The bar in `grep -oE 'a|b' f` is not a pipe,
// and splitting on it turns a perfectly ordinary read-only command into two fragments of nonsense that
// then get asked about regardless — which would defeat the point, since grep with a regex is the single
// most common command there is.

// shellSeg 是复合命令里的一段：`a | b && c` 有三段。
// shellSeg is one segment of a compound command: `a | b && c` has three.
type shellSeg struct {
	// 去掉引号之后的词。words[0] 是这一段要跑的命令。
	// The words with quoting removed. words[0] is the command this segment runs.
	words []string
	// 这一段里有没有未加引号的 > 或 >>。输入重定向 < 不算——那是读。
	// Whether this segment has an unquoted > or >>. Input redirection with < does not count: that reads.
	redirect bool
}

// scanBash 读一条命令。ok 为假表示没读明白（引号没闭合之类），调用方应当按最坏情况处理。
// subst 为真表示命令里有命令替换，那意味着它真正要碰什么在跑之前根本无法确定。
//
// scanBash reads one command. ok is false when it could not be read (unbalanced quotes and the like),
// and the caller should assume the worst. subst reports command substitution, which makes what the
// command will actually touch undecidable before it runs.
func scanBash(command string) (segs []shellSeg, subst bool, ok bool) {
	var cur shellSeg
	var word strings.Builder
	started := false

	flush := func() {
		if started {
			cur.words = append(cur.words, word.String())
			word.Reset()
			started = false
		}
	}
	endSeg := func() {
		flush()
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
			// 单引号里什么都不解释，整段原样进当前的词
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
			// 双引号里的命令替换照样会执行
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
			endSeg()
			// && 和 || 是一个操作符，别当成两次切分
			// && and || are single operators, not two splits
			if i+1 < len(r) && r[i+1] == c {
				i++
			}
		case '>':
			flush()
			cur.redirect = true
			if i+1 < len(r) && r[i+1] == '>' {
				i++
			}
		case '<':
			// 读输入不算副作用。<< 是 heredoc，正文不单独解析（见文件头）。
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

// 只读命令白名单。判据是"跑它不会改变这台机器上的任何东西"——写文件、发网络请求、
// 起别的进程，任何一条沾上就不算。
//
// find 不在名单里：它自带 -delete 和 -exec，无需任何元字符就能绕过审批门。
// xargs、env、awk 同理，都能从参数里把别的命令拉起来。
//
// The read-only whitelist. The test is "running it changes nothing on this machine" — writing a file,
// making a network request or starting another process each disqualify a command.
//
// find is not on the list: its built-in -delete and -exec walk past the approval gate without needing a
// metacharacter. xargs, env and awk are out for the same reason — each can summon another command out
// of its arguments.
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

// git 的只读子命令。整个 git 不能上白名单——commit/push/checkout/clean 就在旁边。
// git's read-only subcommands. git as a whole cannot go on the whitelist: commit, push, checkout and
// clean live right next to these.
var safeGitSubcommands = map[string]bool{
	"status": true, "log": true, "diff": true, "show": true,
	"ls-files": true, "rev-parse": true, "blame": true,
	"shortlog": true, "describe": true, "cat-file": true,
}

// safeSegment 判断一段命令是不是只读。
// safeSegment reports whether one segment is read-only.
func safeSegment(words []string) bool {
	if len(words) == 0 {
		return false
	}
	switch words[0] {
	case "sed":
		// sed -i 是就地改写文件；不带 -i 就只是把结果打到 stdout
		// sed -i rewrites files in place; without it the result only goes to stdout
		for _, w := range words[1:] {
			if strings.HasPrefix(w, "-i") {
				return false
			}
		}
		return true
	case "git":
		return len(words) > 1 && safeGitSubcommands[words[1]]
	}
	return safeBashCommands[words[0]]
}

// bashReadOnly 判断整条命令是不是纯读：每一段都只读，没有重定向，没有命令替换。
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
