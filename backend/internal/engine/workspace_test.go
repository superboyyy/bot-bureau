package engine

import (
	"botbureau/backend/internal/config"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceArchiveListDetailAndDelete(t *testing.T) {
	dataDir := t.TempDir()
	cfg := config.BotConfig{Name: "wren", DisplayName: "Wren", Role: "writer"}
	workspace := WorkspaceDir(dataDir, cfg.Name)
	if err := os.MkdirAll(filepath.Join(workspace, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "MEMORY.md"), []byte("remember this for later"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "notes", "draft.txt"), []byte("draft"), 0o644); err != nil {
		t.Fatal(err)
	}

	archived, err := ArchiveWorkspace(dataDir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if archived == "" || !strings.HasPrefix(archived, "wren.removed-") {
		t.Fatalf("archive name = %q", archived)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("serving workspace still exists: %v", err)
	}

	items := ListDeparted(dataDir)
	if len(items) != 1 || items[0].Dir != archived || items[0].ID != "wren" || items[0].DisplayName != "Wren" || !items[0].HasMemory {
		t.Fatalf("departed list = %#v", items)
	}
	if items[0].Files != 2 || items[0].Bytes != int64(len("remember this for later")+len("draft")) {
		t.Fatalf("archive stats = files %d bytes %d", items[0].Files, items[0].Bytes)
	}

	mem, files, truncated, err := DepartedDetail(dataDir, archived, 8)
	if err != nil {
		t.Fatal(err)
	}
	if mem != "remember" || !truncated || len(files) != 2 {
		t.Fatalf("archive detail = mem %q truncated %v files %#v", mem, truncated, files)
	}
	if !files[0].Dir || files[0].Name != "notes" || files[1].Dir || files[1].Name != "MEMORY.md" {
		t.Fatalf("archive files = %#v", files)
	}
	if err := DeleteDeparted(dataDir, "../secrets"); err == nil {
		t.Fatal("path traversal archive should be rejected")
	}
	if err := DeleteDeparted(dataDir, archived); err != nil {
		t.Fatal(err)
	}
	if got := ListDeparted(dataDir); len(got) != 0 {
		t.Fatalf("archives after delete = %#v", got)
	}
}

func TestWorkspaceMissingAndPurgeValidation(t *testing.T) {
	dataDir := t.TempDir()
	if got, err := ArchiveWorkspace(dataDir, config.BotConfig{Name: "missing"}); err != nil || got != "" {
		t.Fatalf("missing archive = %q, %v", got, err)
	}
	if err := PurgeWorkspace(dataDir, "bad/name"); err == nil {
		t.Fatal("invalid purge name should fail")
	}
	if err := os.MkdirAll(WorkspaceDir(dataDir, "wren"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(WorkspaceDir(dataDir, "wren"), "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := PurgeWorkspace(dataDir, "wren"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(WorkspaceDir(dataDir, "wren")); !os.IsNotExist(err) {
		t.Fatalf("purged workspace still exists: %v", err)
	}
}

func TestDepartedNameValidation(t *testing.T) {
	for _, name := range []string{"wren.removed-1", "wren.removed-1-2", "bot_2.removed-123"} {
		if !validDepartedDir(name) {
			t.Errorf("valid archive %q rejected", name)
		}
	}
	for _, name := range []string{"", "wren", "Wren.removed-1", "wren.removed-", "wren.removed-x", "../wren.removed-1", "wren.removed-1/child"} {
		if validDepartedDir(name) {
			t.Errorf("invalid archive %q accepted", name)
		}
	}
}
