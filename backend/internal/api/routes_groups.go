package api

import (
	"botbureau/backend/internal/config"
	"botbureau/backend/internal/httpx"
	"botbureau/backend/internal/i18n"
	"net/http"
	"strings"
)

func (a *App) registerGroupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/group/set", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Group string `json:"group"`
			Name  string `json:"name"`
			In    bool   `json:"in"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil || body.Name == "" {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		chat := body.Group
		if chat == "" {
			chat = "group"
		}
		ok, changed := a.bus.SetGroupMemberIn(chat, body.Name, body.In)
		if !ok {
			httpx.WriteJSON(rw, 404, map[string]any{"error": i18n.T("No bot named ") + body.Name + i18n.T(" exists")})
			return
		}

		// Announce only when the membership actually moved, and under the name people see rather than
		// the internal id. Saving the group settings may submit every bot at once, and announcing each
		// of them would fill the chat with a screenful of notices for a single change.
		if changed {
			who := body.Name
			if w := a.bus.Bot(body.Name); w != nil {
				who = w.Cfg.Title()
			}
			verb := i18n.T("was removed from the group chat")
			if body.In {
				verb = i18n.T("was added to the group chat")
			}
			a.bus.Emit("system", chat, "system", who+" "+verb, nil)
		}
		a.bus.Emit("refresh", "", "system", "group_members", nil)
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true})
	}))

	mux.HandleFunc("/api/groups", cors(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			httpx.WriteJSON(rw, 405, map[string]any{"error": "method not allowed"})
			return
		}
		var body struct {
			Title   string   `json:"title"`
			Avatar  string   `json:"avatar"`
			Members []string `json:"members"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		if body.Avatar != "" && !config.ValidAvatar(body.Avatar) {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("The avatar must be a #rrggbb color or a small png/jpeg/webp image")})
			return
		}
		g, err := a.bus.CreateGroup(body.Title, body.Avatar, body.Members)
		if err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": err.Error()})
			return
		}
		a.bus.Emit("refresh", "", "system", i18n.T("New group chat created"), nil)
		httpx.WriteJSON(rw, 200, g)
	}))

	mux.HandleFunc("/api/groups/update", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			ID      string    `json:"id"`
			Title   *string   `json:"title"`
			Avatar  *string   `json:"avatar"`
			Members *[]string `json:"members"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil || body.ID == "" {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		if body.Avatar != nil && *body.Avatar != "" && !config.ValidAvatar(*body.Avatar) {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("The avatar must be a #rrggbb color or a small png/jpeg/webp image")})
			return
		}
		if err := a.bus.UpdateGroup(body.ID, body.Title, body.Avatar, body.Members); err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": err.Error()})
			return
		}
		if body.ID == "group" && (body.Title != nil || body.Avatar != nil) {
			a.settings.SetGroupMeta(body.Title, body.Avatar)
		}
		a.bus.Emit("refresh", "", "system", "groups", nil)
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true})
	}))

	mux.HandleFunc("/api/groups/delete", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			ID string `json:"id"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil || body.ID == "" {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		if err := a.bus.DeleteGroup(body.ID); err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": err.Error()})
			return
		}
		a.settings.SetPinned(body.ID, false)

		// Deleting the conversation deletes the record. Once history is promised to be permanent,
		// deleting has to mean it, or the user's only way of clearing anything is an empty gesture.
		a.bus.DeleteChat(body.ID)
		a.bus.Emit("refresh", "", "system", i18n.T("Group chat deleted"), nil)
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true})
	}))

	mux.HandleFunc("/api/pins", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Chat   string `json:"chat"`
			Pinned bool   `json:"pinned"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil || strings.TrimSpace(body.Chat) == "" {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		chat := strings.TrimSpace(body.Chat)
		if body.Pinned && !a.chatExists(chat) {
			httpx.WriteJSON(rw, 404, map[string]any{"error": i18n.T("There is no such conversation")})
			return
		}
		if a.settings.SetPinned(chat, body.Pinned) {

			// Other devices are on the same engine, and the list has to reorder there too
			a.bus.Emit("refresh", "", "system", "pins", nil)
		}
		httpx.WriteJSON(rw, 200, map[string]any{"pins": a.settings.Pins()})
	}))

}
