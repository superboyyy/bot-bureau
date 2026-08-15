package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"botbureau/backend/internal/i18n"
)

// Settings 是引擎的持久化设置（data/settings.json）。
// Settings holds the engine's persisted settings (data/settings.json).
type Settings struct {
	path string
	mu   sync.Mutex
	// "auto" | "zh" | "en"
	LocalePref string `json:"locale"`
	// 群聊的显示名与头像，留空则用默认（"群聊" + 成员头像叠放）
	// The group chat's display name and avatar; empty means the defaults ("Group chat" + stacked member faces)
	GroupTitle  string `json:"group_title"`
	GroupAvatar string `json:"group_avatar"`
	// 全局权限档位，单个 bot 没设时用它 / global permission tier, used when a bot sets none
	Permission string `json:"permission"`
	// 置顶的会话 id：群是 "group" / "g_xxxxxxxx"，私聊是 "dm:<bot>"。
	//
	// 存在引擎而不是浏览器本地：置顶说的是"这几个会话对这套人马最要紧"，换台设备连上来
	// 该还是那几个；深浅色那种"这块屏幕多亮"才是设备自己的事（见渲染器里的 THEME_KEY）。
	//
	// 表里的顺序不参与排序——置顶区内部仍按最后动静排，和列表其余部分同一个规矩，
	// 所以这里只是一个存成数组的集合。
	//
	// Pinned conversation ids: "group" / "g_xxxxxxxx" for groups, "dm:<bot>" for DMs.
	//
	// Kept in the engine rather than in browser storage: pinning says "these conversations matter most
	// to this team", and connecting from another device should find the same ones on top. How bright a
	// screen is stays that device's own business (see THEME_KEY in the renderer).
	//
	// The order here does not drive sorting — pinned rows are still ordered by their last activity,
	// the same rule as the rest of the list — so this is a set that happens to be stored as an array.
	Pinned []string `json:"pinned"`
}

func NewSettings(dataDir string) *Settings {
	s := &Settings{path: filepath.Join(dataDir, "settings.json"), LocalePref: "auto", Permission: string(DefaultPerm)}
	if raw, err := os.ReadFile(s.path); err == nil {
		var f struct {
			Locale      string   `json:"locale"`
			GroupTitle  string   `json:"group_title"`
			GroupAvatar string   `json:"group_avatar"`
			Permission  string   `json:"permission"`
			Pinned      []string `json:"pinned"`
		}
		if json.Unmarshal(raw, &f) == nil {
			if f.Locale == "zh" || f.Locale == "en" || f.Locale == "auto" {
				s.LocalePref = f.Locale
			}
			s.GroupTitle, s.GroupAvatar = f.GroupTitle, f.GroupAvatar
			if ValidPerm(f.Permission) {
				s.Permission = f.Permission
			}
			s.Pinned = dedupePins(f.Pinned)
		}
	}
	s.apply()
	return s
}

// apply 把偏好解析成生效语言。
// apply resolves the preference into the effective locale.
func (s *Settings) apply() {
	if s.LocalePref == "auto" {
		i18n.SetLocale(i18n.DetectSystemLocale())
		return
	}
	i18n.SetLocale(s.LocalePref)
}

// SetLocalePref 更新语言偏好并持久化；pref 非法时返回 false。
// SetLocalePref updates the language preference and persists it; returns false when pref is invalid.
func (s *Settings) SetLocalePref(pref string) bool {
	if pref != "auto" && pref != "zh" && pref != "en" {
		return false
	}
	s.mu.Lock()
	s.LocalePref = pref
	s.apply()
	s.saveLocked()
	s.mu.Unlock()
	return true
}

// SetGroupMeta 改群聊的显示名和头像；两个参数都为 nil 时什么也不做。
// SetGroupMeta updates the group chat's display name and avatar; a nil argument leaves that field alone.
func (s *Settings) SetGroupMeta(title, avatar *string) {
	s.mu.Lock()
	if title != nil {
		s.GroupTitle = strings.TrimSpace(*title)
	}
	if avatar != nil {
		s.GroupAvatar = *avatar
	}
	s.saveLocked()
	s.mu.Unlock()
}

// SetPermission 改全局权限档位；非法值返回 false。
// SetPermission updates the global permission tier; an invalid value returns false.
func (s *Settings) SetPermission(level string) bool {
	if !ValidPerm(level) {
		return false
	}
	s.mu.Lock()
	s.Permission = level
	s.saveLocked()
	s.mu.Unlock()
	return true
}

// Perm 读当前全局档位（并发安全：bot 每次调工具都会问一次）。
// Perm reads the current global tier (concurrency-safe: every tool call asks).
func (s *Settings) Perm() string {
	if s == nil {
		return string(DefaultPerm)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Permission
}

// Pins 返回置顶的会话 id（副本，调用方随便改）。
// Pins returns the pinned conversation ids (a copy the caller may keep).
func (s *Settings) Pins() []string {
	if s == nil {
		return []string{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.Pinned))
	copy(out, s.Pinned)
	return out
}

// SetPinned 置顶或取消置顶一个会话，返回这次调用有没有真的改动。
//
// 取消不校验会话还在不在：删掉 bot 或群时正是靠它把留下的置顶项摘掉，
// 那时候会话已经不存在了。
//
// SetPinned pins or unpins a conversation and reports whether anything actually changed.
//
// Unpinning does not check that the conversation still exists: deleting a bot or a group calls this to
// strip the pin it left behind, and by then the conversation is gone.
func (s *Settings) SetPinned(chat string, pinned bool) bool {
	chat = strings.TrimSpace(chat)
	if chat == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := -1
	for i, c := range s.Pinned {
		if c == chat {
			idx = i
			break
		}
	}
	if pinned == (idx >= 0) {
		return false
	}
	if pinned {
		s.Pinned = append(s.Pinned, chat)
	} else {
		s.Pinned = append(s.Pinned[:idx], s.Pinned[idx+1:]...)
	}
	s.saveLocked()
	return true
}

// dedupePins 去重并丢掉空串，保持首次出现的顺序。
// dedupePins drops blanks and repeats, keeping first-seen order.
func dedupePins(ids []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func (s *Settings) saveLocked() {
	pinned := s.Pinned
	if pinned == nil {
		pinned = []string{}
	}
	out, _ := json.MarshalIndent(map[string]any{
		"locale": s.LocalePref, "group_title": s.GroupTitle, "group_avatar": s.GroupAvatar,
		"permission": s.Permission, "pinned": pinned,
	}, "", "  ")
	_ = os.MkdirAll(filepath.Dir(s.path), 0o755)
	_ = os.WriteFile(s.path, out, 0o644)
}

func (s *Settings) Status() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{
		"locale_pref": s.LocalePref, "locale": i18n.Locale(),
		"group_title": s.GroupTitle, "group_avatar": s.GroupAvatar,
		"permission": s.Permission,
	}
}
