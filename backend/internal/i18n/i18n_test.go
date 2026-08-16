package i18n

import (
	"os"
	"testing"
)

func TestLocaleTranslationAndFallback(t *testing.T) {
	SetLocale("zh")
	if Locale() != "zh" {
		t.Fatalf("Locale() = %q", Locale())
	}
	if got := T("Already bound — just send a message."); got == "Already bound — just send a message." || got == "" {
		t.Fatalf("known Chinese translation = %q", got)
	}
	if got := T("not translated"); got != "not translated" {
		t.Fatalf("fallback = %q", got)
	}
	SetLocale("en")
	if got := T("Already bound — just send a message."); got != "Already bound — just send a message." {
		t.Fatalf("English source = %q", got)
	}
	SetLocale("unsupported")
	if Locale() != "zh" {
		t.Fatalf("unsupported locale should coerce to zh, got %q", Locale())
	}
	SetLocale("en")
}

func TestDetectSystemLocalePrecedence(t *testing.T) {
	for _, key := range []string{"BOTBUREAU_LOCALE", "LC_ALL", "LC_MESSAGES", "LANG"} {
		t.Setenv(key, "")
	}
	if got := DetectSystemLocale(); got != "en" {
		t.Fatalf("empty environment = %q", got)
	}
	t.Setenv("LANG", "zh_CN.UTF-8")
	if got := DetectSystemLocale(); got != "zh" {
		t.Fatalf("LANG = %q", got)
	}
	t.Setenv("LC_ALL", "en_AU.UTF-8")
	if got := DetectSystemLocale(); got != "en" {
		t.Fatalf("LC_ALL precedence = %q", got)
	}
	t.Setenv("BOTBUREAU_LOCALE", "zh-CN")
	if got := DetectSystemLocale(); got != "zh" {
		t.Fatalf("BOTBUREAU_LOCALE precedence = %q", got)
	}
	_ = os.Unsetenv("BOTBUREAU_LOCALE")
}
