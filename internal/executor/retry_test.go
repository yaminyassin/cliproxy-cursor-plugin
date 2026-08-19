package executor

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/router-for-me/cliproxy-cursor-plugin/internal/cursorpb/gen"
)

// TestIsRetryableStreamError_ClassifiesGoawayAndConnectionFailures pins the
// classification that drives runCursorStream's retry. The GOAWAY case is
// the one observed live (2026-08-19): Cursor cycles HTTP/2 connections, and
// Go's transport refuses to retry a request whose streaming body was
// already written, surfacing "graceful shutdown GOAWAY ... cannot retry
// err ... define Request.GetBody".
func TestIsRetryableStreamError_ClassifiesGoawayAndConnectionFailures(t *testing.T) {
	retryable := []string{
		`Post "https://api2.cursor.sh/agent.v1.AgentService/Run": http2: Transport: cannot retry err [http2: Transport received Server's graceful shutdown GOAWAY] after Request.Body was written; define Request.GetBody to avoid this error`,
		"http2: server sent GOAWAY and closed the connection",
		"stream error: stream ID 1; REFUSED_STREAM",
		"read tcp 127.0.0.1:1234->1.2.3.4:443: connection reset by peer",
		"write tcp: broken pipe",
		"unexpected EOF",
		"http2: client connection lost",
	}
	for _, msg := range retryable {
		if !isRetryableStreamError(errors.New(msg)) {
			t.Errorf("expected retryable, got not-retryable for: %s", msg)
		}
	}

	// Request/protocol/Cursor-level failures must surface, never silently
	// re-run: retrying them would burn quota and hide real errors.
	notRetryable := []string{
		"cursor: connect error unauthenticated: invalid token",
		"cursor: streaming run request returned status 400: bad request",
		"cursor: failed to decode server message: proto: cannot parse",
		"cursor: account is degraded",
	}
	for _, msg := range notRetryable {
		if isRetryableStreamError(errors.New(msg)) {
			t.Errorf("expected NOT retryable, got retryable for: %s", msg)
		}
	}

	if isRetryableStreamError(nil) {
		t.Errorf("nil error must not be retryable")
	}
}

// failFirstTransport fails the first RoundTrip with a fixed error, then
// delegates to the real transport. This injects the EXACT error Go's http2
// transport produced live when Cursor sent GOAWAY mid-request, which is
// not reproducible from httptest (an h2 test server cannot be made to
// GOAWAY on demand, and HTTP/2 has no Hijacker to hard-close with).
type failFirstTransport struct {
	base    http.RoundTripper
	failErr error

	mu    sync.Mutex
	calls int
}

func (t *failFirstTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.calls++
	n := t.calls
	t.mu.Unlock()
	if n == 1 {
		return nil, t.failErr
	}
	return t.base.RoundTrip(req)
}

func (t *failFirstTransport) callCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
}

// TestRunCursorStream_RetriesGoawayThenSucceeds proves the retry actually
// recovers a turn after the live GOAWAY failure: attempt 1 fails with the
// verbatim error observed in production, attempt 2 completes normally.
// Without the retry, a real client saw a failed turn (the flakiness
// symptom reported on 2026-08-19).
func TestRunCursorStream_RetriesGoawayThenSucceeds(t *testing.T) {
	fake := newFakeCursorRunServer(t)
	fake.scriptedUpdates = []*gen.InteractionUpdate{
		{Message: &gen.InteractionUpdate_TextDelta{TextDelta: &gen.TextDeltaUpdate{Text: "recovered after retry"}}},
		{Message: &gen.InteractionUpdate_TurnEnded{TurnEnded: &gen.TurnEndedUpdate{}}},
	}

	goaway := errors.New(`Post "https://api2.cursor.sh/agent.v1.AgentService/Run": http2: Transport: cannot retry err [http2: Transport received Server's graceful shutdown GOAWAY] after Request.Body was written; define Request.GetBody to avoid this error`)
	flaky := &failFirstTransport{base: fake.client.Transport, failErr: goaway}

	client := NewAgentClient(activeAccountStore(), &http.Client{Transport: flaky}, fake.server.URL, "")
	runReq := &gen.AgentRunRequest{
		Action: &gen.ConversationAction{
			Action: &gen.ConversationAction_UserMessageAction{
				UserMessageAction: &gen.UserMessageAction{UserMessage: &gen.UserMessage{Text: "hi"}},
			},
		},
	}

	result, err := client.runCursorStream(context.Background(), fake.server.URL, "test-token", runReq, newBlobStore())
	if err != nil {
		t.Fatalf("expected the retry to recover the turn, got error: %v", err)
	}
	if result == nil || len(result.updates) == 0 {
		t.Fatalf("expected accumulated updates from the successful retry, got %+v", result)
	}

	var acc responseAccumulator
	for _, u := range result.updates {
		acc.accumulate(u)
	}
	resp := acc.toChatCompletionsResponse("cursor-fast", "resp-1", "hi")
	if resp.Choices[0].Message.Content != "recovered after retry" {
		t.Errorf("content = %q, want %q", resp.Choices[0].Message.Content, "recovered after retry")
	}
	if flaky.callCount() < 2 {
		t.Errorf("expected at least 2 transport attempts (first GOAWAY, second success), got %d", flaky.callCount())
	}
}

// TestRunCursorStream_DoesNotRetryNonRetryableError verifies a
// request/protocol-level failure is surfaced immediately instead of being
// re-run, so real errors are never hidden and quota is never burned.
func TestRunCursorStream_DoesNotRetryNonRetryableError(t *testing.T) {
	fake := newFakeCursorRunServer(t)
	fatal := errors.New("cursor: connect error unauthenticated: invalid token")
	flaky := &failFirstTransport{base: fake.client.Transport, failErr: fatal}

	client := NewAgentClient(activeAccountStore(), &http.Client{Transport: flaky}, fake.server.URL, "")
	runReq := &gen.AgentRunRequest{
		Action: &gen.ConversationAction{
			Action: &gen.ConversationAction_UserMessageAction{
				UserMessageAction: &gen.UserMessageAction{UserMessage: &gen.UserMessage{Text: "hi"}},
			},
		},
	}

	_, err := client.runCursorStream(context.Background(), fake.server.URL, "test-token", runReq, newBlobStore())
	if err == nil {
		t.Fatalf("expected a non-retryable error to surface, got success")
	}
	if flaky.callCount() != 1 {
		t.Errorf("expected exactly 1 attempt for a non-retryable error, got %d", flaky.callCount())
	}
}
