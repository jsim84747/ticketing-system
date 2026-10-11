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
	for _, event := range events {
		if err := q.InitEvent(ctx, event, integer("EVENT_TICKETS", 10000)); err != nil {
			log.Fatal(err)
		}
	}
	interval := duration("WORKER_INTERVAL", "1s")
	if interval <= 0 {
		log.Fatal("WORKER_INTERVAL must be positive")
	}
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		q.RunWorker(ctx, events, interval, func(err error) { log.Printf("worker: %v", err) })
	}()
	srv := &http.Server{Addr: env("HTTP_ADDR", "127.0.0.1:8080"), Handler: httpapi.Handler(q, signer, events), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second}
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
