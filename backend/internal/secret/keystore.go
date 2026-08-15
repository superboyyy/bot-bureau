package secret

import (
	"botbureau/backend/internal/i18n"

	"encoding/json"
	"errors"
	"os"
	"regexp"
	"sort"
	"sync"
)

var keyNameRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// KeyStore 管理 UI 里录入的 API key，存于 data/keys.json（0600）。
// 名字沿用环境变量命名（ANTHROPIC_API_KEY / XAI_API_KEY / …）；
// 解析顺序：先查存储，再回退到真实环境变量——bots.yaml 里的 api_key_env 两边通用。
// KeyStore manages API keys entered in the UI, stored in data/keys.json (0600).
// Names follow environment-variable naming (ANTHROPIC_API_KEY / XAI_API_KEY / ...);
// resolution order: check the store first, then fall back to real environment variables — api_key_env in bots.yaml works with either.
type KeyStore struct {
	path string
	mu   sync.Mutex
	keys map[string]string
}

func NewKeyStore(path string) *KeyStore {
	ks := &KeyStore{path: path, keys: map[string]string{}}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &ks.keys)
	}
	return ks
}

func (k *KeyStore) Get(name string) string {
	k.mu.Lock()
	v := k.keys[name]
	k.mu.Unlock()
	if v != "" {
		return v
	}
	return os.Getenv(name)
}

func (k *KeyStore) Set(name, value string) error {
	if !keyNameRe.MatchString(name) {
		return errors.New(i18n.T("The name must use uppercase environment-variable style (e.g. XAI_API_KEY)"))
	}
	if value == "" {
		return errors.New(i18n.T("The key cannot be empty"))
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.keys[name] = value
	return k.save()
}

func (k *KeyStore) Delete(name string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.keys[name]; !ok {
		return false
	}
	delete(k.keys, name)
	_ = k.save()
	return true
}

// 调用方（Set/Delete）已经持锁，所以这里只管编码和写盘。
// Callers (Set/Delete) already hold the lock, so this only encodes and writes.
func (k *KeyStore) save() error {
	out, err := marshalSecret(k.keys)
	if err != nil {
		return err
	}
	return writeSecretFile(k.path, out) // 仅本用户可读 / readable only by this user
}

func maskKey(v string) string {
	if len(v) <= 8 {
		return "••••••"
	}
	return v[:4] + "…" + v[len(v)-4:]
}

// List 返回已存 key 的名字与掩码（绝不返回明文）。
// List returns the names and masked forms of stored keys (never the plaintext).
func (k *KeyStore) List() []map[string]string {
	k.mu.Lock()
	defer k.mu.Unlock()
	names := make([]string, 0, len(k.keys))
	for n := range k.keys {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]map[string]string, 0, len(names))
	for _, n := range names {
		out = append(out, map[string]string{"name": n, "masked": maskKey(k.keys[n])})
	}
	return out
}
