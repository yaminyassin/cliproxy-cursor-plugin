package executor

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/router-for-me/cliproxy-cursor-plugin/internal/cursorpb/gen"
)

// buildAgentRunRequest translates a chat-completions request into a
// Cursor AgentRunRequest. The last user message becomes the turn's
// UserMessageAction text (the action Cursor is being asked to run next);
// every prior message (system/assistant/tool history) is encoded into
// ConversationState.Turns as serialized AgentConversationTurnStructure
// blobs, per fact-r5-tool-roundtrip: a client-supplied tool-role message
// is re-encoded back into the matching Cursor ToolCall variant's Result
// field (found generically via reflection, mirroring the extraction
// side in translate_response.go) so a multi-turn tool-using conversation
// round-trips correctly, not just a single isolated turn.
func buildAgentRunRequest(req chatCompletionsRequest) (*gen.AgentRunRequest, error) {
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("cursor: at least one message is required")
	}

	lastUserIdx := lastUserMessageIndex(req.Messages)
	if lastUserIdx == -1 {
		return nil, fmt.Errorf("cursor: no user message found in request")
	}

	messageID, err := newMessageID()
	if err != nil {
		return nil, err
	}

	userMessage := &gen.UserMessage{
		Text:      req.Messages[lastUserIdx].Content,
		MessageId: messageID,
	}

	action := &gen.ConversationAction{
		Action: &gen.ConversationAction_UserMessageAction{
			UserMessageAction: &gen.UserMessageAction{
				UserMessage: userMessage,
			},
		},
	}

	turns, err := buildHistoryTurns(req.Messages[:lastUserIdx])
	if err != nil {
		return nil, fmt.Errorf("cursor: failed to encode conversation history: %w", err)
	}

	return &gen.AgentRunRequest{
		ConversationState: &gen.ConversationStateStructure{Turns: turns},
		Action:             action,
		ModelDetails:       &gen.ModelDetails{ModelId: req.Model},
	}, nil
}

// buildHistoryTurns encodes every message before the current turn's user
// message into serialized ConversationTurnStructure blobs.
// ConversationStateStructure.Turns is a [][]byte field: Cursor's wire
// contract stores turns as opaque serialized blobs, and
// AgentConversationTurnStructure itself stores its UserMessage/Steps as
// serialized []byte / [][]byte sub-blobs rather than typed submessages
// (see the generated field tags in internal/cursorpb/gen/agent.pb.go).
//
// Tool-role messages are matched to the immediately preceding
// assistant tool_calls entry by tool_call_id/id, and re-encoded into the
// same Cursor ToolCall variant (found by name from the surfaced
// tool_calls[].function.name, e.g. "shell_tool_call") with its generic
// Result field populated from the client's tool-result content. Roles
// this v1 does not have a Cursor-side representation for (system) are
// folded into a synthetic user-authored turn rather than dropped
// silently - see the inline comment below.
func buildHistoryTurns(messages []chatMessage) ([][]byte, error) {
	var turns [][]byte

	// Track the most recent assistant tool_calls by id, so a subsequent
	// tool-role message can be matched back to the ToolCall variant name
	// Execute originally surfaced (see translate_response.go's
	// toChatToolCall, which puts the oneof field name in Function.Name).
	pendingToolCallsByID := map[string]chatToolCall{}

	for _, msg := range messages {
		switch msg.Role {
		case "system":
			if msg.Content == "" {
				continue
			}
			turn, err := marshalUserTurn("[system] " + msg.Content)
			if err != nil {
				return nil, err
			}
			turns = append(turns, turn)

		case "user":
			turn, err := marshalUserTurn(msg.Content)
			if err != nil {
				return nil, err
			}
			turns = append(turns, turn)

		case "assistant":
			steps := make([]*gen.ConversationStep, 0, 1+len(msg.ToolCalls))
			if msg.Content != "" {
				steps = append(steps, &gen.ConversationStep{
					Message: &gen.ConversationStep_AssistantMessage{
						AssistantMessage: &gen.AssistantMessage{Text: msg.Content},
					},
				})
			}
			for _, tc := range msg.ToolCalls {
				pendingToolCallsByID[tc.ID] = tc
				toolCallMsg, err := chatToolCallToToolCall(tc)
				if err != nil {
					return nil, err
				}
				steps = append(steps, &gen.ConversationStep{
					Message: &gen.ConversationStep_ToolCall{ToolCall: toolCallMsg},
				})
			}
			turn, err := marshalStepsTurn(steps)
			if err != nil {
				return nil, err
			}
			turns = append(turns, turn)

		case "tool":
			original, ok := pendingToolCallsByID[msg.ToolCallID]
			if !ok {
				// No matching prior tool_calls entry to attach this
				// result to; fold it into a plain user-visible turn
				// rather than silently dropping the result content.
				turn, err := marshalUserTurn("[tool result] " + msg.Content)
				if err != nil {
					return nil, err
				}
				turns = append(turns, turn)
				continue
			}
			toolCallWithResult, err := chatToolCallToToolCallWithResult(original, msg.Content)
			if err != nil {
				return nil, err
			}
			turn, err := marshalStepsTurn([]*gen.ConversationStep{
				{Message: &gen.ConversationStep_ToolCall{ToolCall: toolCallWithResult}},
			})
			if err != nil {
				return nil, err
			}
			turns = append(turns, turn)
			delete(pendingToolCallsByID, msg.ToolCallID)
		}
	}

	return turns, nil
}

