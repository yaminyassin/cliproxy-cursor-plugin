package executor

import (
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/router-for-me/cliproxy-cursor-plugin/internal/cursorpb/gen"
)

// TestBuildAgentRunRequest_EncodesConversationHistory verifies that prior
// turns (not just the final user message) are encoded into
// ConversationState.Turns, addressing the architect-review finding that
// v1 only translated the last message and silently dropped multi-turn
// history.
func TestBuildAgentRunRequest_EncodesConversationHistory(t *testing.T) {
	req := chatCompletionsRequest{
		Model: "cursor-fast",
		Messages: []chatMessage{
			{Role: "user", Content: "first question"},
			{Role: "assistant", Content: "first answer"},
			{Role: "user", Content: "second question"},
		},
	}

	agentReq, err := buildAgentRunRequest(req)
	if err != nil {
		t.Fatalf("buildAgentRunRequest failed: %v", err)
	}

	if agentReq.GetAction().GetUserMessageAction().GetUserMessage().GetText() != "second question" {
		t.Errorf("current turn text = %q, want %q", agentReq.GetAction().GetUserMessageAction().GetUserMessage().GetText(), "second question")
	}

	turns := agentReq.GetConversationState().GetTurns()
	if len(turns) != 2 {
		t.Fatalf("expected 2 history turns (first user + first assistant), got %d", len(turns))
	}

	var turn0 gen.ConversationTurnStructure
	if err := proto.Unmarshal(turns[0], &turn0); err != nil {
		t.Fatalf("failed to unmarshal turn 0: %v", err)
	}
	agentTurn0 := turn0.GetAgentConversationTurn()
	if agentTurn0 == nil {
		t.Fatalf("turn 0 is not an AgentConversationTurn")
	}
	var userMsg0 gen.UserMessage
	if err := proto.Unmarshal(agentTurn0.GetUserMessage(), &userMsg0); err != nil {
		t.Fatalf("failed to unmarshal turn 0 user message: %v", err)
	}
	if userMsg0.GetText() != "first question" {
		t.Errorf("turn 0 user message = %q, want %q", userMsg0.GetText(), "first question")
	}

	var turn1 gen.ConversationTurnStructure
	if err := proto.Unmarshal(turns[1], &turn1); err != nil {
		t.Fatalf("failed to unmarshal turn 1: %v", err)
	}
	agentTurn1 := turn1.GetAgentConversationTurn()
	if agentTurn1 == nil || len(agentTurn1.GetSteps()) != 1 {
		t.Fatalf("expected turn 1 to have 1 step, got %+v", agentTurn1)
	}
	var step1 gen.ConversationStep
	if err := proto.Unmarshal(agentTurn1.GetSteps()[0], &step1); err != nil {
		t.Fatalf("failed to unmarshal turn 1 step: %v", err)
	}
	if step1.GetAssistantMessage().GetText() != "first answer" {
		t.Errorf("turn 1 assistant message = %q, want %q", step1.GetAssistantMessage().GetText(), "first answer")
	}
}

// TestBuildAgentRunRequest_ToolResultRoundTrip verifies that a tool-role
// message is re-encoded into the matching Cursor ToolCall variant's
// result field, addressing the architect-review finding that tool
// results were never carried back to Cursor at all.
func TestBuildAgentRunRequest_ToolResultRoundTrip(t *testing.T) {
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

	agentReq, err := buildAgentRunRequest(req)
	if err != nil {
		t.Fatalf("buildAgentRunRequest failed: %v", err)
	}

	turns := agentReq.GetConversationState().GetTurns()
	if len(turns) != 3 {
		t.Fatalf("expected 3 history turns (user, assistant-with-tool-call, tool-result), got %d", len(turns))
	}

	var toolResultTurn gen.ConversationTurnStructure
	if err := proto.Unmarshal(turns[2], &toolResultTurn); err != nil {
		t.Fatalf("failed to unmarshal tool-result turn: %v", err)
	}
	agentTurn := toolResultTurn.GetAgentConversationTurn()
	if agentTurn == nil || len(agentTurn.GetSteps()) != 1 {
		t.Fatalf("expected the tool-result turn to have 1 step, got %+v", agentTurn)
	}
	var step gen.ConversationStep
	if err := proto.Unmarshal(agentTurn.GetSteps()[0], &step); err != nil {
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
	if shellCall.GetResult() == nil {
		t.Fatalf("expected the round-tripped tool call to carry a Result, got none")
	}
}
