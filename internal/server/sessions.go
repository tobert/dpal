package server

import (
	"sync"

	deepseek "github.com/cohesion-org/deepseek-go"
)

const defaultMaxSessions = 100

// Session holds the running chat history for one conversation.
// Callers must hold mu while reading or mutating messages.
type Session struct {
	id       string
	mu       sync.Mutex
	messages []deepseek.ChatCompletionMessage
	accessN  uint64 // generation counter used for LRU eviction
}

// ID returns the session identifier.
func (s *Session) ID() string { return s.id }

// Sessions is a capacity-bounded in-memory store of Sessions with LRU
// eviction. There is intentionally no time-based expiration — sessions
// live as long as the process and only get evicted when the cap forces it.
// Safe for concurrent use.
type Sessions struct {
	mu      sync.Mutex
	byID    map[string]*Session
	maxSize int
	nextN   uint64
}

// NewSessions builds a Sessions store with the given capacity.
// A maxSize of 0 disables the cap.
func NewSessions(maxSize int) *Sessions {
	return &Sessions{
		byID:    make(map[string]*Session),
		maxSize: maxSize,
	}
}

// Acquire returns the live Session for id, creating a fresh one if it
// does not exist (evicting the least-recently-used entry first if the
// cap is full). The session's access generation is bumped before
// return. Callers must Lock the returned session before mutating its
// messages.
func (s *Sessions) Acquire(id string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.nextN++

	if sess, ok := s.byID[id]; ok {
		sess.accessN = s.nextN
		return sess
	}

	if s.maxSize > 0 && len(s.byID) >= s.maxSize {
		s.evictOldestLocked()
	}

	sess := &Session{id: id, accessN: s.nextN}
	s.byID[id] = sess
	return sess
}

// Len returns the current number of stored sessions.
func (s *Sessions) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byID)
}

// evictOldestLocked removes the session with the smallest accessN.
// Caller must hold s.mu.
func (s *Sessions) evictOldestLocked() {
	var oldestID string
	var oldestN uint64
	first := true
	for id, sess := range s.byID {
		if first || sess.accessN < oldestN {
			oldestID = id
			oldestN = sess.accessN
			first = false
		}
	}
	if !first {
		delete(s.byID, oldestID)
	}
}
