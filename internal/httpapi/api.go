package httpapi

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jsim84747/ticketing-system/internal/auth"
	"github.com/jsim84747/ticketing-system/internal/queue"
	"html"
	"io/fs"
	"net/http"
	"strconv"
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

// Handler serves the user-facing queue API and pages. When adminToken is
// non-empty it also serves the admin console at /admin, whose API requires
// that token as a bearer credential; when empty the admin API is disabled.
// Handler serves the user-facing queue API and pages. Events are the ones in
// the Redis registry (managed from the admin page) plus any passed in events,
// which tests and the load harness use without registering them.
func Handler(q *queue.Store, signer *auth.Signer, events []string, adminToken string) http.Handler {
	allowed := map[string]bool{}
	for _, e := range events {
		allowed[e] = true
	}
	known := func(r *http.Request, event string) bool {
		if allowed[event] {
			return true
		}
		ok, _ := q.HasEvent(r.Context(), event)
		return ok
	}
	// listEvents is the passed-in events followed by registered ones.
	listEvents := func(r *http.Request) ([]string, error) {
		registered, err := q.Events(r.Context())
		if err != nil {
			return nil, err
		}
		out := append([]string{}, events...)
		for _, e := range registered {
			if !allowed[e] {
				out = append(out, e)
			}
		}
		return out, nil
	}
	defaultEvent := func(r *http.Request) string {
		if def, err := q.DefaultEvent(r.Context()); err == nil && def != "" {
			return def
		}
		if len(events) > 0 {
			return events[0]
		}
		return ""
	}
	m := http.NewServeMux()
	static, _ := fs.Sub(webFiles, "web")
	m.Handle("GET /", http.FileServer(http.FS(static)))
	// The bare root serves the main event, so the shareable link needs no
	// ?event= query. Other events stay reachable with ?event=<id>.
	page, _ := fs.ReadFile(static, "index.html")
	m.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(strings.Replace(string(page), `<html lang="en">`, `<html lang="en" data-event="`+html.EscapeString(defaultEvent(r))+`">`, 1)))
	})
	m.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, static, "admin.html")
	})
	admin := func(fn http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if adminToken == "" {
				problem(w, 404, "admin disabled")
				return
			}
			given := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(given), []byte(adminToken)) != 1 {
				problem(w, 401, "admin password required")
				return
			}
			if id := r.PathValue("id"); id != "" && !known(r, id) {
				problem(w, 404, "unknown event")
				return
			}
			fn(w, r)
		}
	}
	m.HandleFunc("GET /admin/api/events", admin(func(w http.ResponseWriter, r *http.Request) {
		type eventStats struct {
			ID      string `json:"id"`
			Default bool   `json:"default"`
			queue.Stats
		}
		list, err := listEvents(r)
		if err != nil {
			problem(w, 503, "queue unavailable")
			return
		}
		def := defaultEvent(r)
		out := make([]eventStats, 0, len(list))
		for _, e := range list {
			s, err := q.Stats(r.Context(), e)
			if err != nil {
				problem(w, 503, "queue unavailable")
				return
			}
			out = append(out, eventStats{e, e == def, s})
		}
		respond(w, 200, out)
	}))
	m.HandleFunc("POST /admin/api/events", admin(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Tickets     int    `json:"tickets"`
			ActiveLimit int    `json:"active_limit"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			problem(w, 400, "invalid event")
			return
		}
		if body.Tickets < 0 || body.Tickets > 10000000 {
			problem(w, 400, "tickets must be between 0 and 10000000")
			return
		}
		settings := queue.Settings{Name: &body.Name}
		if body.ActiveLimit != 0 {
			settings.ActiveLimit = &body.ActiveLimit
		}
		if err := settings.Validate(); err != nil {
			problem(w, 400, err.Error())
			return
		}
		switch err := q.CreateEvent(r.Context(), body.ID, body.Tickets); {
		case errors.Is(err, queue.ErrInvalidEvent):
			problem(w, 400, err.Error())
			return
		case errors.Is(err, queue.ErrEventExists):
			problem(w, 409, err.Error())
			return
		case err != nil:
			problem(w, 503, "queue unavailable")
			return
		}
		if err := q.UpdateSettings(r.Context(), body.ID, settings); err != nil {
			problem(w, 503, "queue unavailable")
			return
		}
		respond(w, 201, map[string]string{"id": body.ID})
	}))
	m.HandleFunc("DELETE /admin/api/events/{id}", admin(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if allowed[id] {
			problem(w, 400, "this event is built into the server and cannot be deleted")
			return
		}
		list, err := listEvents(r)
		if err != nil {
			problem(w, 503, "queue unavailable")
			return
		}
		if len(list) <= 1 {
			problem(w, 400, "keep at least one event")
			return
		}
		if err := q.DeleteEvent(r.Context(), id); err != nil {
			problem(w, 503, "queue unavailable")
			return
		}
		w.WriteHeader(204)
	}))
	m.HandleFunc("POST /admin/api/events/{id}/default", admin(func(w http.ResponseWriter, r *http.Request) {
		if err := q.SetDefaultEvent(r.Context(), r.PathValue("id")); err != nil {
			problem(w, 503, "queue unavailable")
			return
		}
		w.WriteHeader(204)
	}))
	m.HandleFunc("GET /admin/api/events/{id}/active", admin(func(w http.ResponseWriter, r *http.Request) {
		users, err := q.ActiveUsers(r.Context(), r.PathValue("id"))
		if err != nil {
			problem(w, 503, "queue unavailable")
			return
		}
		total := len(users)
		users = users[:min(total, 100)]
		respond(w, 200, map[string]any{"total": total, "users": users})
	}))
	m.HandleFunc("POST /admin/api/events/{id}/settings", admin(func(w http.ResponseWriter, r *http.Request) {
		var v queue.Settings
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&v); err != nil {
			problem(w, 400, "invalid settings")
			return
		}
		if err := v.Validate(); err != nil {
			problem(w, 400, err.Error())
			return
		}
		if err := q.UpdateSettings(r.Context(), r.PathValue("id"), v); err != nil {
			problem(w, 503, "queue unavailable")
			return
		}
		w.WriteHeader(204)
	}))
	m.HandleFunc("POST /admin/api/events/{id}/admit", admin(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Count int `json:"count"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Count < 1 || body.Count > 1000 {
			problem(w, 400, "count must be between 1 and 1000")
			return
		}
		ids, err := q.Admit(r.Context(), r.PathValue("id"), body.Count)
		if err != nil {
			problem(w, 503, "queue unavailable")
			return
		}
		respond(w, 200, map[string]int{"admitted": len(ids)})
	}))
	m.HandleFunc("GET /admin/api/events/{id}/reservations", admin(func(w http.ResponseWriter, r *http.Request) {
		list, err := q.Reservations(r.Context(), r.PathValue("id"))
		if err != nil {
			problem(w, 503, "queue unavailable")
			return
		}
		total := len(list)
		// ?limit=N returns only the newest N, for the dashboard's live table.
		if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n < total {
			list = list[:n]
		}
		respond(w, 200, map[string]any{"total": total, "reservations": list})
	}))
	m.HandleFunc("PUT /admin/api/events/{id}/reservations/{user}", admin(func(w http.ResponseWriter, r *http.Request) {
		var who queue.Customer
		if err := json.NewDecoder(r.Body).Decode(&who); err != nil {
			problem(w, 400, "first name, last name and email are required")
			return
		}
		if _, err := who.NormalizeAdmin(); err != nil {
			problem(w, 400, err.Error())
			return
		}
		v, err := q.UpdateReservation(r.Context(), r.PathValue("id"), r.PathValue("user"), who)
		if errors.Is(err, queue.ErrReservationNotFound) {
			problem(w, 404, "reservation not found")
			return
		}
		if err != nil {
			problem(w, 503, "queue unavailable")
			return
		}
		respond(w, 200, v)
	}))
	m.HandleFunc("DELETE /admin/api/events/{id}/reservations/{user}", admin(func(w http.ResponseWriter, r *http.Request) {
		err := q.DeleteReservation(r.Context(), r.PathValue("id"), r.PathValue("user"))
		if errors.Is(err, queue.ErrReservationNotFound) {
			problem(w, 404, "reservation not found")
			return
		}
		if err != nil {
			problem(w, 503, "queue unavailable")
			return
		}
		w.WriteHeader(204)
	}))
	m.HandleFunc("POST /admin/api/events/{id}/kick-all", admin(func(w http.ResponseWriter, r *http.Request) {
		n, err := q.LeaveAll(r.Context(), r.PathValue("id"))
		if err != nil {
			problem(w, 503, "queue unavailable")
			return
		}
		respond(w, 200, map[string]int{"removed": n})
	}))
	m.HandleFunc("POST /admin/api/events/{id}/kick", admin(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			UserID string `json:"user_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.UserID == "" {
			problem(w, 400, "user_id required")
			return
		}
		if err := q.Leave(r.Context(), r.PathValue("id"), body.UserID); err != nil {
			problem(w, 503, "queue unavailable")
			return
		}
		w.WriteHeader(204)
	}))
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
			if !known(r, event) {
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
			if errors.Is(err, queue.ErrNotOpen) {
				problem(w, 409, "not open yet")
				return
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
		ticker := time.NewTicker(time.Second)
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
		var body struct {
			queue.Customer
			Quantity int `json:"quantity"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			problem(w, 400, "first name, last name and email are required")
			return
		}
		who, err := body.Customer.Normalize()
		if err != nil {
			problem(w, 400, err.Error())
			return
		}
		if body.Quantity == 0 {
			body.Quantity = 1
		}
		id, err := auth.ID()
		if err != nil {
			problem(w, 500, "reservation unavailable")
			return
		}
		v, err := q.Reserve(r.Context(), event, user, c.Generation, id, who, body.Quantity)
		if err != nil {
			switch strings.TrimPrefix(err.Error(), "ERR ") {
			case "NOT_ADMITTED":
				problem(w, 403, "admission expired")
			case "SOLD_OUT", "ALREADY_RESERVED":
				problem(w, 409, strings.TrimPrefix(err.Error(), "ERR "))
			case "BAD_QUANTITY":
				problem(w, 400, "that many tickets cannot be reserved in one order")
			default:
				if left, ok := strings.CutPrefix(strings.TrimPrefix(err.Error(), "ERR "), "NOT_ENOUGH "); ok {
					problem(w, 409, "only "+left+" tickets left")
					return
				}
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
