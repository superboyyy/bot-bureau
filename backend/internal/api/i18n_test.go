package api

import (
	"botbureau/backend/internal/config"
	"botbureau/backend/internal/i18n"

	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withLocale switches the locale temporarily and restores it afterwards — tests in this package share one process, so no side effect may leak.
func withLocale(t *testing.T, l string, fn func()) {
	t.Helper()
	old := i18n.Locale()
	i18n.SetLocale(l)
	defer i18n.SetLocale(old)
	fn()
}

func TestTranslate(t *testing.T) {
	withLocale(t, "zh", func() {
		if got := i18n.T("Message is empty"); got != "消息为空" {
			t.Fatalf("zh should return Chinese: %q", got)
		}
	})
	withLocale(t, "en", func() {
		if got := i18n.T("Message is empty"); got != "Message is empty" {
			t.Fatalf("en should return English: %q", got)
		}
	})

	// An invalid value falls back to Chinese
	withLocale(t, "fr", func() {
		if got := i18n.T("en"); got != "中" {
			t.Fatalf("an unknown locale should fall back to zh: %q", got)
		}
	})
}

func TestDetectSystemLocale(t *testing.T) {
	for _, k := range []string{"BOTBUREAU_LOCALE", "LC_ALL", "LC_MESSAGES", "LANG"} {
		t.Setenv(k, "")
	}
	t.Setenv("LANG", "zh_CN.UTF-8")
	if got := i18n.DetectSystemLocale(); got != "zh" {
		t.Fatalf("zh_CN should resolve to zh: %q", got)
	}
	t.Setenv("LANG", "en_US.UTF-8")
	if got := i18n.DetectSystemLocale(); got != "en" {
		t.Fatalf("en_US should resolve to en: %q", got)
	}

	// LC_ALL takes precedence over LANG
	t.Setenv("LC_ALL", "zh_TW.UTF-8")
	if got := i18n.DetectSystemLocale(); got != "zh" {
		t.Fatalf("LC_ALL should win: %q", got)
	}

	// The language handed in by the shell outranks the rest: on the desktop it is the only one there
	// is, since LANG and LC_ALL are simply absent
	t.Setenv("BOTBUREAU_LOCALE", "en-AU")
	if got := i18n.DetectSystemLocale(); got != "en" {
		t.Fatalf("BOTBUREAU_LOCALE should win over LC_ALL: %q", got)
	}
}

func TestSettingsLocalePersistence(t *testing.T) {
	old := i18n.Locale()
	defer i18n.SetLocale(old)

	dir := t.TempDir()
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_MESSAGES", "")
	t.Setenv("LANG", "zh_CN.UTF-8")

	// The default "auto" follows the system
	s := config.NewSettings(dir)
	if s.LocalePref != "auto" || i18n.Locale() != "zh" {
		t.Fatalf("auto should follow the system: pref=%s locale=%s", s.LocalePref, i18n.Locale())
	}

	// Explicitly set en and persist it
	if !s.SetLocalePref("en") || i18n.Locale() != "en" {
		t.Fatalf("setting en failed: %s", i18n.Locale())
	}
	raw, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil || !strings.Contains(string(raw), `"en"`) {
		t.Fatalf("settings.json was not persisted: %v %s", err, raw)
	}

	// Reload: the explicit preference overrides the system language
	i18n.SetLocale("zh")
	s2 := config.NewSettings(dir)
	if s2.LocalePref != "en" || i18n.Locale() != "en" {
		t.Fatalf("should still be en after reload: pref=%s locale=%s", s2.LocalePref, i18n.Locale())
	}

	// Invalid values are rejected
	if s2.SetLocalePref("de") {
		t.Fatal("an invalid locale should be rejected")
	}
	if st := s2.Status(); st["locale_pref"] != "en" || st["locale"] != "en" {
		t.Fatalf("wrong Status: %v", st)
	}
}

// Runtime messages follow the locale: the same API error differs between the two languages.
func TestRuntimeMessagesFollowLocale(t *testing.T) {
	app, srv := newTestApp(t)
	defer i18n.SetLocale("en")

	app.settings.SetLocalePref("zh")
	code, out := postJSON(t, srv.URL+"/api/send", map[string]any{"chat": "group", "text": "  "})
	if code != 400 || !strings.Contains(out["error"].(string), "消息为空") {
		t.Fatalf("zh should return a Chinese error: %d %v", code, out)
	}
	app.settings.SetLocalePref("en")
	code, out = postJSON(t, srv.URL+"/api/send", map[string]any{"chat": "group", "text": "  "})
	if code != 400 || !strings.Contains(out["error"].(string), "Message is empty") {
		t.Fatalf("en should return an English error: %d %v", code, out)
	}
}
