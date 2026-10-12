package queue

import (
	"context"
	"fmt"
	"github.com/redis/go-redis/v9"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Add(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.t = c.t.Add(d) }
func setup(t *testing.T, c Config) (*Store, string, *fakeClock) {
	t.Helper()
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:16379"
	}
	r := redis.NewClient(&redis.Options{Addr: addr, PoolSize: 64, PoolTimeout: 60 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second})
	if err := r.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("real Redis required at %s: %v", addr, err)
	}
	clock := &fakeClock{t: time.Unix(1800000000, 0)}
	s, err := New(r, c, clock)
	if err != nil {
		t.Fatal(err)
	}
	event := fmt.Sprintf("test-%d", time.Now().UnixNano())
	t.Cleanup(func() { r.Del(context.Background(), s.keys(event)...); r.Close() })
	return s, event, clock
}
func TestJoinFIFOAndDuplicate(t *testing.T) {
	s, e, _ := setup(t, DefaultConfig())
	ctx := context.Background()
	for i := 1; i <= 20; i++ {
		v, err := s.Join(ctx, e, fmt.Sprint(i))
		if err != nil || v.Position != int64(i) {
			t.Fatalf("join %d: %+v %v", i, v, err)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := s.Join(ctx, e, "1")
			if err != nil || v.Position != 1 {
				t.Errorf("duplicate: %+v %v", v, err)
			}
		}()
	}
	wg.Wait()
	n := s.Client.ZCard(ctx, s.keys(e)[0]).Val()
	if n != 20 {
		t.Fatalf("duplicates: %d", n)
	}
	v, err := s.Status(ctx, e, "20")
	if err != nil || v.Ahead != 19 {
		t.Fatalf("status: %+v %v", v, err)
	}
}

