package i18n

// 国际化：源码里的用户可见文案一律写英文原文，其它语言放在 locales/*.json 里按原文查表。
// 这样代码只有一种语言、diff 干净，加一门语言只要多一个 json 文件；查不到就原样返回英文，
// 漏翻译不会变成空白界面。
//
// i18n: user-facing text is written in English in the source, and other languages live in
// locales/*.json keyed by that English text. The code then speaks one language, diffs stay clean, and
// adding a language is one more JSON file. A missing entry falls back to the English source, so an
// untranslated string never renders as a blank.
//
// 语言取值：设置里的显式选择 > 系统环境（LANG/LC_ALL）> en。
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
	localeTable   atomic.Value // map[string]string，英文原文 → 译文 / English source → translation
)

func init() {
	currentLocale.Store("en")
	localeTable.Store(map[string]string{})
}

// loadLocaleTable 载入某种语言的译文表；英文就是源码本身，无需查表。
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

// T 按当前语言返回文案；查不到就返回英文原文。
// T returns the text for the current locale, falling back to the English source.
func T(en string) string {
	if m, _ := localeTable.Load().(map[string]string); m != nil {
		if v, ok := m[en]; ok && v != "" {
			return v
		}
	}
	return en
}

// Locale 返回当前生效的语言（"zh" 或 "en"）。
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

// detectSystemLocale 从环境变量猜测语言：含 zh 视为中文，其余按英文。
//
// BOTBUREAU_LOCALE 排在最前，因为 LANG/LC_ALL 这条路在桌面端根本走不通：从图标启动的
// GUI 进程一个语言变量都没有，引擎于是一律判成英文，而外壳（Electron 主进程）是知道系统
// 语言的——所以由它 spawn 时递进来。
//
// detectSystemLocale guesses the locale from the environment: anything containing zh means Chinese, otherwise English.
//
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
