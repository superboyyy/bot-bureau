package skill

import (
	"embed"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed bundled
var bundledFS embed.FS

// SeedBundled copies starter skills into dest. A skill directory that already
// exists is left alone (including user edits). Starters that are missing are
// added, so a team that already has edit-code still receives pdf later.
//
// A .keep file in dest opts out of seeding entirely — tests use this to keep
// an empty library.
func SeedBundled(dest string) error {
	if dest == "" {
		return nil
	}
	if _, err := os.Stat(filepath.Join(dest, ".keep")); err == nil {
		return nil
	}
	names, err := fs.Glob(bundledFS, "bundled/*/SKILL.md")
	if err != nil {
		return err
	}
	for _, name := range names {
		rel := strings.TrimPrefix(name, "bundled/")
		if err := seedOne(dest, path.Dir(rel)); err != nil {
			return err
		}
	}
	return nil
}

func seedOne(dest, name string) error {
	outDir := filepath.Join(dest, name)
	if _, err := os.Stat(filepath.Join(outDir, "SKILL.md")); err == nil {
		return nil
	}
	srcDir := "bundled/" + name
	return fs.WalkDir(bundledFS, srcDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == srcDir {
			return os.MkdirAll(outDir, 0o755)
		}
		rel := strings.TrimPrefix(p, srcDir+"/")
		out := filepath.Join(outDir, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		raw, err := bundledFS.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		return os.WriteFile(out, raw, 0o644)
	})
}

// BundledNames is the sorted starter-skill list shipped in this binary.
func BundledNames() []string {
	names, err := fs.Glob(bundledFS, "bundled/*/SKILL.md")
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, path.Base(path.Dir(n)))
	}
	sort.Strings(out)
	return out
}
