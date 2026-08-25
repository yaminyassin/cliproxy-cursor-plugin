package dispatch

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

func decodeEnvelope(t *testing.T, raw []byte) envelope {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("failed to decode envelope: %v (raw=%s)", err, raw)
	}
	return env
}

func TestHandleMethod_PluginRegister_ReturnsValidCapabilities(t *testing.T) {
	for _, method := range []string{pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure} {
		raw, err := HandleMethod(method, nil)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", method, err)
		}
		env := decodeEnvelope(t, raw)
		if !env.OK {
			t.Fatalf("%s: expected ok envelope, got error: %+v", method, env.Error)
		}

		var reg registration
		if errUnmarshal := json.Unmarshal(env.Result, &reg); errUnmarshal != nil {
			t.Fatalf("%s: failed to decode registration: %v", method, errUnmarshal)
		}

		if reg.SchemaVersion != pluginabi.SchemaVersion {
			t.Errorf("%s: schema_version = %d, want %d (pinned to pluginabi.SchemaVersion)", method, reg.SchemaVersion, pluginabi.SchemaVersion)
		}
		if reg.Metadata.Name == "" {
			t.Errorf("%s: metadata.name must not be empty", method)
		}
		if reg.Metadata.Version == "" {
			t.Errorf("%s: metadata.version must not be empty", method)
		}
		if reg.Metadata.Version != version {
			t.Errorf("%s: metadata.version = %q, want %q", method, reg.Metadata.Version, version)
		}
		if reg.Metadata.Logo != cursorLogoURL {
			t.Errorf("%s: metadata.logo = %q, want %q", method, reg.Metadata.Logo, cursorLogoURL)
		}
		if reg.Metadata.GitHubRepository != pluginRepositoryURL {
			t.Errorf("%s: metadata.github_repository = %q, want %q", method, reg.Metadata.GitHubRepository, pluginRepositoryURL)
		}

		caps := reg.Capabilities
		if !caps.AuthProvider {
			t.Errorf("%s: expected auth_provider capability true", method)
		}
		if !caps.ModelRegistrar {
			t.Errorf("%s: expected model_registrar capability true", method)
		}
		if !caps.ModelProvider {
			t.Errorf("%s: expected model_provider capability true", method)
		}
		if !caps.Executor {
			t.Errorf("%s: expected executor capability true", method)
		}
		if caps.ExecutorModelScope != "both" {
			t.Errorf("%s: executor_model_scope = %q, want %q", method, caps.ExecutorModelScope, "both")
		}
		if len(caps.ExecutorInputFormats) != 1 || caps.ExecutorInputFormats[0] != "chat-completions" {
			t.Errorf("%s: executor_input_formats = %v, want [chat-completions]", method, caps.ExecutorInputFormats)
		}
		if len(caps.ExecutorOutputFormats) != 1 || caps.ExecutorOutputFormats[0] != "chat-completions" {
			t.Errorf("%s: executor_output_formats = %v, want [chat-completions]", method, caps.ExecutorOutputFormats)
		}
	}
}

// TestHandleMethod_RoutesEveryDeclaredMethod verifies the ABI dispatch
// table routes each host-defined method name to a handler that returns a
// well-formed envelope, rather than silently falling through to the
// unknown-method branch. This is the acceptance criterion for G002:
// "unit test confirms the ABI dispatch table routes each method name
// correctly."
func TestHandleMethod_RoutesEveryDeclaredMethod(t *testing.T) {
	// Methods this plugin declares capabilities for and must route to a
	// real (or explicitly not-yet-implemented) handler, never the
	// catch-all unknown_method branch.
	declaredMethods := []string{
		pluginabi.MethodPluginRegister,
		pluginabi.MethodPluginReconfigure,
		pluginabi.MethodAuthIdentifier,
		pluginabi.MethodAuthParse,
		pluginabi.MethodAuthLoginStart,
		pluginabi.MethodAuthLoginPoll,
		pluginabi.MethodAuthRefresh,
		pluginabi.MethodModelRegister,
		pluginabi.MethodModelStatic,
		pluginabi.MethodModelForAuth,
		pluginabi.MethodExecutorIdentifier,
		pluginabi.MethodExecutorExecute,
		pluginabi.MethodExecutorExecuteStream,
		pluginabi.MethodExecutorCountTokens,
		pluginabi.MethodExecutorHTTPRequest,
	}

	for _, method := range declaredMethods {
		raw, err := HandleMethod(method, nil)
		if err != nil {
			t.Fatalf("%s: unexpected transport-level error: %v", method, err)
		}
		env := decodeEnvelope(t, raw)
		if !env.OK && env.Error != nil && env.Error.Code == "unknown_method" {
			t.Errorf("%s: routed to the unknown_method fallback; dispatch table is missing this case", method)
		}
	}
}

func TestHandleMethod_UnknownMethod_ReturnsTypedError(t *testing.T) {
	raw, err := HandleMethod("totally.made.up.method", nil)
	if err != nil {
		t.Fatalf("unexpected transport-level error: %v", err)
	}
	env := decodeEnvelope(t, raw)
	if env.OK {
		t.Fatalf("expected error envelope for unknown method, got ok=true")
	}
	if env.Error == nil || env.Error.Code != "unknown_method" {
		t.Fatalf("expected unknown_method error code, got %+v", env.Error)
	}
}

func TestHandleMethod_AuthIdentifier_ReturnsStableIdentifier(t *testing.T) {
	raw, err := HandleMethod(pluginabi.MethodAuthIdentifier, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	env := decodeEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("expected ok envelope, got error: %+v", env.Error)
	}
	var resp identifierResponse
	if errUnmarshal := json.Unmarshal(env.Result, &resp); errUnmarshal != nil {
		t.Fatalf("failed to decode identifier response: %v", errUnmarshal)
	}
	if resp.Identifier != "cursor" {
		t.Errorf("identifier = %q, want %q", resp.Identifier, "cursor")
	}
}

func TestHandleMethod_ExecutorIdentifier_ReturnsStableIdentifier(t *testing.T) {
	raw, err := HandleMethod(pluginabi.MethodExecutorIdentifier, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	env := decodeEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("expected ok envelope, got error: %+v", env.Error)
	}
	var resp identifierResponse
	if errUnmarshal := json.Unmarshal(env.Result, &resp); errUnmarshal != nil {
		t.Fatalf("failed to decode identifier response: %v", errUnmarshal)
	}
	if resp.Identifier != "cursor" {
		t.Errorf("identifier = %q, want %q", resp.Identifier, "cursor")
	}
}
