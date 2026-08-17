package executor

import (
	"fmt"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/router-for-me/cliproxy-cursor-plugin/internal/cursorpb/gen"
)

// handleExecServerMessage answers a server-initiated ExecServerMessage
// (Cursor's live in-turn execution-request channel, distinct from the
// InteractionUpdate.ToolCallCompleted accumulation this plugin already
// surfaces to the local client). This channel requires a synchronous
// ExecClientMessage reply on the same open stream before Cursor
// continues - ported from gajae-code's handleExecServerMessage
// (packages/ai/src/providers/cursor.ts:1006-1241), which is the closest
// reference for the wire contract, though gajae-code answers these by
// actually executing the requested tool locally (it IS the agent).
//
// This plugin must never execute tools itself (fact-r5-tool-roundtrip):
// execution-type requests (readArgs, lsArgs, shellArgs, writeArgs,
// deleteArgs, grepArgs, ...) are answered with each result type's
// "rejected" oneof case instead of being executed, so Cursor's exchange
// terminates cleanly (never hangs waiting for a reply that never comes)
// without the plugin silently performing an execution action outside the
// approved translator role. requestContextArgs is answered with a
// minimal real RequestContextResult (matching gajae-code's shape),
// since it is informational, not an execution request.
//
// The mapping from each ExecServerMessage_XxxArgs oneof field to its
// ExecClientMessage_XxxResult counterpart is regular (the field name
// suffix changes from "_args" to "_result"), so this is implemented
// generically via protoreflect across all message kinds rather than
// hand-writing each of the 16 cases, mirroring the same generic approach
// used for tool-call extraction/reconstruction in translate_response.go/
// translate_request.go.
func handleExecServerMessage(execMsg *gen.ExecServerMessage, kv *kvWriter) error {
	reflectMsg := execMsg.ProtoReflect()
	oneofDesc := reflectMsg.Descriptor().Oneofs().ByName("message")
	if oneofDesc == nil {
		return fmt.Errorf("cursor: ExecServerMessage descriptor missing expected 'message' oneof")
	}
	whichField := reflectMsg.WhichOneof(oneofDesc)
	if whichField == nil {
		// No populated variant; nothing to answer.
		return nil
	}

	argsFieldName := string(whichField.Name())
	if argsFieldName == "request_context_args" {
		return sendExecClientMessage(kv, execMsg, "request_context_result", buildMinimalRequestContextResult())
	}

	resultFieldName, ok := execArgsToResultFieldName(argsFieldName)
	if !ok {
		// Unrecognized/unmapped variant: acknowledge nothing rather than
		// guessing a response shape that doesn't exist on
		// ExecClientMessage, and let the caller's normal read loop
		// continue; Cursor's own default case in gajae-code is similarly
		// permissive for cases it doesn't specifically handle.
		return nil
	}

	declinedMsg, err := buildRejectedResult(resultFieldName)
	if err != nil {
		return fmt.Errorf("cursor: failed to build decline result for %s: %w", resultFieldName, err)
	}
	return sendExecClientMessage(kv, execMsg, resultFieldName, declinedMsg)
}

// execFieldNameOverrides lists the exceptions to the regular "_args" ->
// "_result" naming convention, confirmed by cross-checking every
// declared ExecServerMessage/ExecClientMessage oneof field name in
// internal/cursorpb/gen/agent.pb.go (see
// TestExecArgsToResultFieldName_MapsAllDeclaredVariants, which fails if
// this map ever falls out of sync with the schema). shell_stream_args is
// the sole known exception: it answers to "shell_stream", not
// "shell_stream_result".
var execFieldNameOverrides = map[string]string{
	"shell_stream_args": "shell_stream",
}

// execArgsToResultFieldName derives the ExecClientMessage oneof field
// name that answers a given ExecServerMessage oneof field name, per the
// regular "_args" -> "_result" naming convention confirmed across most
// declared variants in internal/cursorpb/gen/agent.pb.go, with known
// exceptions in execFieldNameOverrides.
func execArgsToResultFieldName(argsFieldName string) (string, bool) {
	if override, ok := execFieldNameOverrides[argsFieldName]; ok {
		return override, true
	}
	const suffix = "_args"
	if len(argsFieldName) <= len(suffix) || argsFieldName[len(argsFieldName)-len(suffix):] != suffix {
		return "", false
	}
	return argsFieldName[:len(argsFieldName)-len(suffix)] + "_result", true
}

