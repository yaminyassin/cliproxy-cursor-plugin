// Package dispatch implements the C ABI v1 JSON-envelope method dispatch
// table for the Cursor OAuth plugin, mirroring the shape CLIProxyAPI's own
// examples/plugin/simple/go/main.go uses for dynamic-library plugins: a
// local, JSON-serializable registration/capability struct (distinct from
// the SDK's interface-based pluginapi.Capabilities, which is for in-process
// Go plugin embedding, not the C ABI wire contract) plus pluginapi's plain
// data request/response types for each declared method.
package dispatch

import (
	"encoding/json"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const pluginIdentifier = "cursor"

// version is the plugin release version reported in Metadata and used to
// derive the semantic-versioning stance from the ralplan-approved plan:
// bumped on any capability surface or wire-contract change.
const version = "0.1.0"

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
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return okEnvelope(currentRegistration())

	case pluginabi.MethodAuthIdentifier:
		return okEnvelope(identifierResponse{Identifier: pluginIdentifier})
	case pluginabi.MethodAuthParse:
		return notImplementedEnvelope(pluginabi.MethodAuthParse), nil
	case pluginabi.MethodAuthLoginStart:
		return notImplementedEnvelope(pluginabi.MethodAuthLoginStart), nil
	case pluginabi.MethodAuthLoginPoll:
		return notImplementedEnvelope(pluginabi.MethodAuthLoginPoll), nil
	case pluginabi.MethodAuthRefresh:
		return notImplementedEnvelope(pluginabi.MethodAuthRefresh), nil

	case pluginabi.MethodModelRegister, pluginabi.MethodModelStatic, pluginabi.MethodModelForAuth:
		return okEnvelope(pluginapi.ModelResponse{Provider: pluginIdentifier, Models: nil})

	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(identifierResponse{Identifier: pluginIdentifier})
	case pluginabi.MethodExecutorExecute:
		return notImplementedEnvelope(pluginabi.MethodExecutorExecute), nil
	case pluginabi.MethodExecutorExecuteStream:
		return notImplementedEnvelope(pluginabi.MethodExecutorExecuteStream), nil
	case pluginabi.MethodExecutorCountTokens:
		return notImplementedEnvelope(pluginabi.MethodExecutorCountTokens), nil
	case pluginabi.MethodExecutorHTTPRequest:
		return notImplementedEnvelope(pluginabi.MethodExecutorHTTPRequest), nil

	default:
		return errorEnvelope("unknown_method", fmt.Sprintf("unknown method: %s", method)), nil
	}
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
// G003 (auth_provider) and G004 (executor/model_registrar) replace these
// with real capability implementations; this keeps the dispatch table
// (and its routing test) accurate about what is currently wired versus
// what is a placeholder, per the fail-loud principle: callers see an
// explicit typed error, never a silent empty success.
func notImplementedEnvelope(method string) []byte {
	return errorEnvelope("not_implemented", fmt.Sprintf("%s is not yet implemented (scheduled in a later story)", method))
}
