// Package dispatch implements the C ABI v1 JSON-envelope method dispatch
// table for the Cursor OAuth plugin, mirroring the shape CLIProxyAPI's own
// examples/plugin/simple/go/main.go uses for dynamic-library plugins: a
// local, JSON-serializable registration/capability struct (distinct from
// the SDK's interface-based pluginapi.Capabilities, which is for in-process
// Go plugin embedding, not the C ABI wire contract) plus pluginapi's plain
// data request/response types for each declared method.
package dispatch

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/router-for-me/cliproxy-cursor-plugin/internal/account"
	"github.com/router-for-me/cliproxy-cursor-plugin/internal/auth"
	"github.com/router-for-me/cliproxy-cursor-plugin/internal/discovery"
	"github.com/router-for-me/cliproxy-cursor-plugin/internal/executor"
)

const pluginIdentifier = "cursor"

// version is the plugin release version reported in Metadata and used to
// derive the semantic-versioning stance from the ralplan-approved plan:
// bumped on any capability surface or wire-contract change.
const version = "0.1.0"

// accountStore, authProvider, agentClient, executorImpl, and discoverer
// hold the plugin's process-lifetime state. A single dynamic-library
// plugin instance serves one C ABI dispatch table for its whole lifetime
// (per cliproxy_plugin_init), so package-level state here mirrors that:
// exactly one Cursor auth provider, executor, and account store per
// loaded plugin instance, all sharing the same account.Store per the
// account-state ownership design.
var (
	accountStore = account.NewStore()
	authProvider = auth.NewProvider(accountStore, nil)
	agentClient  = executor.NewAgentClient(accountStore, nil, "", "")
	executorImpl = executor.NewExecutor(agentClient)
	discoverer   = discovery.NewDiscoverer(agentClient.Service, accountStore)
)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// registration is the JSON-serializable capability declaration crossing
// the C ABI boundary at plugin.register / plugin.reconfigure. It mirrors
// examples/plugin/simple/go/main.go's local registrationCapability shape.
type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	AuthProvider          bool     `json:"auth_provider"`
	ModelRegistrar        bool     `json:"model_registrar"`
	ModelProvider         bool     `json:"model_provider"`
	Executor              bool     `json:"executor"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string `json:"executor_output_formats,omitempty"`
}

type identifierResponse struct {
	Identifier string `json:"identifier"`
}

// HandleMethod routes one C-ABI method call by name to its typed handler
// and returns the marshaled envelope response. It is the single dispatch
// table entry point called from cmd/plugin's cgo boundary.
func HandleMethod(method string, request []byte) ([]byte, error) {
	ctx := context.Background()

	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return okEnvelope(currentRegistration())

	case pluginabi.MethodAuthIdentifier:
		return okEnvelope(identifierResponse{Identifier: authProvider.Identifier()})
	case pluginabi.MethodAuthParse:
		return handleAuthParse(ctx, request)
	case pluginabi.MethodAuthLoginStart:
		return handleAuthLoginStart(ctx, request)
	case pluginabi.MethodAuthLoginPoll:
		return handleAuthLoginPoll(ctx, request)
	case pluginabi.MethodAuthRefresh:
		return handleAuthRefresh(ctx, request)

	case pluginabi.MethodModelRegister:
		return okEnvelope(pluginapi.ModelRegistrationResponse{Provider: pluginIdentifier, Models: nil})
	case pluginabi.MethodModelStatic:
		return handleModelStatic(ctx, request)
	case pluginabi.MethodModelForAuth:
		return handleModelForAuth(ctx, request)

	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(identifierResponse{Identifier: executorImpl.Identifier()})
	case pluginabi.MethodExecutorExecute:
		return handleExecutorExecute(ctx, request)
	case pluginabi.MethodExecutorExecuteStream:
		return handleExecutorExecuteStream(ctx, request)
	case pluginabi.MethodExecutorCountTokens:
		return handleExecutorCountTokens(ctx, request)
	case pluginabi.MethodExecutorHTTPRequest:
		return notImplementedEnvelope(pluginabi.MethodExecutorHTTPRequest), nil

	default:
		return errorEnvelope("unknown_method", fmt.Sprintf("unknown method: %s", method)), nil
	}
}

func handleAuthParse(ctx context.Context, request []byte) ([]byte, error) {
	var req pluginapi.AuthParseRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return errorEnvelope("invalid_request", fmt.Sprintf("failed to decode auth.parse request: %v", err)), nil
	}
	resp, err := authProvider.ParseAuth(ctx, req)
	if err != nil {
		return errorEnvelope("auth_parse_failed", err.Error()), nil
	}
	return okEnvelope(resp)
}

func handleAuthLoginStart(ctx context.Context, request []byte) ([]byte, error) {
	var req pluginapi.AuthLoginStartRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return errorEnvelope("invalid_request", fmt.Sprintf("failed to decode auth.login.start request: %v", err)), nil
	}
	resp, err := authProvider.StartLogin(ctx, req)
	if err != nil {
		return errorEnvelope("auth_login_start_failed", err.Error()), nil
	}
	return okEnvelope(resp)
}

func handleAuthLoginPoll(ctx context.Context, request []byte) ([]byte, error) {
	var req pluginapi.AuthLoginPollRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return errorEnvelope("invalid_request", fmt.Sprintf("failed to decode auth.login.poll request: %v", err)), nil
	}
	resp, err := authProvider.PollLogin(ctx, req)
	if err != nil {
		return errorEnvelope("auth_login_poll_failed", err.Error()), nil
	}
	return okEnvelope(resp)
}

func handleAuthRefresh(ctx context.Context, request []byte) ([]byte, error) {
	var req pluginapi.AuthRefreshRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return errorEnvelope("invalid_request", fmt.Sprintf("failed to decode auth.refresh request: %v", err)), nil
	}
	resp, err := authProvider.RefreshAuth(ctx, req)
	if err != nil {
		return errorEnvelope("auth_refresh_failed", err.Error()), nil
	}
	return okEnvelope(resp)
}

func handleModelStatic(ctx context.Context, request []byte) ([]byte, error) {
	var req pluginapi.StaticModelRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return errorEnvelope("invalid_request", fmt.Sprintf("failed to decode model.static request: %v", err)), nil
	}
	resp, err := discoverer.StaticModels(ctx, req)
	if err != nil {
		return errorEnvelope("model_static_failed", err.Error()), nil
	}
	return okEnvelope(resp)
}

func handleModelForAuth(ctx context.Context, request []byte) ([]byte, error) {
	var req pluginapi.AuthModelRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return errorEnvelope("invalid_request", fmt.Sprintf("failed to decode model.for_auth request: %v", err)), nil
	}
	resp, err := discoverer.ModelsForAuth(ctx, req)
	if err != nil {
		return errorEnvelope("model_for_auth_failed", err.Error()), nil
	}
	return okEnvelope(resp)
}

func handleExecutorExecute(ctx context.Context, request []byte) ([]byte, error) {
	var req pluginapi.ExecutorRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return errorEnvelope("invalid_request", fmt.Sprintf("failed to decode executor.execute request: %v", err)), nil
	}
	resp, err := executorImpl.Execute(ctx, req)
	if err != nil {
		return errorEnvelope("executor_execute_failed", err.Error()), nil
	}
	return okEnvelope(resp)
}

func handleExecutorExecuteStream(ctx context.Context, request []byte) ([]byte, error) {
	var req pluginapi.ExecutorRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return errorEnvelope("invalid_request", fmt.Sprintf("failed to decode executor.execute_stream request: %v", err)), nil
	}
	streamResp, err := executorImpl.ExecuteStream(ctx, req)
	if err != nil {
		return errorEnvelope("executor_execute_stream_failed", err.Error()), nil
	}

	// Drain the channel into a JSON-serializable envelope response; the
	// C ABI's executor.execute_stream method returns the full chunk set
	// in one envelope (matching examples/plugin/simple/go/main.go's
	// streamResponse{Chunks: []ExecutorStreamChunk} shape), not a
	// separate host-native streaming transport.
	type chunkPayload struct {
		Payload []byte `json:"payload,omitempty"`
		Err     string `json:"err,omitempty"`
	}
	var chunks []chunkPayload
	for chunk := range streamResp.Chunks {
		cp := chunkPayload{Payload: chunk.Payload}
		if chunk.Err != nil {
			cp.Err = chunk.Err.Error()
		}
		chunks = append(chunks, cp)
	}
	return okEnvelope(struct {
		Chunks []chunkPayload `json:"chunks,omitempty"`
	}{Chunks: chunks})
}

func handleExecutorCountTokens(ctx context.Context, request []byte) ([]byte, error) {
	var req pluginapi.ExecutorRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return errorEnvelope("invalid_request", fmt.Sprintf("failed to decode executor.count_tokens request: %v", err)), nil
	}
	resp, err := executorImpl.CountTokens(ctx, req)
	if err != nil {
		return errorEnvelope("executor_count_tokens_failed", err.Error()), nil
	}
	return okEnvelope(resp)
}

// currentRegistration builds the plugin.register / plugin.reconfigure
// payload. ABI version is pinned to pluginabi.ABIVersion at build time
// (not hand-copied), per the ralplan-approved plan's versioning stance.
func currentRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "cursor",
			Version:          version,
			Author:           "router-for-me",
			GitHubRepository: "https://github.com/router-for-me/cliproxy-cursor-plugin",
			ConfigFields: []pluginapi.ConfigField{
				{
					Name:        "x_cursor_client_version",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Overrides the x-cursor-client-version header sent on every Cursor backend request. Defaults to the plugin's compiled-in value.",
				},
			},
		},
		Capabilities: registrationCapability{
			AuthProvider:          true,
			ModelRegistrar:        true,
			ModelProvider:         true,
			Executor:              true,
			ExecutorModelScope:    string(pluginapi.ExecutorModelScopeBoth),
			ExecutorInputFormats:  []string{"chat-completions"},
			ExecutorOutputFormats: []string{"chat-completions"},
		},
	}
}

func okEnvelope(v any) ([]byte, error) {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

// notImplementedEnvelope marks a declared-but-not-yet-implemented method.
// executor.http_request has no v1 implementation (see
// internal/executor.Executor.HttpRequest); this keeps the dispatch table
// (and its routing test) accurate about what is currently wired versus
// what is a placeholder, per the fail-loud principle: callers see an
// explicit typed error, never a silent empty success.
func notImplementedEnvelope(method string) []byte {
	return errorEnvelope("not_implemented", fmt.Sprintf("%s is not yet implemented (scheduled in a later story)", method))
}