func TestConcurrent10000AndWorkers(t *testing.T) {
	s, e, _ := setup(t, DefaultConfig())
	ctx := context.Background()
	workerCtx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	for i := 0; i < 4; i++ {
		// Independent connection pools simulate separate server instances.
		client := redis.NewClient(s.Client.Options())
		t.Cleanup(func() { client.Close() })
		other, err := New(client, s.Config, s.Clock)
		if err != nil {
			t.Fatal(err)
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			other.RunWorker(workerCtx, []string{e}, time.Millisecond, func(err error) { t.Error(err) })
		}()
	}
	var wg sync.WaitGroup
	gate := make(chan struct{})
	for i := 0; i < 10000; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-gate
			if _, err := s.Join(ctx, e, fmt.Sprintf("u%05d", i)); err != nil {
				t.Error(err)
			}
		}(i)
	}
	close(gate)
	wg.Wait()
	cancel()
	workers.Wait()
	if _, err := s.Tick(ctx, e); err != nil {
		t.Fatal(err)
	}
	n := s.Client.ZCard(ctx, s.keys(e)[1]).Val()
	w := s.Client.ZCard(ctx, s.keys(e)[0]).Val()
	if n != 1000 || w != 9000 {
		t.Fatalf("active=%d waiting=%d", n, w)
	}
	peak := s.Client.HGet(ctx, s.keys(e)[6], "peak_active").Val()
	if peak != "1000" {
		t.Fatalf("peak %s", peak)
	}
	if v := s.Client.HGet(ctx, s.keys(e)[6], "order_violations").Val(); v != "" && v != "0" {
		t.Fatalf("FIFO violations %s", v)
	}
}
func TestLeavePromotesExactlyFIFO(t *testing.T) {
	s, e, _ := setup(t, DefaultConfig())
	ctx := context.Background()
	for i := 1; i <= 1200; i++ {
		if _, err := s.Join(ctx, e, fmt.Sprint(i)); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := s.Tick(ctx, e)
	if err != nil || len(ids) != 1000 {
		t.Fatalf("tick %d %v", len(ids), err)
	}
	for i := 1; i <= 100; i++ {
		if err := s.Leave(ctx, e, fmt.Sprint(i)); err != nil {
			t.Fatal(err)
		}
	}
	ids, err = s.Tick(ctx, e)
	if err != nil || len(ids) != 100 {
		t.Fatalf("refill %d %v", len(ids), err)
	}
	for i, id := range ids {
		if id != fmt.Sprint(1001+i) {
			t.Fatalf("FIFO at %d: %s", i, id)
		}
	}
	v, err := s.Join(ctx, e, "1001")
	if err != nil || v.Status != "admitted" {
		t.Fatalf("admitted duplicate %+v %v", v, err)
	}
}

// Settings live in Redis, so a server started with different defaults still
// enforces the stored limit instead of disagreeing with its peers.
func TestStoredSettingsWinOverServerDefaults(t *testing.T) {
	c := DefaultConfig()
	c.ActiveLimit = 1
	s, e, _ := setup(t, c)
	ctx := context.Background()
	s.Join(ctx, e, "one")
	s.Join(ctx, e, "two")
	other := *s
	other.Config.ActiveLimit = 2000
	if ids, err := other.Tick(ctx, e); err != nil || len(ids) != 1 {
		t.Fatalf("tick with other defaults admitted %v %v", ids, err)
	}
}

func TestManualModeAndSettings(t *testing.T) {
	c := DefaultConfig()
	c.ActiveLimit = 2
	s, e, _ := setup(t, c)
	ctx := context.Background()
	for i := 1; i <= 6; i++ {
		s.Join(ctx, e, fmt.Sprint(i))
	}
	manual, name := "manual", "Spring Concert"
	if err := s.UpdateSettings(ctx, e, Settings{Mode: &manual, Name: &name}); err != nil {
		t.Fatal(err)
	}
	// In manual mode the worker only expires leases; it admits nobody.
	if ids, err := s.Tick(ctx, e); err != nil || len(ids) != 0 {
		t.Fatalf("manual tick admitted %v %v", ids, err)
	}
	if v, _ := s.Status(ctx, e, "1"); v.Status != "waiting" || v.Name != name || v.Mode != "manual" || v.EstimatedWaitSeconds != nil {
		t.Fatalf("waiting status %+v", v)
	}
	// The admin admits users in FIFO order, still capped by the limit.
	if ids, err := s.Admit(ctx, e, 1); err != nil || len(ids) != 1 || ids[0] != "1" {
		t.Fatalf("admit 1: %v %v", ids, err)
	}
	if ids, err := s.Admit(ctx, e, 5); err != nil || len(ids) != 1 || ids[0] != "2" {
		t.Fatalf("admit capped by limit: %v %v", ids, err)
	}
	// Raising the limit makes room; switching back to auto lets the worker fill it.
	limit, auto, remaining := 4, "auto", 7
	if err := s.UpdateSettings(ctx, e, Settings{ActiveLimit: &limit, Mode: &auto, Remaining: &remaining}); err != nil {
		t.Fatal(err)
	}
	if ids, err := s.Tick(ctx, e); err != nil || len(ids) != 2 || ids[0] != "3" || ids[1] != "4" {
		t.Fatalf("auto tick after raising limit: %v %v", ids, err)
	}
	st, err := s.Stats(ctx, e)
	if err != nil || st.Active != 4 || st.Waiting != 2 || st.ActiveLimit != 4 || st.Mode != "auto" || st.Name != name || st.Remaining != 7 || st.OrderViolations != 0 {
		t.Fatalf("stats %+v %v", st, err)
	}
	closed, long := "Closed", strings.Repeat("x", 41)
	if err := s.UpdateSettings(ctx, e, Settings{Badge: &closed}); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Status(ctx, e, "6"); v.Badge != "Closed" {
		t.Fatalf("badge not shown to visitors: %+v", v)
	}
	if st, _ := s.Stats(ctx, e); st.Badge != "Closed" {
		t.Fatalf("badge not in stats: %+v", st)
	}
	if err := s.UpdateSettings(ctx, e, Settings{Badge: &long}); err == nil {
		t.Fatal("41-character badge accepted")
	}
	bad, badMode := 0, "sometimes"
	if err := s.UpdateSettings(ctx, e, Settings{ActiveLimit: &bad}); err == nil {
		t.Fatal("limit 0 accepted")
	}
	if err := s.UpdateSettings(ctx, e, Settings{Mode: &badMode}); err == nil {
		t.Fatal("unknown mode accepted")
	}
	if _, err := s.Admit(ctx, e, 0); err == nil {
		t.Fatal("admit 0 accepted")
	}
}

func TestScheduledOpening(t *testing.T) {
	c := DefaultConfig()
	c.ActiveLimit = 2
	s, e, clock := setup(t, c)
	ctx := context.Background()
	manual := "manual"
	openAt := clock.Now().Add(time.Minute).UnixMilli()
	if err := s.UpdateSettings(ctx, e, Settings{Mode: &manual, OpenAt: &openAt}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		s.Join(ctx, e, fmt.Sprint(i))
	}
	// Before opening, users only line up.
	clock.Add(59 * time.Second)
	if ids, err := s.Tick(ctx, e); err != nil || len(ids) != 0 {
		t.Fatalf("admitted before opening: %v %v", ids, err)
	}
	if v, _ := s.Status(ctx, e, "1"); v.Status != "waiting" || v.OpenAt != openAt || v.Mode != "manual" {
		t.Fatalf("waiting status %+v", v)
	}
	// At the opening time, admission starts in FIFO order and the event switches to auto.
	clock.Add(time.Second)
	if ids, err := s.Tick(ctx, e); err != nil || len(ids) != 2 || ids[0] != "1" || ids[1] != "2" {
		t.Fatalf("admitted at opening: %v %v", ids, err)
	}
	if st, _ := s.Stats(ctx, e); st.Mode != "auto" || st.OpenAt != 0 {
		t.Fatalf("after opening %+v", st)
	}
	// Setting open_at to 0 cancels the schedule, so manual mode stays closed.
	cancel := int64(0)
	if err := s.UpdateSettings(ctx, e, Settings{Mode: &manual, OpenAt: &cancel}); err != nil {
		t.Fatal(err)
	}
	s.Leave(ctx, e, "1")
	clock.Add(time.Hour)
	if ids, _ := s.Tick(ctx, e); len(ids) != 0 {
		t.Fatalf("cancelled schedule still admitted %v", ids)
	}
	auto := "auto"
	later := clock.Now().Add(time.Hour).UnixMilli()
	s.UpdateSettings(ctx, e, Settings{OpenAt: &later})
	if err := s.UpdateSettings(ctx, e, Settings{Mode: &auto}); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Stats(ctx, e); st.OpenAt != 0 {
		t.Fatalf("switching to auto kept the schedule: %+v", st)
	}
	negative := int64(-1)
	if err := s.UpdateSettings(ctx, e, Settings{OpenAt: &negative}); err == nil {
		t.Fatal("negative opening time accepted")
	}
}

func TestSchedulingSendsAdmittedUsersBackInOrder(t *testing.T) {
	c := DefaultConfig()
	c.ActiveLimit = 2
	s, e, clock := setup(t, c)
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		s.Join(ctx, e, fmt.Sprint(i))
	}
	if ids, _ := s.Tick(ctx, e); len(ids) != 2 {
		t.Fatalf("auto admitted %v", ids)
	}
	manual := "manual"
	openAt := clock.Now().Add(time.Hour).UnixMilli()
	if err := s.UpdateSettings(ctx, e, Settings{Mode: &manual, OpenAt: &openAt}); err != nil {
		t.Fatal(err)
	}
	// Nobody is inside before opening; the two admitted users are back at the front.
	for want, id := range map[int64]string{1: "1", 2: "2", 3: "3"} {
		if v, _ := s.Status(ctx, e, id); v.Status != "waiting" || v.Position != want {
			t.Fatalf("user %s: %+v", id, v)
		}
	}
	clock.Add(time.Hour)
	if ids, err := s.Tick(ctx, e); err != nil || len(ids) != 2 || ids[0] != "1" || ids[1] != "2" {
		t.Fatalf("admitted at opening %v %v", ids, err)
	}
	if st, _ := s.Stats(ctx, e); st.OrderViolations != 0 || st.Active != 2 || st.Waiting != 1 {
		t.Fatalf("stats %+v", st)
	}
}

func TestDoorsStayClosedUntilOpening(t *testing.T) {
	c := DefaultConfig()
	c.ActiveLimit = 1
	s, e, clock := setup(t, c)
	ctx := context.Background()
	s.Join(ctx, e, "early")
	s.Join(ctx, e, "next")
	if ids, _ := s.Tick(ctx, e); len(ids) != 1 {
		t.Fatalf("auto admitted %v", ids)
	}
	// Schedule directly in Redis, skipping UpdateSettings, as if the admitted
	// user had slipped in some other way. The next tick must still clear the room.
	openAt := clock.Now().Add(time.Hour).UnixMilli()
	s.Client.HSet(ctx, s.keys(e)[7], "mode", "manual", "open_at", openAt)
	if ids, err := s.Tick(ctx, e); err != nil || len(ids) != 0 {
		t.Fatalf("tick while closed %v %v", ids, err)
	}
	if v, _ := s.Status(ctx, e, "early"); v.Status != "waiting" || v.Position != 1 {
		t.Fatalf("early user should be back at #1: %+v", v)
	}
	// The admit button does nothing before opening.
	if ids, err := s.Admit(ctx, e, 5); err != nil || len(ids) != 0 {
		t.Fatalf("admit while closed %v %v", ids, err)
	}
	if st, _ := s.Stats(ctx, e); st.Active != 0 || st.Waiting != 2 {
		t.Fatalf("room not empty before opening %+v", st)
	}
	clock.Add(time.Hour)
	if ids, _ := s.Tick(ctx, e); len(ids) != 1 || ids[0] != "early" {
		t.Fatalf("opening admitted %v", ids)
	}
}

