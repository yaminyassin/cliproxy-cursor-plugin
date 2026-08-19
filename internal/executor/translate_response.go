package executor

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/router-for-me/cliproxy-cursor-plugin/internal/cursorpb/gen"
)

// responseAccumulator collects one Cursor Run response's InteractionUpdate
// stream into a single chat-completions response, per the per-turn
// tool-call batching decision (Architect Finding 3 / ralplan plan
// Section 5): Cursor tool-call oneofs are accumulated across the turn
// and flushed as ONE tool_calls array, not one entry per event, with
// full-argument buffering for v1 (no partial-argument streaming deltas).
type responseAccumulator struct {
	text      strings.Builder
	toolCalls []chatToolCall
	err       error
	// completionTokens accumulates Cursor's own TokenDelta counts so the
	// response can report a real usage block (see chatUsage: clients
	// reject all-zero usage as an anomalous/failed response).
	completionTokens int
}

// accumulate folds one InteractionUpdate into the accumulator. Unknown or
// non-chat-relevant update kinds (diagnostics, MCP resource listings,
// step/summary bookkeeping, etc.) are safely ignored, not translated, per
// the plan's explicit scope boundary and Acceptance Criteria.
func (a *responseAccumulator) accumulate(update *gen.InteractionUpdate) {
	if update == nil {
		return
	}
	switch msg := update.GetMessage().(type) {
	case *gen.InteractionUpdate_TextDelta:
		if msg.TextDelta != nil {
			a.text.WriteString(msg.TextDelta.GetText())
		}
	case *gen.InteractionUpdate_ToolCallCompleted:
		if msg.ToolCallCompleted != nil {
			tc, err := toChatToolCall(msg.ToolCallCompleted.GetToolCall())
			if err != nil {
				a.err = fmt.Errorf("cursor: failed to translate tool call: %w", err)
				return
			}
			if tc != nil {
				a.toolCalls = append(a.toolCalls, *tc)
			}
		}
	case *gen.InteractionUpdate_TokenDelta:
		// Cursor's own output-token accounting for this turn; folded into
		// the response's usage block rather than ignored, because real
		// OpenAI-compatible clients treat all-zero usage as a failed
		// response.
		if msg.TokenDelta != nil {
			if tokens := int(msg.TokenDelta.GetTokens()); tokens > 0 {
				a.completionTokens += tokens
			}
		}
	// InteractionUpdate_PartialToolCall / _ToolCallDelta / _ToolCallStarted:
	// intermediate tool-call construction events. v1 buffers the full
	// tool call from ToolCallCompleted rather than streaming partial
	// arguments (documented deferred scope, not a silent gap).
	//
	// InteractionUpdate_ThinkingDelta / _ThinkingCompleted / _Summary /
	// _SummaryStarted / _SummaryCompleted / _ShellOutputDelta /
	// _Heartbeat / _StepStarted / _StepCompleted / _UserMessageAppended /
	// _TokenDelta / _TurnEnded: non-chat-relevant agent bookkeeping,
	// safely ignored per the plan's Acceptance Criteria ("Diagnostics and
	// other non-chat agent events are received and safely ignored, not
	// translated").
	default:
		// Explicitly does nothing; documented above.
	}
}

// toChatCompletionsResponse renders the accumulated turn into a
// non-streaming chat-completions response for the given model/request id.
// promptText is the request text used only to estimate prompt tokens
// (Cursor reports no input-token count of its own).
func (a *responseAccumulator) toChatCompletionsResponse(model, responseID, promptText string) chatCompletionsResponse {
	finishReason := "stop"
	if len(a.toolCalls) > 0 {
		finishReason = "tool_calls"
	}

	content := a.text.String()
	usage := a.usage(promptText, content)

	if a.err != nil {
		return chatCompletionsResponse{
			ID:     responseID,
			Object: "chat.completion",
			Model:  model,
			Usage:  usage,
			Error:  &chatErrorInfo{Message: a.err.Error(), Type: "cursor_translation_error"},
		}
	}
	return chatCompletionsResponse{
		ID:     responseID,
		Object: "chat.completion",
		Model:  model,
		Choices: []chatChoice{
			{
				Index: 0,
				Message: chatMessage{
					Role:      "assistant",
					Content:   content,
					ToolCalls: a.toolCalls,
				},
				FinishReason: finishReason,
			},
		},
		Usage: usage,
	}
}

