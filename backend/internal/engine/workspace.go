package engine

import (
	"botbureau/backend/internal/config"

	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The layout of the workspaces directory lives in this one file: data/workspaces/<id> is a serving
// member's workspace and data/workspaces/<id>.removed-<unix> is a departed member's archive.

// The dot separator is safe: an id is drawn from [a-z0-9_-] (config.ValidBotName) and never contains
// one, so an archive can never collide with a serving member's directory, and scanning the
// directory does not have to cross-check bots.yaml.
const (
	departedSuffix  = ".removed-"
	departedProfile = "departed.json"

	// The cap on entries walked while sizing an archive. A workspace may well contain a node_modules,
	// and the number only feeds a "N files, so many MB" line in settings — not worth walking a disk for.
	statWalkCap = 20000
)

// WorkspaceDir returns the path to a bot's workspace.
func WorkspaceDir(dataDir, name string) string {
	return filepath.Join(dataDir, "workspaces", name)
}

// Departed summarises one archive for the "Former members" pane in settings.
type Departed struct {
	Dir         string `json:"dir"`
	ID          string `json:"id"`
	DisplayName string `json:"display_name,omitempty"`
	Role        string `json:"role,omitempty"`
	RemovedAt   int64  `json:"removed_at"`
	Files       int    `json:"files"`
	Bytes       int64  `json:"bytes"`
	Truncated   bool   `json:"truncated,omitempty"`
	HasMemory   bool   `json:"has_memory"`
}

// DepartedFile is one top-level entry inside an archive.
type DepartedFile struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
	Dir   bool   `json:"dir"`
}

// ArchiveWorkspace marks a departing member's workspace as an archive and returns its new name.

// Renaming rather than leaving it in place: an id now carries a random suffix (wumin-k3f9a) and never
// appears in the UI, so the bare directory name identifies neither who it was nor whether they still
// work here. A departed.json goes in afterwards with the display name and the date — an archive has to
// be legible before keeping it means anything.
func ArchiveWorkspace(dataDir string, cfg config.BotConfig) (string, error) {
	src := WorkspaceDir(dataDir, cfg.Name)
	if _, err := os.Stat(src); err != nil {

		// No workspace (removed before it ever worked) means nothing to archive, which is not an error
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	stamp := time.Now().Unix()
	base := cfg.Name + departedSuffix + strconv.FormatInt(stamp, 10)
	dst := filepath.Join(dataDir, "workspaces", base)

	// The same id cannot be archived twice in one second (ids are unique while serving), but a
	// hand-edited data directory can contain anything
	for i := 2; i < 100; i++ {
		if _, err := os.Stat(dst); errors.Is(err, fs.ErrNotExist) {
			break
		}
		base = fmt.Sprintf("%s%s%d-%d", cfg.Name, departedSuffix, stamp, i)
		dst = filepath.Join(dataDir, "workspaces", base)
	}
	if err := os.Rename(src, dst); err != nil {
		return "", err
	}
	profile := map[string]any{
		"id":           cfg.Name,
		"display_name": cfg.DisplayName,
		"role":         cfg.Role,
		"removed_at":   stamp,
	}

	// A failed write still counts as archived: the directory is already renamed, and a missing caption
	// beats failing the removal outright
	if raw, err := json.MarshalIndent(profile, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(dst, departedProfile), raw, 0o644)
	}
	return base, nil
}

// PurgeWorkspace deletes a serving id's workspace; only reached when the user explicitly asked for it.
func PurgeWorkspace(dataDir, name string) error {
	if !config.ValidBotName(name) {
		return errors.New("invalid bot id")
	}
	return os.RemoveAll(WorkspaceDir(dataDir, name))
}

