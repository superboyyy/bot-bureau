package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"botbureau/backend/internal/i18n"
)

// Settings holds the engine's persisted settings (data/settings.json).
type Settings struct {
	path string
	mu   sync.Mutex
	// "auto" | "zh" | "en"
	LocalePref string `json:"locale"`

	// The group chat's display name and avatar; empty means the defaults ("Group chat" + stacked member faces)
	GroupTitle  string `json:"group_title"`
	GroupAvatar string `json:"group_avatar"`
	// global permission tier, used when a bot sets none
	Permission string `json:"permission"`

	// Pinned conversation ids: "group" / "g_xxxxxxxx" for groups, "dm:<bot>" for DMs.

	// Kept in the engine rather than in browser storage: pinning says "these conversations matter most
	// to this team", and connecting from another device should find the same ones on top. How bright a
	// screen is stays that device's own business (see THEME_KEY in the renderer).

	// The order here does not drive sorting — pinned rows are still ordered by their last activity,
	// the same rule as the rest of the list — so this is a set that happens to be stored as an array.
	Pinned []string `json:"pinned"`

	// Hostnames fetch_url and web_search may open. Empty means today's public-internet policy
	// (still no loopback or private addresses). Non-empty means only these hosts, exact match.
	fetchHosts []string
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
			FetchHosts  []string `json:"fetch_hosts"`
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
			s.fetchHosts = NormalizeFetchHosts(f.FetchHosts)
		}
	}
	s.apply()
	return s
}

// apply resolves the preference into the effective locale.
func (s *Settings) apply() {
	if s.LocalePref == "auto" {
		i18n.SetLocale(i18n.DetectSystemLocale())
		return
	}
	i18n.SetLocale(s.LocalePref)
}

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

// Perm reads the current global tier (concurrency-safe: every tool call asks).
func (s *Settings) Perm() string {
	if s == nil {
		return string(DefaultPerm)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Permission
}

// FetchHosts is a copy of the fetch/search hostname allowlist. Empty means any public host.
func (s *Settings) FetchHosts() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.fetchHosts))
	copy(out, s.fetchHosts)
	return out
}

// SetFetchHosts replaces the allowlist (normalized) and persists it.
func (s *Settings) SetFetchHosts(hosts []string) {
	s.mu.Lock()
	s.fetchHosts = NormalizeFetchHosts(hosts)
	s.saveLocked()
	s.mu.Unlock()
}

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

// SetPinned pins or unpins a conversation and reports whether anything actually changed.

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
	hosts := s.fetchHosts
	if hosts == nil {
		hosts = []string{}
	}
	out, _ := json.MarshalIndent(map[string]any{
		"locale": s.LocalePref, "group_title": s.GroupTitle, "group_avatar": s.GroupAvatar,
		"permission": s.Permission, "pinned": pinned, "fetch_hosts": hosts,
	}, "", "  ")
	_ = os.MkdirAll(filepath.Dir(s.path), 0o755)
	_ = os.WriteFile(s.path, out, 0o644)
}

func (s *Settings) Status() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	hosts := append([]string{}, s.fetchHosts...)
	return map[string]any{
		"locale_pref": s.LocalePref, "locale": i18n.Locale(),
		"group_title": s.GroupTitle, "group_avatar": s.GroupAvatar,
		"permission":  s.Permission,
		"fetch_hosts": hosts,
	}
}
