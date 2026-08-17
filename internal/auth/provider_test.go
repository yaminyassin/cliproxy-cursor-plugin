package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/router-for-me/cliproxy-cursor-plugin/internal/account"
)

// mockCursorServer stands in for api2.cursor.sh's poll and refresh
// endpoints across the five auth.login.poll outcomes and refresh
// success/failure/dedup cases this suite covers.
type mockCursorServer struct {
	server *httptest.Server

	mu              sync.Mutex
	pollBehavior    func(uuid, verifier string) (status int, body string)
	refreshCalls    int32
	refreshBehavior func(bearer string) (status int, body string)
	refreshDelay    time.Duration
}

func newMockCursorServer() *mockCursorServer {
	m := &mockCursorServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/poll", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		behavior := m.pollBehavior
		m.mu.Unlock()
		if behavior == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		status, body := behavior(r.URL.Query().Get("uuid"), r.URL.Query().Get("verifier"))
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/auth/exchange_user_api_key", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&m.refreshCalls, 1)
		if m.refreshDelay > 0 {
			time.Sleep(m.refreshDelay)
		}
		m.mu.Lock()
		behavior := m.refreshBehavior
		m.mu.Unlock()
		if behavior == nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		status, body := behavior(r.Header.Get("Authorization"))
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	m.server = httptest.NewServer(mux)
	return m
}

func (m *mockCursorServer) Close() { m.server.Close() }

func (m *mockCursorServer) setPollBehavior(f func(uuid, verifier string) (int, string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pollBehavior = f
}

func (m *mockCursorServer) setRefreshBehavior(f func(bearer string) (int, string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refreshBehavior = f
}

// rewriteTransport redirects every outbound request to the mock server
// while leaving login.go/poll.go/refresh.go's real cursor.com/
// api2.cursor.sh URL construction untouched, so these tests exercise the
// exact request-building code that runs in production.
type rewriteTransport struct {
	target string
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme = "http"
	req.URL.Host = strings.TrimPrefix(t.target, "http://")
	return http.DefaultTransport.RoundTrip(req)
}

func newTestHTTPClient(mockServerURL string) *http.Client {
	return &http.Client{
		Timeout:   5 * time.Second,
		Transport: &rewriteTransport{target: mockServerURL},
	}
}

// --- auth.login.poll: five status outcomes ---

func TestPollLogin_Success(t *testing.T) {
	mock := newMockCursorServer()
	defer mock.Close()
	mock.setPollBehavior(func(uuid, verifier string) (int, string) {
		resp, _ := json.Marshal(cursorTokenResponse{AccessToken: fakeJWT(time.Now().Add(time.Hour)), RefreshToken: "refresh-abc"})
		return http.StatusOK, string(resp)
	})

	accounts := account.NewStore()
	provider := NewProvider(accounts, newTestHTTPClient(mock.server.URL))

	startResp, err := provider.StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{})
	if err != nil {
		t.Fatalf("StartLogin failed: %v", err)
	}

	pollResp, err := provider.PollLogin(context.Background(), pluginapi.AuthLoginPollRequest{State: startResp.State})
	if err != nil {
		t.Fatalf("PollLogin failed: %v", err)
	}
	if pollResp.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("status = %q, want success; message=%q", pollResp.Status, pollResp.Message)
	}
	if pollResp.Auth.Provider != "cursor" {
		t.Errorf("auth.provider = %q, want cursor", pollResp.Auth.Provider)
	}
	if len(pollResp.Auth.StorageJSON) == 0 {
		t.Errorf("expected non-empty StorageJSON on success")
	}

	st, ok := accounts.Peek("cursor")
	if !ok || st.Status != account.StatusActive {
		t.Errorf("expected account store to record an active cursor account after success, got %+v (ok=%v)", st, ok)
	}
}