// buildRejectedResult constructs a zero-value instance of the concrete
// result message named by resultFieldName on ExecClientMessage's oneof,
// with its own "rejected" case populated (a generic, execution-refusing
// response), via reflection.
func buildRejectedResult(resultFieldName string) (protoreflect.Message, error) {
	clientMsg := &gen.ExecClientMessage{}
	reflectMsg := clientMsg.ProtoReflect()
	oneofDesc := reflectMsg.Descriptor().Oneofs().ByName("message")
	if oneofDesc == nil {
		return nil, fmt.Errorf("cursor: ExecClientMessage descriptor missing expected 'message' oneof")
	}
	fieldDesc := oneofDesc.Fields().ByName(protoreflect.Name(resultFieldName))
	if fieldDesc == nil || fieldDesc.Message() == nil {
		return nil, nil
	}

	resultMsg := reflectMsg.NewField(fieldDesc).Message()
	if err := populateDeclineOutcome(resultMsg); err != nil {
		return nil, err
	}
	return resultMsg, nil
}

// declineOutcomeFieldPriority lists the outcome-oneof case names tried in
// order to represent "this execution request was declined" on a result
// message. Not every Cursor result type declares a "rejected" case
// (confirmed by inspecting internal/cursorpb/gen/agent.pb.go: e.g.
// FetchResult, WriteShellStdinResult, and ComputerUseResult only declare
// success/error; RecordScreenResult only declares
// start_success/save_success/discard_success/failure) - falling back to
// "error" or "failure" still communicates a genuine non-success terminal
// outcome to Cursor rather than silently sending an empty, unset result
// wrapper (which an earlier version of this function did, a real defect
// caught in boundary review: Cursor would receive neither a rejection
// nor any typed failure signal for those variants).
var declineOutcomeFieldPriority = []string{"rejected", "error", "failure"}

// populateDeclineOutcome finds resultMsg's own outcome oneof (e.g.
// ReadResult's success/error/rejected/... oneof) and populates the
// highest-priority available case from declineOutcomeFieldPriority with
// an explanatory message, so every constructed result always carries a
// genuine terminal decline signal - never an empty/unset wrapper.
func populateDeclineOutcome(resultMsg protoreflect.Message) error {
	resultOneof := firstRealOneof(resultMsg.Descriptor())
	if resultOneof == nil {
		return fmt.Errorf("cursor: result message %s has no outcome oneof to populate a decline signal on", resultMsg.Descriptor().FullName())
	}

	for _, name := range declineOutcomeFieldPriority {
		field := resultOneof.Fields().ByName(protoreflect.Name(name))
		if field == nil || field.Message() == nil {
			continue
		}
		inner := resultMsg.NewField(field).Message()
		setFirstStringField(inner, "cursor plugin does not execute tools locally; this request was declined")
		resultMsg.Set(field, protoreflect.ValueOfMessage(inner))
		return nil
	}

	return fmt.Errorf("cursor: result message %s has an outcome oneof but none of %v are available to signal a decline", resultMsg.Descriptor().FullName(), declineOutcomeFieldPriority)
}

// buildMinimalRequestContextResult builds a minimal but real
// RequestContextResult (empty rules/tools/repository info), matching
// gajae-code's own minimal-context reply shape for requestContextArgs
// (cursor.ts:1015-1036) - this plugin has no local repository/tool
// context of its own to offer, so an empty-but-successful context is the
// honest answer rather than a rejection (requestContextArgs is
// informational, not an execution request).
func buildMinimalRequestContextResult() protoreflect.Message {
	result := &gen.RequestContextResult{
		Result: &gen.RequestContextResult_Success{
			Success: &gen.RequestContextSuccess{
				RequestContext: &gen.RequestContext{},
			},
		},
	}
	return result.ProtoReflect()
}

// sendExecClientMessage builds and writes one ExecClientMessage frame
// (wrapped in an AgentClientMessage) back on the open stream, copying
// the request's Id/ExecId per gajae-code's sendExecClientMessage.
func sendExecClientMessage(kv *kvWriter, execMsg *gen.ExecServerMessage, resultFieldName string, resultMsg protoreflect.Message) error {
	clientMsg := &gen.ExecClientMessage{
		Id:     execMsg.GetId(),
		ExecId: execMsg.GetExecId(),
	}
	reflectClientMsg := clientMsg.ProtoReflect()
	oneofDesc := reflectClientMsg.Descriptor().Oneofs().ByName("message")
	if oneofDesc == nil {
		return fmt.Errorf("cursor: ExecClientMessage descriptor missing expected 'message' oneof")
	}
	fieldDesc := oneofDesc.Fields().ByName(protoreflect.Name(resultFieldName))
	if fieldDesc == nil {
		return fmt.Errorf("cursor: unknown ExecClientMessage result field %q", resultFieldName)
	}
	reflectClientMsg.Set(fieldDesc, protoreflect.ValueOfMessage(resultMsg))

	return kv.write(&gen.AgentClientMessage{
		Message: &gen.AgentClientMessage_ExecClientMessage{ExecClientMessage: clientMsg},
	})
}
