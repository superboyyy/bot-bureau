package api

import (
	"botbureau/backend/internal/httpx"
	"botbureau/backend/internal/i18n"
	"net/http"
)

func (a *App) registerOAuthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/xai/oauth/start", cors(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			httpx.WriteJSON(rw, 405, map[string]any{"error": "method not allowed"})
			return
		}
		st, err := a.deps.XAI.Start()
		if err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": err.Error()})
			return
		}
		httpx.WriteJSON(rw, 200, st)
	}))

	mux.HandleFunc("/api/xai/oauth/status", cors(func(rw http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(rw, 200, a.deps.XAI.Status())
	}))

	mux.HandleFunc("/api/xai/oauth/cancel", cors(func(rw http.ResponseWriter, r *http.Request) {
		a.deps.XAI.Cancel()
		httpx.WriteJSON(rw, 200, a.deps.XAI.Status())
	}))

	mux.HandleFunc("/api/xai/oauth/logout", cors(func(rw http.ResponseWriter, r *http.Request) {
		a.deps.XAI.Logout()
		a.bus.Emit("refresh", "", "system", i18n.T("Signed out of SuperGrok"), nil)
		httpx.WriteJSON(rw, 200, a.deps.XAI.Status())
	}))

	mux.HandleFunc("/api/chatgpt/oauth/start", cors(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			httpx.WriteJSON(rw, 405, map[string]any{"error": "method not allowed"})
			return
		}
		st, err := a.deps.ChatGPT.Start()
		if err != nil {
			httpx.WriteJSON(rw, 400, map[string]any{"error": err.Error()})
			return
		}
		httpx.WriteJSON(rw, 200, st)
	}))

	mux.HandleFunc("/api/chatgpt/oauth/status", cors(func(rw http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(rw, 200, a.deps.ChatGPT.Status())
	}))

	mux.HandleFunc("/api/chatgpt/oauth/cancel", cors(func(rw http.ResponseWriter, r *http.Request) {
		a.deps.ChatGPT.Cancel()
		httpx.WriteJSON(rw, 200, a.deps.ChatGPT.Status())
	}))

	mux.HandleFunc("/api/chatgpt/oauth/logout", cors(func(rw http.ResponseWriter, r *http.Request) {
		a.deps.ChatGPT.Logout()
		a.bus.Emit("refresh", "", "system", i18n.T("Signed out of ChatGPT"), nil)
		httpx.WriteJSON(rw, 200, a.deps.ChatGPT.Status())
	}))

}
