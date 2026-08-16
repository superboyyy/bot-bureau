package api

import (
	"botbureau/backend/internal/config"
	"botbureau/backend/internal/httpx"
	"botbureau/backend/internal/i18n"
	"botbureau/backend/internal/model"
	"net/http"
	"strings"
)

func (a *App) registerCatalogRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/efforts", cors(func(rw http.ResponseWriter, r *http.Request) {
		// The tiers vary by concrete model, hence ?provider= and ?model=.
		httpx.WriteJSON(rw, 200, map[string]any{"levels": model.ReasoningEffortOptions(
			r.Context(), r.URL.Query().Get("provider"), r.URL.Query().Get("model"),
		)})
	}))

	mux.HandleFunc("/api/permissions", cors(func(rw http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(rw, 200, map[string]any{
			"levels": config.PermOptions(), "default": string(config.DefaultPerm),
			"scope_note": config.PermScopeNote(),
		})
	}))

	mux.HandleFunc("/api/providers", cors(func(rw http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(rw, 200, map[string]any{"providers": model.ProviderCatalog()})
	}))

	mux.HandleFunc("/api/models", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			ProviderID string `json:"provider_id"`
			BaseURL    string `json:"base_url"`
			KeyEnv     string `json:"key_env"`
			Auth       string `json:"auth"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		models, err := model.ListModels(r.Context(), a.credentials(), body.ProviderID,
			strings.TrimSpace(body.BaseURL), strings.TrimSpace(body.KeyEnv), body.Auth)
		if err != nil {
			httpx.WriteJSON(rw, 200, map[string]any{"models": []string{}, "error": err.Error()})
			return
		}
		httpx.WriteJSON(rw, 200, map[string]any{"models": models})
	}))

}