// usage builds the response's usage block. Completion tokens prefer
// Cursor's own TokenDelta accounting and fall back to an estimate from
// the produced text (including tool-call arguments) when Cursor sent
// none; prompt tokens are always estimated, since Cursor's agent.v1
// protocol reports no input-token count. Estimates are explicitly
// approximations, but a real approximation is far more useful to clients
// than a bare zero, which they reject outright as an anomalous response.
func (a *responseAccumulator) usage(promptText, content string) chatUsage {
	completion := a.completionTokens
	if completion == 0 {
		billable := content
		for _, tc := range a.toolCalls {
			billable += tc.Function.Name + tc.Function.Arguments
		}
		completion = estimateTokens(billable)
	}
	prompt := estimateTokens(promptText)
	return chatUsage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      prompt + completion,
	}
}

// estimateTokens approximates a token count from text length using the
// widely-used ~4-characters-per-token heuristic, returning at least 1 for
// any non-empty text so a real response never reports zero tokens.
func estimateTokens(text string) int {
	if text == "" {
		return 0
	}
	if n := len(text) / 4; n > 0 {
		return n
	}
	return 1
}

// toChatToolCall generically extracts whichever oneof variant is set on a
// Cursor ToolCall (one of 30+ concrete tool types: ShellToolCall,
// ReadToolCall, GrepToolCall, ...) using protoreflect, instead of a
// hand-written switch over every tool type. The oneof field's proto name
// becomes the surfaced tool name (e.g. "shell_tool_call"), and the
// concrete tool message is protojson-marshaled as the function arguments
// - this is a faithful, generic representation of "whichever tool Cursor
// asked for" without needing per-tool-type Go structs on the
// chat-completions side.
func toChatToolCall(toolCall *gen.ToolCall) (*chatToolCall, error) {
	if toolCall == nil {
		return nil, nil
	}

	reflectMsg := toolCall.ProtoReflect()
	oneofDesc := reflectMsg.Descriptor().Oneofs().ByName("tool")
	if oneofDesc == nil {
		return nil, fmt.Errorf("cursor: ToolCall descriptor missing expected 'tool' oneof")
	}
	whichField := reflectMsg.WhichOneof(oneofDesc)
	if whichField == nil {
		return nil, fmt.Errorf("cursor: tool call has no populated variant")
	}

	// SPECIAL CASE: mcp_tool_call is Cursor's generic wrapper for
	// invoking a tool the CLIENT declared via tools[] (see
	// translate_request.go's buildMcpTools). Surfacing it under Cursor's
	// internal oneof name ("mcp_tool_call") is useless to the client,
	// which can only dispatch tools it actually declared - a live run
	// (2026-08-19) showed a real client receive a tool call, fail to
	// match it to any declared tool, and produce an empty response. The
	// client-facing name must therefore be McpArgs.Name (e.g. "read"),
	// with the arguments rebuilt from McpArgs.Args.
	if mcpCall, isMCP := toolCall.GetTool().(*gen.ToolCall_McpToolCall); isMCP {
		mcpArgs := mcpCall.McpToolCall.GetArgs()
		name := mcpArgs.GetName()
		if name == "" {
			// The declined echo Cursor emits after we refuse to execute a
			// client tool inline: it carries only the rejection result,
			// with no tool name or arguments. The real invocation was
			// already captured from the ExecServerMessage exec request
			// (see runStreamResult.toolRequests) and is surfaced from
			// there, so emitting this stub would only add an
			// undispatchable "mcp_tool_call" entry alongside it.
			return nil, nil
		}
		if name != "" {
			argsJSON, errArgs := mcpArgsToJSON(mcpArgs.GetArgs())
			if errArgs != nil {
				return nil, fmt.Errorf("cursor: failed to render mcp tool args for %s: %w", name, errArgs)
			}
			callID, errID := newToolCallID()
			if errID != nil {
				return nil, errID
			}
			return &chatToolCall{
				ID:       callID,
				Type:     "function",
				Function: chatToolCallFunc{Name: name, Arguments: argsJSON},
			}, nil
		}
	}

	fieldValue := reflectMsg.Get(whichField)
	if !fieldValue.Message().IsValid() {
		return nil, fmt.Errorf("cursor: tool call field %s has no message value", whichField.Name())
	}

	// Skip completion/decline ECHOES. After this plugin declines a native
	// tool inline (fact-r5-tool-roundtrip), Cursor emits a
	// ToolCallCompleted carrying only the rejection `result` and no
	// `args`. Surfacing that as a tool_calls entry produced exactly the
	// undispatchable "read_tool_call"/"glob_tool_call" entries a real
	// client rejected on 2026-08-19, with a rejection payload sitting
	// where the arguments belong. The actionable request was already
	// captured from the exec channel and is surfaced from there
	// (runStreamResult.toolRequests), so an args-less echo is pure noise.
	if isToolCallCompletionEcho(fieldValue.Message()) {
		return nil, nil
	}

	toolMessage := fieldValue.Message().Interface()
	argsJSON, err := protojson.MarshalOptions{EmitUnpopulated: false}.Marshal(toolMessage)
	if err != nil {
		return nil, fmt.Errorf("cursor: failed to marshal tool call args for %s: %w", whichField.Name(), err)
	}

	// The chat-completions-facing tool_calls[].id is ALWAYS a
	// freshly-generated clean opaque token (call_<hex>), never Cursor's
	// own call_id passed through verbatim. A real live E2E run
	// (2026-08-19) observed Cursor sending a call_id containing an
	// embedded newline plus a second concatenated internal id (e.g.
	// "call_xyz\nfc_0e03a4cd..."), which is not a clean OpenAI-compatible
	// token. This plugin never sends the client-facing id back to
	// Cursor (see translate_request.go's chatToolCallToToolCall*, which
	// reconstructs the Cursor ToolCall from Function.Name/Arguments
	// only, not from ID), so generating a fresh id independent of
	// Cursor's raw value is always safe and keeps the OpenAI-compatible
	// surface contract (a clean, whitespace-free opaque string)
	// regardless of what Cursor's backend sends internally.
	callID, errID := newToolCallID()
	if errID != nil {
		return nil, errID
	}

	return &chatToolCall{
		ID:   callID,
		Type: "function",
		Function: chatToolCallFunc{
			Name:      string(whichField.Name()),
			Arguments: string(argsJSON),
		},
	}, nil
}

// newToolCallID generates the chat-completions-facing tool_calls[].id.
// It is ALWAYS a freshly-generated clean opaque token (call_<hex>), never
// Cursor's own call_id passed through verbatim: a real live E2E run
// (2026-08-19) observed Cursor sending a call_id containing an embedded
// newline plus a second concatenated internal id (e.g.
// "call_xyz\nfc_0e03a4cd..."), which is not a clean OpenAI-compatible
// token. This plugin never sends the client-facing id back to Cursor
// (translate_request.go reconstructs the Cursor ToolCall from
// Function.Name/Arguments only), so generating a fresh id is always safe.
func newToolCallID() (string, error) {
	generated, err := newMessageID()
	if err != nil {
		return "", err
	}
	return "call_" + generated, nil
}

// mcpArgsToJSON renders Cursor's McpArgs.Args (a map of parameter name to
// raw JSON-encoded bytes) into a single JSON object string suitable for
// chat-completions tool_calls[].function.arguments. A value that is not
// already valid JSON is encoded as a JSON string rather than dropped or
// emitted raw, so a malformed upstream value can never produce invalid
// JSON on the client-facing surface.
func mcpArgsToJSON(args map[string][]byte) (string, error) {
	if len(args) == 0 {
		return "{}", nil
	}
	obj := make(map[string]json.RawMessage, len(args))
	for key, raw := range args {
		// Cursor encodes each argument value as a serialized
		// google.protobuf.Value (the same well-known-type encoding this
		// plugin uses for McpToolDefinition.input_schema). Decoding it as
		// such is what strips the protobuf framing: treating the bytes as
		// text instead leaks wire bytes into the JSON a client receives
		// (observed live on 2026-08-19 as {"city":"\u001a\u0005Seoul"}).
		var value structpb.Value
		if err := proto.Unmarshal(raw, &value); err == nil && value.GetKind() != nil {
			if encoded, errJSON := protojson.Marshal(&value); errJSON == nil {
				obj[key] = json.RawMessage(encoded)
				continue
			}
		}
		if json.Valid(raw) {
			obj[key] = json.RawMessage(raw)
			continue
		}
		encoded, err := json.Marshal(string(raw))
		if err != nil {
			return "", err
		}
		obj[key] = json.RawMessage(encoded)
	}
	rendered, err := json.Marshal(obj)
	if err != nil {
		return "", err
	}
	return string(rendered), nil
}

func newResponseID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "cursor-" + hex.EncodeToString(b), nil
}

// addCapturedToolRequests folds tool invocations captured from Cursor's
// inline exec channel into the accumulated tool_calls array.
//
// Two kinds arrive here:
//
//   - Client-declared tools (mcp_args): Cursor echoes the client's own
//     tool name and per-parameter values, so they pass through exactly.
//   - Cursor NATIVE tools (read/glob/shell/...): surfaced only when the
//     client declared an equivalent tool, remapped onto that declared
//     name via the toolmap heuristic. Cursor's model reaches for its
//     native tools even when the client declared its own set, and a live
//     run on 2026-08-19 showed a real client rejecting every one of them
//     ("Tool read_tool_call not found"). An unmatched native tool is
//     dropped here and remains declined upstream, so this can only turn
//     otherwise-dead calls into dispatchable ones.
func (a *responseAccumulator) addCapturedToolRequests(requests []capturedToolRequest, tools *clientToolIndex) error {
	for _, req := range requests {
		name := req.Name
		argsJSON := ""

		switch {
		case name != "":
			// Client-declared tool: exact name and schema.
			rendered, err := mcpArgsToJSON(req.Args)
			if err != nil {
				return fmt.Errorf("cursor: failed to render arguments for tool %q: %w", name, err)
			}
			argsJSON = rendered
		default:
			// Native Cursor tool: only surface it if the client declared
			// something equivalent to dispatch it with.
			mapped, ok := tools.resolveClientTool(req.Field)
			if !ok {
				continue
			}
			name = mapped
			argsJSON = req.ArgsJSON
			if argsJSON == "" {
				argsJSON = "{}"
			}
		}

		callID, errID := newToolCallID()
		if errID != nil {
			return errID
		}
		a.toolCalls = append(a.toolCalls, chatToolCall{
			ID:       callID,
			Type:     "function",
			Function: chatToolCallFunc{Name: name, Arguments: argsJSON},
		})
	}
	return nil
}

// isToolCallCompletionEcho reports whether a Cursor ToolCall variant is a
// completion/decline echo rather than an actionable request: its schema
// declares an `args` field but only `result` is populated.
func isToolCallCompletionEcho(msg protoreflect.Message) bool {
	fields := msg.Descriptor().Fields()
	argsField := fields.ByName("args")
	if argsField == nil {
		return false
	}
	if msg.Has(argsField) {
		return false
	}
	if resultField := fields.ByName("result"); resultField != nil && msg.Has(resultField) {
		return true
	}
	return false
}
