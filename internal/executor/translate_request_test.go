package executor

import (
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/router-for-me/cliproxy-cursor-plugin/internal/cursorpb/gen"
)

// TestBuildAgentRunRequest_EncodesConversationHistoryAsBlobIDs verifies
// that prior turns are encoded as content-addressed blob IDs (resolved
// through the blobStore, matching Cursor's real wire contract - see
// stream.go's KV handshake), not as directly-embedded serialized bytes.
// This addresses the terminal-critic-verified finding that the previous
// implementation embedded bytes directly, which does not match
// ../gajae-code/packages/ai/src/providers/cursor.ts's
// storeCursorBlob/buildConversationTurns (turns[] entries are
// sha256(bytes), and the bytes themselves are only ever transmitted in
// response to a server-initiated getBlobArgs request).
func TestBuildAgentRunRequest_EncodesConversationHistoryAsBlobIDs(t *testing.T) {
	blobs := newBlobStore()
	req := chatCompletionsRequest{
		Model: "cursor-fast",
		Messages: []chatMessage{
			{Role: "user", Content: "first question"},
			{Role: "assistant", Content: "first answer"},
			{Role: "user", Content: "second question"},
		},
	}

	agentReq, err := buildAgentRunRequest(req, blobs)
	if err != nil {
		t.Fatalf("buildAgentRunRequest failed: %v", err)
	}

	if agentReq.GetAction().GetUserMessageAction().GetUserMessage().GetText() != "second question" {
		t.Errorf("current turn text = %q, want %q", agentReq.GetAction().GetUserMessageAction().GetUserMessage().GetText(), "second question")
	}

	turnIDs := agentReq.GetConversationState().GetTurns()
	if len(turnIDs) != 2 {
		t.Fatalf("expected 2 history turn blob ids (first user + first assistant), got %d", len(turnIDs))
	}

	// Each turns[] entry must be a blob ID resolvable through the shared
	// blobStore, not the serialized turn bytes themselves.
	turn0Bytes, ok := blobs.get(turnIDs[0])
	if !ok {
		t.Fatalf("turn 0 blob id not found in blobStore")
	}
	var turn0 gen.ConversationTurnStructure
	if err := proto.Unmarshal(turn0Bytes, &turn0); err != nil {
		t.Fatalf("failed to unmarshal turn 0 blob: %v", err)
	}
	agentTurn0 := turn0.GetAgentConversationTurn()
	if agentTurn0 == nil {
		t.Fatalf("turn 0 is not an AgentConversationTurn")
	}

	// The turn's own user_message field is ALSO a blob id, one level
	// deeper.
	userMsgBytes, ok := blobs.get(agentTurn0.GetUserMessage())
	if !ok {
		t.Fatalf("turn 0 user_message blob id not found in blobStore")
	}
	var userMsg0 gen.UserMessage
	if err := proto.Unmarshal(userMsgBytes, &userMsg0); err != nil {
		t.Fatalf("failed to unmarshal turn 0 user message blob: %v", err)
	}
	if userMsg0.GetText() != "first question" {
		t.Errorf("turn 0 user message = %q, want %q", userMsg0.GetText(), "first question")
	}

	turn1Bytes, ok := blobs.get(turnIDs[1])
	if !ok {
		t.Fatalf("turn 1 blob id not found in blobStore")
	}
	var turn1 gen.ConversationTurnStructure
	if err := proto.Unmarshal(turn1Bytes, &turn1); err != nil {
		t.Fatalf("failed to unmarshal turn 1 blob: %v", err)
	}
	agentTurn1 := turn1.GetAgentConversationTurn()
	if agentTurn1 == nil || len(agentTurn1.GetSteps()) != 1 {
		t.Fatalf("expected turn 1 to have 1 step blob id, got %+v", agentTurn1)
	}
	stepBytes, ok := blobs.get(agentTurn1.GetSteps()[0])
	if !ok {
		t.Fatalf("turn 1 step blob id not found in blobStore")
	}
	var step1 gen.ConversationStep
	if err := proto.Unmarshal(stepBytes, &step1); err != nil {
		t.Fatalf("failed to unmarshal turn 1 step blob: %v", err)
	}
	if step1.GetAssistantMessage().GetText() != "first answer" {
		t.Errorf("turn 1 assistant message = %q, want %q", step1.GetAssistantMessage().GetText(), "first answer")
	}
}

