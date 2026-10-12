package queue

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
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
	Name                 string `json:"name"`
	Mode                 string `json:"mode"`
	OpenAt               int64  `json:"open_at"`
	Badge                string `json:"badge"`
	JoinLock             bool   `json:"join_lock"`
	Now                  int64  `json:"now"`
	MaxPerOrder          int    `json:"max_per_order"`
	InLine               int64  `json:"in_line"`
	Inside               int64  `json:"inside"`
	Remaining            int64  `json:"remaining"`
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

// ErrNotOpen means joining is locked until the scheduled opening time.
var ErrNotOpen = errors.New("not open yet")

func (s *Store) Join(ctx context.Context, event, user string) (Status, error) {
	v, err := s.status(ctx, event, "join", user)
	if err != nil && strings.Contains(err.Error(), "NOT_OPEN") {
		return v, ErrNotOpen
	}
	return v, err
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

// LeaveAll removes every admitted user and reports how many there were.
func (s *Store) LeaveAll(ctx context.Context, event string) (int, error) {
	raw, err := s.run(ctx, event, "kick_all", "")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(raw)
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
	Generation string `json:"generation,omitempty"`
	UserID     string `json:"user_id,omitempty"`
	Customer
	Quantity   int   `json:"quantity,omitempty"`
	ReservedAt int64 `json:"reserved_at,omitempty"`
}

// Customer is who a ticket is reserved for, entered on the purchase page.
type Customer struct {
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Email     string `json:"email"`
}

var emailPattern = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// Normalize trims the fields and checks they are present and plausible.
func (c Customer) Normalize() (Customer, error) {
	c.FirstName, c.LastName, c.Email = strings.TrimSpace(c.FirstName), strings.TrimSpace(c.LastName), strings.TrimSpace(c.Email)
	switch {
	case c.FirstName == "" || len([]rune(c.FirstName)) > 100:
		return c, fmt.Errorf("first name is required (up to 100 characters)")
	case c.LastName == "" || len([]rune(c.LastName)) > 100:
		return c, fmt.Errorf("last name is required (up to 100 characters)")
	case len(c.Email) > 254 || !emailPattern.MatchString(c.Email):
		return c, fmt.Errorf("a valid email address is required")
	}
	return c, nil
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
func (s *Store) Reserve(ctx context.Context, event, user, generation, id string, who Customer, quantity int) (Reservation, error) {
	who, err := who.Normalize()
	if err != nil {
		return Reservation{}, err
	}
	raw, err := s.run(ctx, event, "reserve", user, generation, id, who.FirstName, who.LastName, who.Email, quantity)
	if err != nil {
		return Reservation{}, err
	}
	var v Reservation
	err = json.Unmarshal([]byte(raw), &v)
	return v, err
}

// ErrReservationNotFound means the user has no reservation for the event.
var ErrReservationNotFound = errors.New("reservation not found")

// NormalizeAdmin is the looser check for admin edits: names are still
// required, but the email is free text so admins can record whatever
// contact detail they have.
func (c Customer) NormalizeAdmin() (Customer, error) {
	c.FirstName, c.LastName, c.Email = strings.TrimSpace(c.FirstName), strings.TrimSpace(c.LastName), strings.TrimSpace(c.Email)
	switch {
	case c.FirstName == "" || len([]rune(c.FirstName)) > 100:
		return c, fmt.Errorf("first name is required (up to 100 characters)")
	case c.LastName == "" || len([]rune(c.LastName)) > 100:
		return c, fmt.Errorf("last name is required (up to 100 characters)")
	case len([]rune(c.Email)) > 254:
		return c, fmt.Errorf("email must be at most 254 characters")
	}
	return c, nil
}

// UpdateReservation changes who a reservation is for. The ticket itself,
// its number and time are unchanged.
func (s *Store) UpdateReservation(ctx context.Context, event, user string, who Customer) (Reservation, error) {
	who, err := who.NormalizeAdmin()
	if err != nil {
		return Reservation{}, err
	}
	raw, err := s.run(ctx, event, "edit_reservation", user, who.FirstName, who.LastName, who.Email)
	if err != nil {
		if strings.Contains(err.Error(), "NOT_FOUND") {
			return Reservation{}, ErrReservationNotFound
		}
		return Reservation{}, err
	}
	var v Reservation
	err = json.Unmarshal([]byte(raw), &v)
	v.UserID, v.Generation = user, ""
	return v, err
}

// DeleteReservation cancels a reservation and puts its ticket back on sale.
func (s *Store) DeleteReservation(ctx context.Context, event, user string) error {
	_, err := s.run(ctx, event, "delete_reservation", user)
	if err != nil && strings.Contains(err.Error(), "NOT_FOUND") {
		return ErrReservationNotFound
	}
	return err
}

// Reservations returns every reservation for an event, newest first.
func (s *Store) Reservations(ctx context.Context, event string) ([]Reservation, error) {
	if !eventPattern.MatchString(event) {
		return nil, fmt.Errorf("invalid event")
	}
	all, err := s.Client.HGetAll(ctx, s.keys(event)[8]).Result()
	if err != nil {
		return nil, err
	}
	out := make([]Reservation, 0, len(all))
	for user, raw := range all {
		var v Reservation
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			return nil, err
		}
		v.UserID, v.Generation = user, ""
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ReservedAt > out[j].ReservedAt })
	return out, nil
}

