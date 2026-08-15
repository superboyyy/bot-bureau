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
}

func NewSettings(dataDir string) *Settings {
	s := &Settings{path: filepath.Join(dataDir, "settings.json"), LocalePref: "auto", Permission: string(DefaultPerm)}
	if raw, err := os.ReadFile(s.path); err == nil {
		var f struct {
			Locale      string `json:"locale"`
			GroupTitle  string `json:"group_title"`
			GroupAvatar string `json:"group_avatar"`
			Permission  string `json:"permission"`
		}
		if json.Unmarshal(raw, &f) == nil {
			if f.Locale == "zh" || f.Locale == "en" || f.Locale == "auto" {
				s.LocalePref = f.Locale
			}
			s.GroupTitle, s.GroupAvatar = f.GroupTitle, f.GroupAvatar
			if ValidPerm(f.Permission) {
				s.Permission = f.Permission
			}
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

func (s *Settings) saveLocked() {
	out, _ := json.MarshalIndent(map[string]string{
		"locale": s.LocalePref, "group_title": s.GroupTitle, "group_avatar": s.GroupAvatar,
		"permission": s.Permission,
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
