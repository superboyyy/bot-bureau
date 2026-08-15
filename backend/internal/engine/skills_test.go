package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"botbureau/backend/internal/config"
)

func writeTestSkill(t *testing.T, dataDir, name, desc, body string) {
	t.Helper()
	dir := filepath.Join(dataDir, "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nname: " + name + "\ndescription: " + desc + "\n---\n\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
}

// 系统提示里只应出现技能的名字和描述，正文不该在里面——两段式加载的全部意义就在这儿：
// 装几十个技能只多几十行提示词。这条断言一旦挂了，说明上下文正在被白白烧掉。
//
// Only a skill's name and description belong in the system prompt, never its body — that is the whole
// point of two-stage loading: fifty installed skills cost fifty lines of prompt. If this assertion ever
// fails, context is being burned for nothing.
func TestSystemPromptListsSkillsWithoutBodies(t *testing.T) {
	dataDir := t.TempDir()
	writeTestSkill(t, dataDir, "release-notes",
		"Turn merged pull requests into release notes.",
		"SECRET_BODY_MARKER: group by impact, lead with a verb.")

	deps := newTestDeps(t, dataDir)
	deps.Skills.Rescan()
	bus := NewBus()
	sched := NewScheduler(bus, filepath.Join(dataDir, "routines.json"))
	w, err := NewBotWorker(config.BotConfig{Name: "a", Role: "test", Provider: "fake"}, bus, sched, dataDir, deps)
	if err != nil {
		t.Fatal(err)
	}
	bus.Register(w)

	prompt := w.systemPrompt("group")
	if !strings.Contains(prompt, "release-notes") {
		t.Fatalf("the skill name should be listed:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Turn merged pull requests into release notes.") {
		t.Fatal("the description is what the model matches on, so it must be present")
	}
	if strings.Contains(prompt, "SECRET_BODY_MARKER") {
		t.Fatal("the body must NOT be in the system prompt — it is loaded on demand via read_skill")
	}

	// 工具要出现，而且参数只允许已装的技能名
	// The tool must be offered, and its argument restricted to the installed skill names
	var found bool
	for _, d := range w.Toolbox().Defs() {
		if d.Name != "read_skill" {
			continue
		}
		found = true
		prop, _ := d.Properties["name"].(map[string]any)
		names, _ := prop["enum"].([]string)
		if len(names) != 1 || names[0] != "release-notes" {
			t.Fatalf("read_skill should enumerate the installed skills, got %v", names)
		}
	}
	if !found {
		t.Fatal("read_skill should be offered once a skill is installed")
	}

	// 真正读的时候才拿到正文
	// The body only arrives when it is actually read
	out, isErr := w.Toolbox().Execute("read_skill", map[string]any{"name": "release-notes"})
	if isErr || !strings.Contains(out, "SECRET_BODY_MARKER") {
		t.Fatalf("read_skill should return the full body: %q", out)
	}
}

// 没装技能时，提示里不该出现"技能"这一节，工具也不该给出去——
// 摆着一个读技能的工具却一个技能都没有，模型只会拿它去猜名字。
//
// With no skills installed there should be no skills section and no tool: offering a reader with nothing
// to read only invites the model to guess at names.
func TestNoSkillsMeansNoSectionAndNoTool(t *testing.T) {
	w, _, _ := newTestWorker(t, "a", nil)
	if strings.Contains(w.systemPrompt("group"), "# Skills") {
		t.Fatal("the skills section should be absent when none are installed")
	}
	for _, d := range w.Toolbox().Defs() {
		if d.Name == "read_skill" {
			t.Fatal("read_skill should not be offered when no skills exist")
		}
	}
}

// 导入来的成员模板（agents/*.md 的正文）必须真的进系统提示，否则"把 agent 变成团队成员"
// 就只是抄了个名字。同时它必须在引擎自己那套规则之后，不能覆盖协作与权限的底线。
//
// An imported member template (the body of an agents/*.md file) has to reach the system prompt, or
// "turning an agent into a team member" is just copying a name across. It must also come after the
// engine's own rules, never overriding the collaboration and permission floor.
func TestCustomPromptIsAppendedAfterEngineRules(t *testing.T) {
	dataDir := t.TempDir()
	deps := newTestDeps(t, dataDir)
	bus := NewBus()
	sched := NewScheduler(bus, filepath.Join(dataDir, "routines.json"))
	cfg := config.BotConfig{
		Name: "rev", Role: "code reviewer", Provider: "fake",
		Prompt: "IMPORTED_ROLE_MARKER: always read the surrounding function first.",
	}
	w, err := NewBotWorker(cfg, bus, sched, dataDir, deps)
	if err != nil {
		t.Fatal(err)
	}
	bus.Register(w)

	prompt := w.systemPrompt("group")
	idx := strings.Index(prompt, "IMPORTED_ROLE_MARKER")
	if idx < 0 {
		t.Fatalf("the imported role instructions should be in the prompt:\n%s", prompt)
	}
	// 权限那句是引擎的底线，必须排在导入内容之前
	// The approval sentence is the engine's floor and must come before the imported text
	gate := strings.Index(prompt, "need user approval")
	if gate < 0 || gate > idx {
		t.Fatal("the engine's own rules must precede the imported instructions")
	}
}
