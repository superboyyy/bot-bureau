package api

import (
	"botbureau/backend/internal/config"
	"botbureau/backend/internal/engine"
	"botbureau/backend/internal/httpx"
	"botbureau/backend/internal/i18n"
	"net/http"
)

func (a *App) registerBotRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/bots", cors(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			httpx.WriteJSON(rw, 405, map[string]any{"error": "method not allowed"})
			return
		}
		var cfg config.BotConfig
		if err := httpx.ReadJSON(r, &cfg); err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		if _, err := a.AddBot(cfg); err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": err.Error()})
			return
		}
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true})
	}))

	mux.HandleFunc("/api/bots/update", cors(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			httpx.WriteJSON(rw, 405, map[string]any{"error": "method not allowed"})
			return
		}
		var cfg config.BotConfig
		if err := httpx.ReadJSON(r, &cfg); err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		if err := a.UpdateBot(cfg); err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": err.Error()})
			return
		}
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true})
	}))

	mux.HandleFunc("/api/bots/delete", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`

			// Whether to delete this member's memory and work files, decided by the checkbox in the
			// removal dialog. Absent means false — a caller that omits the field (an older client, a
			// script) must not lose data over it.
			Purge bool `json:"purge"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil || body.Name == "" {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		warning, err := a.RemoveBot(body.Name, body.Purge)
		if err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": err.Error()})
			return
		}
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true, "warning": warning})
	}))

	mux.HandleFunc("/api/bots/departed", cors(func(rw http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(rw, 200, map[string]any{"departed": engine.ListDeparted(a.dataDir)})
	}))

	mux.HandleFunc("/api/bots/departed/detail", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Dir string `json:"dir"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil || body.Dir == "" {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}

		// Only this much memory text: a reminder of what the archive holds, not a full-text reader
		mem, files, truncated, err := engine.DepartedDetail(a.dataDir, body.Dir, 20000)
		if err != nil {
			httpx.WriteJSON(rw, 404, map[string]any{"error": i18n.T("That archive is gone")})
			return
		}
		httpx.WriteJSON(rw, 200, map[string]any{"memory": mem, "files": files, "truncated": truncated})
	}))

	mux.HandleFunc("/api/bots/departed/delete", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Dir string `json:"dir"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil || body.Dir == "" {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		if err := engine.DeleteDeparted(a.dataDir, body.Dir); err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": err.Error()})
			return
		}
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true})
	}))

	mux.HandleFunc("/api/bots/roots/remove", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
			Dir  string `json:"dir"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		w := a.bus.Bot(body.Name)
		if w == nil {
			httpx.WriteJSON(rw, 404, map[string]any{"error": i18n.T("No bot named ") + body.Name + i18n.T(" exists")})
			return
		}
		if !w.Roots().Remove(body.Dir) {
			httpx.WriteJSON(rw, 404, map[string]any{"error": i18n.T("That directory is not on this member's list")})
			return
		}
		a.bus.Emit("refresh", "", "system", "bots", nil)
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true})
	}))

}
