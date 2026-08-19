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

// mcpToolsProviderIdentifier is the provider_identifier value set on
// every McpToolDefinition this plugin builds from a client-supplied
// tools[] array, matching gajae-code's own convention
// (providerIdentifier: "pi-agent" in buildMcpToolDefinitions) of
// stamping a stable, host-identifying string rather than leaving it
// empty.
const mcpToolsProviderIdentifier = "cliproxy-cursor-plugin"

// nativeToolsOnlyMcpSystemPrompt is appended as the last root-prompt-JSON
// system entry (see buildRootPromptMessageBlobIDs) whenever the client
// declares at least one custom tool via tools[]. This plugin already
// declines every Cursor-native tool-execution request via the
// ExecServerMessage handshake (execmessage.go, fact-r5-tool-roundtrip:
// never execute tools in-plugin), but Cursor's model itself doesn't know
// that ahead of time and may still choose to invoke a native tool
// (shell/read/glob/...) when one looks like a better fit than a declared
// MCP tool - the resulting decline is functionally safe (no execution
// happens) but wastes a full round trip and confuses the model's own
// plan. Explicitly instructing the model up front to prefer/only use the
// declared MCP tool set avoids that wasted round trip.
const nativeToolsOnlyMcpSystemPrompt = "Every tool you call is executed by the user's client, not by you. You may use your built-in file, search and shell tools - those calls are forwarded to the user's client for execution and their results are returned to you. Tools listed explicitly for you cover capabilities the client provides beyond those. Do not attempt screen recording, image generation, or computer-use tools; those are unavailable and will be declined."

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
func buildAgentRunRequest(req chatCompletionsRequest, blobs *blobStore, conversationID string, base *gen.ConversationStateStructure) (*gen.AgentRunRequest, error) {
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("cursor: at least one message is required")
	}

	// Split the conversation into "history" and "the action to run now".
	//
	// The action is NOT simply the last user message. In an agent loop the
	// client's final message is a TOOL RESULT, e.g.
	//
	//	[system, user, assistant(tool_calls), tool(result)]
	//
	// Selecting the last *user* message there would re-issue the original
	// question as the action and place everything after it outside history,
	// silently discarding the assistant's tool call AND its result. The
	// model would then never see the result and would request the same tool
	// again, forever: a live session on 2026-08-19 ran 23 turns over 9
	// minutes on "read go.mod and tell me the module name" without ever
	// converging, for exactly this reason.
	//
	// Cursor's protocol expresses "continue, I have given you the tool
	// results" as a ResumeAction, which is what gajae-code uses whenever
	// there is no new user content (streamCursor: userMessageAction when
	// user text/images exist, otherwise resumeAction). So:
	//
	//   - Last message is new user content -> UserMessageAction with it,
	//     history is everything before it.
	//   - Otherwise (tool result or assistant continuation) -> ResumeAction,
	//     and history includes EVERY message so the tool call and its
	//     result actually reach the model.
	lastIdx := len(req.Messages) - 1
	lastMsg := req.Messages[lastIdx]

	var (
		action  *gen.ConversationAction
		history []chatMessage
	)

	if lastMsg.Role == "user" && lastMsg.Content != "" {
		messageID, errID := newMessageID()
		if errID != nil {
			return nil, errID
		}
		action = &gen.ConversationAction{
			Action: &gen.ConversationAction_UserMessageAction{
				UserMessageAction: &gen.UserMessageAction{
					UserMessage: &gen.UserMessage{
						Text:      lastMsg.Content,
						MessageId: messageID,
					},
				},
			},
		}
		history = req.Messages[:lastIdx]
	} else {
		action = &gen.ConversationAction{
			Action: &gen.ConversationAction_ResumeAction{
				ResumeAction: &gen.ResumeAction{},
			},
		}
		history = req.Messages
	}

	turns, err := buildHistoryTurnBlobIDs(history, blobs)
	if err != nil {
		return nil, fmt.Errorf("cursor: failed to encode conversation history turns: %w", err)
	}

	rootPromptIDs, err := buildRootPromptMessageBlobIDs(history, req.Tools, blobs)
	if err != nil {
		return nil, fmt.Errorf("cursor: failed to encode root prompt messages: %w", err)
	}

	mcpTools, err := buildMcpTools(req.Tools)
	if err != nil {
		return nil, fmt.Errorf("cursor: failed to encode client tool definitions: %w", err)
	}

	// Build on the last state Cursor acknowledged for this conversation
	// when we have one, so non-history state it accumulated (todos,
	// pending tool calls, file state, summaries) survives instead of being
	// reset every turn; only the history fields are replaced with our own
	// encoding. Without a checkpoint this starts from an empty state, as
	// before.
	state := &gen.ConversationStateStructure{}
	if base != nil {
		if cloned, ok := proto.Clone(base).(*gen.ConversationStateStructure); ok {
			state = cloned
		}
	}
	state.Turns = turns
	state.RootPromptMessagesJson = rootPromptIDs

	runReq := &gen.AgentRunRequest{
		ConversationState: state,
		Action:            action,
		ModelDetails:      &gen.ModelDetails{ModelId: req.Model},
		McpTools:          mcpTools,
	}
	if conversationID != "" {
		runReq.ConversationId = &conversationID
	}
	return runReq, nil
}

