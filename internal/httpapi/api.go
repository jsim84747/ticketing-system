package httpapi

import (
	"encoding/json"
	"github.com/jsim84747/ticketing-system/internal/queue"
	"net/http"
)

func Handler(q *queue.Store) http.Handler {
	m := http.NewServeMux()
	handle := func(join bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			user := r.Header.Get("X-User-ID")
			if user == "" {
				http.Error(w, "user required", 401)
				return
			}
			var v queue.Status
			var err error
			if join {
				v, err = q.Join(r.Context(), r.PathValue("id"), user)
			} else {
				v, err = q.Status(r.Context(), r.PathValue("id"), user)
			}
			if err != nil {
				http.Error(w, "queue unavailable", 503)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			json.NewEncoder(w).Encode(v)
		}
	}
	m.HandleFunc("POST /events/{id}/queue", handle(true))
	m.HandleFunc("GET /events/{id}/queue/status", handle(false))
	m.HandleFunc("POST /events/{id}/queue/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		user := r.Header.Get("X-User-ID")
		if user == "" {
			http.Error(w, "user required", 401)
			return
		}
		v, err := q.Heartbeat(r.Context(), r.PathValue("id"), user)
		if err != nil {
			http.Error(w, "queue unavailable", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	})
	m.HandleFunc("POST /events/{id}/queue/leave", func(w http.ResponseWriter, r *http.Request) {
		user := r.Header.Get("X-User-ID")
		if user == "" {
			http.Error(w, "user required", 401)
			return
		}
		if err := q.Leave(r.Context(), r.PathValue("id"), user); err != nil {
			http.Error(w, "queue unavailable", 503)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return m
}
