package server

import (
	"sync"

	deepseek "github.com/cohesion-org/deepseek-go"
)

const defaultMaxSessions = 100

// ReasoningEntry is one R1 reasoning_content payload captured at a
// specific turn. Stored parallel to Session.messages — never replayed
// to the model, but kept for display via dpal://session/{id}.
type ReasoningEntry struct {
	TurnIndex int    `json:"turn_index"` // 1-based user-turn index this reasoning answered
	Content   string `json:"content"`
}

// Session holds the running chat history for one conversation.
// Callers must hold mu while reading or mutating messages or reasoning.
//
// accessN is the LRU generation counter and is owned by the parent
// Sessions: it is only read or written under Sessions.mu (never under
// sess.mu), so accessing it from anywhere else is a data race.
type Session struct {
	id        string
	mu        sync.Mutex
	messages  []deepseek.ChatCompletionMessage
	reasoning []ReasoningEntry
	accessN   uint64
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

// SessionInfo is a snapshot of a Session's external-facing metadata.
type SessionInfo struct {
	ID           string `json:"id"`
	MessageCount int    `json:"message_count"`
	UserTurns    int    `json:"user_turns"`
	AccessGen    uint64 `json:"access_gen"`
}

// TranscriptMessage is one entry in a session transcript, deep-copied
// from the live session so callers can read it without locking.
type TranscriptMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Snapshot returns lightweight metadata for every stored session,
// sorted by AccessGen ascending (oldest-first). Briefly locks each
// session to read message count, so concurrent writes serialise.
//
// Pointers and access generations are copied under the map lock and
// iterated after release, so a concurrent Acquire that triggers
// eviction may produce a result containing entries that are no longer
// in the live map. The per-session lock keeps the message read itself
// safe; callers just shouldn't treat the snapshot as authoritative for
// "is X still tracked".
func (s *Sessions) Snapshot() []SessionInfo {
	type entry struct {
		sess    *Session
		accessN uint64 // captured under s.mu — owner of accessN
	}
	s.mu.Lock()
	all := make([]entry, 0, len(s.byID))
	for _, sess := range s.byID {
		all = append(all, entry{sess: sess, accessN: sess.accessN})
	}
	s.mu.Unlock()

	out := make([]SessionInfo, 0, len(all))
	for _, e := range all {
		e.sess.mu.Lock()
		userTurns := 0
		for _, m := range e.sess.messages {
			if m.Role == "user" {
				userTurns++
			}
		}
		out = append(out, SessionInfo{
			ID:           e.sess.id,
			MessageCount: len(e.sess.messages),
			UserTurns:    userTurns,
			AccessGen:    e.accessN,
		})
		e.sess.mu.Unlock()
	}

	// Sort by AccessGen so output is deterministic.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].AccessGen > out[j].AccessGen; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// Transcript returns a deep copy of the message log for id. The second
// return value is false if no such session exists.
func (s *Sessions) Transcript(id string) ([]TranscriptMessage, bool) {
	s.mu.Lock()
	sess, ok := s.byID[id]
	s.mu.Unlock()
	if !ok {
		return nil, false
	}

	sess.mu.Lock()
	defer sess.mu.Unlock()
	out := make([]TranscriptMessage, len(sess.messages))
	for i, m := range sess.messages {
		out[i] = TranscriptMessage{Role: m.Role, Content: m.Content}
	}
	return out, true
}

// Reasoning returns a deep copy of the per-turn reasoning log for id.
// Empty slice (not nil) when the session exists but has no R1 turns
// recorded; (nil, false) when no such session exists.
func (s *Sessions) Reasoning(id string) ([]ReasoningEntry, bool) {
	s.mu.Lock()
	sess, ok := s.byID[id]
	s.mu.Unlock()
	if !ok {
		return nil, false
	}

	sess.mu.Lock()
	defer sess.mu.Unlock()
	out := make([]ReasoningEntry, len(sess.reasoning))
	copy(out, sess.reasoning)
	return out, true
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
