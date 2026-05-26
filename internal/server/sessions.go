package server

import (
	"sync"
	"time"

	deepseek "github.com/cohesion-org/deepseek-go"
)

const (
	defaultSessionTTL  = 1 * time.Hour
	defaultMaxSessions = 100
)

// Session holds the running chat history for one conversation.
// Callers must hold mu while reading or mutating messages.
type Session struct {
	id         string
	mu         sync.Mutex
	messages   []deepseek.ChatCompletionMessage
	lastAccess time.Time
}

// ID returns the session identifier.
func (s *Session) ID() string { return s.id }

// Sessions is a TTL-and-capacity-bounded in-memory store of Sessions.
// It is safe for concurrent use.
type Sessions struct {
	mu      sync.Mutex
	byID    map[string]*Session
	ttl     time.Duration
	maxSize int
	now     func() time.Time
}

// NewSessions builds a Sessions store with the given TTL and capacity.
// A ttl of 0 disables expiration; maxSize of 0 disables the cap.
func NewSessions(ttl time.Duration, maxSize int) *Sessions {
	return &Sessions{
		byID:    make(map[string]*Session),
		ttl:     ttl,
		maxSize: maxSize,
		now:     time.Now,
	}
}

// Acquire returns the live Session for id, creating a fresh one if no
// entry exists, has expired, or was evicted. The session's lastAccess is
// updated before return. The caller must Lock the returned session before
// mutating its messages.
func (s *Sessions) Acquire(id string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()

	if sess, ok := s.byID[id]; ok {
		if !s.isExpired(sess, now) {
			sess.lastAccess = now
			return sess
		}
		delete(s.byID, id)
	}

	if s.maxSize > 0 && len(s.byID) >= s.maxSize {
		s.evictOldestLocked()
	}

	sess := &Session{id: id, lastAccess: now}
	s.byID[id] = sess
	return sess
}

// Len returns the current number of stored sessions.
func (s *Sessions) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byID)
}

func (s *Sessions) isExpired(sess *Session, now time.Time) bool {
	if s.ttl <= 0 {
		return false
	}
	return now.Sub(sess.lastAccess) > s.ttl
}

// evictOldestLocked removes the session with the oldest lastAccess.
// Caller must hold s.mu.
func (s *Sessions) evictOldestLocked() {
	var oldestID string
	var oldestAt time.Time
	first := true
	for id, sess := range s.byID {
		if first || sess.lastAccess.Before(oldestAt) {
			oldestID = id
			oldestAt = sess.lastAccess
			first = false
		}
	}
	if !first {
		delete(s.byID, oldestID)
	}
}
