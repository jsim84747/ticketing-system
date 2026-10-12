package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jsim84747/ticketing-system/internal/auth"
	"github.com/jsim84747/ticketing-system/internal/queue"
	"github.com/redis/go-redis/v9"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type testClock struct{ t time.Time }

func (c *testClock) Now() time.Time { return c.t }
func TestReservationSecurityAndInventory(t *testing.T) {
	ctx := context.Background()
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:16379"
	}
	r := redis.NewClient(&redis.Options{Addr: addr})
	defer r.Close()
	clock := &testClock{time.Unix(1800000000, 0)}
	cfg := queue.DefaultConfig()
	cfg.ActiveLimit = 2
	q, err := queue.New(r, cfg, clock)
	if err != nil {
		t.Fatal(err)
	}
	event := fmt.Sprintf("http-%d", time.Now().UnixNano())
	if err := q.InitEvent(ctx, event, 1); err != nil {
		t.Fatal(err)
	}
	defer func() {
		keys, _ := r.Keys(ctx, "vwr:{"+event+"}:*").Result()
		if len(keys) > 0 {
			r.Del(ctx, keys...)
		}
	}()
	signer, _ := auth.New([]byte(strings.Repeat("k", 32)), clock.Now)
	h := Handler(q, signer, []string{event, "other"}, "")
	call := func(method, path, session, admission string) *httptest.ResponseRecorder {
		body := ""
		if strings.HasSuffix(path, "/reserve") {
			body = `{"first_name":"Jiwon","last_name":"Shim","email":"jiwon@example.com"}`
		}
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if session != "" {
			req.Header.Set("Authorization", "Bearer "+session)
		}
		req.Header.Set("X-Admission-Token", admission)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	session := func() (string, string) {
		w := call("POST", "/session", "", "")
		if w.Code != 201 {
			t.Fatal(w.Body.String())
		}
		var v map[string]string
		json.Unmarshal(w.Body.Bytes(), &v)
		return v["user_id"], v["session_token"]
	}
	u, a := session()
	_, b := session()
	base := "/events/" + event
	assertCode := func(w *httptest.ResponseRecorder, n int) {
		t.Helper()
		if w.Code != n {
			t.Fatalf("want %d got %d %s", n, w.Code, w.Body.String())
		}
	}
	assertCode(call("POST", base+"/queue", "", ""), 401)
	assertCode(call("POST", base+"/reserve", a, ""), 403)
	assertCode(call("POST", base+"/reserve", a, a), 403)
	assertCode(call("POST", base+"/queue", a, ""), 200)
	assertCode(call("POST", base+"/queue", b, ""), 200)
	if _, err := q.Tick(ctx, event); err != nil {
		t.Fatal(err)
	}
	token := func(session string) string {
		w := call("GET", base+"/queue/status", session, "")
		assertCode(w, 200)
		var v queue.Status
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		if v.Token == "" {
			t.Fatal("missing token")
		}
		return v.Token
	}
	old := token(a)
	bt := token(b)
	assertCode(call("POST", base+"/reserve", b, old), 403)
	assertCode(call("POST", "/events/other/reserve", a, old), 403)
	assertCode(call("POST", base+"/reserve", a, old+"x"), 403)
	assertCode(call("POST", base+"/queue/leave", a, ""), 204)
	assertCode(call("POST", base+"/reserve", a, old), 403)
	assertCode(call("POST", base+"/queue", a, ""), 200)
	q.Tick(ctx, event)
	assertCode(call("POST", base+"/reserve", a, old), 403)
	fresh := token(a)
	// Competing buyers and concurrent retries must consume one ticket exactly once.
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ss, tk := a, fresh
			if i%2 == 1 {
				ss, tk = b, bt
			}
			w := call("POST", base+"/reserve", ss, tk)
			if w.Code != 200 && w.Code != 409 {
				t.Errorf("reservation %d: %s", w.Code, w.Body.String())
			}
		}(i)
	}
	wg.Wait()
	if n := r.Get(ctx, "vwr:{"+event+"}:stock").Val(); n != "0" {
		t.Fatalf("stock %s", n)
	}
	if n := r.HLen(ctx, "vwr:{"+event+"}:reservations").Val(); n != 1 {
		t.Fatalf("reservations %d", n)
	}
	// Remaining admission must expire even when worker has not executed.
	v, _ := q.Status(ctx, event, u)
	loser, lt := a, fresh
	if v.Status == "completed" {
		loser, lt = b, bt
	}
	clock.t = clock.t.Add(30 * time.Second)
	assertCode(call("POST", base+"/reserve", loser, lt), 403)
	assertCode(call("POST", base+"/queue/heartbeat", loser, ""), 200)
}

