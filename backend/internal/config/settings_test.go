package config

import (
	"botbureau/backend/internal/i18n"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSettingsPersistAndDedupe(t *testing.T) {
	i18n.SetLocale("en")
	dir := t.TempDir()
	s := NewSettings(dir)
	if s.Perm() != string(DefaultPerm) || s.LocalePref != "auto" {
		t.Fatalf("defaults = %#v", s.Status())
	}
	if s.SetLocalePref("invalid") || s.SetPermission("invalid") {
		t.Fatal("invalid settings should be rejected")
	}
	if !s.SetLocalePref("en") || !s.SetPermission(string(PermAuto)) {
		t.Fatal("valid settings were rejected")
	}
	title, avatar := "  War room ", "#123456"
	s.SetGroupMeta(&title, &avatar)
	if !s.SetPinned("group", true) || s.SetPinned("group", true) {
		t.Fatal("pin idempotence is wrong")
	}
	if !s.SetPinned(" dm:wren ", true) || !s.SetPinned("group", false) || s.SetPinned("group", false) {
		t.Fatal("pin/unpin behavior is wrong")
	}
	if s.SetPinned("", true) {
		t.Fatal("blank chat should not be pinnable")
	}
	if got, want := s.Pins(), []string{"dm:wren"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("pins = %#v, want %#v", got, want)
	}

	path := filepath.Join(dir, "settings.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	reloaded := NewSettings(dir)
	if reloaded.Perm() != string(PermAuto) || reloaded.GroupTitle != "War room" || reloaded.GroupAvatar != avatar {
		t.Fatalf("reloaded settings = %#v", reloaded.Status())
	}
	if got, want := reloaded.Pins(), []string{"dm:wren"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reloaded pins = %#v, want %#v", got, want)
	}
}

func TestSettingsFetchHostsPersist(t *testing.T) {
	i18n.SetLocale("en")
	dir := t.TempDir()
	s := NewSettings(dir)
	s.SetFetchHosts([]string{"https://GitHub.com/foo", "github.com", "golang.org/pkg", ""})
	if got, want := s.FetchHosts(), []string{"github.com", "golang.org"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized = %#v, want %#v", got, want)
	}
	st, _ := s.Status()["fetch_hosts"].([]string)
	if !reflect.DeepEqual(st, []string{"github.com", "golang.org"}) {
		t.Fatalf("status = %#v", s.Status()["fetch_hosts"])
	}
	reloaded := NewSettings(dir)
	if got, want := reloaded.FetchHosts(), []string{"github.com", "golang.org"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reloaded = %#v, want %#v", got, want)
	}
	s.SetFetchHosts(nil)
	if n := len(NewSettings(dir).FetchHosts()); n != 0 {
		t.Fatalf("clearing should persist an empty list, got %d", n)
	}
}

func TestSettingsSandboxPersistAndOmit(t *testing.T) {
	i18n.SetLocale("en")
	dir := t.TempDir()
	s := NewSettings(dir)
	if !s.SandboxEnabled() || s.SandboxAutoAllowBash() || !s.SandboxAllowUnsandboxed() {
		t.Fatalf("defaults: enabled=%v auto=%v hatch=%v", s.SandboxEnabled(), s.SandboxAutoAllowBash(), s.SandboxAllowUnsandboxed())
	}
	off, on := false, true
	s.SetSandbox(&off, &on, &off)
	if s.SandboxEnabled() || !s.SandboxAutoAllowBash() || s.SandboxAllowUnsandboxed() {
		t.Fatal("SetSandbox did not stick")
	}
	reloaded := NewSettings(dir)
	if reloaded.SandboxEnabled() || !reloaded.SandboxAutoAllowBash() || reloaded.SandboxAllowUnsandboxed() {
		t.Fatalf("reloaded sandbox = %#v", reloaded.Status()["sandbox"])
	}
	// Partial update must not wipe the other knobs.
	s.SetSandbox(&on, nil, nil)
	if !s.SandboxEnabled() || !s.SandboxAutoAllowBash() || s.SandboxAllowUnsandboxed() {
		t.Fatalf("partial update wiped siblings: %#v", s.Status()["sandbox"])
	}
}

func TestSettingsIgnoreInvalidPersistedValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"locale":"fr","permission":"danger","pinned":["", "group", "group", " dm:a "]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewSettings(dir)
	if s.LocalePref != "auto" || s.Perm() != string(DefaultPerm) {
		t.Fatalf("invalid persisted values were accepted: %#v", s.Status())
	}
	if got, want := s.Pins(), []string{"group", "dm:a"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("deduped pins = %#v, want %#v", got, want)
	}
}
