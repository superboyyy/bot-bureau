package sandbox

import (
	"os"
	"path/filepath"
)

// GitDirs returns .git directories that sit directly in the given roots.
// Linked worktrees (.git is a file) are skipped: Seatbelt/bwrap can only
// re-bind a directory.
func GitDirs(roots []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range roots {
		if r == "" {
			continue
		}
		g := filepath.Join(r, ".git")
		st, err := os.Stat(g)
		if err != nil || !st.IsDir() {
			continue
		}
		if seen[g] {
			continue
		}
		seen[g] = true
		out = append(out, g)
	}
	return out
}
