// Package auth provides versioned, purpose-bound HMAC credentials for the demo.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

var ErrToken = errors.New("invalid or expired token")

type Claims struct {
	Purpose    string `json:"purpose"`
	User       string `json:"sub"`
	Event      string `json:"event,omitempty"`
	Generation string `json:"generation,omitempty"`
	Expires    int64  `json:"exp_ms"`
}
type Signer struct {
	key []byte
	now func() time.Time
}

func New(key []byte, now func() time.Time) (*Signer, error) {
	if len(key) < 32 {
		return nil, errors.New("TOKEN_SECRET must be at least 32 bytes")
	}
	if now == nil {
		now = time.Now
	}
	return &Signer{append([]byte(nil), key...), now}, nil
}
func ID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
func (s *Signer) Sign(c Claims) string {
	b, _ := json.Marshal(c)
	payload := base64.RawURLEncoding.EncodeToString(b)
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte("v1." + payload))
	return "v1." + payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func (s *Signer) Verify(token, purpose string) (Claims, error) {
	var c Claims
	if len(token) > 4096 {
		return c, ErrToken
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return c, ErrToken
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return c, ErrToken
	}
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return c, ErrToken
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return c, ErrToken
	}
	if json.Unmarshal(b, &c) != nil || c.Purpose != purpose || c.User == "" || s.now().UnixMilli() >= c.Expires {
		return Claims{}, ErrToken
	}
	return c, nil
}
func (s *Signer) Session() (string, string, error) {
	id, err := ID()
	if err != nil {
		return "", "", err
	}
	return id, s.Sign(Claims{Purpose: "session", User: id, Expires: s.now().Add(24 * time.Hour).UnixMilli()}), nil
}