// marshalUserTurn wraps plain text as a user-message-only conversation
// turn.
func marshalUserTurn(text string) ([]byte, error) {
	userMsgBytes, err := proto.Marshal(&gen.UserMessage{Text: text})
	if err != nil {
		return nil, fmt.Errorf("cursor: failed to marshal user message: %w", err)
	}
	return marshalTurnStructure(&gen.AgentConversationTurnStructure{UserMessage: userMsgBytes})
}

// marshalStepsTurn wraps a set of conversation steps (assistant text
// and/or tool calls) as a conversation turn.
func marshalStepsTurn(steps []*gen.ConversationStep) ([]byte, error) {
	stepBytes := make([][]byte, 0, len(steps))
	for _, step := range steps {
		raw, err := proto.Marshal(step)
		if err != nil {
			return nil, fmt.Errorf("cursor: failed to marshal conversation step: %w", err)
		}
		stepBytes = append(stepBytes, raw)
	}
	return marshalTurnStructure(&gen.AgentConversationTurnStructure{Steps: stepBytes})
}

func marshalTurnStructure(turn *gen.AgentConversationTurnStructure) ([]byte, error) {
	wrapped := &gen.ConversationTurnStructure{
		Turn: &gen.ConversationTurnStructure_AgentConversationTurn{AgentConversationTurn: turn},
	}
	raw, err := proto.Marshal(wrapped)
	if err != nil {
		return nil, fmt.Errorf("cursor: failed to marshal conversation turn: %w", err)
	}
	return raw, nil
}

// chatToolCallToToolCall reconstructs a Cursor ToolCall from a
// chat-completions tool_calls entry (as surfaced by toChatToolCall in
// translate_response.go): Function.Name is the oneof field name (e.g.
// "shell_tool_call") and Function.Arguments is the protojson-marshaled
// concrete tool message, both generically round-tripped via
// protoreflect/protojson rather than a hand-written switch per tool type.
func chatToolCallToToolCall(tc chatToolCall) (*gen.ToolCall, error) {
	return buildToolCallFromNameAndArgs(tc.Function.Name, tc.Function.Arguments, "")
}

// chatToolCallToToolCallWithResult is the same reconstruction, plus
// setting the concrete tool message's generic "result" field (found by
// reflection, since every concrete tool type - ShellToolCall,
// ReadToolCall, etc. - has its own typed Result field) from the client's
// tool-role message content.
func chatToolCallToToolCallWithResult(tc chatToolCall, resultContent string) (*gen.ToolCall, error) {
	return buildToolCallFromNameAndArgs(tc.Function.Name, tc.Function.Arguments, resultContent)
}

func buildToolCallFromNameAndArgs(fieldName, argsJSON, resultContent string) (*gen.ToolCall, error) {
	toolCall := &gen.ToolCall{}
	reflectMsg := toolCall.ProtoReflect()
	oneofDesc := reflectMsg.Descriptor().Oneofs().ByName("tool")
	if oneofDesc == nil {
		return nil, fmt.Errorf("cursor: ToolCall descriptor missing expected 'tool' oneof")
	}

	fieldDesc := oneofDesc.Fields().ByName(protoreflect.Name(fieldName))
	if fieldDesc == nil {
		return nil, fmt.Errorf("cursor: unknown tool call variant %q", fieldName)
	}
	if fieldDesc.Message() == nil {
		return nil, fmt.Errorf("cursor: tool call variant %q is not a message field", fieldName)
	}

	concreteMsg := reflectMsg.NewField(fieldDesc).Message()
	if argsJSON != "" {
		if err := protojson.Unmarshal([]byte(argsJSON), concreteMsg.Interface()); err != nil {
			return nil, fmt.Errorf("cursor: failed to unmarshal tool call arguments for %s: %w", fieldName, err)
		}
	}

	if resultContent != "" {
		if err := setGenericResultField(concreteMsg, resultContent); err != nil {
			return nil, fmt.Errorf("cursor: failed to set tool result for %s: %w", fieldName, err)
		}
	}

	reflectMsg.Set(fieldDesc, protoreflect.ValueOfMessage(concreteMsg))
	return toolCall, nil
}

// setGenericResultField finds the concrete tool message's "result" field
// (e.g. ShellToolCall.Result *ShellResult) via reflection and populates
// it from resultContent. Every concrete Cursor tool type has its own
// typed *XxxResult message with a "success"/text-shaped field; rather
// than hand-writing all 30, this sets whichever singular string-ish
// field exists first on the result message (matching gajae-code's own
// pattern of representing tool results primarily as text - see
// toolResultToText in providers/cursor.ts). If no result field exists on
// this tool type, the result content is dropped from the wire but the
// tool_call step itself is still recorded, which is a documented v1
// scope limitation, not a silent failure of the surrounding call.
func setGenericResultField(concreteMsg protoreflect.Message, resultContent string) error {
	fields := concreteMsg.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		field := fields.Get(i)
		if string(field.Name()) != "result" {
			continue
		}
		if field.Message() == nil {
			continue
		}
		resultMsg := concreteMsg.NewField(field).Message()
		if err := setFirstStringField(resultMsg, resultContent); err != nil {
			return err
		}
		concreteMsg.Set(field, protoreflect.ValueOfMessage(resultMsg))
		return nil
	}
	return nil
}

func setFirstStringField(msg protoreflect.Message, value string) error {
	fields := msg.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		field := fields.Get(i)
		if field.Kind() == protoreflect.StringKind {
			msg.Set(field, protoreflect.ValueOfString(value))
			return nil
		}
	}
	return nil
}

func lastUserMessageIndex(messages []chatMessage) int {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return i
		}
	}
	return -1
}

func newMessageID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