// ListDeparted scans for archives, most recently departed first.
func ListDeparted(dataDir string) []Departed {
	root := filepath.Join(dataDir, "workspaces")
	entries, err := os.ReadDir(root)
	if err != nil {
		return []Departed{}
	}
	out := []Departed{}
	for _, e := range entries {
		if !e.IsDir() || !validDepartedDir(e.Name()) {
			continue
		}
		d := Departed{Dir: e.Name()}
		d.ID, d.RemovedAt = splitDepartedDir(e.Name())
		dir := filepath.Join(root, e.Name())
		if raw, err := os.ReadFile(filepath.Join(dir, departedProfile)); err == nil {
			var p struct {
				DisplayName string `json:"display_name"`
				Role        string `json:"role"`
				RemovedAt   int64  `json:"removed_at"`
			}
			if json.Unmarshal(raw, &p) == nil {
				d.DisplayName, d.Role = p.DisplayName, p.Role
				if p.RemovedAt > 0 {
					d.RemovedAt = p.RemovedAt
				}
			}
		}
		if st, err := os.Stat(filepath.Join(dir, "MEMORY.md")); err == nil && st.Size() > 0 {
			d.HasMemory = true
		}
		d.Files, d.Bytes, d.Truncated = dirStats(dir)
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RemovedAt > out[j].RemovedAt })
	return out
}

// DepartedDetail returns an archive's memory text and top-level entries for the "View" action.
// Top level only: this is a preview to remind the user what an archive holds, not a file manager.
func DepartedDetail(dataDir, dir string, memCap int) (string, []DepartedFile, bool, error) {
	if !validDepartedDir(dir) {
		return "", nil, false, errors.New("invalid archive")
	}
	root := filepath.Join(dataDir, "workspaces", dir)
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", nil, false, err
	}
	files := []DepartedFile{}
	for _, e := range entries {
		if e.Name() == departedProfile {
			continue
		}
		f := DepartedFile{Name: e.Name(), Dir: e.IsDir()}
		if info, err := e.Info(); err == nil && !e.IsDir() {
			f.Bytes = info.Size()
		}
		files = append(files, f)
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].Dir != files[j].Dir {
			return files[i].Dir
		}
		return files[i].Name < files[j].Name
	})
	mem, truncated := "", false
	if raw, err := os.ReadFile(filepath.Join(root, "MEMORY.md")); err == nil {
		mem = string(raw)
		if memCap > 0 && len(mem) > memCap {
			mem, truncated = mem[:memCap], true
		}
	}
	return mem, files, truncated, nil
}

// DeleteDeparted permanently deletes one archive.
func DeleteDeparted(dataDir, dir string) error {
	if !validDepartedDir(dir) {
		return errors.New("invalid archive")
	}
	return os.RemoveAll(filepath.Join(dataDir, "workspaces", dir))
}

// validDepartedDir reports whether a directory name is an archive. It doubles as the safety boundary
// of the delete endpoint: the name arrives over HTTP, so only the exact shape
// <valid id>.removed-<digits>[-<digits>] is accepted and anything carrying a path separator or ..
// is rejected here, well before it can be joined into a RemoveAll.
func validDepartedDir(dir string) bool {
	if dir == "" || dir != filepath.Base(dir) || strings.ContainsAny(dir, `/\`) || dir == "." || dir == ".." {
		return false
	}
	i := strings.Index(dir, departedSuffix)
	if i <= 0 || !config.ValidBotName(dir[:i]) {
		return false
	}
	for _, part := range strings.Split(dir[i+len(departedSuffix):], "-") {
		if part == "" {
			return false
		}
		if _, err := strconv.ParseInt(part, 10, 64); err != nil {
			return false
		}
	}
	return true
}

func splitDepartedDir(dir string) (id string, removedAt int64) {
	i := strings.Index(dir, departedSuffix)
	if i <= 0 {
		return dir, 0
	}
	id = dir[:i]
	stamp, _, _ := strings.Cut(dir[i+len(departedSuffix):], "-")
	removedAt, _ = strconv.ParseInt(stamp, 10, 64)
	return id, removedAt
}

// dirStats counts files and bytes in an archive, stopping at statWalkCap entries and saying so.
func dirStats(root string) (files int, bytes int64, truncated bool) {
	stop := errors.New("stop")
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip what cannot be read; the count is best-effort
		}
		if d.IsDir() || d.Name() == departedProfile {
			return nil // the caption we wrote is not one of their files
		}
		if files >= statWalkCap {
			return stop
		}
		files++
		if info, err := d.Info(); err == nil {
			bytes += info.Size()
		}
		return nil
	})
	return files, bytes, errors.Is(err, stop)
}
