package server

import (
	"testing"
	"time"
)

// fakeClock returns a now-function bound to a *time.Time you can advance.
func fakeClock(t *time.Time) func() time.Time {
	return func() time.Time { return *t }
}

func TestSessions_AcquireCreatesNewSession(t *testing.T) {
	s := NewSessions(time.Hour, 10)
	sess := s.Acquire("a")
	if sess == nil {
		t.Fatal("Acquire returned nil")
	}
	if sess.ID() != "a" {
		t.Errorf("ID = %q, want %q", sess.ID(), "a")
	}
	if s.Len() != 1 {
		t.Errorf("Len = %d, want 1", s.Len())
	}
}

func TestSessions_AcquireReturnsSameSessionByID(t *testing.T) {
	s := NewSessions(time.Hour, 10)
	first := s.Acquire("a")
	second := s.Acquire("a")
	if first != second {
		t.Error("second Acquire returned a different session pointer")
	}
}

func TestSessions_DifferentIDsAreIsolated(t *testing.T) {
	s := NewSessions(time.Hour, 10)
	a := s.Acquire("a")
	b := s.Acquire("b")
	if a == b {
		t.Error("different IDs returned the same session")
	}
	if s.Len() != 2 {
		t.Errorf("Len = %d, want 2", s.Len())
	}
}

func TestSessions_ExpiredSessionReplaced(t *testing.T) {
	now := time.Unix(0, 0)
	s := NewSessions(time.Minute, 10)
	s.now = fakeClock(&now)

	first := s.Acquire("a")

	now = now.Add(2 * time.Minute) // past TTL
	second := s.Acquire("a")

	if first == second {
		t.Error("expected expired session to be replaced with a fresh one")
	}
}

func TestSessions_AccessRefreshesTTL(t *testing.T) {
	now := time.Unix(0, 0)
	s := NewSessions(time.Minute, 10)
	s.now = fakeClock(&now)

	first := s.Acquire("a")

	now = now.Add(30 * time.Second) // within TTL
	again := s.Acquire("a")
	if again != first {
		t.Fatal("session replaced before TTL elapsed")
	}

	now = now.Add(45 * time.Second) // 75s since first, 45s since refresh — still fresh
	stillSame := s.Acquire("a")
	if stillSame != first {
		t.Error("access did not refresh lastAccess")
	}
}

func TestSessions_MaxSizeEvictsOldest(t *testing.T) {
	now := time.Unix(0, 0)
	s := NewSessions(time.Hour, 2)
	s.now = fakeClock(&now)

	s.Acquire("a")
	now = now.Add(time.Second)
	s.Acquire("b")
	now = now.Add(time.Second)
	// Inserting a third should evict "a" (oldest lastAccess).
	s.Acquire("c")

	if s.Len() != 2 {
		t.Fatalf("Len = %d, want 2", s.Len())
	}
	if _, ok := s.byID["a"]; ok {
		t.Error("expected 'a' to be evicted")
	}
	if _, ok := s.byID["b"]; !ok {
		t.Error("expected 'b' to be retained")
	}
	if _, ok := s.byID["c"]; !ok {
		t.Error("expected 'c' to be present")
	}
}

func TestSessions_ZeroTTLDisablesExpiration(t *testing.T) {
	now := time.Unix(0, 0)
	s := NewSessions(0, 10)
	s.now = fakeClock(&now)

	first := s.Acquire("a")
	now = now.Add(1000 * time.Hour)
	again := s.Acquire("a")
	if first != again {
		t.Error("zero TTL should disable expiration; session was replaced")
	}
}
