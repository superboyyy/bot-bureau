package api

import (
	"botbureau/backend/internal/config"
	"botbureau/backend/internal/httpx"
	"botbureau/backend/internal/i18n"
	"net/http"
	"strings"
)

func (a *App) registerSettingsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/keys", cors(func(rw http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			httpx.WriteJSON(rw, 200, map[string]any{"keys": a.deps.KS.List()})
		case http.MethodPost:
			var body struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			}
			if err := httpx.ReadJSON(r, &body); err != nil {
				httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
				return
			}
			if err := a.deps.KS.Set(strings.TrimSpace(body.Name), strings.TrimSpace(body.Value)); err != nil {
				httpx.WriteJSON(rw, 400, map[string]any{"error": err.Error()})
				return
			}
			a.bus.Emit("refresh", "", "system", "API key "+body.Name+i18n.T(" updated"), nil)
			httpx.WriteJSON(rw, 200, map[string]any{"ok": true})
		default:
			httpx.WriteJSON(rw, 405, map[string]any{"error": "method not allowed"})
		}
	}))

	mux.HandleFunc("/api/keys/delete", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil || body.Name == "" {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		if !a.deps.KS.Delete(body.Name) {
			httpx.WriteJSON(rw, 404, map[string]any{"error": i18n.T("No stored key named ") + body.Name})
			return
		}
		a.bus.Emit("refresh", "", "system", "API key "+body.Name+i18n.T(" deleted"), nil)
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true})
	}))

	mux.HandleFunc("/api/settings", cors(func(rw http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			httpx.WriteJSON(rw, 200, a.settings.Status())
		case http.MethodPost:

			// Pointer fields: omitted entries stay as they are, so changing the language cannot wipe the group name
			var body struct {
				Locale      *string `json:"locale"`
				GroupTitle  *string `json:"group_title"`
				GroupAvatar *string `json:"group_avatar"`
				Permission  *string `json:"permission"`
			}
			if err := httpx.ReadJSON(r, &body); err != nil {
				httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
				return
			}
			if body.Locale != nil && !a.settings.SetLocalePref(strings.TrimSpace(*body.Locale)) {
				httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("locale must be auto / zh / en")})
				return
			}
			if body.GroupAvatar != nil && !config.ValidAvatar(*body.GroupAvatar) {
				httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("The avatar must be a #rrggbb color or a small png/jpeg/webp image")})
				return
			}
			if body.GroupTitle != nil || body.GroupAvatar != nil {
				a.settings.SetGroupMeta(body.GroupTitle, body.GroupAvatar)
			}
			if body.Permission != nil && !a.settings.SetPermission(strings.TrimSpace(*body.Permission)) {
				httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("The permission tier must be ask / edit / auto / full")})
				return
			}
			a.bus.Emit("refresh", "", "system", "settings", nil)
			httpx.WriteJSON(rw, 200, a.settings.Status())
		default:
			httpx.WriteJSON(rw, 405, map[string]any{"error": "method not allowed"})
		}
	}))

	mux.HandleFunc("/api/telegram", cors(func(rw http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			httpx.WriteJSON(rw, 200, a.tg.Status())
		case http.MethodPost:
			var body struct {
				Enabled bool `json:"enabled"`
			}
			if err := httpx.ReadJSON(r, &body); err != nil {
				httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
				return
			}
			if err := a.tg.SetEnabled(body.Enabled); err != nil {
				httpx.WriteJSON(rw, 400, map[string]any{"error": err.Error()})
				return
			}
			a.bus.Emit("refresh", "", "system", "telegram", nil)
			httpx.WriteJSON(rw, 200, a.tg.Status())
		default:
			httpx.WriteJSON(rw, 405, map[string]any{"error": "method not allowed"})
		}
	}))

}
