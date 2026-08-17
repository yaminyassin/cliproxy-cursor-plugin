package auth

import (
	"sync"
	"time"
)

// pendingLogin holds the PKCE verifier and deadline for one in-flight
// login attempt, keyed by its attempt id (surfaced to the host as
// AuthLoginStartResponse.State). Keeping this per-attempt-id, not global,
// is what lets concurrent logins for different accounts never collide
// (fact-r4-login-edges / Round 4 concurrency model).
type pendingLogin struct {
	Verifier   string
	CursorUUID string
	Deadline   time.Time
}

// pendingLoginStore is a small in-process registry of in-flight login
// attempts. It is intentionally simple (no persistence): login attempts
// are short-lived (bounded by maxPollDuration) and do not need to survive
// a plugin process restart.
type pendingLoginStore struct {
	mu   sync.Mutex
	byID map[string]pendingLogin
}

func newPendingLoginStore() *pendingLoginStore {
	return &pendingLoginStore{byID: make(map[string]pendingLogin)}
}

func (s *pendingLoginStore) put(attemptID string, p pendingLogin) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID[attemptID] = p
}

func (s *pendingLoginStore) get(attemptID string) (pendingLogin, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.byID[attemptID]
	return p, ok
}

func (s *pendingLoginStore) delete(attemptID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byID, attemptID)
}
