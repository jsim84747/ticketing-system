package queue

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/redis/go-redis/v9"
)

// Clock is optional: production uses Redis TIME inside the atomic script.
// Tests inject it to advance leases without sleeping.
type Clock interface{ Now() time.Time }
type Config struct {
	ActiveLimit      int
	HeartbeatTimeout time.Duration
	MaxStay          time.Duration
}

func DefaultConfig() Config { return Config{1000, 30 * time.Second, 10 * time.Minute} }
func (c Config) Validate() error {
	if c.ActiveLimit < 1 || c.ActiveLimit > 100000 || c.HeartbeatTimeout < time.Millisecond || c.MaxStay < c.HeartbeatTimeout {
		return fmt.Errorf("invalid queue config")
	}
	return nil
}

type Status struct {
	ReservationID        string `json:"reservation_id,omitempty"`
	Status               string `json:"status"`
	Position             int64  `json:"position"`
	Ahead                int64  `json:"ahead"`
	Ticket               int64  `json:"ticket,omitempty,string"`
	EstimatedWaitSeconds *int64 `json:"estimated_wait_seconds"`
	Generation           string `json:"generation,omitempty"`
	ExpiresAt            int64  `json:"expires_at,omitempty"`
	Token                string `json:"token,omitempty"`
}
type Store struct {
	Client *redis.Client
	Config Config
	Clock  Clock
}

//go:embed queue.lua
var scriptSource string
var script = redis.NewScript(scriptSource)
var eventPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func New(client *redis.Client, c Config, clock Clock) (*Store, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &Store{client, c, clock}, nil
}
func (s *Store) keys(event string) []string {
	p := "vwr:{" + event + "}:"
	names := []string{"waiting", "active", "started", "generation", "sequence", "joined", "metrics", "config", "reservations", "stock"}
	for i := range names {
		names[i] = p + names[i]
	}
	return names
}
func (s *Store) run(ctx context.Context, event, action, user string, extra ...any) (string, error) {
	if !eventPattern.MatchString(event) {
		return "", fmt.Errorf("invalid event")
	}
	if len(user) > 128 {
		return "", fmt.Errorf("invalid user")
	}
	now := int64(0)
	if s.Clock != nil {
		now = s.Clock.Now().UnixMilli()
	}
	args := []any{action, user, now, s.Config.ActiveLimit, s.Config.HeartbeatTimeout.Milliseconds(), s.Config.MaxStay.Milliseconds()}
	args = append(args, extra...)
	return script.Run(ctx, s.Client, s.keys(event), args...).Text()
}
func (s *Store) status(ctx context.Context, event, action, user string) (Status, error) {
	raw, err := s.run(ctx, event, action, user)
	if err != nil {
		return Status{}, err
	}
	var v Status
	err = json.Unmarshal([]byte(raw), &v)
	return v, err
}
func (s *Store) Join(ctx context.Context, event, user string) (Status, error) {
	return s.status(ctx, event, "join", user)
}
func (s *Store) Status(ctx context.Context, event, user string) (Status, error) {
	return s.status(ctx, event, "status", user)
}

func (s *Store) Tick(ctx context.Context, event string) ([]string, error) {
	raw, err := s.run(ctx, event, "tick", "")
	if err != nil {
		return nil, err
	}
	var ids []string
	err = json.Unmarshal([]byte(raw), &ids)
	return ids, err
}
func (s *Store) Leave(ctx context.Context, event, user string) error {
	_, err := s.run(ctx, event, "leave", user)
	return err
}

// RunWorker deliberately needs no distributed lock. Each Tick is atomic in Redis.
func (s *Store) RunWorker(ctx context.Context, events []string, interval time.Duration, onError func(error)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		for _, event := range events {
			if _, err := s.Tick(ctx, event); err != nil && ctx.Err() == nil && onError != nil {
				onError(err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Store) Heartbeat(ctx context.Context, event, user string) (Status, error) {
	return s.status(ctx, event, "heartbeat", user)
}

type Reservation struct {
	ID         string `json:"reservation_id"`
	Generation string `json:"generation"`
}

func (s *Store) InitEvent(ctx context.Context, event string, tickets int) error {
	if tickets < 0 {
		return fmt.Errorf("invalid inventory")
	}
	if _, err := s.run(ctx, event, "status", ""); err != nil {
		return err
	}
	return s.Client.SetNX(ctx, s.keys(event)[9], tickets, 0).Err()
}
func (s *Store) Reserve(ctx context.Context, event, user, generation, id string) (Reservation, error) {
	raw, err := s.run(ctx, event, "reserve", user, generation, id)
	if err != nil {
		return Reservation{}, err
	}
	var v Reservation
	err = json.Unmarshal([]byte(raw), &v)
	return v, err
}

// Stats is read atomically; peak and FIFO violations are recorded at admission,
// not sampled, so even transient breaches are observable by the load harness.
type Stats struct {
	Active          int64 `json:"active"`
	Waiting         int64 `json:"waiting"`
	Admissions      int64 `json:"admissions"`
	PeakActive      int64 `json:"peak_active"`
	OrderViolations int64 `json:"order_violations"`
	WaitTotalMS     int64 `json:"wait_total_ms"`
	WaitMaxMS       int64 `json:"wait_max_ms"`
	Remaining       int64 `json:"remaining"`
	Reserved        int64 `json:"reserved"`
}

func (s *Store) Stats(ctx context.Context, event string) (Stats, error) {
	raw, err := s.run(ctx, event, "stats", "")
	if err != nil {
		return Stats{}, err
	}
	var v Stats
	err = json.Unmarshal([]byte(raw), &v)
	return v, err
}
func (s *Store) ActiveUsers(ctx context.Context, event string) ([]string, error) {
	if !eventPattern.MatchString(event) {
		return nil, fmt.Errorf("invalid event")
	}
	return s.Client.ZRange(ctx, s.keys(event)[1], 0, -1).Result()
}
