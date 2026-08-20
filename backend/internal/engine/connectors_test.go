package engine

import (
	"botbureau/backend/internal/config"
	"botbureau/backend/internal/plugin"
	"strings"
	"testing"
)

func TestListConnectorsMentionsGitHubAndJira(t *testing.T) {
	w, _, _ := newTestWorker(t, "a", nil)
	out, isErr := w.toolbox.runListConnectors()
	if isErr {
		t.Fatal(out)
	}
	for _, want := range []string{"github", "atlassian", "jira", "sentry", "slack", "figma", "stripe", "google-drive"} {
		if !strings.Contains(out, want) {
			t.Fatalf("list should mention %s:\n%s", want, out)
		}
	}
}

func TestEnableConnectorSubscribesAfterApproval(t *testing.T) {
	script := writeFakeMCPServer(t)
	dataDir := t.TempDir()
	bus := NewBus()
	sched := NewScheduler(bus, dataDir+"/routines.json")
	deps := newTestDeps(t, dataDir)
	deps.Settings = config.NewSettings(dataDir)
	deps.Settings.SetPermission(string(config.PermFull))
	w, err := NewBotWorker(config.BotConfig{Name: "a", Role: "test", Provider: "fake"}, bus, sched, dataDir, deps)
	if err != nil {
		t.Fatal(err)
	}
	bus.Register(w)

	subscribed := ""
	deps.SubscribeMCP = func(botName, server string) error {
		subscribed = botName + ":" + server
		w.Cfg.MCP = append(w.Cfg.MCP, server)
		w.toolbox.SetMCPServers(w.Cfg.MCP)
		return nil
	}

	// Pre-install under a catalog name so enable skips network (npx) and only subscribes.
	if err := deps.MCP.Add(plugin.MCPServerConfig{Name: "memory", Command: "python3", Args: []string{script}}); err != nil {
		t.Fatal(err)
	}

	out, isErr := w.toolbox.runEnableConnector("memory", "", "")
	if isErr {
		t.Fatalf("enable: %s", out)
	}
	if subscribed != "a:memory" {
		t.Fatalf("subscribe callback: %q", subscribed)
	}
	var names []string
	for _, d := range w.toolbox.Defs() {
		names = append(names, d.Name)
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "mcp_memory_echo") {
		t.Fatalf("defs should include plugin tools after enable: %v", names)
	}
	if !strings.Contains(joined, "list_connectors") || !strings.Contains(joined, "enable_connector") {
		t.Fatalf("defs should include connector tools: %v", names)
	}
}

func TestEnableConnectorRejectsUnknown(t *testing.T) {
	w, _, _ := newTestWorker(t, "a", nil)
	out, isErr := w.toolbox.runEnableConnector("not-a-real-connector", "", "")
	if !isErr || !strings.Contains(out, "Unknown") {
		t.Fatalf("expected unknown error, got %q (err=%v)", out, isErr)
	}
}

func TestEnableConnectorAliasJira(t *testing.T) {
	if got := plugin.ResolveCatalogName("jira"); got != "atlassian" {
		t.Fatalf("jira alias: %s", got)
	}
	e, ok := plugin.LookupCatalog("jira")
	if !ok || e.Name != "atlassian" {
		t.Fatalf("lookup jira: %+v ok=%v", e, ok)
	}
	if got := plugin.ResolveCatalogName("gdrive"); got != "google-drive" {
		t.Fatalf("gdrive alias: %s", got)
	}
	if _, ok := plugin.LookupCatalog("slack"); !ok {
		t.Fatal("slack missing from catalog")
	}
	if _, ok := plugin.LookupCatalog("figma"); !ok {
		t.Fatal("figma missing from catalog")
	}
}

func TestCatalogToConfigPathAndOAuth(t *testing.T) {
	fs, ok := plugin.LookupCatalog("fs")
	if !ok {
		t.Fatal("fs missing")
	}
	cfg, err := fs.ToConfig("/tmp/workspace-demo")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Command != "npx" || len(cfg.Args) < 1 || cfg.Args[len(cfg.Args)-1] != "/tmp/workspace-demo" {
		t.Fatalf("fs config: %+v", cfg)
	}
	at, ok := plugin.LookupCatalog("atlassian")
	if !ok {
		t.Fatal("atlassian missing")
	}
	cfg, err = at.ToConfig("")
	if err != nil || cfg.Auth != "oauth" || cfg.URL == "" {
		t.Fatalf("atlassian config: %+v err=%v", cfg, err)
	}
	gh, ok := plugin.LookupCatalog("github")
	if !ok {
		t.Fatal("github missing")
	}
	if !gh.OAuth || gh.Need != nil {
		t.Fatalf("github should be browser OAuth with no PAT, got %+v", gh)
	}
	cfg, err = gh.ToConfig("")
	if err != nil || cfg.Auth != "oauth" || cfg.URL == "" {
		t.Fatalf("github config: %+v err=%v", cfg, err)
	}
}
