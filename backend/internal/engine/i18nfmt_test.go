package engine

import (
	"botbureau/backend/internal/i18n"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// 译文里的占位符必须和原文一一对应，而且顺序一致。
//
// Sprintf 拿到的是同一串参数，译文只换字不换顺序——把 "HTTP %d from %s" 译成 "%s 返回 HTTP %d"，
// 中文用户看到的就是 %!s(int=404)。这种错在英文环境下永远测不出来，所以在这里一次性拦掉。
//
// Placeholders in a translation must match the original one for one, in the same order.
//
// Sprintf receives the same arguments either way: a translation may change the words but never their
// order. Rendering "HTTP %d from %s" as "%s returned HTTP %d" shows Chinese users %!s(int=404), and the
// mistake is invisible in an English environment — so it gets caught here instead.
func TestTranslationsKeepPlaceholderOrder(t *testing.T) {
	raw, err := os.ReadFile("../i18n/locales/zh.json")
	if err != nil {
		t.Fatal(err)
	}
	var table map[string]string
	if err := json.Unmarshal(raw, &table); err != nil {
		t.Fatal(err)
	}
	verb := regexp.MustCompile(`%[-+# 0-9.]*[a-zA-Z]`)
	for en, zh := range table {
		a, b := verb.FindAllString(en, -1), verb.FindAllString(zh, -1)
		if strings.Join(a, ",") != strings.Join(b, ",") {
			t.Errorf("placeholders differ\n  en: %v\n  zh: %v\n  key: %.80q", a, b, en)
		}
	}
	_ = i18n.T
}
