package skill

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

//go:embed bundled/*/SKILL.md
var bundledFS embed.FS

// SeedBundled copies the starter skills into dest when that directory is missing or empty.
// An existing library — even one file the user put there — is never overwritten.
func SeedBundled(dest string) error {
	if dest == "" {
		return nil
	}
	entries, err := os.ReadDir(dest)
	if err == nil && len(entries) > 0 {
		return nil
	}
	names, err := fs.Glob(bundledFS, "bundled/*/SKILL.md")
	if err != nil {
		return err
	}
	for _, name := range names {
		rel := strings.TrimPrefix(name, "bundled/")
		out := filepath.Join(dest, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		raw, err := bundledFS.ReadFile(name)
		if err != nil {
			return err
		}
		if err := os.WriteFile(out, raw, 0o644); err != nil {
			return err
		}
	}
	return nil
}
