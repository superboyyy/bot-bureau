package model

// Model-specific reasoning controls from the public catalog used by OpenCode.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"botbureau/backend/internal/config"
)

const (
	modelsDevCatalogURL = "https://models.dev/api.json"
	modelsDevCacheTTL   = 6 * time.Hour
	modelsDevRetryDelay = 10 * time.Minute
)

type modelsDevProvider struct {
	Models map[string]modelsDevModel `json:"models"`
}

type modelsDevModel struct {
	ReasoningOptions []modelsDevReasoningOption `json:"reasoning_options"`
}

type modelsDevReasoningOption struct {
	Type   string   `json:"type"`
	Values []string `json:"values"`
}

var modelsDevCache = struct {
	sync.Mutex
	providers  map[string]modelsDevProvider
	loadedAt   time.Time
	retryAfter time.Time
}{}

// ReasoningEffortOptions prefers model/provider metadata from Models.dev and falls back to the
// small built-in compatibility table when the catalog is unavailable or has no effort entry.
// The catalog request contains only a provider id and model id; no user credential is sent.
func ReasoningEffortOptions(ctx context.Context, providerID, modelID string) []map[string]any {
	if ids, ok := modelsDevEffortIDs(ctx, providerID, modelID); ok {
		return config.EffortOptionsForIDs(ids)
	}
	return config.EffortOptionsForModel(providerID, modelID)
}

func ReasoningEffortSupported(ctx context.Context, providerID, modelID, effort string) bool {
	if effort == "" {
		return true
	}
	for _, option := range ReasoningEffortOptions(ctx, providerID, modelID) {
		if option["id"] == effort {
			return true
		}
	}
	return false
}

func modelsDevEffortIDs(ctx context.Context, providerID, modelID string) ([]string, bool) {
	if len(modelsDevProviderKeys(providerID)) == 0 || strings.TrimSpace(modelID) == "" {
		return nil, false
	}
	providers, ok := loadModelsDevCatalog(ctx)
	if !ok {
		return nil, false
	}
	model, ok := findModelsDevModel(providers, providerID, modelID)
	if !ok {
		return nil, false
	}
	ids := effortIDsFromReasoningOptions(model.ReasoningOptions)
	return ids, len(ids) > 0
}

func modelsDevProviderKeys(providerID string) []string {
	switch strings.ToLower(strings.TrimSpace(providerID)) {
	case "anthropic", "openai", "xai", "deepseek", "opencode":
		return []string{strings.ToLower(strings.TrimSpace(providerID))}
	case "opencode-go":
		return []string{"opencode-go", "opencode"}
	case "moonshot":
		return []string{"moonshot", "moonshotai"}
	default:
		return nil
	}
}

func loadModelsDevCatalog(ctx context.Context) (map[string]modelsDevProvider, bool) {
	now := time.Now()

	modelsDevCache.Lock()
	defer modelsDevCache.Unlock()
	if modelsDevCache.providers != nil && now.Sub(modelsDevCache.loadedAt) < modelsDevCacheTTL {
		return modelsDevCache.providers, true
	}
	if now.Before(modelsDevCache.retryAfter) {
		return modelsDevCache.providers, modelsDevCache.providers != nil
	}

	requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, modelsDevCatalogURL, nil)
	if err != nil {
		return modelsDevCache.providers, modelsDevCache.providers != nil
	}
	res, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err == nil {
		defer res.Body.Close()
	}
	if err != nil || res.StatusCode < http.StatusOK || res.StatusCode >= http.StatusMultipleChoices {
		modelsDevCache.retryAfter = now.Add(modelsDevRetryDelay)
		return modelsDevCache.providers, modelsDevCache.providers != nil
	}

	raw, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		modelsDevCache.retryAfter = now.Add(modelsDevRetryDelay)
		return modelsDevCache.providers, modelsDevCache.providers != nil
	}
	providers, err := parseModelsDevCatalog(raw)
	if err != nil {
		modelsDevCache.retryAfter = now.Add(modelsDevRetryDelay)
		return modelsDevCache.providers, modelsDevCache.providers != nil
	}
	modelsDevCache.providers = providers
	modelsDevCache.loadedAt = now
	modelsDevCache.retryAfter = time.Time{}
	return providers, true
}

func parseModelsDevCatalog(raw []byte) (map[string]modelsDevProvider, error) {
	var providers map[string]modelsDevProvider
	if err := json.Unmarshal(raw, &providers); err != nil {
		return nil, err
	}
	if len(providers) == 0 {
		return nil, errors.New("models.dev returned no providers")
	}
	return providers, nil
}

func findModelsDevModel(providers map[string]modelsDevProvider, providerID, modelID string) (modelsDevModel, bool) {
	providerKeys := modelsDevProviderKeys(providerID)

	modelID = strings.TrimSpace(modelID)
	modelKeys := []string{modelID}
	if slash := strings.LastIndex(modelID, "/"); slash >= 0 && slash+1 < len(modelID) {
		modelKeys = append(modelKeys, modelID[slash+1:])
	}
	if colon := strings.LastIndex(modelID, ":"); colon >= 0 && colon+1 < len(modelID) {
		modelKeys = append(modelKeys, modelID[colon+1:])
	}

	for _, providerKey := range providerKeys {
		provider, ok := providers[providerKey]
		if !ok {
			for key, candidate := range providers {
				if strings.EqualFold(key, providerKey) {
					provider, ok = candidate, true
					break
				}
			}
		}
		if !ok {
			continue
		}
		for _, modelKey := range modelKeys {
			if model, ok := provider.Models[modelKey]; ok {
				return model, true
			}
			for key, model := range provider.Models {
				if strings.EqualFold(key, modelKey) {
					return model, true
				}
			}
		}
	}
	return modelsDevModel{}, false
}

func effortIDsFromReasoningOptions(options []modelsDevReasoningOption) []string {
	seen := map[string]bool{}
	var ids []string
	for _, option := range options {
		if !strings.EqualFold(strings.TrimSpace(option.Type), "effort") {
			continue
		}
		for _, value := range option.Values {
			value = strings.ToLower(strings.TrimSpace(value))
			if value == "" || !config.ValidEffort(value) || seen[value] {
				continue
			}
			seen[value] = true
			ids = append(ids, value)
		}
	}
	return ids
}
