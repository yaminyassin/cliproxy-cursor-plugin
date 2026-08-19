package executor

import "testing"

func clientTools(names ...string) []chatToolDefinition {
	out := make([]chatToolDefinition, 0, len(names))
	for _, n := range names {
		out = append(out, chatToolDefinition{Type: "function", Function: chatToolDefinitionFunc{Name: n}})
	}
	return out
}

// TestResolveClientTool_MapsNativeCursorToolsOntoDeclaredEquivalents covers
// the case observed live on 2026-08-19: a real client declared Read/Glob/
// Bash-style tools, Cursor called its own native read/glob/shell, and every
// call was rejected as "Tool read_tool_call not found".
func TestResolveClientTool_MapsNativeCursorToolsOntoDeclaredEquivalents(t *testing.T) {
	idx := newClientToolIndex(clientTools("Read", "Glob", "Bash", "Grep", "Edit", "Write"))

	cases := map[string]string{
		"read_tool_call":  "Read",
		"glob_tool_call":  "Glob",
		"shell_tool_call": "Bash",
		"grep_tool_call":  "Grep",
		"edit_tool_call":  "Edit",
		"write_tool_call": "Write",
		// exec-side field names must resolve identically
		"read_args":  "Read",
		"shell_args": "Bash",
	}
	for cursorField, want := range cases {
		got, ok := idx.resolveClientTool(cursorField)
		if !ok {
			t.Errorf("%s: expected a mapping to %q, got none", cursorField, want)
			continue
		}
		if got != want {
			t.Errorf("%s: mapped to %q, want %q", cursorField, got, want)
		}
	}
}

// TestResolveClientTool_NoInventionWhenClientDeclaredNothingEquivalent is
// the safety property: an unmatched native tool must NOT be redirected to
// some unrelated declared tool. It stays unmapped so the caller keeps
// Cursor's own name and the existing decline behavior.
func TestResolveClientTool_NoInventionWhenClientDeclaredNothingEquivalent(t *testing.T) {
	idx := newClientToolIndex(clientTools("get_weather", "send_email"))

	for _, cursorField := range []string{"read_tool_call", "shell_tool_call", "glob_tool_call"} {
		if got, ok := idx.resolveClientTool(cursorField); ok {
			t.Errorf("%s: expected no mapping, got %q", cursorField, got)
		}
	}

	// And with no declared tools at all.
	if _, ok := newClientToolIndex(nil).resolveClientTool("read_tool_call"); ok {
		t.Errorf("expected no mapping when the client declared no tools")
	}
}

// TestResolveClientTool_NormalizesNaming verifies the match is
// case/separator-insensitive, so read_file / readFile / Read-File all work.
func TestResolveClientTool_NormalizesNaming(t *testing.T) {
	for _, declared := range []string{"read_file", "readFile", "Read-File", "READ_FILE"} {
		idx := newClientToolIndex(clientTools(declared))
		got, ok := idx.resolveClientTool("read_tool_call")
		if !ok || got != declared {
			t.Errorf("declared %q: got (%q, %v), want (%q, true)", declared, got, ok, declared)
		}
	}
}

// TestResolveCursorToolField_ReverseMappingExists verifies a tool RESULT
// returned under the client's own tool name maps back onto the Cursor
// variant it was surfaced from, and that only real ToolCall oneof fields
// are ever returned.
func TestResolveCursorToolField_ReverseMappingExists(t *testing.T) {
	cases := map[string]string{
		"Read": "read_tool_call",
		"Bash": "shell_tool_call",
		"Glob": "glob_tool_call",
		"Grep": "grep_tool_call",
	}
	for clientTool, want := range cases {
		got, ok := resolveCursorToolField(clientTool)
		if !ok {
			t.Errorf("%s: expected reverse mapping to %q, got none", clientTool, want)
			continue
		}
		if got != want {
			t.Errorf("%s: reverse mapped to %q, want %q", clientTool, got, want)
		}
		if !toolCallOneofHasField(got) {
			t.Errorf("%s: reverse mapping %q is not a real ToolCall oneof field", clientTool, got)
		}
	}

	// A client-specific tool has no native counterpart and must not map.
	if got, ok := resolveCursorToolField("get_weather"); ok {
		t.Errorf("expected no reverse mapping for a client-only tool, got %q", got)
	}
}

// TestAddCapturedToolRequests_SurfacesMappedNativeToolAndDropsUnmapped
// exercises the accumulator end of the mapping.
func TestAddCapturedToolRequests_SurfacesMappedNativeToolAndDropsUnmapped(t *testing.T) {
	var acc responseAccumulator
	requests := []capturedToolRequest{
		{Field: "read_args", ArgsJSON: `{"path":"main.go"}`},
		{Field: "record_screen_args", ArgsJSON: `{}`}, // nothing equivalent declared
	}

	if err := acc.addCapturedToolRequests(requests, newClientToolIndex(clientTools("Read"))); err != nil {
		t.Fatalf("addCapturedToolRequests failed: %v", err)
	}

	if len(acc.toolCalls) != 1 {
		t.Fatalf("expected exactly 1 surfaced tool call (mapped read, unmapped dropped), got %d: %+v", len(acc.toolCalls), acc.toolCalls)
	}
	tc := acc.toolCalls[0]
	if tc.Function.Name != "Read" {
		t.Errorf("surfaced tool name = %q, want %q", tc.Function.Name, "Read")
	}
	if tc.Function.Arguments != `{"path":"main.go"}` {
		t.Errorf("surfaced args = %s, want %s", tc.Function.Arguments, `{"path":"main.go"}`)
	}
	if tc.ID == "" {
		t.Errorf("expected a generated tool call id")
	}
}

// TestBuildMcpTools_DeduplicatesAgainstNativeCursorTools verifies that a
// client tool Cursor already has natively is NOT also declared as an MCP
// tool. Declaring both gives the model two competing ways to do one thing
// and bloats the prompt; the native call is mapped back onto the client's
// name instead (see toolmap.go), so the client still receives a call under
// its own name.
func TestBuildMcpTools_DeduplicatesAgainstNativeCursorTools(t *testing.T) {
	tools := clientTools("Read", "Bash", "Glob", "Grep", "get_weather", "send_email")

	mcpTools, err := buildMcpTools(tools)
	if err != nil {
		t.Fatalf("buildMcpTools failed: %v", err)
	}
	if mcpTools == nil {
		t.Fatalf("expected client-only tools to still be declared, got nil")
	}

	got := map[string]bool{}
	for _, def := range mcpTools.GetMcpTools() {
		got[def.GetName()] = true
	}

	for _, native := range []string{"Read", "Bash", "Glob", "Grep"} {
		if got[native] {
			t.Errorf("%q has a native Cursor equivalent and must not be declared as an MCP tool", native)
		}
	}
	for _, clientOnly := range []string{"get_weather", "send_email"} {
		if !got[clientOnly] {
			t.Errorf("%q has no native equivalent and must still be declared, got %v", clientOnly, got)
		}
	}
}

// TestBuildMcpTools_AllNativeEquivalents_YieldsNoMcpTools verifies the
// all-deduplicated case returns nil rather than an empty-but-present
// McpTools, matching the no-tools contract.
func TestBuildMcpTools_AllNativeEquivalents_YieldsNoMcpTools(t *testing.T) {
	mcpTools, err := buildMcpTools(clientTools("Read", "Bash"))
	if err != nil {
		t.Fatalf("buildMcpTools failed: %v", err)
	}
	if mcpTools != nil {
		t.Errorf("expected nil McpTools when every client tool has a native equivalent, got %+v", mcpTools)
	}
}