// Admit lets up to n users in from the front of the queue, never exceeding the
// active limit. It is how admins admit users in manual mode.
func (s *Store) Admit(ctx context.Context, event string, n int) ([]string, error) {
	if n < 1 || n > 1000 {
		return nil, fmt.Errorf("invalid admit count")
	}
	raw, err := s.run(ctx, event, "admit", "", n)
	if err != nil {
		return nil, err
	}
	var ids []string
	err = json.Unmarshal([]byte(raw), &ids)
	return ids, err
}

// Settings are the per-event values an admin can change at runtime. Nil
// fields are left unchanged.
type Settings struct {
	ActiveLimit *int    `json:"active_limit"`
	Name        *string `json:"name"`
	// Badge is the sale status shown to visitors, such as "Open" or "Closed".
	Badge     *string `json:"badge"`
	Remaining *int    `json:"remaining"`
	Mode      *string `json:"mode"`
	// OpenAt schedules manual mode to switch to auto admission at this
	// unix time in milliseconds; 0 cancels the schedule.
	OpenAt *int64 `json:"open_at"`
	// JoinLock keeps the join button locked until OpenAt instead of letting
	// people line up early.
	JoinLock *bool `json:"join_lock"`
	// MaxPerOrder caps how many tickets one reservation may take.
	MaxPerOrder *int `json:"max_per_order"`
}

func (v Settings) Validate() error {
	switch {
	case v.ActiveLimit != nil && (*v.ActiveLimit < 1 || *v.ActiveLimit > 100000):
		return fmt.Errorf("max concurrent users must be between 1 and 100000")
	case v.Name != nil && len([]rune(*v.Name)) > 80:
		return fmt.Errorf("ticket name must be at most 80 characters")
	case v.Badge != nil && len([]rune(strings.TrimSpace(*v.Badge))) > 40:
		return fmt.Errorf("sale status must be at most 40 characters")
	case v.Remaining != nil && (*v.Remaining < 0 || *v.Remaining > 10000000):
		return fmt.Errorf("remaining tickets must be between 0 and 10000000")
	case v.Mode != nil && *v.Mode != "auto" && *v.Mode != "manual":
		return fmt.Errorf(`mode must be "auto" or "manual"`)
	case v.MaxPerOrder != nil && (*v.MaxPerOrder < 1 || *v.MaxPerOrder > 20):
		return fmt.Errorf("max tickets per order must be between 1 and 20")
	case v.OpenAt != nil && *v.OpenAt < 0:
		return fmt.Errorf("opening time must not be negative")
	}
	return nil
}

// UpdateSettings applies the given settings in one transaction. Lowering the
// limit never evicts anyone; admission simply pauses until occupancy drops.
// Scheduling an opening sends admitted users back to the front of the line,
// so nobody is inside before the doors open.
func (s *Store) UpdateSettings(ctx context.Context, event string, v Settings) error {
	if !eventPattern.MatchString(event) {
		return fmt.Errorf("invalid event")
	}
	if err := v.Validate(); err != nil {
		return err
	}
	// Seed defaults first so a partial update never leaves an event half-configured.
	if _, err := s.run(ctx, event, "status", ""); err != nil {
		return err
	}
	k := s.keys(event)
	_, err := s.Client.TxPipelined(ctx, func(p redis.Pipeliner) error {
		if v.ActiveLimit != nil {
			p.HSet(ctx, k[7], "limit", *v.ActiveLimit)
		}
		if v.Name != nil {
			p.HSet(ctx, k[7], "name", *v.Name)
		}
		if v.Badge != nil {
			p.HSet(ctx, k[7], "badge", strings.TrimSpace(*v.Badge))
		}
		if v.Mode != nil {
			p.HSet(ctx, k[7], "mode", *v.Mode)
		}
		if v.Remaining != nil {
			p.Set(ctx, k[9], *v.Remaining, 0)
		}
		if v.MaxPerOrder != nil {
			p.HSet(ctx, k[7], "max_per_order", *v.MaxPerOrder)
		}
		if v.JoinLock != nil {
			lock := "0"
			if *v.JoinLock {
				lock = "1"
			}
			p.HSet(ctx, k[7], "join_lock", lock)
		}
		if v.OpenAt != nil {
			p.HSet(ctx, k[7], "open_at", *v.OpenAt)
		} else if v.Mode != nil && *v.Mode == "auto" {
			// Switching to auto by hand drops any pending schedule, so it
			// cannot fire unexpectedly after a later switch back to manual.
			p.HSet(ctx, k[7], "open_at", 0)
		}
		return nil
	})
	if err == nil && v.OpenAt != nil && *v.OpenAt > 0 {
		_, err = s.run(ctx, event, "requeue", "")
	}
	return err
}

// Stats is read atomically; peak and FIFO violations are recorded at admission,
// not sampled, so even transient breaches are observable by the load harness.
type Stats struct {
	Active          int64  `json:"active"`
	Waiting         int64  `json:"waiting"`
	Admissions      int64  `json:"admissions"`
	PeakActive      int64  `json:"peak_active"`
	OrderViolations int64  `json:"order_violations"`
	WaitTotalMS     int64  `json:"wait_total_ms"`
	WaitMaxMS       int64  `json:"wait_max_ms"`
	Remaining       int64  `json:"remaining"`
	Reserved        int64  `json:"reserved"`
	ActiveLimit     int64  `json:"active_limit"`
	Mode            string `json:"mode"`
	Name            string `json:"name"`
	OpenAt          int64  `json:"open_at"`
	Badge           string `json:"badge"`
	JoinLock        bool   `json:"join_lock"`
	MaxPerOrder     int    `json:"max_per_order"`
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
