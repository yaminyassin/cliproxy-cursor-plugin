// Package executor implements the executor and model_registrar/
// model_provider capabilities: translating CLIProxyAPI's chat-completions
// requests to Cursor's Connect/gRPC agent.v1.AgentService protocol and
// back (including a per-turn-batched tool-call round-trip through the
// local client per fact-r5-tool-roundtrip), plus host-conformant dynamic
// model discovery via GetUsableModels, per the ralplan-approved plan.
package executor

import (
	"net/http"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	"github.com/router-for-me/cliproxy-cursor-plugin/internal/account"
	"github.com/router-for-me/cliproxy-cursor-plugin/internal/cursorpb/gen/genconnect"
)

// cursorBaseURL is Cursor's Connect/gRPC backend, ported from gajae-code's
// CURSOR_API_URL. HTTP/2 is required: api2.cursor.sh rejects HTTP/1.1
// with status 464 (see internal/cursorpb/README.md).
const cursorBaseURL = "https://api2.cursor.sh"

// defaultClientVersion is the compiled-in default for the
// x-cursor-client-version header, matching gajae-code's current
// CURSOR_CLIENT_VERSION constant (packages/ai/src/providers/cursor/
// client-version.ts) as of this writing. Cursor gates backend
// features/minimum versions on this header, and the reference value
// changes over time as Cursor ships new client releases - this constant
// will drift and needs periodic re-syncing against the reference.
//
// KNOWN GAP: the x_cursor_client_version ConfigField is declared at
// plugin.register (internal/dispatch/dispatch.go) for exactly this
// reason, but plugin.reconfigure does not yet parse and apply it to the
// running AgentClient - the SDK's public lifecycle request shape
// (examples/plugin/simple/go/main.go's local lifecycleRequest struct)
// does not expose a per-plugin config payload in this SDK version, so
// wiring this safely needs either a newer SDK release or independent
// verification of the host's actual reconfigure payload shape before
// implementing it, rather than guessing at an unverified internal API.
const defaultClientVersion = "cli-2026.02.13-41ac335"

// newHTTP2Client builds an http.Client with HTTP/2 support forced over
// TLS, matching gajae-code's http2.connect usage.
func newHTTP2Client() *http.Client {
	transport := &http2.Transport{}
	return &http.Client{
		Timeout:   60 * time.Second,
		Transport: transport,
	}
}

// staticHeaderTransport injects headers Cursor requires on every request
// regardless of account (x-cursor-client-version, x-ghost-mode,
// x-cursor-client-type), ported from gajae-code's buildRequestHeaders.
// Per-account bearer auth is set per-call via Connect request headers
// instead (see withAuth below), since it varies by account.
type staticHeaderTransport struct {
	base          http.RoundTripper
	clientVersion string
}

func (t *staticHeaderTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	version := t.clientVersion
	if version == "" {
		version = defaultClientVersion
	}
	req.Header.Set("x-cursor-client-version", version)
	req.Header.Set("x-ghost-mode", "true")
	req.Header.Set("x-cursor-client-type", "cli")
	if req.Header.Get("x-request-id") == "" {
		req.Header.Set("x-request-id", newRequestID())
	}
	return t.base.RoundTrip(req)
}

// AgentClient bundles the generated Connect-RPC client for
// agent.v1.AgentService (used for simple unary calls like
// GetUsableModels) with the raw HTTP client and base URL needed for the
// real bidirectional Run exchange (stream.go), which cannot go through
// the generated unary client - see stream.go's package doc for why.
type AgentClient struct {
	Service       genconnect.AgentServiceClient
	Accounts      *account.Store
	ClientVersion string

	httpClient *http.Client
	baseURL    string
}

// NewAgentClient builds an AgentClient pointed at Cursor's real backend.
// httpClient may be overridden by tests; clientVersion overrides the
// compiled-in default x-cursor-client-version when non-empty.
func NewAgentClient(accounts *account.Store, httpClient *http.Client, baseURL, clientVersion string) *AgentClient {
	if httpClient == nil {
		httpClient = newHTTP2Client()
	}
	if baseURL == "" {
		baseURL = cursorBaseURL
	}
	base := httpClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	versioned := &http.Client{
		Timeout: httpClient.Timeout,
		Transport: &staticHeaderTransport{
			base:          base,
			clientVersion: clientVersion,
		},
	}

	return &AgentClient{
		Service:       genconnect.NewAgentServiceClient(versioned, baseURL),
		Accounts:      accounts,
		ClientVersion: clientVersion,
		httpClient:    versioned,
		baseURL:       baseURL,
	}
}

// withAuth sets the per-account Bearer authorization header on a Connect
// request, matching gajae-code's `authorization: Bearer ${accessToken}`.
func withAuth[T any](req *connect.Request[T], accessToken string) *connect.Request[T] {
	req.Header().Set("authorization", "Bearer "+accessToken)
	return req
}
