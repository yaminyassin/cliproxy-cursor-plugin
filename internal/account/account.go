// Package account is the single source of truth for Cursor account status
// and token state, per the ralplan-approved plan's account-state ownership
// design: internal/auth is the sole writer; internal/executor and
// internal/discovery read through this package's interface before making
// any Cursor backend call, failing fast on a degraded account rather than
// attempting a request with a known-stale token.
package account

import (
	"errors"
	"sync"
	"time"
)

// Status describes the current health of a stored Cursor account.
type Status string

const (
	// StatusActive means the account has a usable access token.
	StatusActive Status = "active"
	// StatusDegraded means the account's refresh failed and it needs
	// re-authentication before it can serve requests again.
	StatusDegraded Status = "degraded"
	// StatusNeedsReauth is a stronger degraded signal: the stored
	// refresh token itself was rejected, so a full login is required
	// (as opposed to a transient refresh failure that may recover).
	StatusNeedsReauth Status = "needs_reauth"
)

// ErrAccountDegraded is returned by Get when the caller must fail fast
// instead of attempting a request with a known-bad account. Callers
// should render this to their own typed "account degraded" error rather
// than propagating it directly across the plugin ABI boundary.
var ErrAccountDegraded = errors.New("account: cursor account is degraded or needs reauthentication")

// State is the durable record for one Cursor account.
type State struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	Status       Status
	// StatusMessage carries a human-readable reason for the current
	// status, surfaced to the management UI.
	StatusMessage string
	UpdatedAt     time.Time
}

// Store is the in-process single source of truth for Cursor account
// state, keyed by the host-assigned auth id. internal/auth is the only
// writer (via Set/MarkDegraded); internal/executor and internal/discovery
// only ever call Get.
type Store struct {
	mu       sync.RWMutex
	accounts map[string]State
}

// NewStore creates an empty account store.
func NewStore() *Store {
	return &Store{accounts: make(map[string]State)}
}

// Set records or replaces the full state for an account, normally after a
// successful login or refresh. Status defaults to StatusActive when the
// caller passes the zero value.
func (s *Store) Set(authID string, st State) {
	if st.Status == "" {
		st.Status = StatusActive
	}
	st.UpdatedAt = time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accounts[authID] = st
}

// MarkDegraded transitions an account to a degraded/needs-reauth status
// without discarding its last-known tokens (they may still be inspectable
// for diagnostics), per the fail-loud principle: a refresh failure is
// recorded, never silently dropped.
func (s *Store) MarkDegraded(authID string, status Status, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.accounts[authID]
	st.Status = status
	st.StatusMessage = message
	st.UpdatedAt = time.Now()
	s.accounts[authID] = st
}

// Get returns the current state for an account. It returns
// ErrAccountDegraded (wrapping the account's status message) when the
// account is not StatusActive, so callers in internal/executor and
// internal/discovery fail fast instead of attempting a request with a
// stale or rejected token.
func (s *Store) Get(authID string) (State, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.accounts[authID]
	if !ok {
		return State{}, errors.New("account: unknown auth id")
	}
	if st.Status != StatusActive {
		msg := st.StatusMessage
		if msg == "" {
			msg = string(st.Status)
		}
		return st, errAccountDegradedWithReason(msg)
	}
	return st, nil
}

// Peek returns the current state for an account without the active-status
// fail-fast check, for diagnostics and management surfaces that need to
// display a degraded account's last-known state.
func (s *Store) Peek(authID string) (State, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.accounts[authID]
	return st, ok
}

func errAccountDegradedWithReason(reason string) error {
	return errors.Join(ErrAccountDegraded, errors.New(reason))
}
