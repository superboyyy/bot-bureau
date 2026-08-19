package api

import (
	"botbureau/backend/internal/engine"
	"botbureau/backend/internal/httpx"
	"botbureau/backend/internal/i18n"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

func (a *App) registerChatRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/history", cors(func(rw http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		chat := q.Get("chat")
		if chat == "" {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Which conversation?")})
			return
		}
		before, _ := strconv.Atoi(q.Get("before"))
		limit, _ := strconv.Atoi(q.Get("limit"))
		if limit <= 0 || limit > 500 {
			limit = 200
		}
		evs, more := a.bus.History(chat, before, limit)
		httpx.WriteJSON(rw, 200, map[string]any{"events": evs, "more": more})
	}))

	mux.HandleFunc("/api/send", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Chat string `json:"chat"`
			Text string `json:"text"`

			// Quote reply: the event id of the message being answered
			ReplyTo int `json:"reply_to"`

			// Files attached in the composer, their bytes base64-encoded. JSON rather than multipart, so
			// this endpoint keeps one shape and every client — Electron, a phone app later — has one path
			// to implement.
			Files []struct {
				Name string `json:"name"`
				MIME string `json:"mime"`
				Data string `json:"data"`
			} `json:"files"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		text := strings.TrimSpace(body.Text)

		// Attachments with not a word typed still make a message: dropping in a picture is itself the thing being said
		if text == "" && len(body.Files) == 0 {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Message is empty")})
			return
		}
		if len(body.Files) > engine.MaxAttachments {
			httpx.WriteJSON(rw, 400, map[string]any{"error": fmt.Sprintf(i18n.T("A message can include at most %d files"), engine.MaxAttachments)})
			return
		}
		var files []engine.Attachment
		var total int
		for _, f := range body.Files {
			raw, err := base64.StdEncoding.DecodeString(f.Data)
			if err != nil {
				httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("An attachment could not be decoded")})
				return
			}
			total += len(raw)
			if total > engine.MaxAttachmentsBytes {
				httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("These files exceed the size limit for a single message")})
				return
			}
			saved, err := a.deps.Uploads.Put(f.Name, f.MIME, raw)
			if err != nil {
				httpx.WriteJSON(rw, 400, map[string]any{"error": err.Error()})
				return
			}
			files = append(files, saved)
		}
		quote := a.quotedIn(body.Chat, body.ReplyTo)
		switch {
		case engine.IsGroupChat(body.Chat) || body.Chat == "":
			chat := body.Chat
			if chat == "" {
				chat = "group"
			}
			targets := a.bus.MentionedBotsIn(chat, text)

			// Nobody was named, but the message answers a bot's own line — then it is addressed to
			// them, and typing the name again is redundant. The quotation is the address.
			if len(targets) == 0 && quote != nil && quote.From != "user" && a.bus.IsGroupMemberOf(chat, quote.From) {
				targets = []string{quote.From}
			}
			if len(targets) == 0 {
				def := a.bus.DefaultGroupMemberOf(chat)
				if def == "" {
					httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("The group chat has no members — add someone in the group settings first")})
					return
				}
				targets = []string{def}
			}
			a.bus.PostGroupUser(chat, text, targets, quote, files)
		case strings.HasPrefix(body.Chat, "dm:"):
			name := strings.TrimPrefix(body.Chat, "dm:")
			if a.bus.Bot(name) == nil {
				httpx.WriteJSON(rw, 404, map[string]any{"error": i18n.T("No bot named ") + name + i18n.T(" exists")})
				return
			}
			ev := a.bus.Emit("msg", body.Chat, "user", text, engine.MsgExtra(quote, files))
			a.bus.DeliverMsg(name, engine.Msg{
				Sender: "user", Content: text, Chat: "dm", Respond: true,
				ID: engine.EventID(ev), Quote: quote, Files: files,
				GrantRoots: true,
			})
		default:
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("chat must be a group or dm:<bot name>")})
			return
		}
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true})
	}))

	mux.HandleFunc("/api/file/", cors(func(rw http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/file/")
		att, raw, err := a.deps.Uploads.Find(id)
		if err != nil {
			httpx.WriteJSON(rw, 404, map[string]any{"error": i18n.T("No such attachment")})
			return
		}
		rw.Header().Set("Content-Type", att.MIME)

		// Content-addressed files never change, so cache them for good and the browser stops re-fetching
		// every image on every scroll
		rw.Header().Set("Cache-Control", "private, max-age=31536000, immutable")

		// No content sniffing: an upload rendered as HTML is a place for a script to run
		rw.Header().Set("X-Content-Type-Options", "nosniff")
		rw.Write(raw)
	}))

	mux.HandleFunc("/api/cancel", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil || body.Name == "" {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		w := a.bus.Bot(body.Name)
		if w == nil {
			httpx.WriteJSON(rw, 404, map[string]any{"error": i18n.T("No bot named ") + body.Name + i18n.T(" exists")})
			return
		}
		w.Cancel()
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true})
	}))

	mux.HandleFunc("/api/approve", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			ID       int    `json:"id"`
			Approved bool   `json:"approved"`
			Reason   string `json:"reason"`

			// Optional replacement bash line. Empty keeps the original. Telegram omits this.
			Command string `json:"command"`

			// "Approve, and make this a working directory" — one click instead of every later question
			// about the same directory
			Grant bool `json:"grant"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}

		// The grant has to land before the decision: the moment the decision goes through that member
		// resumes, and whether the command it runs gets stopped again depends on the directory being on
		// the list by then.
		granted := ""
		if body.Grant && body.Approved {
			ap := a.bus.Approval(body.ID)
			if ap == nil || ap.Dir == "" {
				httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("This approval has no directory to grant")})
				return
			}
			w := a.bus.Bot(ap.Bot)
			if w == nil || !w.Roots().Add(ap.Dir) {
				httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("That directory cannot be made a working directory")})
				return
			}
			granted = ap.Dir
			a.bus.Emit("system", ap.Chat, ap.Bot,
				fmt.Sprintf(i18n.T("%s counts as a working directory for %s from now on: it may read, write and run commands there without asking, as far as its permission tier allows. Remove it in this member's settings."),
					ap.Dir, w.Cfg.Title()), nil)
		}
		if !a.bus.DecideCmd(body.ID, body.Approved, body.Reason, body.Command) {
			httpx.WriteJSON(rw, 404, map[string]any{"error": i18n.T("Approval not found (it may already be handled)")})
			return
		}
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true, "granted": granted})
	}))

	mux.HandleFunc("/api/session/reset", cors(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Bot  string `json:"bot"`
			Chat string `json:"chat"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Invalid request body")})
			return
		}
		chat := strings.TrimSpace(body.Chat)
		bot := strings.TrimSpace(body.Bot)
		if chat == "" {
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Which conversation?")})
			return
		}
		resetOne := func(name, sessionKey string) bool {
			w := a.bus.Bot(name)
			if w == nil {
				httpx.WriteJSON(rw, 404, map[string]any{"error": i18n.T("No bot named ") + name + i18n.T(" exists")})
				return false
			}
			w.ResetChat(sessionKey)
			return true
		}
		switch {
		case strings.HasPrefix(chat, "dm:"):
			name := strings.TrimPrefix(chat, "dm:")
			if bot != "" && bot != name {
				httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("chat must be a group or dm:<bot name>")})
				return
			}
			if !resetOne(name, "dm") {
				return
			}
		case chat == "dm":
			if bot == "" {
				httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("Which conversation?")})
				return
			}
			if !resetOne(bot, "dm") {
				return
			}
		case engine.IsGroupChat(chat):
			if bot != "" {
				if !resetOne(bot, chat) {
					return
				}
				break
			}
			for _, name := range a.bus.GroupMembersOf(chat) {
				if w := a.bus.Bot(name); w != nil {
					w.ResetChat(chat)
				}
			}
		default:
			httpx.WriteJSON(rw, 400, map[string]any{"error": i18n.T("chat must be a group or dm:<bot name>")})
			return
		}
		httpx.WriteJSON(rw, 200, map[string]any{"ok": true})
	}))

}