// buildMcpTools translates a client-supplied OpenAI-style tools[] array
// into Cursor's AgentRunRequest.mcp_tools (McpTools{ []*McpToolDefinition
// }), which is the real field Cursor's model reads to learn about
// tools/functions the CALLER wants it able to invoke - distinct from
// Cursor's own native tools (shell/read/glob/...), which are always
// available to the model independent of what is declared here. Each
// function's JSON Schema `parameters` is re-encoded as a serialized
// google.protobuf.Value (input_schema []byte), matching gajae-code's own
// buildMcpToolDefinitions (toBinary(ValueSchema, fromJson(ValueSchema,
// schemaValue))) - Cursor's wire contract expects the schema as a
// well-known-type Value blob, not raw JSON text. Returns nil (no
// McpTools field set) when the client declared no tools, which is the
// common case and must not send an empty-but-present McpTools that could
// otherwise confuse Cursor's own tool-availability signaling.
func buildMcpTools(tools []chatToolDefinition) (*gen.McpTools, error) {
	if len(tools) == 0 {
		return nil, nil
	}

	defs := make([]*gen.McpToolDefinition, 0, len(tools))
	for _, t := range tools {
		if t.Function.Name == "" {
			continue
		}

		// Deduplicate against Cursor's own native tools: if the client
		// declared something Cursor already has natively (Read/Bash/Glob/
		// Grep/...), declaring it AGAIN as an MCP tool gives the model two
		// competing ways to do one thing and bloats the prompt. Rely on
		// the native tool instead - translate_response.go maps that call
		// back onto this client tool name (see toolmap.go), so the client
		// still receives a call under its own name either way.
		if _, hasNative := resolveCursorToolField(t.Function.Name); hasNative {
			continue
		}

		schemaValue, err := toolParametersToStructValue(t.Function.Parameters)
		if err != nil {
			return nil, fmt.Errorf("cursor: failed to parse tool %q parameters: %w", t.Function.Name, err)
		}
		inputSchema, err := proto.Marshal(schemaValue)
		if err != nil {
			return nil, fmt.Errorf("cursor: failed to marshal tool %q input schema: %w", t.Function.Name, err)
		}

		defs = append(defs, &gen.McpToolDefinition{
			Name:               t.Function.Name,
			Description:        t.Function.Description,
			ProviderIdentifier: mcpToolsProviderIdentifier,
			ToolName:           t.Function.Name,
			InputSchema:        inputSchema,
		})
	}

	if len(defs) == 0 {
		return nil, nil
	}
	return &gen.McpTools{McpTools: defs}, nil
}

