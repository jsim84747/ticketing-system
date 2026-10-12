package httpapi

import (
	"embed"
	"encoding/json"
	"fmt"
	"github.com/jsim84747/ticketing-system/internal/auth"
	"github.com/jsim84747/ticketing-system/internal/queue"
	"io/fs"
	"net/http"
	"strings"
	"time"
)

//go:embed web/*
var webFiles embed.FS

func respond(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
func problem(w http.ResponseWriter, code int, message string) {
	respond(w, code, map[string]string{"error": message})
}
func Handler(q *queue.Store, signer *auth.Signer, events []string) http.Handler {
	allowed := map[string]bool{}
	for _, e := range events {
		allowed[e] = true
	}
	m := http.NewServeMux()
	static, _ := fs.Sub(webFiles, "web")
	m.Handle("GET /", http.FileServer(http.FS(static)))
	m.HandleFunc("POST /session", func(w http.ResponseWriter, r *http.Request) {
		id, token, err := signer.Session()
		if err != nil {
			problem(w, 500, "session unavailable")
			return
		}
		respond(w, 201, map[string]string{"user_id": id, "session_token": token})
	})
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := q.Client.Ping(r.Context()).Err(); err != nil {
			problem(w, 503, "redis unavailable")
			return
		}
		respond(w, 200, map[string]string{"status": "ok"})
	})
	protect := func(fn func(http.ResponseWriter, *http.Request, string, string)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			event := r.PathValue("id")
			if !allowed[event] {
				problem(w, 404, "unknown event")
				return
			}
			session, err := signer.Verify(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "session")
			if err != nil {
				problem(w, 401, "valid session required")
				return
			}
			fn(w, r, event, session.User)
		}
	}
	status := func(action string) http.HandlerFunc {
		return protect(func(w http.ResponseWriter, r *http.Request, event, user string) {
			var v queue.Status
			var err error
			switch action {
			case "join":
				v, err = q.Join(r.Context(), event, user)
			case "heartbeat":
				v, err = q.Heartbeat(r.Context(), event, user)
			default:
				v, err = q.Status(r.Context(), event, user)
			}
			if err != nil {
				problem(w, 503, "queue unavailable")
				return
			}
			if v.Status == "admitted" {
				v.Token = signer.Sign(auth.Claims{Purpose: "admission", User: user, Event: event, Generation: v.Generation, Expires: v.ExpiresAt})
			}
			v.Generation = ""
			respond(w, 200, v)
		})
	}
	m.HandleFunc("POST /events/{id}/queue", status("join"))
	m.HandleFunc("GET /events/{id}/queue/status", status("status"))
	m.HandleFunc("POST /events/{id}/queue/heartbeat", status("heartbeat"))
	m.HandleFunc("GET /events/{id}/queue/stream", protect(func(w http.ResponseWriter, r *http.Request, event, user string) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Accel-Buffering", "no")
		controller := http.NewResponseController(w)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			if _, err := signer.Verify(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "session"); err != nil {
				return
			}
			v, err := q.Status(r.Context(), event, user)
			if err != nil {
				return
			}
			if v.Status == "admitted" {
				v.Token = signer.Sign(auth.Claims{Purpose: "admission", User: user, Event: event, Generation: v.Generation, Expires: v.ExpiresAt})
			}
			v.Generation = ""
			payload, err := json.Marshal(v)
			if err != nil {
				return
			}
			// Renew per-frame deadlines so server WriteTimeout does not cut healthy streams.
			_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := fmt.Fprintf(w, "event: status\ndata: %s\n\n", payload); err != nil {
				return
			}
			if err := controller.Flush(); err != nil {
				return
			}
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
			}
		}
	}))
	m.HandleFunc("POST /events/{id}/queue/leave", protect(func(w http.ResponseWriter, r *http.Request, event, user string) {
		if err := q.Leave(r.Context(), event, user); err != nil {
			problem(w, 503, "queue unavailable")
			return
		}
		w.WriteHeader(204)
	}))
	m.HandleFunc("POST /events/{id}/reserve", protect(func(w http.ResponseWriter, r *http.Request, event, user string) {
		c, err := signer.Verify(r.Header.Get("X-Admission-Token"), "admission")
		if err != nil || c.User != user || c.Event != event || c.Generation == "" {
			problem(w, 403, "valid admission token required")
			return
		}
		id, err := auth.ID()
		if err != nil {
			problem(w, 500, "reservation unavailable")
			return
		}
		v, err := q.Reserve(r.Context(), event, user, c.Generation, id)
		if err != nil {
			switch strings.TrimPrefix(err.Error(), "ERR ") {
			case "NOT_ADMITTED":
				problem(w, 403, "admission expired")
			case "SOLD_OUT", "ALREADY_RESERVED":
				problem(w, 409, strings.TrimPrefix(err.Error(), "ERR "))
			default:
				problem(w, 503, "reservation unavailable")
			}
			return
		}
		v.Generation = ""
		respond(w, 200, v)
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; font-src 'self' https://cdn.jsdelivr.net; frame-ancestors 'none'; base-uri 'none'")
		m.ServeHTTP(w, r)
	})
}