// Cancel at first flushed frame: no sleeps and no access to a live recorder.
type cancelRecorder struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (w *cancelRecorder) Flush() { w.ResponseRecorder.Flush(); w.cancel() }
func TestSSEAndStatic(t *testing.T) {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:16379"
	}
	r := redis.NewClient(&redis.Options{Addr: addr})
	defer r.Close()
	event := fmt.Sprintf("stream-%d", time.Now().UnixNano())
	q, _ := queue.New(r, queue.DefaultConfig(), nil)
	ctx := context.Background()
	if err := q.InitEvent(ctx, event, 1); err != nil {
		t.Fatal(err)
	}
	defer func() {
		keys, _ := r.Keys(ctx, "vwr:{"+event+"}:*").Result()
		if len(keys) > 0 {
			r.Del(ctx, keys...)
		}
	}()
	signer, _ := auth.New([]byte(strings.Repeat("k", 32)), nil)
	user, session, _ := signer.Session()
	h := Handler(q, signer, []string{event}, "")
	if _, err := q.Join(ctx, event, user); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Tick(ctx, event); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req := httptest.NewRequest("GET", "/events/"+event+"/queue/stream", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+session)
	w := &cancelRecorder{httptest.NewRecorder(), cancel}
	h.ServeHTTP(w, req)
	if w.Code != 200 || w.Header().Get("Content-Type") != "text/event-stream" || !strings.Contains(w.Body.String(), `"status":"admitted"`) || !strings.Contains(w.Body.String(), `"token":`) {
		t.Fatalf("stream: %d %s", w.Code, w.Body.String())
	}
	root := httptest.NewRecorder()
	h.ServeHTTP(root, httptest.NewRequest("GET", "/", nil))
	if root.Code != 200 || !strings.Contains(root.Body.String(), `data-event="`+event+`"`) {
		t.Fatalf("root page should default to the first event: %d %s", root.Code, root.Body.String()[:min(200, root.Body.Len())])
	}
	for _, path := range []string{"/", "/app.js", "/style.css", "/admin", "/admin.js", "/admin.css"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Fatalf("static %s: %d", path, w.Code)
		}
	}
}

