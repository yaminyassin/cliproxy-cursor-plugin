package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"golang.org/x/sync/singleflight"
)

// refreshGroup deduplicates concurrent refresh attempts per account id,
// mirroring the host's own kimiRefreshGroup pattern in
// internal/auth/kimi/kimi.go, per fact-r4-login-edges' concurrency model.
var refreshGroup singleflight.Group

// RefreshResult is the outcome of a successful token refresh.
type RefreshResult struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

// cursorRefreshResponse is the JSON body Cursor returns from the refresh
// endpoint, ported from gajae-code's refreshCursorToken.
type cursorRefreshResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
}

// Refresher performs Cursor token refresh, deduplicated per account id.
type Refresher struct {
	httpClient *http.Client
}

// NewRefresher creates a Refresher with a default HTTP client. httpClient
// may be replaced by callers that need proxy configuration or test
// doubles.
func NewRefresher(httpClient *http.Client) *Refresher {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Refresher{httpClient: httpClient}
}

// Refresh exchanges apiKeyOrRefreshToken for a fresh access/refresh token
// pair, deduplicating concurrent refresh calls for the same accountID so
// two near-simultaneous refresh triggers coalesce into one upstream call.
func (r *Refresher) Refresh(ctx context.Context, accountID, apiKeyOrRefreshToken string) (RefreshResult, error) {
	result, err, _ := refreshGroup.Do(accountID, func() (any, error) {
		return r.refreshSingleFlight(context.WithoutCancel(ctx), apiKeyOrRefreshToken)
	})
	if err != nil {
		return RefreshResult{}, err
	}
	refreshResult, ok := result.(RefreshResult)
	if !ok {
		return RefreshResult{}, fmt.Errorf("cursor: refresh failed: invalid single-flight result")
	}
	return refreshResult, nil
}

func (r *Refresher) refreshSingleFlight(ctx context.Context, apiKeyOrRefreshToken string) (RefreshResult, error) {
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, cursorRefreshURL, bytes.NewReader([]byte("{}")))
	if errReq != nil {
		return RefreshResult{}, fmt.Errorf("cursor: failed to build refresh request: %w", errReq)
	}
	req.Header.Set("Authorization", "Bearer "+apiKeyOrRefreshToken)
	req.Header.Set("Content-Type", "application/json")

	resp, errDo := r.httpClient.Do(req)
	if errDo != nil {
		return RefreshResult{}, fmt.Errorf("cursor: refresh request failed: %w", errDo)
	}
	defer resp.Body.Close()

	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return RefreshResult{}, fmt.Errorf("cursor: failed to read refresh response: %w", errRead)
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return RefreshResult{}, fmt.Errorf("cursor: refresh token rejected (status %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return RefreshResult{}, fmt.Errorf("cursor: refresh failed with status %d: %s", resp.StatusCode, string(body))
	}

	var refreshResp cursorRefreshResponse
	if errUnmarshal := json.Unmarshal(body, &refreshResp); errUnmarshal != nil {
		return RefreshResult{}, fmt.Errorf("cursor: failed to parse refresh response: %w", errUnmarshal)
	}
	if refreshResp.AccessToken == "" {
		return RefreshResult{}, fmt.Errorf("cursor: empty access token in refresh response")
	}

	newRefreshToken := refreshResp.RefreshToken
	if newRefreshToken == "" {
		// gajae-code's refreshCursorToken falls back to the input token
		// when Cursor omits a rotated refresh token in the response.
		newRefreshToken = apiKeyOrRefreshToken
	}

	expiresAt := getTokenExpiry(refreshResp.AccessToken)

	return RefreshResult{
		AccessToken:  refreshResp.AccessToken,
		RefreshToken: newRefreshToken,
		ExpiresAt:    expiresAt,
	}, nil
}
