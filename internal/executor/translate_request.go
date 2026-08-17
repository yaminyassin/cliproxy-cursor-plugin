package executor

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/router-for-me/cliproxy-cursor-plugin/internal/cursorpb/gen"
)

// buildAgentRunRequest translates a chat-completions request into a
// Cursor AgentRunRequest. The last user message becomes the turn's
// UserMessageAction text (the action Cursor is being asked to run next).
//
// Prior messages are encoded TWICE, matching gajae-code's own
// buildGrpcRequest (packages/ai/src/providers/cursor.ts:2659-2667):
//   - ConversationState.Turns: a blob-ID-per-entry UI-history view
//     (buildConversationTurns equivalent).
//   - ConversationState.RootPromptMessagesJson: blob IDs pointing to
//     plain JSON {role, content} objects, which is what Cursor's server
//     actually uses to build the model prompt (buildRootPromptMessagesJson
//     equivalent). Turns alone are NOT sufficient for the model to see
//     conversation history - this was a real defect found in boundary
//     review and verified against the reference implementation.
//
// Every blob referenced by ID here must be pre-registered in the shared
// blobStore (via blobs.put) BEFORE the Run request is sent, so an
// eventual server-initiated getBlobArgs for that ID can be answered
// immediately without a round trip - see stream.go's handleKvServerMessage.
func buildAgentRunRequest(req chatCompletionsRequest, blobs *blobStore) (*gen.AgentRunRequest, error) {
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

	history := req.Messages[:lastUserIdx]

	turns, err := buildHistoryTurnBlobIDs(history, blobs)
	if err != nil {
		return nil, fmt.Errorf("cursor: failed to encode conversation history turns: %w", err)
	}

	rootPromptIDs, err := buildRootPromptMessageBlobIDs(history, blobs)
	if err != nil {
		return nil, fmt.Errorf("cursor: failed to encode root prompt messages: %w", err)
	}

	return &gen.AgentRunRequest{
		ConversationState: &gen.ConversationStateStructure{
			Turns:                  turns,
			RootPromptMessagesJson: rootPromptIDs,
		},
		Action:       action,
		ModelDetails: &gen.ModelDetails{ModelId: req.Model},
	}, nil
}

