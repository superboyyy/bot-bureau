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

// 工作目录的布局都在这个文件里：data/workspaces/<id> 是在职成员的工作目录，
// data/workspaces/<id>.removed-<unix> 是离职档案。
//
// 用点号分隔是安全的：id 的字母表是 [a-z0-9_-]（config.ValidBotName），永远不含点，
// 所以离职档案不可能和某位在职成员的目录撞名，扫目录时也不必去比对 bots.yaml。
//
// The layout of the workspaces directory lives in this one file: data/workspaces/<id> is a serving
// member's workspace and data/workspaces/<id>.removed-<unix> is a departed member's archive.
//
// The dot separator is safe: an id is drawn from [a-z0-9_-] (config.ValidBotName) and never contains
// one, so an archive can never collide with a serving member's directory, and scanning the
// directory does not have to cross-check bots.yaml.
const (
	departedSuffix  = ".removed-"
	departedProfile = "departed.json"
	// 统计一份档案时最多走这么多个条目。工作目录里完全可能有个 node_modules，
	// 而这个数字只是拿来在设置里显示"几个文件、多大"，不值得为它把磁盘翻穿。
	// The cap on entries walked while sizing an archive. A workspace may well contain a node_modules,
	// and the number only feeds a "N files, so many MB" line in settings — not worth walking a disk for.
	statWalkCap = 20000
)

// WorkspaceDir 给出某个 bot 的工作目录路径。
// WorkspaceDir returns the path to a bot's workspace.
func WorkspaceDir(dataDir, name string) string {
	return filepath.Join(dataDir, "workspaces", name)
}

// Departed 是一份离职档案的摘要，用于设置里的「已离职成员」。
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

// DepartedFile 是档案里的一个顶层条目。
// DepartedFile is one top-level entry inside an archive.
type DepartedFile struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
	Dir   bool   `json:"dir"`
}

// ArchiveWorkspace 把一位刚离开的成员的工作目录标记为离职档案，返回新的目录名。
//
// 改名而不是原地留着：id 现在是随机后缀（wumin-k3f9a），界面上又从不显示，
// 光看目录名认不出是谁、更认不出他还在不在职。改完名再往里写一份 departed.json，
// 记下显示名和离职时间——档案要能被人读懂，才谈得上"保留"。
//
// ArchiveWorkspace marks a departing member's workspace as an archive and returns its new name.
//
// Renaming rather than leaving it in place: an id now carries a random suffix (wumin-k3f9a) and never
// appears in the UI, so the bare directory name identifies neither who it was nor whether they still
// work here. A departed.json goes in afterwards with the display name and the date — an archive has to
// be legible before keeping it means anything.
func ArchiveWorkspace(dataDir string, cfg config.BotConfig) (string, error) {
	src := WorkspaceDir(dataDir, cfg.Name)
	if _, err := os.Stat(src); err != nil {
		// 没有工作目录（还没干过活就被移除）就没什么可归档的，不是错误
		// No workspace (removed before it ever worked) means nothing to archive, which is not an error
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	stamp := time.Now().Unix()
	base := cfg.Name + departedSuffix + strconv.FormatInt(stamp, 10)
	dst := filepath.Join(dataDir, "workspaces", base)
	// 同一秒里同一个 id 归档两次是撞不上的（id 在职期间唯一），但手改过 yaml 的目录里什么都可能有
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
	// 写不进去也认归档成功：目录已经改好名了，缺一份说明总好过把移除整个判失败
	// A failed write still counts as archived: the directory is already renamed, and a missing caption
	// beats failing the removal outright
	if raw, err := json.MarshalIndent(profile, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(dst, departedProfile), raw, 0o644)
	}
	return base, nil
}

// PurgeWorkspace 删掉某个在职 id 的工作目录，用户明确选了"一并删除资料"时才会走到。
// PurgeWorkspace deletes a serving id's workspace; only reached when the user explicitly asked for it.
func PurgeWorkspace(dataDir, name string) error {
	if !config.ValidBotName(name) {
		return errors.New("invalid bot id")
	}
	return os.RemoveAll(WorkspaceDir(dataDir, name))
}

// ListDeparted 扫出所有离职档案，最近离开的排在前面。
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

// DepartedDetail 返回一份档案的记忆正文和顶层文件清单，供设置里的「查看」用。
// 只列顶层：这是一个让用户想起"这份档案是什么"的预览，不是文件管理器。
//
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

// DeleteDeparted 永久删除一份离职档案。
// DeleteDeparted permanently deletes one archive.
func DeleteDeparted(dataDir, dir string) error {
	if !validDepartedDir(dir) {
		return errors.New("invalid archive")
	}
	return os.RemoveAll(filepath.Join(dataDir, "workspaces", dir))
}

// validDepartedDir 判断一个目录名是不是离职档案。这同时是删除接口的安全边界：
// 目录名从 HTTP 请求里来，所以只认 <合法 id>.removed-<数字>[-<数字>] 这一种形状，
// 任何带路径分隔符或 .. 的东西在这里就被挡掉，不会拼进 RemoveAll。
//
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

// dirStats 数一份档案有多少文件、多大，走满 statWalkCap 个条目就停下并报告截断。
// dirStats counts files and bytes in an archive, stopping at statWalkCap entries and saying so.
func dirStats(root string) (files int, bytes int64, truncated bool) {
	stop := errors.New("stop")
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 读不动的子目录跳过，统计是尽力而为 / skip what cannot be read; the count is best-effort
		}
		if d.IsDir() || d.Name() == departedProfile {
			return nil // 我们自己写的那份说明不算他的文件 / the caption we wrote is not one of their files
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
