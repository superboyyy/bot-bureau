package config

import (
	"botbureau/backend/internal/i18n"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestApprovalTimeoutCanBeReset(t *testing.T) {
	SetApprovalTimeout(3 * time.Second)
	if got := ApprovalTimeout(); got != 3*time.Second {
		t.Fatalf("ApprovalTimeout() = %s, want 3s", got)
	}
	SetApprovalTimeout(0)
	if got := ApprovalTimeout(); got != DefaultApprovalTimeout {
		t.Fatalf("reset ApprovalTimeout() = %s, want %s", got, DefaultApprovalTimeout)
	}
}

func TestValidationAndPermissionRules(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   func(string) bool
		good []string
		bad  []string
	}{
		{name: "bot names", fn: ValidBotName, good: []string{"a", "bot_2", "worker-name", strings.Repeat("a", 24)}, bad: []string{"", "A", "has space", "a.b", strings.Repeat("a", 25)}},
		{name: "effort", fn: ValidEffort, good: []string{"", EffortNone, EffortMinimal, EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax}, bad: []string{"quick", "MAX", "unknown"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, value := range tc.good {
				if !tc.fn(value) {
					t.Errorf("%q should be valid", value)
				}
			}
			for _, value := range tc.bad {
				if tc.fn(value) {
					t.Errorf("%q should be invalid", value)
				}
			}
		})
	}

	for _, tc := range []struct {
		in  string
		out PermLevel
	}{
		{" ASK ", PermAsk}, {"edit", PermEdit}, {"AUTO", PermAuto}, {"full", PermFull}, {"", ""}, {"nope", ""},
	} {
		if got := NormalizePerm(tc.in); got != tc.out {
			t.Errorf("NormalizePerm(%q) = %q, want %q", tc.in, got, tc.out)
		}
	}
	if got := ResolvePerm("", "AUTO"); got != PermAuto {
		t.Fatalf("global permission did not win: %q", got)
	}
	if got := ResolvePerm(" edit ", "full"); got != PermEdit {
		t.Fatalf("bot permission did not win: %q", got)
	}
	if got := ResolvePerm("invalid", "also-invalid"); got != DefaultPerm {
		t.Fatalf("invalid permissions should fall back to %q, got %q", DefaultPerm, got)
	}

	acts := []struct {
		name string
		perm PermLevel
		act  ToolAct
		want bool
	}{
		{"read-only always passes", PermAsk, ToolAct{Kind: ActBash, ReadOnly: true}, false},
		{"ask write", PermAsk, ToolAct{Kind: ActWrite}, true},
		{"edit write", PermEdit, ToolAct{Kind: ActWrite}, false},
		{"auto bash", PermAuto, ToolAct{Kind: ActBash}, false},
		{"edit bash", PermEdit, ToolAct{Kind: ActBash}, true},
		{"auto escape", PermAuto, ToolAct{Kind: ActWrite, Escapes: true}, true},
		{"full escape", PermFull, ToolAct{Kind: ActWrite, Escapes: true}, false},
		{"plugin defaults closed", PermAuto, ToolAct{Kind: ActPlugin}, true},
	}
	for _, tc := range acts {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.perm.NeedsApproval(tc.act); got != tc.want {
				t.Fatalf("NeedsApproval() = %v, want %v", got, tc.want)
			}
		})
	}
	if got := len(PermOptions()); got != 4 {
		t.Fatalf("PermOptions() returned %d options, want 4", got)
	}
}