func TestPollLogin_Pending(t *testing.T) {
	mock := newMockCursorServer()
	defer mock.Close()
	mock.setPollBehavior(func(uuid, verifier string) (int, string) {
		return http.StatusNotFound, ""
	})

	accounts := account.NewStore()
	provider := NewProvider(accounts, newTestHTTPClient(mock.server.URL))

	startResp, err := provider.StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{})
	if err != nil {
		t.Fatalf("StartLogin failed: %v", err)
	}

	pollResp, err := provider.PollLogin(context.Background(), pluginapi.AuthLoginPollRequest{State: startResp.State})
	if err != nil {
		t.Fatalf("PollLogin failed: %v", err)
	}
	if pollResp.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("status = %q, want pending", pollResp.Status)
	}
}

func TestPollLogin_Timeout(t *testing.T) {
	poller := NewPoller(nil)
	// Directly construct a pending login with an already-elapsed
	// deadline, rather than waiting maxPollDuration in a test.
	poller.pending.put("attempt-timeout", pendingLogin{
		Verifier:   "v",
		CursorUUID: "attempt-timeout",
		Deadline:   time.Now().Add(-time.Second),
	})

	result := poller.Poll(context.Background(), "attempt-timeout")
	if result.Status != pluginapi.AuthLoginStatusError {
		t.Fatalf("status = %q, want error", result.Status)
	}
	if !strings.HasPrefix(result.Message, "timeout:") {
		t.Errorf("message = %q, want timeout: prefix", result.Message)
	}
}

func TestPollLogin_NetworkError(t *testing.T) {
	// Point the poller at a server that immediately closes connections,
	// so every request fails at the transport level.
	badServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Skip("hijacking not supported on this platform")
		}
		conn, _, _ := hijacker.Hijack()
		_ = conn.Close()
	}))
	defer badServer.Close()

	poller := NewPoller(newTestHTTPClient(badServer.URL))
	poller.pending.put("attempt-network", pendingLogin{
		Verifier:   "v",
		CursorUUID: "attempt-network",
		Deadline:   time.Now().Add(time.Minute),
	})

	// First two failures should stay pending (below the consecutive
	// threshold); the third should be terminal.
	var last PollResult
	for i := 0; i < maxConsecutiveNetworkErrors; i++ {
		last = poller.Poll(context.Background(), "attempt-network")
		if i < maxConsecutiveNetworkErrors-1 {
			if last.Status != pluginapi.AuthLoginStatusPending {
				t.Fatalf("attempt %d: status = %q, want pending (below threshold)", i, last.Status)
			}
		}
	}
	if last.Status != pluginapi.AuthLoginStatusError {
		t.Fatalf("final status = %q, want error after %d consecutive failures", last.Status, maxConsecutiveNetworkErrors)
	}
	if !strings.HasPrefix(last.Message, "network_error:") {
		t.Errorf("message = %q, want network_error: prefix", last.Message)
	}
}

func TestPollLogin_Rejected(t *testing.T) {
	mock := newMockCursorServer()
	defer mock.Close()
	mock.setPollBehavior(func(uuid, verifier string) (int, string) {
		return http.StatusForbidden, `{"error":"access_denied"}`
	})

	accounts := account.NewStore()
	provider := NewProvider(accounts, newTestHTTPClient(mock.server.URL))

	startResp, err := provider.StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{})
	if err != nil {
		t.Fatalf("StartLogin failed: %v", err)
	}

	pollResp, err := provider.PollLogin(context.Background(), pluginapi.AuthLoginPollRequest{State: startResp.State})
	if err != nil {
		t.Fatalf("PollLogin failed: %v", err)
	}
	if pollResp.Status != pluginapi.AuthLoginStatusError {
		t.Fatalf("status = %q, want error", pollResp.Status)
	}
	if !strings.HasPrefix(pollResp.Message, "rejected:") {
		t.Errorf("message = %q, want rejected: prefix", pollResp.Message)
	}
}

// --- auth.refresh: success, failure, concurrent dedup ---