// toolParametersToStructValue parses a client-supplied JSON Schema
// (tools[].function.parameters) into a google.protobuf.Value, falling
// back to an empty object schema when absent - matching gajae-code's own
// default ({ type: "object", properties: {}, required: [] }) so a tool
// with no declared parameters still round-trips as a valid, empty
// object schema rather than a missing/null value Cursor might reject.
func toolParametersToStructValue(parameters json.RawMessage) (*structpb.Value, error) {
	if len(parameters) == 0 {
		return structpb.NewValue(map[string]any{
			"type":       "object",
			"properties": map[string]any{},
			"required":   []any{},
		})
	}

	var decoded any
	if err := json.Unmarshal(parameters, &decoded); err != nil {
		return nil, err
	}
	return structpb.NewValue(decoded)
}

// buildHistoryTurnBlobIDs encodes history into ConversationState.Turns as
// blob IDs (not serialized bytes directly - Cursor resolves each turns[]
// entry, and each turn's user_message/steps sub-entries, through the
// KvServerMessage getBlobArgs handshake during the live exchange).
//
// Messages are GROUPED into turns the way gajae-code's
// buildConversationTurns does it: one turn per user message, with every
// assistant reply and tool result that follows it recorded as that turn's
// steps. An earlier version emitted one turn per MESSAGE, so each
// assistant reply and each tool result became its own turn - roughly 3-4x
// the turn count Cursor expects in an agent loop, growing every iteration.
// That inflated the blob set re-resolved on each turn and matched the
// escalating per-turn latency measured live on 2026-08-19 (24s -> 60s over
// 8 turns, while an identical large FIRST-turn request took only 4s).
//
// Note this is Cursor's UI-side history view;
// buildRootPromptMessageBlobIDs below is what actually reaches the model.
func buildHistoryTurnBlobIDs(messages []chatMessage, blobs *blobStore) ([][]byte, error) {
	var turns [][]byte
	pendingToolCallsByID := map[string]chatToolCall{}

	opensTurn := func(role string) bool { return role == "user" || role == "system" }

	for i := 0; i < len(messages); {
		msg := messages[i]
		if !opensTurn(msg.Role) {
			// A stray assistant/tool message with no preceding user
			// message cannot open a turn; skip it rather than fabricating
			// an empty user turn for it.
			i++
			continue
		}

		text := msg.Content
		if msg.Role == "system" {
			text = "[system] " + msg.Content
		}
		if text == "" {
			i++
			continue
		}

		userMsgBytes, err := proto.Marshal(&gen.UserMessage{Text: text})
		if err != nil {
			return nil, fmt.Errorf("cursor: failed to marshal user message: %w", err)
		}
		userBlobID := blobs.put(userMsgBytes)
		i++

		// Collect this turn's steps: everything up to the next message
		// that opens a turn.
		var stepBlobIDs [][]byte
		for i < len(messages) && !opensTurn(messages[i].Role) {
			step := messages[i]
			i++

			switch step.Role {
			case "assistant":
				if step.Content != "" {
					stepBytes, errStep := proto.Marshal(&gen.ConversationStep{
						Message: &gen.ConversationStep_AssistantMessage{
							AssistantMessage: &gen.AssistantMessage{Text: step.Content},
						},
					})
					if errStep != nil {
						return nil, errStep
					}
					stepBlobIDs = append(stepBlobIDs, blobs.put(stepBytes))
				}
				for _, tc := range step.ToolCalls {
					pendingToolCallsByID[tc.ID] = tc
					toolCallMsg, errTool := chatToolCallToToolCall(tc)
					if errTool != nil {
						return nil, errTool
					}
					stepBytes, errStep := proto.Marshal(&gen.ConversationStep{
						Message: &gen.ConversationStep_ToolCall{ToolCall: toolCallMsg},
					})
					if errStep != nil {
						return nil, errStep
					}
					stepBlobIDs = append(stepBlobIDs, blobs.put(stepBytes))
				}

			case "tool":
				original, known := pendingToolCallsByID[step.ToolCallID]
				if !known {
					// No originating call in this window: record the result
					// as assistant text so it is not lost, matching
					// gajae-code's own "[Tool Result]" step handling.
					stepBytes, errStep := proto.Marshal(&gen.ConversationStep{
						Message: &gen.ConversationStep_AssistantMessage{
							AssistantMessage: &gen.AssistantMessage{Text: "[Tool Result]\n" + step.Content},
						},
					})
					if errStep != nil {
						return nil, errStep
					}
					stepBlobIDs = append(stepBlobIDs, blobs.put(stepBytes))
					continue
				}
				toolCallWithResult, errTool := chatToolCallToToolCallWithResult(original, step.Content)
				if errTool != nil {
					return nil, errTool
				}
				stepBytes, errStep := proto.Marshal(&gen.ConversationStep{
					Message: &gen.ConversationStep_ToolCall{ToolCall: toolCallWithResult},
				})
				if errStep != nil {
					return nil, errStep
				}
				stepBlobIDs = append(stepBlobIDs, blobs.put(stepBytes))
				delete(pendingToolCallsByID, step.ToolCallID)
			}
		}

		turnBlobID, err := turnStructureBlobID(&gen.AgentConversationTurnStructure{
			UserMessage: userBlobID,
			Steps:       stepBlobIDs,
		}, blobs)
		if err != nil {
			return nil, err
		}
		turns = append(turns, turnBlobID)
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
//
// When tools is non-empty (the client declared its own custom tool
// set), nativeToolsOnlyMcpSystemPrompt is appended as a final system
// entry - UNLESS a system message already present in messages already
// contains that exact instruction (a client that manages its own system
// prompt and already includes the same guidance should not get it
// duplicated).
func buildRootPromptMessageBlobIDs(messages []chatMessage, tools []chatToolDefinition, blobs *blobStore) ([][]byte, error) {
	var ids [][]byte
	alreadyHasNativeToolsInstruction := false

	for _, msg := range messages {
		var entry rootPromptEntry
		switch msg.Role {
		case "system":
			if msg.Content == "" {
				continue
			}
			if strings.Contains(msg.Content, nativeToolsOnlyMcpSystemPrompt) {
				alreadyHasNativeToolsInstruction = true
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

	if len(tools) > 0 && !alreadyHasNativeToolsInstruction {
		entry := rootPromptEntry{Role: "user", Content: []rootPromptContentPart{{Type: "text", Text: "[System]\n" + nativeToolsOnlyMcpSystemPrompt}}}
		raw, err := json.Marshal(entry)
		if err != nil {
			return nil, fmt.Errorf("cursor: failed to marshal native-tools-only system entry: %w", err)
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

	mappedFromClientTool := false
	fieldDesc := oneofDesc.Fields().ByName(protoreflect.Name(fieldName))
	if fieldDesc == nil {
		// The client may be returning a result for a NATIVE Cursor tool
		// that was surfaced under the client's own equivalent tool name
		// (see toolmap.go / addCapturedToolRequests). Map it back to the
		// Cursor variant it came from before falling through to the
		// declared-tool (mcp) path, so the result lands on the right
		// tool instead of inventing an MCP tool Cursor never offered.
		if nativeField, mapped := resolveCursorToolField(fieldName); mapped {
			if nativeDesc := oneofDesc.Fields().ByName(protoreflect.Name(nativeField)); nativeDesc != nil && nativeDesc.Message() != nil {
				fieldDesc = nativeDesc
				mappedFromClientTool = true
			}
		}
	}
	if fieldDesc == nil {
		// Not one of Cursor's own native tool variants, so this is a tool
		// the CLIENT declared via tools[] (see buildMcpTools): Cursor
		// invokes those through the generic mcp_tool_call wrapper, and
		// translate_response.go surfaces them to the client under their
		// own declared name (McpArgs.Name). Round-tripping a result for
		// such a tool therefore has to rebuild that same wrapper rather
		// than fail - a live run (2026-08-19) showed this path erroring
		// with "unknown tool call variant" for every client-declared
		// tool, which broke the tool-result half of the round trip.
		return buildMcpToolCallFromNameAndArgs(fieldName, argsJSON, resultContent)
	}
	if fieldDesc.Message() == nil {
		return nil, fmt.Errorf("cursor: tool call variant %q is not a message field", fieldName)
	}

	concreteMsg := reflectMsg.NewField(fieldDesc).Message()
	if argsJSON != "" {
		if err := applyToolCallArgs(concreteMsg, argsJSON, mappedFromClientTool); err != nil {
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

// buildMcpToolCallFromNameAndArgs rebuilds Cursor's generic
// mcp_tool_call wrapper for a tool the CLIENT declared via tools[]. The
// declared tool name goes back into McpArgs.Name (mirroring how
// translate_response.go's toChatToolCall surfaced it), the JSON arguments
// object is split back into McpArgs.Args's per-parameter raw-JSON map,
// and any client-supplied tool result is written through the same generic
// result-field walker used for Cursor's native tools.
func buildMcpToolCallFromNameAndArgs(toolName, argsJSON, resultContent string) (*gen.ToolCall, error) {
	mcpArgs := &gen.McpArgs{Name: toolName}

	if strings.TrimSpace(argsJSON) != "" {
		var decoded map[string]json.RawMessage
		if err := json.Unmarshal([]byte(argsJSON), &decoded); err != nil {
			return nil, fmt.Errorf("cursor: failed to decode arguments for client tool %q: %w", toolName, err)
		}
		if len(decoded) > 0 {
			mcpArgs.Args = make(map[string][]byte, len(decoded))
			for key, value := range decoded {
				// Symmetric with mcpArgsToJSON's decode: Cursor expects
				// each argument value as a serialized
				// google.protobuf.Value, not raw JSON bytes.
				structValue, errValue := toolParametersToStructValue(json.RawMessage(value))
				if errValue != nil {
					return nil, fmt.Errorf("cursor: failed to encode argument %q for client tool %q: %w", key, toolName, errValue)
				}
				encoded, errMarshal := proto.Marshal(structValue)
				if errMarshal != nil {
					return nil, fmt.Errorf("cursor: failed to marshal argument %q for client tool %q: %w", key, toolName, errMarshal)
				}
				mcpArgs.Args[key] = encoded
			}
		}
	}

	mcpToolCall := &gen.McpToolCall{Args: mcpArgs}
	if resultContent != "" {
		if err := setGenericResultField(mcpToolCall.ProtoReflect(), resultContent, 0); err != nil {
			return nil, fmt.Errorf("cursor: failed to set tool result for client tool %q: %w", toolName, err)
		}
	}

	return &gen.ToolCall{
		Tool: &gen.ToolCall_McpToolCall{McpToolCall: mcpToolCall},
	}, nil
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

func newMessageID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// applyToolCallArgs writes client-supplied arguments onto a Cursor tool
// call message.
//
// Two things make this deliberately forgiving:
//
//   - When the name was mapped from a CLIENT tool onto a Cursor native
//     variant (see toolmap.go), the client's arguments describe the tool's
//     parameters directly (e.g. {"path":"go.mod"}), whereas Cursor nests
//     them one level down in an `args` message. Applying them at the top
//     level fails, so they are written into `args` when that field exists.
//   - DiscardUnknown is set. The mapping is a heuristic on tool NAMES, so a
//     client's parameter names need not match Cursor's. An unrecognized
//     parameter must not fail the whole request: a live run on 2026-08-19
//     showed such a failure returning 500, which made CLIProxyAPI fuse the
//     plugin and every later request fail with auth_unavailable. Dropping
//     the unknown parameter degrades one argument; failing the request
//     took down the provider.
func applyToolCallArgs(concreteMsg protoreflect.Message, argsJSON string, mappedFromClientTool bool) error {
	opts := protojson.UnmarshalOptions{DiscardUnknown: true}

	if mappedFromClientTool {
		if argsField := concreteMsg.Descriptor().Fields().ByName("args"); argsField != nil && argsField.Message() != nil {
			sub := concreteMsg.NewField(argsField).Message()
			if err := opts.Unmarshal([]byte(argsJSON), sub.Interface()); err != nil {
				// Shapes did not line up; leave args unset rather than
				// failing the turn. The tool identity still round-trips.
				return nil
			}
			concreteMsg.Set(argsField, protoreflect.ValueOfMessage(sub))
			return nil
		}
	}

	if err := opts.Unmarshal([]byte(argsJSON), concreteMsg.Interface()); err != nil {
		if mappedFromClientTool {
			return nil
		}
		return err
	}
	return nil
}
