package executor

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/router-for-me/cliproxy-cursor-plugin/internal/cursorpb/gen"
)

// TestToChatToolCall_EmptyToolCall_NoPanic adversarially probes
// toChatToolCall with a ToolCall that has no oneof variant set (a
// malformed/unexpected shape from Cursor), confirming it returns a clean
// error instead of panicking.
func TestToChatToolCall_EmptyToolCall_NoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("toChatToolCall panicked on an empty ToolCall: %v", r)
		}
	}()
	tc, err := toChatToolCall(&gen.ToolCall{})
	if err == nil {
		t.Errorf("expected an error for a ToolCall with no populated variant, got tc=%+v", tc)
	}
}

// TestToChatToolCall_NilToolCall_NoPanic probes the nil case.
func TestToChatToolCall_NilToolCall_NoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("toChatToolCall panicked on a nil ToolCall: %v", r)
		}
	}()
	tc, err := toChatToolCall(nil)
	if err != nil {
		t.Errorf("expected nil,nil for a nil ToolCall, got err=%v", err)
	}
	if tc != nil {
		t.Errorf("expected nil chatToolCall for a nil ToolCall, got %+v", tc)
	}
}

// TestBuildToolCallFromNameAndArgs_NonNativeName_BecomesMcpToolCall
// verifies that a tool name which is NOT one of Cursor's native variants
// is treated as a client-declared tool and rebuilt as Cursor's generic
// mcp_tool_call wrapper (that is how Cursor invokes client tools, and how
// translate_response.go surfaces them), rather than being rejected. An
// earlier version errored with "unknown tool call variant" here, which
// broke the tool-result half of the round trip for every
// client-declared tool - found in a live run on 2026-08-19.
func TestBuildToolCallFromNameAndArgs_NonNativeName_BecomesMcpToolCall(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("buildToolCallFromNameAndArgs panicked on a client-declared tool name: %v", r)
		}
	}()

	tc, err := buildToolCallFromNameAndArgs("get_weather", `{"city":"Seoul"}`, "sunny, 21C")
	if err != nil {
		t.Fatalf("buildToolCallFromNameAndArgs failed for a client-declared tool: %v", err)
	}
	mcpCall := tc.GetMcpToolCall()
	if mcpCall == nil {
		t.Fatalf("expected a client-declared tool to become an McpToolCall, got %+v", tc.GetTool())
	}
	if mcpCall.GetArgs().GetName() != "get_weather" {
		t.Errorf("McpArgs.Name = %q, want %q", mcpCall.GetArgs().GetName(), "get_weather")
	}
	city, ok := mcpCall.GetArgs().GetArgs()["city"]
	if !ok {
		t.Fatalf("expected the city argument to be carried into McpArgs.Args, got %+v", mcpCall.GetArgs().GetArgs())
	}
	// Argument values are serialized google.protobuf.Value, matching how
	// Cursor itself encodes them (mcpArgsToJSON decodes the same way), so
	// decode before asserting rather than comparing raw bytes.
	var cityValue structpb.Value
	if err := proto.Unmarshal(city, &cityValue); err != nil {
		t.Fatalf("McpArgs.Args[city] is not a serialized google.protobuf.Value: %v", err)
	}
	if cityValue.GetStringValue() != "Seoul" {
		t.Errorf("decoded McpArgs.Args[city] = %q, want %q", cityValue.GetStringValue(), "Seoul")
	}

	// And the full round trip back out must produce clean JSON with no
	// protobuf framing leaking into it.
	argsJSON, err := mcpArgsToJSON(mcpCall.GetArgs().GetArgs())
	if err != nil {
		t.Fatalf("mcpArgsToJSON failed: %v", err)
	}
	if argsJSON != `{"city":"Seoul"}` {
		t.Errorf("round-tripped args JSON = %s, want %s", argsJSON, `{"city":"Seoul"}`)
	}
	if mcpCall.GetResult() == nil {
		t.Errorf("expected the supplied tool result to be encoded, got none")
	}
}

// TestBuildToolCallFromNameAndArgs_MalformedArgsJSON_NoPanic probes
// reconstructing a tool call from malformed JSON arguments (as could
// arrive if a client echoes back corrupted tool_calls[].function.arguments).
func TestBuildToolCallFromNameAndArgs_MalformedArgsJSON_NoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("buildToolCallFromNameAndArgs panicked on malformed args JSON: %v", r)
		}
	}()
	_, err := buildToolCallFromNameAndArgs("shell_tool_call", `{not valid json`, "")
	if err == nil {
		t.Errorf("expected an error for malformed tool call arguments JSON, got nil")
	}
}

// TestResponseAccumulator_NilUpdate_NoPanic probes accumulate with a nil
// InteractionUpdate.
func TestResponseAccumulator_NilUpdate_NoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("accumulate panicked on a nil InteractionUpdate: %v", r)
		}
	}()
	var acc responseAccumulator
	acc.accumulate(nil)
}
