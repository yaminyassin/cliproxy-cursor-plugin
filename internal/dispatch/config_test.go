package dispatch

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"

	"github.com/router-for-me/cliproxy-cursor-plugin/internal/account"
	"github.com/router-for-me/cliproxy-cursor-plugin/internal/executor"
)

// TestPluginReconfigure_AppliesClientVersionOverride regressions the
// terminal-critic finding that x_cursor_client_version was a declared
// but functionally dead ConfigField: plugin.register/reconfigure's
// request body genuinely carries {"config_yaml": <bytes>,
// "schema_version": N} (verified against the real downloaded
// github.com/router-for-me/CLIProxyAPI/v7 module's
// internal/pluginhost/rpc_schema.go), and this plugin now parses it and
// applies x_cursor_client_version to the process-global agentClient.
//
// applyLifecycleConfig is exercised directly against a dedicated
// AgentClient pointed at a real local httptest.Server (rather than
// reaching into the process-global singleton's internal transport),
// which is simpler and equally proves the parse-and-apply behavior end
// to end: a real HTTP request's captured x-cursor-client-version header
// changes after applying a config_yaml payload with the override.
func TestPluginReconfigure_AppliesClientVersionOverride(t *testing.T) {
	var capturedVersion string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedVersion = r.Header.Get("x-cursor-client-version")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := executor.NewAgentClient(account.NewStore(), server.Client(), server.URL, "")

	lifecycle := rpcLifecycleRequest{
		ConfigYAML:    []byte("x_cursor_client_version: \"test-override-1.2.3\"\n"),
		SchemaVersion: pluginabi.SchemaVersion,
	}
	reqBytes, err := json.Marshal(lifecycle)
	if err != nil {
		t.Fatalf("failed to marshal lifecycle request: %v", err)
	}

	applyLifecycleConfigTo(client, reqBytes)

	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("failed to build test request: %v", err)
	}
	resp, err := client.HTTPClient().Do(req)
	if err != nil {
		t.Fatalf("test request failed: %v", err)
	}
	_ = resp.Body.Close()

	if capturedVersion != "test-override-1.2.3" {
		t.Errorf("captured x-cursor-client-version = %q, want %q (reconfigure override did not take effect)", capturedVersion, "test-override-1.2.3")
	}
}

// TestHandleMethod_PluginReconfigure_ParsesRequestWithoutError verifies
// the real HandleMethod(plugin.reconfigure) path (the process-global
// agentClient) accepts a config_yaml payload and returns a normal ok
// envelope, without asserting on the process-global singleton's
// internal transport state (which config_test.go's other test verifies
// in isolation against a dedicated client).
func TestHandleMethod_PluginReconfigure_ParsesRequestWithoutError(t *testing.T) {
	lifecycle := rpcLifecycleRequest{
		ConfigYAML:    []byte("x_cursor_client_version: \"another-override\"\n"),
		SchemaVersion: pluginabi.SchemaVersion,
	}
	reqBytes, err := json.Marshal(lifecycle)
	if err != nil {
		t.Fatalf("failed to marshal lifecycle request: %v", err)
	}

	raw, errHandle := HandleMethod(pluginabi.MethodPluginReconfigure, reqBytes)
	if errHandle != nil {
		t.Fatalf("HandleMethod(plugin.reconfigure) failed: %v", errHandle)
	}
	env := decodeEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("expected ok envelope, got error: %+v", env.Error)
	}
	if agentClient.ClientVersion() != "another-override" {
		t.Errorf("process-global agentClient.ClientVersion() = %q, want %q", agentClient.ClientVersion(), "another-override")
	}
}

// TestApplyLifecycleConfig_EmptyOrMalformed_NoPanic adversarially probes
// applyLifecycleConfig with empty, malformed, and adversarial payloads,
// confirming it never panics and never errors the surrounding
// plugin.register/reconfigure call.
func TestApplyLifecycleConfig_EmptyOrMalformed_NoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("applyLifecycleConfig panicked: %v", r)
		}
	}()

	payloads := [][]byte{
		nil,
		{},
		[]byte("not json"),
		[]byte(`{"config_yaml": "not valid base64!!!"}`),
		[]byte(`{"config_yaml": "bm90IHlhbWw6IFsxLDIsM10="}`),
	}
	for _, p := range payloads {
		applyLifecycleConfig(p)
	}
}
