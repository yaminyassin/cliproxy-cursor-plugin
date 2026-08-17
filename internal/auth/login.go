// Package auth implements the auth_provider capability: Cursor OAuth login
// (start/poll), token parsing/storage, and refresh, porting the flow
// verified in ../gajae-code/packages/ai/src/utils/oauth/cursor.ts while
// following CLIProxyAPI's own host-conformant poll/refresh idiom (see
// internal/auth/kimi/kimi.go in the host repo) per fact-r4-host-conformance.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"

	"github.com/google/uuid"
)

const (
	// cursorLoginURL is the Cursor PKCE login endpoint, ported verbatim
	// from gajae-code's CURSOR_LOGIN_URL.
	cursorLoginURL = "https://cursor.com/loginDeepControl"
	// cursorPollURL is the Cursor login-poll endpoint, ported verbatim
	// from gajae-code's CURSOR_POLL_URL.
	cursorPollURL = "https://api2.cursor.sh/auth/poll"
	// cursorRefreshURL is the Cursor token-refresh endpoint, ported
	// verbatim from gajae-code's CURSOR_REFRESH_URL.
	cursorRefreshURL = "https://api2.cursor.sh/auth/exchange_user_api_key"
)

// AuthParams holds the PKCE material and login URL for one login attempt.
// The Verifier must be retained by the caller (keyed by the login-attempt
// State, per the store in poll.go) and reused for the matching poll call.
type AuthParams struct {
	Verifier  string
	Challenge string
	UUID      string
	LoginURL  string
}

// generatePKCE ports gajae-code's generatePKCE(): a 96-byte random
// verifier and its SHA-256 challenge, both base64url-encoded without
// padding (Go's RawURLEncoding matches JS's Buffer.toString("base64url")).
func generatePKCE() (verifier string, challenge string, err error) {
	verifierBytes := make([]byte, 96)
	if _, errRand := rand.Read(verifierBytes); errRand != nil {
		return "", "", fmt.Errorf("cursor: failed to generate PKCE verifier: %w", errRand)
	}
	verifier = base64.RawURLEncoding.EncodeToString(verifierBytes)

	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])

	return verifier, challenge, nil
}

// GenerateAuthParams ports gajae-code's generateCursorAuthParams(): builds
// PKCE material, a random login UUID, and the resulting Cursor login URL
// (challenge + uuid + mode=login + redirectTarget=cli).
func GenerateAuthParams() (AuthParams, error) {
	verifier, challenge, err := generatePKCE()
	if err != nil {
		return AuthParams{}, err
	}
	loginUUID := uuid.New().String()

	loginURL := fmt.Sprintf(
		"%s?challenge=%s&uuid=%s&mode=login&redirectTarget=cli",
		cursorLoginURL,
		challenge,
		loginUUID,
	)

	return AuthParams{
		Verifier:  verifier,
		Challenge: challenge,
		UUID:      loginUUID,
		LoginURL:  loginURL,
	}, nil
}
