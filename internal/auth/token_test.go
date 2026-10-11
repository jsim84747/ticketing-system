package auth

import (
	"strings"
	"testing"
	"time"
)

func TestTokens(t *testing.T) {
	now := time.Unix(1800000000, 0)
	s, err := New([]byte(strings.Repeat("k", 32)), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	token := s.Sign(Claims{Purpose: "admission", User: "u", Event: "e", Generation: "g", Expires: now.Add(time.Minute).UnixMilli()})
	if _, err := s.Verify(token, "admission"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", token + "x", strings.Replace(token, "v1.", "v2.", 1)} {
		if _, err := s.Verify(bad, "admission"); err == nil {
			t.Fatal("accepted tampered token")
		}
	}
	if _, err := s.Verify(token, "session"); err == nil {
		t.Fatal("purpose confusion")
	}
	now = now.Add(time.Minute)
	if _, err := s.Verify(token, "admission"); err == nil {
		t.Fatal("accepted expired token")
	}
}
