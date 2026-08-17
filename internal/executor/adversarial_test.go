package executor

import (
	"testing"

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
	tc, err := toChatToolCall("call-1", &gen.ToolCall{})
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
	tc, err := toChatToolCall("call-1", nil)
	if err != nil {
		t.Errorf("expected nil,nil for a nil ToolCall, got err=%v", err)
	}
	if tc != nil {
		t.Errorf("expected nil chatToolCall for a nil ToolCall, got %+v", tc)
	}
}

// TestBuildToolCallFromNameAndArgs_UnknownVariant_NoPanic probes
// reconstructing a tool call from an unrecognized/adversarial field name
// (as could arrive from a malicious or buggy client sending a fabricated
// tool_calls[].function.name in a follow-up request).
func TestBuildToolCallFromNameAndArgs_UnknownVariant_NoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("buildToolCallFromNameAndArgs panicked on an unknown variant: %v", r)
		}
	}()
	_, err := buildToolCallFromNameAndArgs("totally_made_up_tool_call", `{}`, "")
	if err == nil {
		t.Errorf("expected an error for an unknown tool call variant, got nil")
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