func TestAvatarAndThinkingBudget(t *testing.T) {
	for _, value := range []string{"", "#aBc123", "data:image/png;base64,abc", "data:image/jpeg;base64,abc", "data:image/webp;base64,abc"} {
		if !ValidAvatar(value) {
			t.Errorf("ValidAvatar(%q) = false", value)
		}
	}
	for _, value := range []string{"#fff", "data:image/gif;base64,abc", "plain text", strings.Repeat("x", AvatarMaxBytes+1)} {
		if ValidAvatar(value) {
			t.Errorf("ValidAvatar(%q) = true", value)
		}
	}

	for _, tc := range []struct {
		effort string
		budget int64
	}{
		{"", 0}, {EffortNone, 0}, {EffortMinimal, 1024}, {EffortLow, 2048}, {EffortMedium, 6144}, {EffortHigh, 12288}, {EffortMax, 0},
	} {
		if got := ThinkingBudget(tc.effort); got != tc.budget {
			t.Errorf("ThinkingBudget(%q) = %d, want %d", tc.effort, got, tc.budget)
		}
	}
}

func TestEffortCatalogIsModelSpecific(t *testing.T) {
	i18n.SetLocale("en")
	if got := EffortProviderFamily(" OpenAI_Compatible "); got != "openai" {
		t.Fatalf("provider family = %q", got)
	}
	if got := EffortProviderFamily("unknown"); got != "" {
		t.Fatalf("unknown provider family = %q", got)
	}

	ids := func(options []map[string]any) []string {
		out := make([]string, 0, len(options))
		for _, option := range options {
			out = append(out, option["id"].(string))
		}
		return out
	}
	if got, want := ids(EffortOptionsForModel("anthropic", "claude-opus-5")), []string{"", EffortLow, EffortMedium, EffortHigh}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Anthropic effort ids = %#v, want %#v", got, want)
	}
	if got, want := ids(EffortOptionsForModel("openai", "gpt-5.6-pro")), []string{"", EffortNone, EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax}; !reflect.DeepEqual(got, want) {
		t.Fatalf("GPT effort ids = %#v, want %#v", got, want)
	}
	if got, want := ids(EffortOptionsForIDs([]string{"low", "low", "bad", "", "high"})), []string{"", EffortLow, EffortHigh}; !reflect.DeepEqual(got, want) {
		t.Fatalf("filtered effort ids = %#v, want %#v", got, want)
	}
	if !EffortSupportedForModel("openai", "gpt-5.6", EffortMax) || EffortSupportedForModel("openai", "unknown", EffortMax) {
		t.Fatal("model-specific effort support is incorrect")
	}
}

func TestBotConfigTextAndPersistence(t *testing.T) {
	i18n.SetLocale("en")
	cfg := BotConfig{Name: "wren", Role: "writer", Description: "base", RoleEn: "copywriter", DescriptionEn: "English description", DisplayName: " Wren ", Provider: "fake"}
	if cfg.Title() != "Wren" || cfg.RoleText() != "copywriter" || cfg.DescText() != "English description" {
		t.Fatalf("English BotConfig text did not prefer English fields: %#v", cfg)
	}
	i18n.SetLocale("zh")
	if cfg.RoleText() != "writer" || cfg.DescText() != "base" {
		t.Fatalf("non-English BotConfig text did not fall back: %q / %q", cfg.RoleText(), cfg.DescText())
	}
	i18n.SetLocale("en")

	dir := t.TempDir()
	path := filepath.Join(dir, "bots.yaml")
	want := []BotConfig{cfg, {Name: "coder", Role: "developer", Provider: "fake"}}
	if err := SaveBotConfigs(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadBotConfigs(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip configs = %#v, want %#v", got, want)
	}
	if missing, err := LoadBotConfigs(filepath.Join(dir, "missing.yaml")); err != nil || missing != nil {
		t.Fatalf("missing config = %#v, %v; want nil, nil", missing, err)
	}

	if err := os.WriteFile(path, []byte("bots:\n  - name: bad name\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBotConfigs(path); err == nil {
		t.Fatal("invalid bot name should fail")
	}
	if err := os.WriteFile(path, []byte("bots:\n  - name: same\n  - name: same\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBotConfigs(path); err == nil {
		t.Fatal("duplicate bot name should fail")
	}
}
