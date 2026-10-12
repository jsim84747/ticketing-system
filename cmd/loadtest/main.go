// loadtest exercises real HTTP handlers, independent Redis clients and workers.
// It owns only a unique load-* event; it never flushes the Redis database.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/jsim84747/ticketing-system/internal/auth"
	"github.com/jsim84747/ticketing-system/internal/httpapi"
	"github.com/jsim84747/ticketing-system/internal/queue"
	"github.com/redis/go-redis/v9"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	users := flag.Int("users", 10000, "concurrent join requests")
	limit := flag.Int("limit", 1000, "active capacity")
	instances := flag.Int("instances", 4, "independent HTTP servers / Redis clients / workers")
	tick := flag.Duration("tick", time.Second, "worker interval")
	seed := flag.Int64("seed", 42, "random exit seed")
	duration := flag.Duration("timeout", 2*time.Minute, "maximum run duration")
	redisURL := flag.String("redis-url", "redis://127.0.0.1:16379/0", "dedicated test Redis URL")
	flag.Parse()
	if *users < 1 || *instances < 1 || *instances > 32 || *tick <= 0 || *duration <= 0 {
		return fmt.Errorf("invalid flags")
	}
	options, err := redis.ParseURL(*redisURL)
	if err != nil {
		return err
	}
	options.PoolSize = 64
	options.PoolTimeout = 30 * time.Second
	options.ReadTimeout = 10 * time.Second
	cfg := queue.DefaultConfig()
	cfg.ActiveLimit = *limit
	if err := cfg.Validate(); err != nil {
		return err
	}
	id, err := auth.ID()
	if err != nil {
		return err
	}
	event := "load-" + id
	key, err := auth.ID()
	if err != nil {
		return err
	}
	signer, err := auth.New([]byte(key), nil)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *duration)
	defer cancel()
	clients := make([]*redis.Client, *instances)
	stores := make([]*queue.Store, *instances)
	servers := make([]*httptest.Server, *instances)
	var workerWG sync.WaitGroup
	var failures atomic.Int64
	var firstError sync.Once
	failure := func(err error) {
		if err == nil {
			return
		}
		failures.Add(1)
		firstError.Do(func() { fmt.Fprintln(os.Stderr, "first error:", err) })
	}
	for i := range stores {
		clients[i] = redis.NewClient(options)
		defer clients[i].Close()
		stores[i], err = queue.New(clients[i], cfg, nil)
		if err != nil {
			return err
		}
		if err = stores[i].InitEvent(ctx, event, *users); err != nil {
			return err
		}
		servers[i] = httptest.NewServer(httpapi.Handler(stores[i], signer, []string{event}))
		defer servers[i].Close()
	}
	defer func() {
		// Only this invocation's random event is removed, even on failure.
		cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		var cursor uint64
		for {
			keys, next, err := clients[0].Scan(cleanup, cursor, "vwr:{"+event+"}:*", 100).Result()
			if err != nil {
				break
			}
			if len(keys) > 0 {
				clients[0].Del(cleanup, keys...)
			}
			cursor = next
			if cursor == 0 {
				break
			}
		}
	}()
	workerCtx, stopWorkers := context.WithCancel(ctx)
	defer func() { stopWorkers(); workerWG.Wait() }()
	for _, q := range stores {
		workerWG.Add(1)
		go func(q *queue.Store) { defer workerWG.Done(); q.RunWorker(workerCtx, []string{event}, *tick, failure) }(q)
	}
	transport := &http.Transport{MaxIdleConns: 256, MaxIdleConnsPerHost: 64, MaxConnsPerHost: 64}
	defer transport.CloseIdleConnections()
	httpClient := &http.Client{Transport: transport, Timeout: 60 * time.Second}
	// Session preparation is outside the measured interval; all join/auth traffic is HTTP.
	sessions := make([]string, *users)
	byUser := make(map[string]string, *users)
	for i := range sessions {
		u, token, err := signer.Session()
		if err != nil {
			return err
		}
		sessions[i] = token
		byUser[u] = token
	}
	request := func(instance int, session, path string) error {
		req, err := http.NewRequestWithContext(ctx, "POST", servers[instance].URL+"/events/"+event+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+session)
		res, err := httpClient.Do(req)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		body, err := io.ReadAll(io.LimitReader(res.Body, 8192))
		if err != nil {
			return err
		}
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			return fmt.Errorf("%s: HTTP %d %s", path, res.StatusCode, body)
		}
		return nil
	}
	start := time.Now()
	var joins sync.WaitGroup
	gate := make(chan struct{})
	for i := range sessions {
		joins.Add(1)
		go func(i int) { defer joins.Done(); <-gate; failure(request(i%*instances, sessions[i], "/queue")) }(i)
	}
	close(gate)
	joins.Wait()
	joinSeconds := time.Since(start).Seconds()
	if failures.Load() != 0 {
		return fmt.Errorf("join failures: %d", failures.Load())
	}
	random := rand.New(rand.NewSource(*seed))
	exitTicker := time.NewTicker(200 * time.Millisecond)
	defer exitTicker.Stop()
	heartbeatTicker := time.NewTicker(10 * time.Second)
	defer heartbeatTicker.Stop()
	for {
		stats, err := stores[0].Stats(ctx, event)
		if err != nil {
			return err
		}
		if stats.Active == 0 && stats.Waiting == 0 {
			break
		}
		heartbeat := false
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-exitTicker.C:
		case <-heartbeatTicker.C:
			heartbeat = true
		}
		active, err := stores[0].ActiveUsers(ctx, event)
		if err != nil {
			return err
		}
		if len(active) == 0 {
			continue
		}
		path := "/queue/heartbeat"
		if !heartbeat {
			path = "/queue/leave"
			random.Shuffle(len(active), func(i, j int) { active[i], active[j] = active[j], active[i] })
			batch := 1 + random.Intn(min(len(active), 500))
			active = active[:batch]
		}
		var wg sync.WaitGroup
		for i, u := range active {
			wg.Add(1)
			go func(i int, u string) { defer wg.Done(); failure(request(i%*instances, byUser[u], path)) }(i, u)
		}
		wg.Wait()
	}
	stopWorkers()
	workerWG.Wait()
	stats, err := stores[0].Stats(ctx, event)
	if err != nil {
		return err
	}
	elapsed := time.Since(start).Seconds()
	report := map[string]any{
		"timestamp": time.Now().UTC().Format(time.RFC3339), "go": runtime.Version(), "platform": runtime.GOOS + "/" + runtime.GOARCH, "gomaxprocs": runtime.GOMAXPROCS(0),
		"max_http_connections_per_instance": 64, "users": *users, "active_limit": *limit, "instances": *instances, "worker_interval": tick.String(), "seed": *seed, "scope": "HTTP auth/join/leave/heartbeat + real Redis; session issuance excluded",
		"join_seconds": joinSeconds, "total_seconds": elapsed, "admissions": stats.Admissions, "max_active": stats.PeakActive, "order_violations": stats.OrderViolations,
		"mean_wait_ms": float64(stats.WaitTotalMS) / float64(max(stats.Admissions, 1)), "max_wait_ms": stats.WaitMaxMS, "admissions_per_second": float64(stats.Admissions) / elapsed,
		"http_or_worker_errors": failures.Load(), "remaining_active": stats.Active, "remaining_waiting": stats.Waiting,
	}
	if version, err := clients[0].Info(ctx, "server").Result(); err == nil {
		for _, line := range strings.Split(version, "\r\n") {
			if v, ok := strings.CutPrefix(line, "redis_version:"); ok {
				report["redis_version"] = v
				break
			}
		}
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return err
	}
	if stats.PeakActive > int64(*limit) || stats.OrderViolations != 0 || stats.Admissions != int64(*users) || failures.Load() != 0 {
		return fmt.Errorf("invariant failed: admissions=%s peak=%d FIFO=%d errors=%d", strconv.FormatInt(stats.Admissions, 10), stats.PeakActive, stats.OrderViolations, failures.Load())
	}
	return nil
}
