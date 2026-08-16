package secret

import (
	"botbureau/backend/internal/i18n"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestKeyStoreRoundTripMaskAndEnvironmentFallback(t *testing.T) {
	i18n.SetLocale("en")
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "keys.json")
	ks := NewKeyStore(path)
	if err := ks.Set("bad-name", "secret"); err == nil {
		t.Fatal("invalid key name should fail")
	}
	if err := ks.Set("TEST_API_KEY", ""); err == nil {
		t.Fatal("empty key should fail")
	}
	if err := ks.Set("TEST_API_KEY", "1234567890"); err != nil {
		t.Fatal(err)
	}
	if err := ks.Set("A_KEY", "short"); err != nil {
		t.Fatal(err)
	}
	if got := ks.Get("TEST_API_KEY"); got != "1234567890" {
		t.Fatalf("stored key = %q", got)
	}
	t.Setenv("ENV_ONLY_KEY", "from-env")
	if got := ks.Get("ENV_ONLY_KEY"); got != "from-env" {
		t.Fatalf("environment fallback = %q", got)
	}
	if got, want := ks.List(), []map[string]string{{"name": "A_KEY", "masked": "••••••"}, {"name": "TEST_API_KEY", "masked": "1234…7890"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("masked keys = %#v, want %#v", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode = %o, want 600", info.Mode().Perm())
	}

	reloaded := NewKeyStore(path)
	if got := reloaded.Get("A_KEY"); got != "short" {
		t.Fatalf("reloaded key = %q", got)
	}
	if !reloaded.Delete("A_KEY") || reloaded.Delete("missing") || reloaded.Get("A_KEY") != "" {
		t.Fatal("delete behavior is wrong")
	}
}

func TestKeyStoreIgnoresMalformedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	if err := os.WriteFile(path, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	ks := NewKeyStore(path)
	if got := ks.List(); len(got) != 0 {
		t.Fatalf("malformed file loaded keys: %#v", got)
	}
}
