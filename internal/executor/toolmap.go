package executor

import (
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/router-for-me/cliproxy-cursor-plugin/internal/cursorpb/gen"
)

// Cursor's model is trained on its OWN native tools (read/glob/shell/...)
// and reaches for them even when a client has declared an equivalent tool
// set of its own. Surfacing those calls under Cursor's internal oneof name
// is useless to the client: a live run on 2026-08-19 showed a real client
// receiving `read_tool_call`, `glob_tool_call` and `shell_tool_call` and
// rejecting every one with "Tool read_tool_call not found".
//
// This file maps a Cursor native tool onto whichever equivalent tool the
// CLIENT actually declared, so those calls become dispatchable instead of
// wasted. It is explicitly a heuristic on tool NAMES, and it only ever
// fires when the client declared a plausible match - there is no
// invention: an unmatched native tool keeps its raw Cursor name and the
// previous decline behavior, so this can only add working calls, never
// silently redirect one to the wrong tool.
//
// Argument shapes are NOT translated beyond unwrapping Cursor's `args`
// envelope (see nativeToolArgsJSON). Parameter names for a mapped tool are
// Cursor's, so a client whose equivalent tool uses different parameter
// names may receive arguments it does not recognize. That is a deliberate
// limit of a name-level heuristic and is why declared-tool calls
// (mcp_tool_call, whose arguments follow the client's own schema exactly)
// remain the preferred path and the system prompt still asks for them.

// nativeToolAliases maps a Cursor native tool's base name to the client
// tool names commonly used for the same capability, in preference order.
// The base name itself is always tried first (see resolveClientTool).
var nativeToolAliases = map[string][]string{
	"shell":        {"bash", "shell", "exec", "run", "terminal", "command", "run_command", "execute_command", "run_terminal_cmd"},
	"shell_stream": {"bash", "shell", "exec", "run", "terminal", "command"},
	"read":         {"read", "read_file", "cat", "view", "view_file", "open_file", "get_file"},
	"write":        {"write", "write_file", "create_file", "create", "put_file"},
	"edit":         {"edit", "edit_file", "apply_patch", "str_replace", "str_replace_editor", "patch", "modify_file"},
	"glob":         {"glob", "find", "file_search", "find_files", "fd", "list_files"},
	"grep":         {"grep", "search", "search_files", "ripgrep", "rg", "code_search", "text_search"},
	"ls":           {"ls", "list_dir", "list_directory", "list_files", "dir", "tree"},
	"delete":       {"delete", "rm", "remove", "remove_file", "delete_file"},
	"fetch":        {"fetch", "web_fetch", "http_fetch", "curl", "open_url", "read_url"},
	"web_search":   {"web_search", "search_web", "google", "browse"},
	"sem_search":   {"codebase_search", "semantic_search", "sem_search", "search"},
	"read_lints":   {"read_lints", "diagnostics", "lints", "get_diagnostics"},
	"task":         {"task", "subagent", "spawn_agent"},
	"update_todos": {"update_todos", "todo_write", "set_todos"},
	"read_todos":   {"read_todos", "todo_read", "get_todos"},
}

// clientToolIndex resolves Cursor tool names onto the client's declared
// tool names, and back again.
type clientToolIndex struct {
	// declared maps a normalized client tool name to the exact name the
	// client declared (case and punctuation preserved for dispatch).
	declared map[string]string
}

func newClientToolIndex(tools []chatToolDefinition) *clientToolIndex {
	idx := &clientToolIndex{declared: make(map[string]string, len(tools))}
	for _, t := range tools {
		name := t.Function.Name
		if name == "" {
			continue
		}
		idx.declared[normalizeToolName(name)] = name
	}
	return idx
}

func (i *clientToolIndex) empty() bool {
	return i == nil || len(i.declared) == 0
}

// resolveClientTool maps a Cursor tool field name (e.g. "read_tool_call"
// or "read_args") onto a client-declared tool name. Returns false when the
// client declared nothing plausible, in which case callers must keep
// Cursor's own name and existing behavior.
func (i *clientToolIndex) resolveClientTool(cursorField string) (string, bool) {
	if i.empty() {
		return "", false
	}
	base := cursorToolBaseName(cursorField)
	if base == "" {
		return "", false
	}

	// Exact base name first (a client tool literally called "read"/"grep").
	if actual, ok := i.declared[normalizeToolName(base)]; ok {
		return actual, true
	}
	for _, alias := range nativeToolAliases[base] {
		if actual, ok := i.declared[normalizeToolName(alias)]; ok {
			return actual, true
		}
	}
	return "", false
}

// resolveCursorToolField maps a client tool name back onto the Cursor
// ToolCall oneof field it was surfaced from, so a tool RESULT for a mapped
// native tool round-trips into the right Cursor variant. Only fields that
// genuinely exist on the ToolCall oneof are returned.
func resolveCursorToolField(clientTool string) (string, bool) {
	normalized := normalizeToolName(clientTool)
	if normalized == "" {
		return "", false
	}

	for base, aliases := range nativeToolAliases {
		match := normalizeToolName(base) == normalized
		if !match {
			for _, alias := range aliases {
				if normalizeToolName(alias) == normalized {
					match = true
					break
				}
			}
		}
		if !match {
			continue
		}
		field := base + "_tool_call"
		if toolCallOneofHasField(field) {
			return field, true
		}
	}
	return "", false
}

// toolCallOneofHasField reports whether ToolCall's "tool" oneof declares
// the given field, so a mapping can never point at a nonexistent variant.
func toolCallOneofHasField(field string) bool {
	oneof := (&gen.ToolCall{}).ProtoReflect().Descriptor().Oneofs().ByName("tool")
	if oneof == nil {
		return false
	}
	return oneof.Fields().ByName(protoreflect.Name(field)) != nil
}

// cursorToolBaseName strips Cursor's suffixes to get a comparable base
// name: "read_tool_call" -> "read", "shell_args" -> "shell".
func cursorToolBaseName(field string) string {
	base := field
	for _, suffix := range []string{"_tool_call", "_exec_args", "_args", "_call"} {
		if strings.HasSuffix(base, suffix) {
			base = strings.TrimSuffix(base, suffix)
			break
		}
	}
	return base
}

// normalizeToolName lowercases and strips separators so "read_file",
// "readFile" and "Read-File" all compare equal.
func normalizeToolName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}
