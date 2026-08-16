package api

import (
	"botbureau/backend/internal/engine"
	"botbureau/backend/internal/httpx"
	"botbureau/backend/internal/i18n"
	"fmt"
	"net/http"
	"os"
)

func (a *App) registerCoreRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/", cors(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			httpx.WriteJSON(rw, 404, map[string]any{"error": "not found"})
			return
		}
		rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(rw, i18n.T("Bot Bureau backend is running. Use the Electron client (app/) to access it."))
	}))

	mux.HandleFunc("/api/ping", cors(func(rw http.ResponseWriter, r *http.Request) {
		host, _ := os.Hostname()
		httpx.WriteJSON(rw, 200, map[string]any{
			"app": "botbureau", "name": host, "version": "0.1.0", "instance": instanceID,
		})
	}))

	mux.HandleFunc("/api/state", cors(func(rw http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(rw, 200, a.State())
	}))

	mux.HandleFunc("/api/conversations", cors(func(rw http.ResponseWriter, r *http.Request) {
		conversations := a.bus.ConversationPreviews()
		if conversations == nil {
			conversations = []engine.ConversationPreview{}
		}
		httpx.WriteJSON(rw, 200, map[string]any{"conversations": conversations})
	}))

	mux.HandleFunc("/api/events", cors(a.handleSSE))

}