// TestBuildAgentRunRequest_RootPromptMessagesJson verifies that
// RootPromptMessagesJson is populated (this is what Cursor's server
// actually reads to build the model prompt, per
// cursor.ts:2663-2667 - "Cursor's server uses this field (not turns[])
// to construct the actual model prompt"). The previous implementation
// never set this field at all, meaning conversation history never
// reached the model despite turns[] being populated.
func TestBuildAgentRunRequest_RootPromptMessagesJson(t *testing.T) {
	blobs := newBlobStore()
	req := chatCompletionsRequest{
		Model: "cursor-fast",
		Messages: []chatMessage{
			{Role: "user", Content: "first question"},
			{Role: "assistant", Content: "first answer"},
			{Role: "user", Content: "second question"},
		},
	}

	agentReq, err := buildAgentRunRequest(req, blobs)
	if err != nil {
		t.Fatalf("buildAgentRunRequest failed: %v", err)
	}

	rootPromptIDs := agentReq.GetConversationState().GetRootPromptMessagesJson()
	if len(rootPromptIDs) != 2 {
		t.Fatalf("expected 2 root prompt message blob ids (first user + first assistant), got %d", len(rootPromptIDs))
	}

	entry0Bytes, ok := blobs.get(rootPromptIDs[0])
	if !ok {
		t.Fatalf("root prompt entry 0 blob id not found in blobStore")
	}
	var entry0 rootPromptEntry
	if err := json.Unmarshal(entry0Bytes, &entry0); err != nil {
		t.Fatalf("root prompt entry 0 is not valid JSON: %v", err)
	}
	if entry0.Role != "user" || len(entry0.Content) != 1 || entry0.Content[0].Text != "first question" {
		t.Errorf("root prompt entry 0 = %+v, want role=user text=%q", entry0, "first question")
	}

	entry1Bytes, ok := blobs.get(rootPromptIDs[1])
	if !ok {
		t.Fatalf("root prompt entry 1 blob id not found in blobStore")
	}
	var entry1 rootPromptEntry
	if err := json.Unmarshal(entry1Bytes, &entry1); err != nil {
		t.Fatalf("root prompt entry 1 is not valid JSON: %v", err)
	}
	if entry1.Role != "assistant" || len(entry1.Content) != 1 || entry1.Content[0].Text != "first answer" {
		t.Errorf("root prompt entry 1 = %+v, want role=assistant text=%q", entry1, "first answer")
	}
}

// TestBuildAgentRunRequest_ToolResultRoundTrip verifies that a tool-role
// message is re-encoded into the matching Cursor ToolCall variant's
// result field, with the actual text content surviving at the correct
// nested oneof depth (ShellResult -> ShellResult_Success -> ShellSuccess
// -> Stdout), addressing the terminal-critic-verified finding that the
// previous single-level implementation silently dropped the content
// because ShellResult itself has no direct string field.
func TestBuildAgentRunRequest_ToolResultRoundTrip(t *testing.T) {
	blobs := newBlobStore()
	req := chatCompletionsRequest{
		Model: "cursor-fast",
		Messages: []chatMessage{
			{Role: "user", Content: "list files"},
			{
				Role: "assistant",
				ToolCalls: []chatToolCall{
					{
						ID:   "call-1",
						Type: "function",
						Function: chatToolCallFunc{
							Name:      "shell_tool_call",
							Arguments: `{"args":{"command":"ls -la"}}`,
						},
					},
				},
			},
			{Role: "tool", ToolCallID: "call-1", Content: "file1.txt\nfile2.txt"},
			{Role: "user", Content: "now read file1.txt"},
		},
	}

	agentReq, err := buildAgentRunRequest(req, blobs)
	if err != nil {
		t.Fatalf("buildAgentRunRequest failed: %v", err)
	}

	turnIDs := agentReq.GetConversationState().GetTurns()
	if len(turnIDs) != 3 {
		t.Fatalf("expected 3 history turns (user, assistant-with-tool-call, tool-result), got %d", len(turnIDs))
	}

	toolResultTurnBytes, ok := blobs.get(turnIDs[2])
	if !ok {
		t.Fatalf("tool-result turn blob id not found in blobStore")
	}
	var toolResultTurn gen.ConversationTurnStructure
	if err := proto.Unmarshal(toolResultTurnBytes, &toolResultTurn); err != nil {
		t.Fatalf("failed to unmarshal tool-result turn: %v", err)
	}
	agentTurn := toolResultTurn.GetAgentConversationTurn()
	if agentTurn == nil || len(agentTurn.GetSteps()) != 1 {
		t.Fatalf("expected the tool-result turn to have 1 step blob id, got %+v", agentTurn)
	}
	stepBytes, ok := blobs.get(agentTurn.GetSteps()[0])
	if !ok {
		t.Fatalf("tool-result step blob id not found in blobStore")
	}
	var step gen.ConversationStep
	if err := proto.Unmarshal(stepBytes, &step); err != nil {
		t.Fatalf("failed to unmarshal tool-result step: %v", err)
	}
	toolCall := step.GetToolCall()
	if toolCall == nil {
		t.Fatalf("expected tool-result step to carry a ToolCall")
	}
	shellCall := toolCall.GetShellToolCall()
	if shellCall == nil {
		t.Fatalf("expected the round-tripped tool call to be a ShellToolCall, got %+v", toolCall)
	}
	if shellCall.GetArgs().GetCommand() != "ls -la" {
		t.Errorf("round-tripped args.command = %q, want %q", shellCall.GetArgs().GetCommand(), "ls -la")
	}

	result := shellCall.GetResult()
	if result == nil {
		t.Fatalf("expected the round-tripped tool call to carry a Result, got none")
	}
	success := result.GetSuccess()
	if success == nil {
		t.Fatalf("expected Result to be the Success variant, got %+v", result)
	}
	if success.GetStdout() != "file1.txt\nfile2.txt" {
		t.Errorf("Result.Success.Stdout = %q, want %q (this is the actual content-bearing field; a shallow implementation would leave this empty)", success.GetStdout(), "file1.txt\nfile2.txt")
	}
}

