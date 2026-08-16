package model

import (
	"context"
	"testing"
	"time"
)

func TestModelsDevReasoningOptions(t *testing.T) {
	raw := []byte(`{
		"opencode": {
			"models": {
				"deepseek-v4-flash": {
					"reasoning_options": [
						{"type":"toggle"},
						{"type":"effort", "values":["high", "max", "not-a-real-effort", "high"]}
					]
				}
			}
		},
		"deepseek": {
			"models": {
				"deepseek-v4-pro": {
					"reasoning_options": [{"type":"effort", "values":["high", "max"]}]
				}
			}
		}
	}`)
	providers, err := parseModelsDevCatalog(raw)
	if err != nil {
		t.Fatal(err)
	}

	model, ok := findModelsDevModel(providers, "opencode-go", "opencode/deepseek-v4-flash")
	if !ok {
		t.Fatal("opencode-go should fall back to the opencode catalog")
	}
	if got := effortIDsFromReasoningOptions(model.ReasoningOptions); !sameStrings(got, []string{"high", "max"}) {
		t.Fatalf("got reasoning efforts %v", got)
	}
}

func TestReasoningEffortOptionsPreferCatalog(t *testing.T) {
	raw := []byte(`{"opencode":{"models":{"deepseek-v4-flash":{"reasoning_options":[{"type":"effort","values":["high","max"]}]}}}}`)
	providers, err := parseModelsDevCatalog(raw)
	if err != nil {
		t.Fatal(err)
	}

	modelsDevCache.Lock()
	oldProviders, oldLoadedAt, oldRetryAfter := modelsDevCache.providers, modelsDevCache.loadedAt, modelsDevCache.retryAfter
	modelsDevCache.providers = providers
	modelsDevCache.loadedAt = time.Now()
	modelsDevCache.retryAfter = time.Time{}
	modelsDevCache.Unlock()
	defer func() {
		modelsDevCache.Lock()
		modelsDevCache.providers, modelsDevCache.loadedAt, modelsDevCache.retryAfter = oldProviders, oldLoadedAt, oldRetryAfter
		modelsDevCache.Unlock()
	}()

	if got := optionIDs(ReasoningEffortOptions(context.Background(), "opencode", "deepseek-v4-flash")); !sameStrings(got, []string{"", "high", "max"}) {
		t.Fatalf("catalog options were not used: %v", got)
	}
}

func optionIDs(options []map[string]any) []string {
	ids := make([]string, 0, len(options))
	for _, option := range options {
		ids = append(ids, option["id"].(string))
	}
	return ids
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