// buildHistoryTurnBlobIDs encodes every history message into
// ConversationState.Turns entries as blob IDs (not the serialized bytes
// directly - Cursor resolves each turns[] entry, and each turn's
// user_message/steps sub-entries, through the KvServerMessage
// getBlobArgs handshake during the live exchange). This mirrors
// gajae-code's buildConversationTurns and is the UI-side history view;
// buildRootPromptMessageBlobIDs below is what actually reaches the model.
func buildHistoryTurnBlobIDs(messages []chatMessage, blobs *blobStore) ([][]byte, error) {
	var turns [][]byte
	pendingToolCallsByID := map[string]chatToolCall{}

	for _, msg := range messages {
		switch msg.Role {
		case "system":
			if msg.Content == "" {
				continue
			}
			turn, err := userTurnBlobID("[system] "+msg.Content, blobs)
			if err != nil {
				return nil, err
			}
			turns = append(turns, turn)

		case "user":
			turn, err := userTurnBlobID(msg.Content, blobs)
			if err != nil {
				return nil, err
			}
			turns = append(turns, turn)

		case "assistant":
			var stepBlobIDs [][]byte
			if msg.Content != "" {
				stepBytes, err := proto.Marshal(&gen.ConversationStep{
					Message: &gen.ConversationStep_AssistantMessage{
						AssistantMessage: &gen.AssistantMessage{Text: msg.Content},
					},
				})
				if err != nil {
					return nil, err
				}
				stepBlobIDs = append(stepBlobIDs, blobs.put(stepBytes))
			}
			for _, tc := range msg.ToolCalls {
				pendingToolCallsByID[tc.ID] = tc
				toolCallMsg, err := chatToolCallToToolCall(tc)
				if err != nil {
					return nil, err
				}
				stepBytes, err := proto.Marshal(&gen.ConversationStep{
					Message: &gen.ConversationStep_ToolCall{ToolCall: toolCallMsg},
				})
				if err != nil {
					return nil, err
				}
				stepBlobIDs = append(stepBlobIDs, blobs.put(stepBytes))
			}
			turn, err := stepsTurnBlobID(stepBlobIDs, blobs)
			if err != nil {
				return nil, err
			}
			turns = append(turns, turn)

		case "tool":
			original, ok := pendingToolCallsByID[msg.ToolCallID]
			if !ok {
				turn, err := userTurnBlobID("[tool result] "+msg.Content, blobs)
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
			stepBytes, err := proto.Marshal(&gen.ConversationStep{
				Message: &gen.ConversationStep_ToolCall{ToolCall: toolCallWithResult},
			})
			if err != nil {
				return nil, err
			}
			turn, err := stepsTurnBlobID([][]byte{blobs.put(stepBytes)}, blobs)
			if err != nil {
				return nil, err
			}
			turns = append(turns, turn)
			delete(pendingToolCallsByID, msg.ToolCallID)
		}
	}

	return turns, nil
}

func userTurnBlobID(text string, blobs *blobStore) ([]byte, error) {
	userMsgBytes, err := proto.Marshal(&gen.UserMessage{Text: text})
	if err != nil {
		return nil, fmt.Errorf("cursor: failed to marshal user message: %w", err)
	}
	userMsgBlobID := blobs.put(userMsgBytes)
	return turnStructureBlobID(&gen.AgentConversationTurnStructure{UserMessage: userMsgBlobID}, blobs)
}

func stepsTurnBlobID(stepBlobIDs [][]byte, blobs *blobStore) ([]byte, error) {
	return turnStructureBlobID(&gen.AgentConversationTurnStructure{Steps: stepBlobIDs}, blobs)
}

func turnStructureBlobID(turn *gen.AgentConversationTurnStructure, blobs *blobStore) ([]byte, error) {
	wrapped := &gen.ConversationTurnStructure{
		Turn: &gen.ConversationTurnStructure_AgentConversationTurn{AgentConversationTurn: turn},
	}
	raw, err := proto.Marshal(wrapped)
	if err != nil {
		return nil, fmt.Errorf("cursor: failed to marshal conversation turn: %w", err)
	}
	return blobs.put(raw), nil
}

// rootPromptEntry is the plain-JSON shape gajae-code's
// buildRootPromptMessagesJson pushes per history message (not protobuf -
// Cursor's server parses root_prompt_messages_json blobs as JSON).
type rootPromptEntry struct {
	Role    string                  `json:"role"`
	Content []rootPromptContentPart `json:"content"`
}

type rootPromptContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// buildRootPromptMessageBlobIDs encodes every history message as a
// JSON-blob entry in the field Cursor's server actually reads to build
// the model prompt (root_prompt_messages_json), ported from gajae-code's
// buildRootPromptMessagesJson. Tool-role messages are folded into a
// user-role "[Tool Result]" entry, matching the reference's own
// toolResultToText handling, since root_prompt_messages_json has no
// dedicated tool-result shape.
func buildRootPromptMessageBlobIDs(messages []chatMessage, blobs *blobStore) ([][]byte, error) {
	var ids [][]byte
	for _, msg := range messages {
		var entry rootPromptEntry
		switch msg.Role {
		case "system":
			if msg.Content == "" {
				continue
			}
			entry = rootPromptEntry{Role: "user", Content: []rootPromptContentPart{{Type: "text", Text: "[System]\n" + msg.Content}}}
		case "user":
			if msg.Content == "" {
				continue
			}
			entry = rootPromptEntry{Role: "user", Content: []rootPromptContentPart{{Type: "text", Text: msg.Content}}}
		case "assistant":
			if msg.Content == "" {
				continue
			}
			entry = rootPromptEntry{Role: "assistant", Content: []rootPromptContentPart{{Type: "text", Text: msg.Content}}}
		case "tool":
			if msg.Content == "" {
				continue
			}
			entry = rootPromptEntry{Role: "user", Content: []rootPromptContentPart{{Type: "text", Text: "[Tool Result]\n" + msg.Content}}}
		default:
			continue
		}

		raw, err := json.Marshal(entry)
		if err != nil {
			return nil, fmt.Errorf("cursor: failed to marshal root prompt entry: %w", err)
		}
		ids = append(ids, blobs.put(raw))
	}
	return ids, nil
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
// setting the concrete tool message's result field from the client's
// tool-role message content, via setGenericResultField.
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
		if err := setGenericResultField(concreteMsg, resultContent, 0); err != nil {
			return nil, fmt.Errorf("cursor: failed to set tool result for %s: %w", fieldName, err)
		}
	}

	reflectMsg.Set(fieldDesc, protoreflect.ValueOfMessage(concreteMsg))
	return toolCall, nil
}

// maxResultDescendDepth bounds the recursive oneof descent in
// setGenericResultField, so a pathological/cyclic schema can never cause
// unbounded recursion.
const maxResultDescendDepth = 4

