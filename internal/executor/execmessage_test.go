package executor

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/router-for-me/cliproxy-cursor-plugin/internal/cursorpb/gen"
)

// TestExecute_AnswersRequestContextArgs regressions the terminal-critic
// finding that AgentServerMessage_ExecServerMessage was silently ignored:
// gajae-code's real backend can send requestContextArgs mid-exchange and
// waits for a RequestContextResult reply before continuing (cursor.ts:
// 1006-1037) - if the plugin does not answer, a real exchange could hang
// until the HTTP timeout. This test uses a fake server that requires the
// reply before it will send the next scripted update, so the test itself
// hangs (and the -timeout flag fails it) if the plugin doesn't answer.
func TestExecute_AnswersRequestContextArgs(t *testing.T) {
	fake := newFakeCursorRunServer(t)
	fake.execRequestsRequiringReply = []*gen.ExecServerMessage{
		{
			Id:      1,
			ExecId:  "exec-1",
			Message: &gen.ExecServerMessage_RequestContextArgs{RequestContextArgs: &gen.RequestContextArgs{}},
		},
	}
	fake.scriptedUpdates = []*gen.InteractionUpdate{
		{Message: &gen.InteractionUpdate_TextDelta{TextDelta: &gen.TextDeltaUpdate{Text: "answered context request"}}},
		{Message: &gen.InteractionUpdate_TurnEnded{TurnEnded: &gen.TurnEndedUpdate{}}},
	}

	client := testAgentClient(fake, activeAccountStore())
	runReq := &gen.AgentRunRequest{
		Action: &gen.ConversationAction{
			Action: &gen.ConversationAction_UserMessageAction{
				UserMessageAction: &gen.UserMessageAction{UserMessage: &gen.UserMessage{Text: "hi"}},
			},
		},
	}

	result, err := client.runCursorStream(context.Background(), fake.server.URL, "test-token", runReq, newBlobStore())
	if err != nil {
		t.Fatalf("runCursorStream failed (would hang without the fix, timing out instead): %v", err)
	}
	if len(fake.receivedExecReplies) != 1 {
		t.Fatalf("expected the server to receive exactly 1 exec reply, got %d", len(fake.receivedExecReplies))
	}

	// The reply frame is an AgentClientMessage wrapping the
	// ExecClientMessage, matching what kv.write actually sends on the
	// wire (handleExecServerMessage -> kv.write(&gen.AgentClientMessage{
	// Message: &gen.AgentClientMessage_ExecClientMessage{...}})).
	var agentClientMsg gen.AgentClientMessage
	if err := proto.Unmarshal(fake.receivedExecReplies[0], &agentClientMsg); err != nil {
		t.Fatalf("failed to unmarshal received exec reply: %v", err)
	}
	clientMsg := agentClientMsg.GetExecClientMessage()
	if clientMsg == nil {
		t.Fatalf("expected an ExecClientMessage wrapped in AgentClientMessage, got %+v", agentClientMsg.GetMessage())
	}
	if clientMsg.GetId() != 1 || clientMsg.GetExecId() != "exec-1" {
		t.Errorf("reply id/execId = %d/%q, want 1/exec-1 (must correlate with the request)", clientMsg.GetId(), clientMsg.GetExecId())
	}
	contextResult := clientMsg.GetRequestContextResult()
	if contextResult == nil {
		t.Fatalf("expected a RequestContextResult, got %+v", clientMsg.GetMessage())
	}
	if contextResult.GetSuccess() == nil {
		t.Errorf("expected requestContextArgs to be answered with a success result, got %+v", contextResult)
	}

	if len(result.updates) != 2 {
		t.Errorf("expected the turn to complete normally after the exec reply, got %d updates", len(result.updates))
	}
}

// TestBuildRejectedResult_ExecutionTypeRequest verifies that an
// execution-type ExecServerMessage (e.g. readArgs) is answered with a
// "rejected" result rather than being executed in-plugin, per
// fact-r5-tool-roundtrip (the plugin never executes tools itself).
func TestBuildRejectedResult_ExecutionTypeRequest(t *testing.T) {
	resultMsg, err := buildRejectedResult("read_result")
	if err != nil {
		t.Fatalf("buildRejectedResult failed: %v", err)
	}
	if resultMsg == nil {
		t.Fatalf("expected a non-nil rejected ReadResult")
	}

	readResult := resultMsg.Interface().(*gen.ReadResult)
	if readResult.GetRejected() == nil {
		t.Fatalf("expected ReadResult to carry a Rejected case, got %+v", readResult)
	}
}

// TestExecArgsToResultFieldName_MapsAllDeclaredVariants verifies the
// generic "_args" -> "_result" naming convention holds for every
// ExecServerMessage oneof field actually declared in the schema, so the
// generic mapping in execmessage.go doesn't silently miss a case.
func TestExecArgsToResultFieldName_MapsAllDeclaredVariants(t *testing.T) {
	msg := &gen.ExecServerMessage{}
	oneofDesc := msg.ProtoReflect().Descriptor().Oneofs().ByName("message")
	if oneofDesc == nil {
		t.Fatalf("ExecServerMessage descriptor missing expected 'message' oneof")
	}

	clientMsg := &gen.ExecClientMessage{}
	clientOneof := clientMsg.ProtoReflect().Descriptor().Oneofs().ByName("message")
	if clientOneof == nil {
		t.Fatalf("ExecClientMessage descriptor missing expected 'message' oneof")
	}

	fields := oneofDesc.Fields()
	for i := 0; i < fields.Len(); i++ {
		argsFieldName := string(fields.Get(i).Name())
		if argsFieldName == "request_context_args" {
			continue // handled specially, not via the generic mapping
		}
		resultFieldName, ok := execArgsToResultFieldName(argsFieldName)
		if !ok {
			t.Errorf("%s: no _args->_result mapping derived", argsFieldName)
			continue
		}
		if clientOneof.Fields().ByName(protoreflect.Name(resultFieldName)) == nil {
			t.Errorf("%s: derived result field %q does not exist on ExecClientMessage", argsFieldName, resultFieldName)
		}
	}
}
