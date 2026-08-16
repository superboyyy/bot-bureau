package secret

// The shared way credentials are written to disk. Four things in this package persist to a file (API
// keys, xAI tokens, ChatGPT tokens, MCP connector tokens); all four are credentials that lock you out
// when written badly, so there should be exactly one way to write them.

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// writeSecretFile atomically writes a 0600 file: a temporary file in the same directory first, then a
// rename over the target.

// Why not os.WriteFile directly: it truncates and then writes, so a process killed midway, a full disk,
// or two goroutines saving at once leaves behind a truncated or interleaved credential file — the next
// start simply cannot sign in, with nothing to show how it broke. A rename within one filesystem is
// atomic, so a reader sees either the whole old content or the whole new content.
func writeSecretFile(path string, raw []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	// The temporary file goes in the target's own directory: a cross-filesystem rename fails, and /tmp is
	// not necessarily on the same volume as the data directory.
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // harmlessly fails once the rename succeeded

	if _, err := f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// marshalSecret encodes a credential struct as JSON. It is separate so callers can invoke it while
// holding their lock: encoding reads every field of the struct, and these structs are shared across
// goroutines.
func marshalSecret(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }
