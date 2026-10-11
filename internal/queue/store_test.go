package queue

import (
	"context"
	"fmt"
	"github.com/redis/go-redis/v9"
	"os"
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
		workers.Add(1)
		go func() {
			defer workers.Done()
			s.RunWorker(workerCtx, []string{e}, time.Millisecond, func(err error) { t.Error(err) })
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
func TestConfigMismatchFailsClosed(t *testing.T) {
	s, e, _ := setup(t, DefaultConfig())
	ctx := context.Background()
	if _, err := s.Join(ctx, e, "one"); err != nil {
		t.Fatal(err)
	}
	other := *s
	other.Config.ActiveLimit = 2000
	if _, err := other.Tick(ctx, e); err == nil {
		t.Fatal("different limits accepted")
	}
}
