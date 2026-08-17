package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Host-conformant poll timing, matching internal/auth/kimi/kimi.go in the
// CLIProxyAPI host repo (defaultPollInterval / maxPollDuration), per
// fact-r4-host-conformance and fact-r4-login-edges. This replaces
// gajae-code's reference implementation's own raw ~9-minute
// (150 attempts * up to 10s backoff) polling loop.
const (
	pollInterval                = 5 * time.Second
	maxPollDuration             = 15 * time.Minute
	maxConsecutiveNetworkErrors = 3
)

// cursorTokenResponse is the JSON body Cursor returns on a successful poll.
type cursorTokenResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
}

// PollResult is the outcome of one poll tick against Cursor's login-poll
// endpoint, translated into the plugin ABI's 3-value AuthLoginStatus
// (pending/success/error). The distinct failure kinds the ralplan plan
// calls for (timeout vs network vs rejected) are not separately
// representable in the ABI's status enum, so they are carried in Message
// with a stable "<kind>: <detail>" prefix instead.
type PollResult struct {
	Status       pluginapi.AuthLoginStatus
	Message      string
	AccessToken  string
	RefreshToken string
}

// Poller drives the login-poll state machine for one plugin instance,
// tracking in-flight login attempts (see pending.go).
type Poller struct {
	pending    *pendingLoginStore
	httpClient *http.Client

	// networkErrorMu guards consecutiveNetworkErrors. It is a distinct
	// lock from pendingLoginStore's own mutex (guarding the byID map) so
	// concurrent Poll calls for different attempt ids never race on the
	// error-streak counter: repeated transient network failures
	// accumulate toward a terminal error per attempt id, mirroring the
	// consecutiveErrors >= 3 threshold in gajae-code's own
	// pollCursorAuth reference.
	networkErrorMu           sync.Mutex
	consecutiveNetworkErrors map[string]int
}

// NewPoller creates a Poller with a default HTTP client. httpClient may be
// replaced by callers that need proxy configuration or test doubles.
func NewPoller(httpClient *http.Client) *Poller {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Poller{
		pending:                  newPendingLoginStore(),
		httpClient:               httpClient,
		consecutiveNetworkErrors: make(map[string]int),
	}
}

// StartLogin generates PKCE material, registers the pending attempt, and
// returns the login params for the caller to build an
// AuthLoginStartResponse from.
func (p *Poller) StartLogin() (AuthParams, time.Time, error) {
	params, err := GenerateAuthParams()
	if err != nil {
		return AuthParams{}, time.Time{}, err
	}
	deadline := time.Now().Add(maxPollDuration)
	p.pending.put(params.UUID, pendingLogin{
		Verifier:   params.Verifier,
		CursorUUID: params.UUID,
		Deadline:   deadline,
	})
	return params, deadline, nil
}

// Poll performs exactly one check against Cursor's login-poll endpoint for
// the given attempt id (the State returned from StartLogin), and returns
// the current status. The host is expected to call this repeatedly at its
// own cadence until a terminal status (success or error) is returned;
// pollInterval/maxPollDuration bound the plugin's own internal timing
// expectations but do not block this call.
func (p *Poller) Poll(ctx context.Context, attemptID string) PollResult {
	pending, ok := p.pending.get(attemptID)
	if !ok {
		return PollResult{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "rejected: unknown or already-completed login attempt",
		}
	}

	if time.Now().After(pending.Deadline) {
		p.pending.delete(attemptID)
		p.clearNetworkErrors(attemptID)
		return PollResult{
			Status:  pluginapi.AuthLoginStatusError,
			Message: fmt.Sprintf("timeout: login attempt expired after %s", maxPollDuration),
		}
	}

	url := fmt.Sprintf("%s?uuid=%s&verifier=%s", cursorPollURL, pending.CursorUUID, pending.Verifier)
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if errReq != nil {
		return PollResult{
			Status:  pluginapi.AuthLoginStatusError,
			Message: fmt.Sprintf("rejected: failed to build poll request: %v", errReq),
		}
	}

	resp, errDo := p.httpClient.Do(req)
	if errDo != nil {
		return p.handleNetworkError(attemptID, errDo)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// Still pending; reset the network-error streak on any successful
		// round trip, even a 404, since the transport itself is healthy.
		p.clearNetworkErrors(attemptID)
		return PollResult{Status: pluginapi.AuthLoginStatusPending}
	}

	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return p.handleNetworkError(attemptID, errRead)
	}

	if resp.StatusCode != http.StatusOK {
		p.pending.delete(attemptID)
		p.clearNetworkErrors(attemptID)
		return PollResult{
			Status:  pluginapi.AuthLoginStatusError,
			Message: fmt.Sprintf("rejected: cursor poll returned status %d: %s", resp.StatusCode, string(body)),
		}
	}

	var tokenResp cursorTokenResponse
	if errUnmarshal := json.Unmarshal(body, &tokenResp); errUnmarshal != nil {
		p.pending.delete(attemptID)
		p.clearNetworkErrors(attemptID)
		return PollResult{
			Status:  pluginapi.AuthLoginStatusError,
			Message: fmt.Sprintf("rejected: failed to parse cursor poll response: %v", errUnmarshal),
		}
	}
	if tokenResp.AccessToken == "" {
		p.pending.delete(attemptID)
		p.clearNetworkErrors(attemptID)
		return PollResult{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "rejected: cursor poll response missing access token",
		}
	}

	p.pending.delete(attemptID)
	p.clearNetworkErrors(attemptID)
	return PollResult{
		Status:       pluginapi.AuthLoginStatusSuccess,
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
	}
}

// handleNetworkError implements the consecutive-network-error threshold:
// up to maxConsecutiveNetworkErrors transient transport failures are
// reported as Pending (so the host keeps polling through a network blip),
// and only the threshold-exceeding failure is terminal.
func (p *Poller) handleNetworkError(attemptID string, err error) PollResult {
	count := p.incrementNetworkErrors(attemptID)
	if count >= maxConsecutiveNetworkErrors {
		p.pending.delete(attemptID)
		p.clearNetworkErrors(attemptID)
		return PollResult{
			Status:  pluginapi.AuthLoginStatusError,
			Message: fmt.Sprintf("network_error: too many consecutive poll failures (%d): %v", count, err),
		}
	}
	return PollResult{
		Status:  pluginapi.AuthLoginStatusPending,
		Message: fmt.Sprintf("network_error: transient poll failure %d/%d, retrying: %v", count, maxConsecutiveNetworkErrors, err),
	}
}

func (p *Poller) incrementNetworkErrors(attemptID string) int {
	p.networkErrorMu.Lock()
	defer p.networkErrorMu.Unlock()
	p.consecutiveNetworkErrors[attemptID]++
	return p.consecutiveNetworkErrors[attemptID]
}

func (p *Poller) clearNetworkErrors(attemptID string) {
	p.networkErrorMu.Lock()
	defer p.networkErrorMu.Unlock()
	delete(p.consecutiveNetworkErrors, attemptID)
}
