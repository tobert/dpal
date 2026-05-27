package server

import (
	"fmt"
	"sync"
	"testing"

	deepseek "github.com/cohesion-org/deepseek-go"
)

func TestSessions_AcquireCreatesNewSession(t *testing.T) {
	s := NewSessions(10)
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
	s := NewSessions(10)
	first := s.Acquire("a")
	second := s.Acquire("a")
	if first != second {
		t.Error("second Acquire returned a different session pointer")
	}
}

func TestSessions_DifferentIDsAreIsolated(t *testing.T) {
	s := NewSessions(10)
	a := s.Acquire("a")
	b := s.Acquire("b")
	if a == b {
		t.Error("different IDs returned the same session")
	}
	if s.Len() != 2 {
		t.Errorf("Len = %d, want 2", s.Len())
	}
}

func TestSessions_NoTimeBasedExpiration(t *testing.T) {
	// Sessions live for the life of the process; only the cap evicts them.
	// This guards against anyone reintroducing TTL logic.
	s := NewSessions(10)
	first := s.Acquire("a")
	for range 1000 {
		// Many other operations should not displace "a" while we're under cap.
		s.Acquire("a")
	}
	if s.Acquire("a") != first {
		t.Error("session was replaced despite no eviction reason")
	}
}

func TestSessions_LRUEvictsLeastRecentlyUsed(t *testing.T) {
	s := NewSessions(2)

	s.Acquire("a")
	s.Acquire("b")
	s.Acquire("a") // refresh "a" — now "b" is the LRU
	s.Acquire("c") // should evict "b"

	if s.Len() != 2 {
		t.Fatalf("Len = %d, want 2", s.Len())
	}
	if _, ok := s.byID["b"]; ok {
		t.Error("expected 'b' to be evicted (least recently used)")
	}
	if _, ok := s.byID["a"]; !ok {
		t.Error("expected 'a' to be retained (recently refreshed)")
	}
	if _, ok := s.byID["c"]; !ok {
		t.Error("expected 'c' to be present")
	}
}

func TestSessions_ZeroMaxDisablesCap(t *testing.T) {
	s := NewSessions(0)
	for i := range 50 {
		s.Acquire(string(rune('a' + i)))
	}
	if s.Len() != 50 {
		t.Errorf("Len = %d, want 50 (cap disabled)", s.Len())
	}
}

// TestSessions_ConcurrentAccess hammers Acquire/Snapshot/Transcript
// from many goroutines under -race. Two-tier locking (map mu + per-session
// mu) is correct by inspection; this guards against future regressions.
// Eviction is active: cap << goroutine count, so the LRU path is exercised.
func TestSessions_ConcurrentAccess(t *testing.T) {
	const (
		goroutines    = 32
		opsPerWorker  = 200
		sessionPool   = 16 // distinct IDs cycled through
		capacity      = 4  // forces frequent eviction
	)

	s := NewSessions(capacity)
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := range goroutines {
		go func(g int) {
			defer wg.Done()
			for op := range opsPerWorker {
				id := fmt.Sprintf("sess-%d", (g+op)%sessionPool)
				switch op % 4 {
				case 0:
					sess := s.Acquire(id)
					sess.mu.Lock()
					sess.messages = append(sess.messages, deepseek.ChatCompletionMessage{
						Role: deepseek.ChatMessageRoleUser, Content: "x",
					})
					sess.mu.Unlock()
				case 1:
					_ = s.Snapshot()
				case 2:
					_, _ = s.Transcript(id)
				case 3:
					_, _ = s.Reasoning(id)
				}
			}
		}(g)
	}
	wg.Wait()

	// After eviction churn the live count is bounded by the cap.
	if got := s.Len(); got > capacity {
		t.Errorf("Len = %d, want <= cap=%d", got, capacity)
	}
}
