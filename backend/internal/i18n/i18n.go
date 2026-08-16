package i18n

// i18n: user-facing text is written in English in the source, and other languages live in
// locales/*.json keyed by that English text. The code then speaks one language, diffs stay clean, and
// adding a language is one more JSON file. A missing entry falls back to the English source, so an
// untranslated string never renders as a blank.

// Resolution order: explicit setting > system environment (LANG/LC_ALL) > en.

import (
	"embed"
	"encoding/json"
	"os"
	"strings"
	"sync/atomic"
)

//go:embed locales/*.json
var localeFS embed.FS

var (
	currentLocale atomic.Value // string: "zh" | "en"
	localeTable   atomic.Value // English source → translation
)

func init() {
	currentLocale.Store("en")
	localeTable.Store(map[string]string{})
}

// loadLocaleTable loads one language's translations; English is the source itself and needs no table.
func loadLocaleTable(l string) map[string]string {
	if l == "en" {
		return map[string]string{}
	}
	raw, err := localeFS.ReadFile("locales/" + l + ".json")
	if err != nil {
		return map[string]string{}
	}
	var m map[string]string
	if json.Unmarshal(raw, &m) != nil {
		return map[string]string{}
	}
	return m
}

// T returns the text for the current locale, falling back to the English source.
func T(en string) string {
	if m, _ := localeTable.Load().(map[string]string); m != nil {
		if v, ok := m[en]; ok && v != "" {
			return v
		}
	}
	return en
}

// Locale returns the effective locale ("zh" or "en").
func Locale() string {
	s, _ := currentLocale.Load().(string)
	return s
}

func SetLocale(l string) {
	if l != "en" {
		l = "zh"
	}
	currentLocale.Store(l)
	localeTable.Store(loadLocaleTable(l))
}

// detectSystemLocale guesses the locale from the environment: anything containing zh means Chinese, otherwise English.

// BOTBUREAU_LOCALE comes first because the LANG/LC_ALL route does not work on the desktop at all: a GUI
// process launched from an icon carries no language variables, so the engine always landed on English.
// The shell around it — the Electron main process — does know the system language, and passes it in at
// spawn time.
func DetectSystemLocale() string {
	for _, k := range []string{"BOTBUREAU_LOCALE", "LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := strings.ToLower(os.Getenv(k)); v != "" {
			if strings.Contains(v, "zh") {
				return "zh"
			}
			return "en"
		}
	}
	return "en"
}