func TestAdminAPI(t *testing.T) {
	ctx := context.Background()
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:16379"
	}
	r := redis.NewClient(&redis.Options{Addr: addr})
	defer r.Close()
	cfg := queue.DefaultConfig()
	cfg.ActiveLimit = 1
	q, err := queue.New(r, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	event := fmt.Sprintf("http-admin-%d", time.Now().UnixNano())
	if err := q.InitEvent(ctx, event, 5); err != nil {
		t.Fatal(err)
	}
	defer func() {
		keys, _ := r.Keys(ctx, "vwr:{"+event+"}:*").Result()
		if len(keys) > 0 {
			r.Del(ctx, keys...)
		}
	}()
	signer, _ := auth.New([]byte(strings.Repeat("k", 32)), nil)
	call := func(h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	assertCode := func(w *httptest.ResponseRecorder, n int) {
		t.Helper()
		if w.Code != n {
			t.Fatalf("want %d got %d %s", n, w.Code, w.Body.String())
		}
	}

	// Without ADMIN_TOKEN the admin API does not exist.
	disabled := Handler(q, signer, []string{event}, "")
	assertCode(call(disabled, "GET", "/admin/api/events", "", ""), 404)
	assertCode(call(disabled, "GET", "/admin/api/events", "anything", ""), 404)

	h := Handler(q, signer, []string{event}, "admin-secret")
	assertCode(call(h, "GET", "/admin/api/events", "", ""), 401)
	assertCode(call(h, "GET", "/admin/api/events", "wrong", ""), 401)
	// A user session token must not unlock the admin API.
	_, session, _ := signer.Session()
	assertCode(call(h, "GET", "/admin/api/events", session, ""), 401)
	assertCode(call(h, "GET", "/admin/api/events/other/active", "admin-secret", ""), 404)

	for _, u := range []string{"first", "second"} {
		if _, err := q.Join(ctx, event, u); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := q.Tick(ctx, event); err != nil {
		t.Fatal(err)
	}

	w := call(h, "GET", "/admin/api/events", "admin-secret", "")
	assertCode(w, 200)
	var list []struct {
		ID string `json:"id"`
		queue.Stats
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != event || list[0].ActiveLimit != 1 || list[0].Active != 1 || list[0].Waiting != 1 || list[0].Remaining != 5 || list[0].Reserved != 0 {
		t.Fatalf("events: %s", w.Body.String())
	}

	w = call(h, "GET", "/admin/api/events/"+event+"/active", "admin-secret", "")
	assertCode(w, 200)
	if !strings.Contains(w.Body.String(), `"users":["first"]`) || !strings.Contains(w.Body.String(), `"total":1`) {
		t.Fatalf("active: %s", w.Body.String())
	}

	// Kicking the admitted user frees the slot for the next one in line.
	assertCode(call(h, "POST", "/admin/api/events/"+event+"/kick", "admin-secret", `{}`), 400)
	assertCode(call(h, "POST", "/admin/api/events/"+event+"/kick", "admin-secret", `{"user_id":"first"}`), 204)
	if _, err := q.Tick(ctx, event); err != nil {
		t.Fatal(err)
	}
	if v, _ := q.Status(ctx, event, "first"); v.Status != "absent" {
		t.Fatalf("kicked user status %q", v.Status)
	}
	if v, _ := q.Status(ctx, event, "second"); v.Status != "admitted" {
		t.Fatalf("next user status %q", v.Status)
	}

	// Settings: validation, then name, capacity, inventory and manual mode.
	settings := "/admin/api/events/" + event + "/settings"
	assertCode(call(h, "POST", settings, "", `{"mode":"manual"}`), 401)
	assertCode(call(h, "POST", settings, "admin-secret", `{"active_limit":0}`), 400)
	assertCode(call(h, "POST", settings, "admin-secret", `{"mode":"later"}`), 400)
	assertCode(call(h, "POST", settings, "admin-secret", `{"unknown":1}`), 400)
	assertCode(call(h, "POST", settings, "admin-secret", `{"name":"Spring Concert","active_limit":2,"remaining":3,"mode":"manual"}`), 204)
	for _, u := range []string{"third", "fourth"} {
		q.Join(ctx, event, u)
	}
	if ids, _ := q.Tick(ctx, event); len(ids) != 0 {
		t.Fatalf("manual mode worker admitted %v", ids)
	}
	admit := "/admin/api/events/" + event + "/admit"
	assertCode(call(h, "POST", admit, "admin-secret", `{"count":0}`), 400)
	w = call(h, "POST", admit, "admin-secret", `{"count":5}`)
	assertCode(w, 200)
	if !strings.Contains(w.Body.String(), `"admitted":1`) {
		t.Fatalf("admit should fill only the one free slot: %s", w.Body.String())
	}
	assertCode(call(h, "POST", settings, "admin-secret", `{"open_at":-5}`), 400)
	// Scheduling an opening sends the two admitted users back to the line.
	assertCode(call(h, "POST", settings, "admin-secret", `{"open_at":4102444800000}`), 204)
	w = call(h, "GET", "/admin/api/events", "admin-secret", "")
	for _, want := range []string{`"name":"Spring Concert"`, `"active_limit":2`, `"remaining":3`, `"mode":"manual"`, `"open_at":4102444800000`, `"active":0`, `"waiting":3`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("events missing %s: %s", want, w.Body.String())
		}
	}

	// With the join button locked, a new visitor cannot join before opening.
	assertCode(call(h, "POST", settings, "admin-secret", `{"join_lock":true}`), 204)
	_, visitor, _ := signer.Session()
	if w := call(h, "POST", "/events/"+event+"/queue", visitor, ""); w.Code != 409 || !strings.Contains(w.Body.String(), "not open yet") {
		t.Fatalf("locked join: %d %s", w.Code, w.Body.String())
	}
}

func TestReservationDetailsAndAdminList(t *testing.T) {
	ctx := context.Background()
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:16379"
	}
	r := redis.NewClient(&redis.Options{Addr: addr})
	defer r.Close()
	cfg := queue.DefaultConfig()
	cfg.ActiveLimit = 1
	q, err := queue.New(r, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	event := fmt.Sprintf("http-details-%d", time.Now().UnixNano())
	if err := q.InitEvent(ctx, event, 5); err != nil {
		t.Fatal(err)
	}
	defer func() {
		keys, _ := r.Keys(ctx, "vwr:{"+event+"}:*").Result()
		if len(keys) > 0 {
			r.Del(ctx, keys...)
		}
	}()
	signer, _ := auth.New([]byte(strings.Repeat("k", 32)), nil)
	h := Handler(q, signer, []string{event}, "admin-secret")
	call := func(method, path, bearer, admission, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+bearer)
		if admission != "" {
			req.Header.Set("X-Admission-Token", admission)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	user, session, _ := signer.Session()
	base := "/events/" + event
	call("POST", base+"/queue", session, "", "")
	q.Tick(ctx, event)
	var st queue.Status
	json.Unmarshal(call("GET", base+"/queue/status", session, "", "").Body.Bytes(), &st)
	if st.Token == "" {
		t.Fatalf("not admitted: %+v", st)
	}
	for _, bad := range []string{``, `{}`, `{"first_name":"Jiwon","last_name":"Shim","email":"not-an-email"}`, `{"first_name":" ","last_name":"Shim","email":"a@b.co"}`} {
		if w := call("POST", base+"/reserve", session, st.Token, bad); w.Code != 400 {
			t.Fatalf("details %q: want 400 got %d %s", bad, w.Code, w.Body.String())
		}
	}
	if n := r.Get(ctx, "vwr:{"+event+"}:stock").Val(); n != "5" {
		t.Fatalf("rejected details must not use a ticket, stock %s", n)
	}
	w := call("POST", base+"/reserve", session, st.Token, `{"first_name":" Jiwon ","last_name":"Shim","email":"jiwon@example.com"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"first_name":"Jiwon"`) {
		t.Fatalf("reserve: %d %s", w.Code, w.Body.String())
	}
	if w := call("GET", "/admin/api/events/"+event+"/reservations", "wrong", "", ""); w.Code != 401 {
		t.Fatalf("reservations without admin password: %d", w.Code)
	}
	w = call("GET", "/admin/api/events/"+event+"/reservations", "admin-secret", "", "")
	for _, want := range []string{`"total":1`, `"user_id":"` + user + `"`, `"first_name":"Jiwon"`, `"last_name":"Shim"`, `"email":"jiwon@example.com"`, `"reserved_at":`} {
		if w.Code != 200 || !strings.Contains(w.Body.String(), want) {
			t.Fatalf("reservations missing %s: %d %s", want, w.Code, w.Body.String())
		}
	}
	if w := call("GET", "/admin/api/events/"+event+"/reservations?limit=1", "admin-secret", "", ""); !strings.Contains(w.Body.String(), `"total":1`) {
		t.Fatalf("limited reservations: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"generation"`) {
		t.Fatalf("reservations leak the admission generation: %s", w.Body.String())
	}

	// Editing a reservation changes the details only.
	edit := "/admin/api/events/" + event + "/reservations/" + user
	if w := call("PUT", edit, "wrong", "", `{"first_name":"A","last_name":"B","email":"a@b.co"}`); w.Code != 401 {
		t.Fatalf("edit without admin password: %d", w.Code)
	}
	if w := call("PUT", edit, "admin-secret", "", `{"first_name":" ","last_name":"B","email":"a@b.co"}`); w.Code != 400 {
		t.Fatalf("edit without a first name: %d", w.Code)
	}
	if w := call("PUT", "/admin/api/events/"+event+"/reservations/nobody", "admin-secret", "", `{"first_name":"A","last_name":"B","email":"a@b.co"}`); w.Code != 404 {
		t.Fatalf("edit unknown reservation: %d", w.Code)
	}
	before := r.HGet(ctx, "vwr:{"+event+"}:reservations", user).Val()
	// Admins may save any contact detail, even one that is not an email address.
	w = call("PUT", edit, "admin-secret", "", `{"first_name":"Ji-won","last_name":"Shim","email":" call 010-1234-5678 "}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"email":"call 010-1234-5678"`) || strings.Contains(w.Body.String(), `"generation"`) {
		t.Fatalf("edit: %d %s", w.Code, w.Body.String())
	}
	var was, now queue.Reservation
	json.Unmarshal([]byte(before), &was)
	json.Unmarshal([]byte(r.HGet(ctx, "vwr:{"+event+"}:reservations", user).Val()), &now)
	if now.FirstName != "Ji-won" || now.ID != was.ID || now.ReservedAt != was.ReservedAt || now.Generation != was.Generation {
		t.Fatalf("edit changed more than the details: %+v -> %+v", was, now)
	}
	if n := r.Get(ctx, "vwr:{"+event+"}:stock").Val(); n != "4" {
		t.Fatalf("editing must not touch inventory, stock %s", n)
	}

	// Deleting cancels the reservation and returns its ticket.
	if w := call("DELETE", edit, "wrong", "", ""); w.Code != 401 {
		t.Fatalf("delete without admin password: %d", w.Code)
	}
	if w := call("DELETE", edit, "admin-secret", "", ""); w.Code != 204 {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	if w := call("DELETE", edit, "admin-secret", "", ""); w.Code != 404 {
		t.Fatalf("second delete: %d", w.Code)
	}
	if n := r.Get(ctx, "vwr:{"+event+"}:stock").Val(); n != "5" {
		t.Fatalf("deleted reservation should return its ticket, stock %s", n)
	}
	if w := call("GET", "/admin/api/events/"+event+"/reservations", "admin-secret", "", ""); !strings.Contains(w.Body.String(), `"total":0`) {
		t.Fatalf("reservation still listed: %s", w.Body.String())
	}
}

func TestTicketQuantities(t *testing.T) {
	ctx := context.Background()
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:16379"
	}
	r := redis.NewClient(&redis.Options{Addr: addr})
	defer r.Close()
	cfg := queue.DefaultConfig()
	cfg.ActiveLimit = 2
	q, _ := queue.New(r, cfg, nil)
	event := fmt.Sprintf("http-qty-%d", time.Now().UnixNano())
	if err := q.InitEvent(ctx, event, 5); err != nil {
		t.Fatal(err)
	}
	defer func() {
		keys, _ := r.Keys(ctx, "vwr:{"+event+"}:*").Result()
		if len(keys) > 0 {
			r.Del(ctx, keys...)
		}
	}()
	signer, _ := auth.New([]byte(strings.Repeat("k", 32)), nil)
	h := Handler(q, signer, []string{event}, "admin-secret")
	call := func(method, path, bearer, admission, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+bearer)
		if admission != "" {
			req.Header.Set("X-Admission-Token", admission)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	base := "/events/" + event
	admit := func() (string, string) {
		user, session, _ := signer.Session()
		call("POST", base+"/queue", session, "", "")
		q.Tick(ctx, event)
		var st queue.Status
		json.Unmarshal(call("GET", base+"/queue/status", session, "", "").Body.Bytes(), &st)
		if st.Token == "" || st.MaxPerOrder != 4 {
			t.Fatalf("admitted status: %+v", st)
		}
		return user, session + "|" + st.Token
	}
	details := func(n int) string {
		return fmt.Sprintf(`{"first_name":"A","last_name":"B","email":"a@b.co","quantity":%d}`, n)
	}
	reserve := func(pair string, n int) *httptest.ResponseRecorder {
		s, tk, _ := strings.Cut(pair, "|")
		return call("POST", base+"/reserve", s, tk, details(n))
	}
	first, a := admit()
	_, b := admit()
	if w := reserve(a, 5); w.Code != 400 {
		t.Fatalf("5 tickets is over the default limit of 4: %d %s", w.Code, w.Body.String())
	}
	if w := reserve(a, 4); w.Code != 200 || !strings.Contains(w.Body.String(), `"quantity":4`) {
		t.Fatalf("reserve 4: %d %s", w.Code, w.Body.String())
	}
	if w := reserve(b, 2); w.Code != 409 || !strings.Contains(w.Body.String(), "only 1 tickets left") {
		t.Fatalf("reserve 2 with 1 left: %d %s", w.Code, w.Body.String())
	}
	if n := r.Get(ctx, "vwr:{"+event+"}:stock").Val(); n != "1" {
		t.Fatalf("stock after 4 of 5: %s", n)
	}
	// Deleting returns all four tickets.
	if w := call("DELETE", "/admin/api/events/"+event+"/reservations/"+first, "admin-secret", "", ""); w.Code != 204 {
		t.Fatalf("delete: %d", w.Code)
	}
	if n := r.Get(ctx, "vwr:{"+event+"}:stock").Val(); n != "5" {
		t.Fatalf("stock after delete: %s", n)
	}
	// Admins can change the per-order limit.
	if w := call("POST", "/admin/api/events/"+event+"/settings", "admin-secret", "", `{"max_per_order":21}`); w.Code != 400 {
		t.Fatalf("limit 21: %d", w.Code)
	}
	if w := call("POST", "/admin/api/events/"+event+"/settings", "admin-secret", "", `{"max_per_order":1}`); w.Code != 204 {
		t.Fatalf("limit 1: %d", w.Code)
	}
	if w := reserve(b, 2); w.Code != 400 {
		t.Fatalf("2 tickets over a limit of 1: %d %s", w.Code, w.Body.String())
	}
	if w := reserve(b, 1); w.Code != 200 {
		t.Fatalf("reserve 1: %d %s", w.Code, w.Body.String())
	}
}
