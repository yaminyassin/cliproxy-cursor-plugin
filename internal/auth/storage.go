package auth

import (
	"encoding/json"
	"time"
)

// TokenStorage is the persisted JSON shape for a Cursor account, matching
// the host's own kimi provider's structural template (KimiTokenStorage in
// internal/auth/kimi): AccessToken/RefreshToken/Expired RFC3339
// string/Type, persisted under auths/ by the host.
type TokenStorage struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Expired      string `json:"expired,omitempty"`
	Type         string `json:"type"`
}

// NewTokenStorage builds the persisted storage record for a fresh or
// refreshed Cursor account.
func NewTokenStorage(accessToken, refreshToken string, expiresAt time.Time) TokenStorage {
	expired := ""
	if !expiresAt.IsZero() {
		expired = expiresAt.UTC().Format(time.RFC3339)
	}
	return TokenStorage{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		Expired:      expired,
		Type:         "cursor",
	}
}

// Marshal serializes the storage record to the StorageJSON bytes the host
// persists via pluginapi.AuthData.StorageJSON.
func (t TokenStorage) Marshal() ([]byte, error) {
	return json.Marshal(t)
}

// ParseTokenStorage decodes a persisted StorageJSON payload back into a
// TokenStorage record, for auth.parse.
func ParseTokenStorage(raw []byte) (TokenStorage, error) {
	var t TokenStorage
	if err := json.Unmarshal(raw, &t); err != nil {
		return TokenStorage{}, err
	}
	return t, nil
}

// ExpiresAt parses the stored Expired RFC3339 string back into a
// time.Time. A zero or unparsable value returns the zero time.
func (t TokenStorage) ExpiresAt() time.Time {
	if t.Expired == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339, t.Expired)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

// authMetadata exposes the current access token through the host's canonical
// metadata field. CLIProxyAPI's management /api-call replaces $TOKEN$ from
// metadata.access_token, which lets the management backend query Cursor's
// account-scoped usage endpoint without returning the token to the browser.
// Refresh material remains only in plugin-owned storage.
func authMetadata(accessToken string, metadata map[string]any) map[string]any {
	cloned := make(map[string]any, len(metadata)+2)
	for key, value := range metadata {
		cloned[key] = value
	}
	cloned["type"] = providerIdentifier
	cloned["access_token"] = accessToken
	return cloned
}