func TestRefreshAuth_Success(t *testing.T) {
	mock := newMockCursorServer()
	defer mock.Close()
	mock.setRefreshBehavior(func(bearer string) (int, string) {
		resp, _ := json.Marshal(cursorRefreshResponse{AccessToken: fakeJWT(time.Now().Add(time.Hour)), RefreshToken: "new-refresh"})
		return http.StatusOK, string(resp)
	})

	accounts := account.NewStore()
	provider := NewProvider(accounts, newTestHTTPClient(mock.server.URL))

	storage := NewTokenStorage("old-access", "old-refresh", time.Now().Add(-time.Minute))
	storageJSON, _ := storage.Marshal()

	resp, err := provider.RefreshAuth(context.Background(), pluginapi.AuthRefreshRequest{
		AuthID:      "cursor",
		StorageJSON: storageJSON,
	})
	if err != nil {
		t.Fatalf("RefreshAuth failed: %v", err)
	}
	if len(resp.Auth.StorageJSON) == 0 {
		t.Errorf("expected non-empty refreshed StorageJSON")
	}

	st, ok := accounts.Peek("cursor")
	if !ok || st.Status != account.StatusActive {
		t.Errorf("expected account marked active after successful refresh, got %+v (ok=%v)", st, ok)
	}
}

func TestRefreshAuth_Failure_MarksAccountDegraded(t *testing.T) {
	mock := newMockCursorServer()
	defer mock.Close()
	mock.setRefreshBehavior(func(bearer string) (int, string) {
		return http.StatusUnauthorized, `{"error":"invalid_grant"}`
	})

	accounts := account.NewStore()
	provider := NewProvider(accounts, newTestHTTPClient(mock.server.URL))

	storage := NewTokenStorage("old-access", "old-refresh", time.Now().Add(-time.Minute))
	storageJSON, _ := storage.Marshal()

	_, err := provider.RefreshAuth(context.Background(), pluginapi.AuthRefreshRequest{
		AuthID:      "cursor",
		StorageJSON: storageJSON,
	})
	if err == nil {
		t.Fatalf("expected RefreshAuth to fail on 401, got nil error")
	}

	st, ok := accounts.Peek("cursor")
	if !ok {
		t.Fatalf("expected account entry to exist after failed refresh")
	}
	if st.Status != account.StatusNeedsReauth {
		t.Errorf("status = %q, want %q (never silently dropped)", st.Status, account.StatusNeedsReauth)
	}

	// Get() must fail fast on a degraded account instead of returning
	// stale-looking active state.
	if _, errGet := accounts.Get("cursor"); errGet == nil {
		t.Errorf("expected accounts.Get to fail fast on a degraded account")
	}
}

func TestRefreshAuth_ConcurrentDedup(t *testing.T) {
	mock := newMockCursorServer()
	defer mock.Close()
	mock.refreshDelay = 100 * time.Millisecond
	mock.setRefreshBehavior(func(bearer string) (int, string) {
		resp, _ := json.Marshal(cursorRefreshResponse{AccessToken: fakeJWT(time.Now().Add(time.Hour)), RefreshToken: "new-refresh"})
		return http.StatusOK, string(resp)
	})

	accounts := account.NewStore()
	provider := NewProvider(accounts, newTestHTTPClient(mock.server.URL))

	storage := NewTokenStorage("old-access", "old-refresh", time.Now().Add(-time.Minute))
	storageJSON, _ := storage.Marshal()

	var wg sync.WaitGroup
	errs := make([]error, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, err := provider.RefreshAuth(context.Background(), pluginapi.AuthRefreshRequest{
				AuthID:      "cursor",
				StorageJSON: storageJSON,
			})
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent refresh %d failed: %v", i, err)
		}
	}

	calls := atomic.LoadInt32(&mock.refreshCalls)
	if calls != 1 {
		t.Errorf("expected exactly 1 upstream refresh call for 5 concurrent triggers on the same account, got %d", calls)
	}
}

// fakeJWT builds a minimally valid unsigned JWT with the given exp claim,
// for tests that need getTokenExpiry to parse a realistic token shape.
func fakeJWT(exp time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp.Unix())))
	return header + "." + payload + ".sig"
}
