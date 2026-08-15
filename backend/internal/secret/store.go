package secret

// 凭据落盘的公共写法。这个包里有四份东西要存到磁盘（API key、xAI 令牌、ChatGPT 令牌、
// MCP 连接器令牌），四份都是"写坏了就登不上"的凭据，所以写法应当只有一种。
//
// The shared way credentials are written to disk. Four things in this package persist to a file (API
// keys, xAI tokens, ChatGPT tokens, MCP connector tokens); all four are credentials that lock you out
// when written badly, so there should be exactly one way to write them.

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// writeSecretFile 原子地写一个 0600 文件：先写同目录下的临时文件，再改名覆盖。
//
// 为什么不用 os.WriteFile 直接写：它是「截断再写」，中途进程被杀、磁盘满、或者两个 goroutine
// 同时在存，留下的就是一个被截断或交错的凭据文件——下次启动直接登不上，而且看不出是怎么坏的。
// rename 在同一文件系统内是原子的，所以读到的要么是旧的完整内容，要么是新的完整内容。
//
// writeSecretFile atomically writes a 0600 file: a temporary file in the same directory first, then a
// rename over the target.
//
// Why not os.WriteFile directly: it truncates and then writes, so a process killed midway, a full disk,
// or two goroutines saving at once leaves behind a truncated or interleaved credential file — the next
// start simply cannot sign in, with nothing to show how it broke. A rename within one filesystem is
// atomic, so a reader sees either the whole old content or the whole new content.
func writeSecretFile(path string, raw []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// 临时文件放在目标同目录：跨文件系统 rename 会失败，/tmp 与数据目录不一定在同一个盘上。
	// The temporary file goes in the target's own directory: a cross-filesystem rename fails, and /tmp is
	// not necessarily on the same volume as the data directory.
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // 成功改名后这一步无害地失败 / harmlessly fails once the rename succeeded

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

// marshalSecret 把凭据结构体编码成 JSON。单独拎出来是为了让调用方能在持锁时调用它——
// 编码要读遍结构体的每个字段，而这些结构体是跨 goroutine 共享的。
//
// marshalSecret encodes a credential struct as JSON. It is separate so callers can invoke it while
// holding their lock: encoding reads every field of the struct, and these structs are shared across
// goroutines.
func marshalSecret(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }
