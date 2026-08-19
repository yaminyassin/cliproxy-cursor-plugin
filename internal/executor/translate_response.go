package executor

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"

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
func (a *responseAccumulator) toChatCompletionsResponse(model, responseID string) chatCompletionsResponse {
	finishReason := "stop"
	if len(a.toolCalls) > 0 {
		finishReason = "tool_calls"
	}
	if a.err != nil {
		return chatCompletionsResponse{
			ID:     responseID,
			Object: "chat.completion",
			Model:  model,
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
					Content:   a.text.String(),
					ToolCalls: a.toolCalls,
				},
				FinishReason: finishReason,
			},
		},
	}
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

	fieldValue := reflectMsg.Get(whichField)
	if !fieldValue.Message().IsValid() {
		return nil, fmt.Errorf("cursor: tool call field %s has no message value", whichField.Name())
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
	generated, errID := newMessageID()
	if errID != nil {
		return nil, errID
	}
	callID := "call_" + generated

	return &chatToolCall{
		ID:   callID,
		Type: "function",
		Function: chatToolCallFunc{
			Name:      string(whichField.Name()),
			Arguments: string(argsJSON),
		},
	}, nil
}

func newResponseID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "cursor-" + hex.EncodeToString(b), nil
}
