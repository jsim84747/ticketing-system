package main

import (
	"context"
	"errors"
	"github.com/jsim84747/ticketing-system/internal/auth"
	"github.com/jsim84747/ticketing-system/internal/httpapi"
	"github.com/jsim84747/ticketing-system/internal/queue"
	"github.com/redis/go-redis/v9"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func integer(k string, d int) int {
	v, err := strconv.Atoi(env(k, strconv.Itoa(d)))
	if err != nil {
		log.Fatalf("%s: %v", k, err)
	}
	return v
}
func duration(k, d string) time.Duration {
	v, err := time.ParseDuration(env(k, d))
	if err != nil {
		log.Fatalf("%s: %v", k, err)
	}
	return v
}

// listenAddr prefers HTTP_ADDR. Hosting platforms such as Render set PORT
// instead and need the server reachable from outside the container.
func listenAddr() string {
	if addr := os.Getenv("HTTP_ADDR"); addr != "" {
		return addr
	}
	if port := os.Getenv("PORT"); port != "" {
		return "0.0.0.0:" + port
	}
	return "127.0.0.1:8080"
}
func main() {
	signer, err := auth.New([]byte(os.Getenv("TOKEN_SECRET")), nil)
	if err != nil {
		log.Fatal(err)
	}
	options, err := redis.ParseURL(env("REDIS_URL", "redis://127.0.0.1:6379/0"))
	if err != nil {
		log.Fatal(err)
	}
	options.DialTimeout = 3 * time.Second
	options.ReadTimeout = 3 * time.Second
	options.WriteTimeout = 3 * time.Second
	client := redis.NewClient(options)
	defer client.Close()
	cfg := queue.Config{ActiveLimit: integer("ACTIVE_LIMIT", 1000), HeartbeatTimeout: duration("HEARTBEAT_TIMEOUT", "30s"), MaxStay: duration("MAX_STAY", "10m")}
	q, err := queue.New(client, cfg, nil)
	if err != nil {
		log.Fatal(err)
	}
	events := strings.Split(env("EVENT_IDS", "demo"), ",")
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	// EVENT_IDS only seeds the registry the first time; after that events are
	// created and deleted from the admin page.
	if err := q.SeedEvents(ctx, events, integer("EVENT_TICKETS", 10000)); err != nil {
		log.Fatal(err)
	}
	adminToken := os.Getenv("ADMIN_TOKEN")
	if adminToken == "" {
		log.Print("ADMIN_TOKEN not set: admin console disabled")
	}
	interval := duration("WORKER_INTERVAL", "1s")
	if interval <= 0 {
		log.Fatal("WORKER_INTERVAL must be positive")
	}
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		q.RunWorkerFor(ctx, q.Events, interval, func(err error) { log.Printf("worker: %v", err) })
	}()
	srv := &http.Server{Addr: listenAddr(), Handler: httpapi.Handler(q, signer, nil, adminToken), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		srv.Shutdown(shutdown)
	}()
	log.Printf("ticketing server on http://%s (active limit %d)", srv.Addr, cfg.ActiveLimit)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	cancel()
	<-workerDone
}
