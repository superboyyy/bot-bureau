package api

import (
	"botbureau/backend/internal/config"
	"botbureau/backend/internal/httpx"
	"botbureau/backend/internal/i18n"
	"botbureau/backend/internal/plugin"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

func (a *App) registerPluginRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/mcp", cors(func(rw http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(rw, 200, map[string]any{"mcp": a.deps.MCP.Status()})
	}))

	mux.HandleFunc("/api/mcp/catalog", cors(func(rw http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(rw, 200, map[string]any{"catalog": plugin.Catalog()})
	}))

	mux.HandleFunc("/api/mcp/add", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Name      string            `json:"name"`
			Command   string            `json:"command"`
			Args      string            `json:"args"` // space-separated (simpler UI input)
			URL       string            `json:"url"`
			BearerKey string            `json:"bearer_key"`
			Env       map[string]string `json:"env"`
			Auth      string            `json:"auth"` // "oauth" for remote connectors
		}
		if err := httpx.ReadJSON(r, &body); err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		cfg := plugin.MCPServerConfig{
			Name: strings.TrimSpace(body.Name), Command: strings.TrimSpace(body.Command),
			Args: strings.Fields(body.Args), URL: strings.TrimSpace(body.URL),
			BearerKey: strings.TrimSpace(body.BearerKey), Env: body.Env,
			Auth: strings.TrimSpace(body.Auth),
		}
		if err := a.deps.MCP.Add(cfg); err != nil {
			// OAuth remotes are persisted before a token exists. Treat that as success so the client
			// can open the browser Authorize flow next, instead of looking like a failed install.
			if cfg.Auth == "oauth" && a.deps.MCP.Has(cfg.Name) {
				a.bus.Emit("refresh", "", "system", i18n.T("Plugin ")+cfg.Name+i18n.T(" added"), nil)
				httpx.WriteJSON(rw, 200, map[string]any{"ok": true, "needs_auth": true})
				return
			}
			httpx.WriteJSON(rw, 400, map[string]any{"error": err.Error()})
			return
		}
		a.bus.Emit("refresh", "", "system", i18n.T("Plugin ")+cfg.Name+i18n.T(" connected"), nil)
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true})
	}))

	mux.HandleFunc("/api/mcp/delete", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil || body.Name == "" {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		if !a.deps.MCP.Remove(body.Name) {
			httpx.WriteJSON(rw, 404, map[string]any{"error": i18n.T("No plugin named ") + body.Name + i18n.T(" exists")})
			return
		}

		// remove it from each bot's subscriptions as well, then persist
		a.mu.Lock()
		for i := range a.cfgs {
			kept := a.cfgs[i].MCP[:0]
			for _, s := range a.cfgs[i].MCP {
				if s != body.Name {
					kept = append(kept, s)
				}
			}
			a.cfgs[i].MCP = kept
			if w := a.bus.Bot(a.cfgs[i].Name); w != nil {
				w.Cfg.MCP = kept
				w.Toolbox().SetMCPServers(kept)
			}
		}
		_ = config.SaveBotConfigs(a.cfgPath, a.cfgs)
		a.mu.Unlock()
		a.bus.Emit("refresh", "", "system", i18n.T("Plugin ")+body.Name+i18n.T(" removed"), nil)
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true})
	}))

	mux.HandleFunc("/api/mcp/oauth/start", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil || body.Name == "" {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}

		// The URL of an already-saved plugin wins; the one in the request is only used when adding a new
		// connector, or a caller could aim the engine's authorization flow at any address it liked.
		target := body.URL
		if u := a.deps.MCP.URLOf(body.Name); u != "" {
			target = u
		}
		st, err := a.deps.MCPAuth.Start(body.Name, target)
		if err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": err.Error()})
			return
		}
		httpx.WriteJSON(rw, 200, st)
	}))

	mux.HandleFunc("/api/mcp/oauth/status", cors(func(rw http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		st := a.deps.MCPAuth.Status(name)

		// Reconnect once as soon as authorization lands: otherwise the user approves in the browser and
		// still has to hit "reconnect" by hand before any tools appear
		if st["status"] == "done" && a.deps.MCP.Has(name) {
			if err := a.deps.MCP.Reconnect(name); err != nil {
				st["error"] = err.Error()
			}
			a.deps.MCPAuth.ClearPending(name)
			a.bus.Emit("refresh", "", "system", i18n.T("Plugin ")+name+i18n.T(" authorized"), nil)
		}
		httpx.WriteJSON(rw, 200, st)
	}))

	mux.HandleFunc("/api/mcp/oauth/logout", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil || body.Name == "" {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		a.deps.MCPAuth.Logout(body.Name)
		a.bus.Emit("refresh", "", "system", i18n.T("Plugin ")+body.Name+i18n.T(" signed out"), nil)
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true})
	}))

	mux.HandleFunc("/api/mcp/tools", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Name  string   `json:"name"`
			Tools []string `json:"tools"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil || body.Name == "" {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		if err := a.deps.MCP.SetTools(body.Name, body.Tools); err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": err.Error()})
			return
		}
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true})
	}))

	mux.HandleFunc("/api/mcp/reconnect", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil || body.Name == "" {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		if err := a.deps.MCP.Reconnect(body.Name); err != nil {
			a.bus.Emit("refresh", "", "system", i18n.T("Plugin reconnect failed"), nil)
			httpx.WriteJSON(rw, 400, map[string]any{"error": err.Error()})
			return
		}
		a.bus.Emit("refresh", "", "system", i18n.T("Plugin ")+body.Name+i18n.T(" reconnected"), nil)
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true})
	}))

	mux.HandleFunc("/api/plugins/install", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Source string `json:"source"`

			// Non-empty selects one entry from the marketplace listing at that address
			Plugin string `json:"plugin"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		var b *plugin.Bundle
		var err error
		if body.Plugin != "" {
			b, err = a.deps.Bundles.InstallFromMarketplace(body.Source, body.Plugin)
		} else {
			b, err = a.deps.Bundles.Install(body.Source)
		}

		// The address gave a marketplace listing rather than a single plugin: not a failure — hand the
		// listing to the client so the user can pick one.
		var mk *plugin.MarketplaceError
		if errors.As(err, &mk) {
			httpx.WriteJSON(rw, 200, map[string]any{
				"marketplace": map[string]any{"name": mk.Marketplace, "plugins": mk.Entries},
				"source":      body.Source,
			})
			return
		}
		if err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": err.Error()})
			return
		}
		a.bus.Emit("refresh", "", "system", i18n.T("Plugin ")+b.Name+i18n.T(" installed"), nil)
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true, "plugin": b})
	}))

	mux.HandleFunc("/api/plugins/update", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil || body.Name == "" {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		b, err := a.deps.Bundles.Update(body.Name)
		if err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": err.Error()})
			return
		}
		a.bus.Emit("refresh", "", "system", i18n.T("Plugin ")+b.Name+i18n.T(" updated"), nil)
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true, "plugin": b})
	}))

	mux.HandleFunc("/api/plugins/remove", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil || body.Name == "" {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		if err := a.deps.Bundles.Remove(body.Name); err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": err.Error()})
			return
		}
		a.bus.Emit("refresh", "", "system", i18n.T("Plugin ")+body.Name+i18n.T(" removed"), nil)
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true})
	}))

	mux.HandleFunc("/api/skills/rescan", cors(func(rw http.ResponseWriter, r *http.Request) {
		a.deps.Bundles.Rescan()
		a.deps.SyncSkillRoots()
		a.bus.Emit("refresh", "", "system", "skills", nil)
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true, "skills": a.deps.Skills.List()})
	}))

	mux.HandleFunc("/api/tasks/clear_done", cors(func(rw http.ResponseWriter, r *http.Request) {
		n := a.deps.Board.ClearDone()
		a.bus.Emit("refresh", "", "system", fmt.Sprintf(i18n.T("Cleared %d completed task(s)"), n), nil)
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true, "cleared": n})
	}))

	mux.HandleFunc("/api/routines/update", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
			Bot  string `json:"bot"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil || body.Name == "" || body.Bot == "" {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		w := a.bus.Bot(body.Bot)
		if w == nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": fmt.Errorf(i18n.T("There is no bot named %s"), body.Bot).Error()})
			return
		}
		if !a.sched.Reassign(body.Name, body.Bot) {
			httpx.WriteJSON(rw, 404, map[string]any{"error": i18n.T("No routine named ") + body.Name + i18n.T(" exists")})
			return
		}
		a.bus.Emit("refresh", "", "system",
			fmt.Sprintf(i18n.T("Routine \"%s\" is now assigned to %s"), body.Name, w.Cfg.Title()), nil)
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true})
	}))

	mux.HandleFunc("/api/routines/delete", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil || body.Name == "" {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		if !a.sched.Remove(body.Name) {
			httpx.WriteJSON(rw, 404, map[string]any{"error": i18n.T("No routine named ") + body.Name + i18n.T(" exists")})
			return
		}
		a.bus.Emit("refresh", "", "system", i18n.T("Routine ")+body.Name+i18n.T(" deleted"), nil)
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true})
	}))

}
