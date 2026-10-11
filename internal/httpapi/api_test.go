package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jsim84747/ticketing-system/internal/auth"
	"github.com/jsim84747/ticketing-system/internal/queue"
	"github.com/redis/go-redis/v9"
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
	h := Handler(q, signer, []string{event, "other"})
	call := func(method, path, session, admission string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
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
	h := Handler(q, signer, []string{event})
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
	for _, path := range []string{"/", "/app.js", "/style.css"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Fatalf("static %s: %d", path, w.Code)
		}
	}
}