// TestBuildAgentRunRequest_EncodesClientToolsAsMcpTools verifies that a
// client-supplied OpenAI-style tools[] array is translated into
// AgentRunRequest.McpTools (McpToolDefinition entries), which is the
// real field Cursor's model reads to learn about caller-provided
// tools/functions - distinct from Cursor's own native tools.
func TestBuildAgentRunRequest_EncodesClientToolsAsMcpTools(t *testing.T) {
	blobs := newBlobStore()
	req := chatCompletionsRequest{
		Model: "cursor-fast",
		Messages: []chatMessage{
			{Role: "user", Content: "search for something"},
		},
		Tools: []chatToolDefinition{
			{
				Type: "function",
				Function: chatToolDefinitionFunc{
					Name:        "custom_search",
					Description: "Searches a custom index",
					Parameters:  json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`),
				},
			},
		},
	}

	agentReq, err := buildAgentRunRequest(req, blobs)
	if err != nil {
		t.Fatalf("buildAgentRunRequest failed: %v", err)
	}

	mcpTools := agentReq.GetMcpTools()
	if mcpTools == nil {
		t.Fatalf("expected McpTools to be set, got nil")
	}
	defs := mcpTools.GetMcpTools()
	if len(defs) != 1 {
		t.Fatalf("expected 1 McpToolDefinition, got %d", len(defs))
	}

	def := defs[0]
	if def.GetName() != "custom_search" || def.GetToolName() != "custom_search" {
		t.Errorf("name/tool_name = %q/%q, want custom_search/custom_search", def.GetName(), def.GetToolName())
	}
	if def.GetDescription() != "Searches a custom index" {
		t.Errorf("description = %q, want %q", def.GetDescription(), "Searches a custom index")
	}
	if def.GetProviderIdentifier() == "" {
		t.Errorf("expected a non-empty provider_identifier")
	}
	if len(def.GetInputSchema()) == 0 {
		t.Fatalf("expected a non-empty input_schema")
	}

	var schemaValue structpb.Value
	if err := proto.Unmarshal(def.GetInputSchema(), &schemaValue); err != nil {
		t.Fatalf("input_schema is not a valid serialized google.protobuf.Value: %v", err)
	}
	schemaJSON := schemaValue.AsInterface()
	schemaMap, ok := schemaJSON.(map[string]any)
	if !ok {
		t.Fatalf("expected decoded schema to be a JSON object, got %T", schemaJSON)
	}
	if schemaMap["type"] != "object" {
		t.Errorf("decoded schema type = %v, want %q", schemaMap["type"], "object")
	}
}

// TestBuildAgentRunRequest_NoTools_NoMcpTools verifies that when the
// client declares no tools, McpTools is left nil rather than an
// empty-but-present message that could confuse Cursor's own
// tool-availability signaling.
func TestBuildAgentRunRequest_NoTools_NoMcpTools(t *testing.T) {
	blobs := newBlobStore()
	req := chatCompletionsRequest{
		Model:    "cursor-fast",
		Messages: []chatMessage{{Role: "user", Content: "hello"}},
	}

	agentReq, err := buildAgentRunRequest(req, blobs)
	if err != nil {
		t.Fatalf("buildAgentRunRequest failed: %v", err)
	}
	if agentReq.GetMcpTools() != nil {
		t.Errorf("expected McpTools to be nil when no tools are declared, got %+v", agentReq.GetMcpTools())
	}
}

// TestBuildAgentRunRequest_ToolsPresent_AppendsNativeToolsOnlySystemPrompt
// verifies that declaring custom tools causes the always-appendix system
// instruction (only call declared MCP tools, never native ones) to be
// appended to root_prompt_messages_json exactly once.
func TestBuildAgentRunRequest_ToolsPresent_AppendsNativeToolsOnlySystemPrompt(t *testing.T) {
	blobs := newBlobStore()
	req := chatCompletionsRequest{
		Model: "cursor-fast",
		Messages: []chatMessage{
			{Role: "user", Content: "do something"},
			{Role: "user", Content: "final question"},
		},
		Tools: []chatToolDefinition{
			{Type: "function", Function: chatToolDefinitionFunc{Name: "custom_tool"}},
		},
	}

	agentReq, err := buildAgentRunRequest(req, blobs)
	if err != nil {
		t.Fatalf("buildAgentRunRequest failed: %v", err)
	}

	rootPromptIDs := agentReq.GetConversationState().GetRootPromptMessagesJson()
	found := 0
	for _, id := range rootPromptIDs {
		raw, ok := blobs.get(id)
		if !ok {
			t.Fatalf("root prompt blob id not found in blobStore")
		}
		var entry rootPromptEntry
		if err := json.Unmarshal(raw, &entry); err != nil {
			t.Fatalf("root prompt entry is not valid JSON: %v", err)
		}
		if len(entry.Content) > 0 && strings.Contains(entry.Content[0].Text, nativeToolsOnlyMcpSystemPrompt) {
			found++
		}
	}
	if found != 1 {
		t.Errorf("expected the native-tools-only system instruction to appear exactly once, found %d times", found)
	}
}

// TestBuildAgentRunRequest_ToolsPresent_SkipsDuplicateInstructionIfClientAlreadyIncludesIt
// verifies that if the client's own system message already contains the
// exact native-tools-only instruction text, it is not duplicated.
func TestBuildAgentRunRequest_ToolsPresent_SkipsDuplicateInstructionIfClientAlreadyIncludesIt(t *testing.T) {
	blobs := newBlobStore()
	req := chatCompletionsRequest{
		Model: "cursor-fast",
		Messages: []chatMessage{
			{Role: "system", Content: "You are a helpful assistant. " + nativeToolsOnlyMcpSystemPrompt},
			{Role: "user", Content: "final question"},
		},
		Tools: []chatToolDefinition{
			{Type: "function", Function: chatToolDefinitionFunc{Name: "custom_tool"}},
		},
	}

	agentReq, err := buildAgentRunRequest(req, blobs)
	if err != nil {
		t.Fatalf("buildAgentRunRequest failed: %v", err)
	}

	rootPromptIDs := agentReq.GetConversationState().GetRootPromptMessagesJson()
	found := 0
	for _, id := range rootPromptIDs {
		raw, ok := blobs.get(id)
		if !ok {
			t.Fatalf("root prompt blob id not found in blobStore")
		}
		var entry rootPromptEntry
		if err := json.Unmarshal(raw, &entry); err != nil {
			t.Fatalf("root prompt entry is not valid JSON: %v", err)
		}
		if len(entry.Content) > 0 && strings.Contains(entry.Content[0].Text, nativeToolsOnlyMcpSystemPrompt) {
			found++
		}
	}
	if found != 1 {
		t.Errorf("expected the native-tools-only instruction to appear exactly once (from the client's own system message, not duplicated), found %d times", found)
	}
}
