package engine

import (
	"botbureau/backend/internal/config"
	"botbureau/backend/internal/i18n"

	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var skipWalkNames = map[string]bool{
	".git":         true,
	"node_modules": true,
	".svn":         true,
	".hg":          true,
}

func (t *Toolbox) runGrep(pattern, path, globPat string, max int, haveMax bool) (string, bool) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return i18n.T("pattern is empty"), true
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return i18n.T("not a valid regular expression: ") + err.Error(), true
	}
	if !haveMax || max <= 0 {
		max = config.GrepMaxMatches
	}
	var globRe *regexp.Regexp
	if globPat != "" {
		globRe, err = globRegexp(globPat)
		if err != nil {
			return i18n.T("not a valid glob: ") + globPat, true
		}
	}
	roots, err := t.searchRoots(path)
	if err != nil {
		return err.Error(), true
	}
	var hits []string
	truncated := false
	walked := 0
	for _, root := range roots {
		info, statErr := os.Stat(root)
		if statErr != nil {
			continue
		}
		if !info.IsDir() {
			if globRe != nil && !globRe.MatchString(filepath.Base(root)) {
				continue
			}
			hits = append(hits, grepFile(root, displayPath(t.workspace, root), re, max-len(hits))...)
			if len(hits) >= max {
				truncated = true
				break
			}
			continue
		}
		err := t.walkFiles(root, func(abs, rel string) error {
			walked++
			if walked > config.SearchWalkCap {
				truncated = true
				return fs.SkipAll
			}
			if globRe != nil && !globRe.MatchString(filepath.ToSlash(rel)) {
				return nil
			}
			left := max - len(hits)
			if left <= 0 {
				truncated = true
				return fs.SkipAll
			}
			hits = append(hits, grepFile(abs, displayPath(t.workspace, abs), re, left)...)
			if len(hits) >= max {
				truncated = true
				return fs.SkipAll
			}
			return nil
		})
		if err != nil {
			return i18n.T("Search failed: ") + err.Error(), true
		}
		if truncated {
			break
		}
	}
	if len(hits) == 0 {
		return i18n.T("No matches"), false
	}
	out := strings.Join(hits, "\n")
	if truncated {
		out += i18n.T("\n...(matches truncated)")
	}
	return truncateOutput(out), false
}

func (t *Toolbox) runGlob(pattern string) (string, bool) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return i18n.T("pattern is empty"), true
	}
	re, err := globRegexp(pattern)
	if err != nil {
		return i18n.T("not a valid glob: ") + pattern, true
	}
	roots, err := t.searchRoots("")
	if err != nil {
		return err.Error(), true
	}
	var found []string
	truncated := false
	walked := 0
	for _, root := range roots {
		err := t.walkFiles(root, func(abs, rel string) error {
			walked++
			if walked > config.SearchWalkCap {
				truncated = true
				return fs.SkipAll
			}
			slash := filepath.ToSlash(rel)
			if !re.MatchString(slash) && !re.MatchString(filepath.Base(abs)) {
				return nil
			}
			found = append(found, displayPath(t.workspace, abs))
			if len(found) >= config.GlobMaxResults {
				truncated = true
				return fs.SkipAll
			}
			return nil
		})
		if err != nil {
			return i18n.T("Search failed: ") + err.Error(), true
		}
		if truncated {
			break
		}
	}
	if len(found) == 0 {
		return i18n.T("No files matched"), false
	}
	out := strings.Join(found, "\n")
	if truncated {
		out += i18n.T("\n...(matches truncated)")
	}
	return truncateOutput(out), false
}

// searchRoots is the directories (or the one file) grep/glob may walk. An explicit path is
// resolved and must sit in bounds. With no path, that is the member's workspace plus any
// directory the user named — not the whole of /tmp.
func (t *Toolbox) searchRoots(path string) ([]string, error) {
	path = strings.TrimSpace(path)
	if path != "" {
		p, err := t.resolve(path)
		if err != nil {
			return nil, err
		}
		return []string{p}, nil
	}
	roots := []string{t.workspace}
	for _, d := range t.roots.List() {
		if d != "" && d != t.workspace {
			roots = append(roots, d)
		}
	}
	return roots, nil
}

func (t *Toolbox) walkFiles(root string, fn func(abs, rel string) error) error {
	return filepath.WalkDir(root, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipWalkNames[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !t.inBounds(abs) {
			return nil
		}
		rel, relErr := filepath.Rel(root, abs)
		if relErr != nil {
			rel = d.Name()
		}
		return fn(abs, rel)
	})
}

func grepFile(abs, shown string, re *regexp.Regexp, left int) []string {
	if left <= 0 {
		return nil
	}
	info, err := os.Stat(abs)
	if err != nil || info.Size() > config.SearchFileMaxBytes {
		return nil
	}
	raw, err := os.ReadFile(abs)
	if err != nil || bytes.IndexByte(raw, 0) >= 0 {
		return nil
	}
	var hits []string
	for i, line := range splitFileLines(string(raw)) {
		if !re.MatchString(line) {
			continue
		}
		hits = append(hits, fmt.Sprintf("%s:%d:%s", shown, i+1, line))
		if len(hits) >= left {
			break
		}
	}
	return hits
}

func displayPath(workspace, abs string) string {
	if rel, err := filepath.Rel(workspace, abs); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return abs
}

func globRegexp(pat string) (*regexp.Regexp, error) {
	pat = filepath.ToSlash(strings.TrimSpace(pat))
	var b strings.Builder
	b.WriteByte('^')
	for i := 0; i < len(pat); {
		if strings.HasPrefix(pat[i:], "**") {
			i += 2
			if i < len(pat) && pat[i] == '/' {
				i++
				b.WriteString("(?:.*/)?")
			} else {
				b.WriteString(".*")
			}
			continue
		}
		c := pat[i]
		i++
		switch c {
		case '*':
			b.WriteString("[^/]*")
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteByte('$')
	return regexp.Compile(b.String())
}