func TestHeartbeatExpiryAndMaxStay(t *testing.T) {
	c := DefaultConfig()
	c.ActiveLimit = 2
	c.MaxStay = time.Minute
	s, e, clock := setup(t, c)
	ctx := context.Background()
	for _, id := range []string{"lost", "live", "next", "last"} {
		if _, err := s.Join(ctx, e, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Tick(ctx, e); err != nil {
		t.Fatal(err)
	}
	clock.Add(29 * time.Second)
	v, err := s.Heartbeat(ctx, e, "live")
	if err != nil || v.Status != "admitted" {
		t.Fatalf("heartbeat %+v %v", v, err)
	}
	if ids, err := s.Tick(ctx, e); err != nil || len(ids) != 0 {
		t.Fatalf("early expiry %v %v", ids, err)
	}
	clock.Add(time.Second)
	ids, err := s.Tick(ctx, e)
	if err != nil || len(ids) != 1 || ids[0] != "next" {
		t.Fatalf("30s expiry %v %v", ids, err)
	}
	v, err = s.Heartbeat(ctx, e, "lost")
	if err != nil || v.Status == "admitted" {
		t.Fatalf("resurrection %+v %v", v, err)
	}
	clock.Add(20 * time.Second)
	for _, id := range []string{"live", "next"} {
		if _, err := s.Heartbeat(ctx, e, id); err != nil {
			t.Fatal(err)
		}
	}
	clock.Add(10 * time.Second)
	// Even before a worker runs, a late heartbeat must never revive max-stay expiry.
	v, err = s.Heartbeat(ctx, e, "live")
	if err != nil || v.Status != "expired" {
		t.Fatalf("max stay %+v %v", v, err)
	}
	ids, err = s.Tick(ctx, e)
	if err != nil || len(ids) != 1 || ids[0] != "last" {
		t.Fatalf("max stay refill %v %v", ids, err)
	}
}
func TestLateHeartbeatCannotRevive(t *testing.T) {
	s, e, clock := setup(t, DefaultConfig())
	ctx := context.Background()
	s.Join(ctx, e, "lost")
	s.Tick(ctx, e)
	clock.Add(30 * time.Second)
	v, err := s.Heartbeat(ctx, e, "lost")
	if err != nil || v.Status != "expired" {
		t.Fatalf("late heartbeat %+v %v", v, err)
	}
	if n := s.Client.ZCard(ctx, s.keys(e)[1]).Val(); n != 0 {
		t.Fatalf("expired still active %d", n)
	}
}
func TestWorkerAutomaticallyRefills(t *testing.T) {
	c := DefaultConfig()
	c.ActiveLimit = 1
	s, e, _ := setup(t, c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Join(ctx, e, "one")
	s.Join(ctx, e, "two")
	s.Tick(ctx, e)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.RunWorker(ctx, []string{e}, time.Millisecond, func(err error) { t.Error(err) })
	}()
	s.Leave(ctx, e, "one")
	deadline := time.Now().Add(2 * time.Second)
	for {
		v, err := s.Status(ctx, e, "two")
		if err != nil {
			t.Fatal(err)
		}
		if v.Status == "admitted" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not promote")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
}

func TestFreshArrivalCannotJumpQueue(t *testing.T) {
	c := DefaultConfig()
	c.ActiveLimit = 1
	s, e, _ := setup(t, c)
	ctx := context.Background()
	for _, id := range []string{"active", "first", "second"} {
		if _, err := s.Join(ctx, e, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Tick(ctx, e); err != nil {
		t.Fatal(err)
	}
	if err := s.Leave(ctx, e, "active"); err != nil {
		t.Fatal(err)
	}
	v, err := s.Join(ctx, e, "newcomer")
	if err != nil || v.Position != 3 {
		t.Fatalf("queue jumping %+v %v", v, err)
	}
	ids, err := s.Tick(ctx, e)
	if err != nil || len(ids) != 1 || ids[0] != "first" {
		t.Fatalf("wrong admission %v %v", ids, err)
	}
}
func TestClockRegressionKeepsFIFO(t *testing.T) {
	s, e, c := setup(t, DefaultConfig())
	ctx := context.Background()
	a, err := s.Join(ctx, e, "first")
	if err != nil {
		t.Fatal(err)
	}
	c.Add(-time.Second)
	b, err := s.Join(ctx, e, "second")
	if err != nil || b.Ticket <= a.Ticket || b.Position != 2 {
		t.Fatalf("clock regression: %+v %+v %v", a, b, err)
	}
}