// setGenericResultField finds the concrete tool message's "result" field
// (e.g. ShellToolCall.Result *ShellResult) via reflection and populates
// it with resultContent. Many Cursor result types (confirmed for
// ShellResult) are themselves a oneof of outcome variants (Success/
// Failure/Timeout/Rejected/...) with no top-level string field - the
// actual text lives one level deeper (e.g. ShellSuccess.Command,
// ShellSuccess.WorkingDirectory are typed fields, but the general pattern
// across Cursor's tool results is a nested oneof holding the descriptive
// message). This walks: result field -> if it has its own oneof, pick a
// "success"-named case when present (falling back to the first declared
// case otherwise) and descend into it -> set the first string field
// found at that level. This generically reaches the actual text payload
// instead of silently writing into an empty top-level wrapper, which was
// the terminal-critic-verified defect in the previous single-level
// implementation.
func setGenericResultField(concreteMsg protoreflect.Message, resultContent string, depth int) error {
	if depth >= maxResultDescendDepth {
		return nil
	}
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
		if err := populateResultMessage(resultMsg, resultContent, depth+1); err != nil {
			return err
		}
		concreteMsg.Set(field, protoreflect.ValueOfMessage(resultMsg))
		return nil
	}
	return nil
}

// populateResultMessage writes resultContent into the given message: if
// the message declares its own oneof (e.g. ShellResult's Success/
// Failure/... outcome oneof), it selects a case (preferring one named
// "success") and recurses into that case's concrete message; otherwise
// it sets the first plain string field found directly on this message.
func populateResultMessage(msg protoreflect.Message, resultContent string, depth int) error {
	if depth >= maxResultDescendDepth {
		return nil
	}

	oneofDesc := firstRealOneof(msg.Descriptor())
	if oneofDesc != nil {
		fields := oneofDesc.Fields()
		if fields.Len() == 0 {
			return nil
		}

		var chosen protoreflect.FieldDescriptor
		for i := 0; i < fields.Len(); i++ {
			f := fields.Get(i)
			if string(f.Name()) == "success" {
				chosen = f
				break
			}
		}
		if chosen == nil {
			chosen = fields.Get(0)
		}
		if chosen.Message() == nil {
			return nil
		}

		caseMsg := msg.NewField(chosen).Message()
		if err := populateResultMessage(caseMsg, resultContent, depth+1); err != nil {
			return err
		}
		msg.Set(chosen, protoreflect.ValueOfMessage(caseMsg))
		return nil
	}

	return setFirstStringField(msg, resultContent)
}

// firstRealOneof returns the first genuinely-declared oneof on a message
// descriptor (e.g. ShellResult's Success/Failure/Timeout/Rejected/...
// outcome oneof), skipping proto3 "optional" fields' synthetic
// single-field oneofs. Proto3 represents every `optional` scalar field
// as its own one-field oneof under the hood; naively picking
// Oneofs().Get(0) can land on one of those synthetic oneofs (e.g.
// ShellResult.sandbox_policy) instead of the real outcome oneof,
// silently mis-selecting a field with no content - this was the root
// cause of a failing round-trip test caught during rework.
func firstRealOneof(desc protoreflect.MessageDescriptor) protoreflect.OneofDescriptor {
	oneofs := desc.Oneofs()
	for i := 0; i < oneofs.Len(); i++ {
		od := oneofs.Get(i)
		if !od.IsSynthetic() {
			return od
		}
	}
	return nil
}

// preferredResultFieldNames lists field names checked in priority order
// before falling back to "the first string field declared". Cursor's
// concrete result types (ShellSuccess, ReadLintsToolResult, etc.) often
// declare an identifying field (e.g. ShellSuccess.Command) before the
// actual content field (ShellSuccess.Stdout); a naive "first string
// field" pick lands on the identifying field, not the content, which was
// part of the terminal-critic-verified silent-drop defect. Checking
// common content-bearing names first fixes the confirmed ShellResult
// case and generalizes reasonably to the other tool result types without
// hand-writing all 30.
var preferredResultFieldNames = []string{"stdout", "output", "content", "text", "result", "message", "body"}

func setFirstStringField(msg protoreflect.Message, value string) error {
	fields := msg.Descriptor().Fields()

	for _, preferred := range preferredResultFieldNames {
		for i := 0; i < fields.Len(); i++ {
			field := fields.Get(i)
			if field.Kind() == protoreflect.StringKind && string(field.Name()) == preferred {
				msg.Set(field, protoreflect.ValueOfString(value))
				return nil
			}
		}
	}

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
