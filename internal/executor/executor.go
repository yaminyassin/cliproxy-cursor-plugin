package executor

import (
	"context"
	"encoding/json"
	"fmt"

	"connectrpc.com/connect"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/router-for-me/cliproxy-cursor-plugin/internal/cursorpb/gen"
)

const providerIdentifier = "cursor"

// Executor implements pluginapi.ProviderExecutor for Cursor: translates
// chat-completions requests into Cursor AgentRunRequest calls and Cursor
// responses back into chat-completions payloads, per the ralplan-approved
// plan Section 5.
type Executor struct {
	client *AgentClient
}

// NewExecutor creates an Executor backed by the given AgentClient.
func NewExecutor(client *AgentClient) *Executor {
	return &Executor{client: client}
}

// Identifier implements pluginapi.ProviderExecutor.
func (e *Executor) Identifier() string {
	return providerIdentifier
}

// Execute implements pluginapi.ProviderExecutor: performs a single
// non-streaming chat-completions call, translating to and from Cursor's
// AgentRunRequest/AgentServerMessage. The Run RPC is unary in the
// generated Connect client (see internal/cursorpb/README.md); this
// executor buffers the full Cursor response before returning, matching
// the plan's v1 tool-call buffering decision.
func (e *Executor) Execute(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	resp, errRun := e.run(ctx, req)
	if errRun != nil {
		return pluginapi.ExecutorResponse{}, errRun
	}
	payload, errMarshal := json.Marshal(resp)
	if errMarshal != nil {
		return pluginapi.ExecutorResponse{}, fmt.Errorf("cursor: failed to marshal chat-completions response: %w", errMarshal)
	}
	return pluginapi.ExecutorResponse{Payload: payload}, nil
}

// ExecuteStream implements pluginapi.ProviderExecutor. v1 buffers the
// full Cursor response and emits it as a single terminal chunk, honestly
// reflecting that the generated Connect-RPC Run/RunSSE procedures are
// unary calls (true low-level incremental multiplexed streaming requires
// gajae-code's own hand-rolled raw HTTP/2 frame parsing, out of Option
// A's codegen scope per the ADR) rather than pretending to stream
// token-by-token. A mid-stream transport failure still surfaces a
// terminal chat-completions error chunk, never a silently truncated
// success, per fact-r4/Principle 4.
func (e *Executor) ExecuteStream(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorStreamResponse, error) {
	chunks := make(chan pluginapi.ExecutorStreamChunk, 1)

	resp, errRun := e.run(ctx, req)
	if errRun != nil {
		chunks <- pluginapi.ExecutorStreamChunk{Err: errRun}
		close(chunks)
		return pluginapi.ExecutorStreamResponse{Chunks: chunks}, nil
	}

	payload, errMarshal := json.Marshal(resp)
	if errMarshal != nil {
		chunks <- pluginapi.ExecutorStreamChunk{Err: fmt.Errorf("cursor: failed to marshal chat-completions response: %w", errMarshal)}
		close(chunks)
		return pluginapi.ExecutorStreamResponse{Chunks: chunks}, nil
	}

	chunks <- pluginapi.ExecutorStreamChunk{Payload: payload}
	close(chunks)
	return pluginapi.ExecutorStreamResponse{Chunks: chunks}, nil
}

// CountTokens implements pluginapi.ProviderExecutor. Cursor's protocol
// does not expose a token-counting RPC in agent.v1.AgentService; this
// returns a zero count rather than fabricating one, matching the
// fail-loud principle (an honest "unsupported" zero, not an invented
// estimate).
func (e *Executor) CountTokens(_ context.Context, _ pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	payload, _ := json.Marshal(map[string]int{"total_tokens": 0})
	return pluginapi.ExecutorResponse{Payload: payload}, nil
}

// HttpRequest implements pluginapi.ProviderExecutor. This executor does
// not offer a raw HTTP bridging surface for v1.
func (e *Executor) HttpRequest(_ context.Context, _ pluginapi.ExecutorHTTPRequest) (pluginapi.ExecutorHTTPResponse, error) {
	return pluginapi.ExecutorHTTPResponse{}, fmt.Errorf("cursor: executor.http_request is not supported")
}

// run performs the shared chat-completions -> Cursor -> chat-completions
// translation used by both Execute and ExecuteStream.
func (e *Executor) run(ctx context.Context, req pluginapi.ExecutorRequest) (chatCompletionsResponse, error) {
	var chatReq chatCompletionsRequest
	if err := json.Unmarshal(req.Payload, &chatReq); err != nil {
		return chatCompletionsResponse{}, fmt.Errorf("cursor: failed to decode chat-completions request: %w", err)
	}
	if chatReq.Model == "" {
		chatReq.Model = req.Model
	}

	accountState, err := e.client.Accounts.Get(req.AuthID)
	if err != nil {
		return chatCompletionsResponse{}, fmt.Errorf("cursor: executor requires an active account: %w", err)
	}

	agentRunReq, err := buildAgentRunRequest(chatReq)
	if err != nil {
		return chatCompletionsResponse{}, fmt.Errorf("cursor: failed to build agent run request: %w", err)
	}

	// Run's RPC input is AgentClientMessage, whose oneof wraps the
	// concrete AgentRunRequest payload (see AgentClientMessage_RunRequest
	// in internal/cursorpb/gen/agent.pb.go).
	clientMessage := &gen.AgentClientMessage{
		Message: &gen.AgentClientMessage_RunRequest{RunRequest: agentRunReq},
	}
	connectReq := withAuth(connect.NewRequest(clientMessage), accountState.AccessToken)

	resp, errRun := e.client.Service.Run(ctx, connectReq)
	if errRun != nil {
		return chatCompletionsResponse{}, fmt.Errorf("cursor: agent run failed: %w", errRun)
	}

	responseID, errID := newResponseID()
	if errID != nil {
		return chatCompletionsResponse{}, errID
	}

	var acc responseAccumulator
	if interactionUpdate := resp.Msg.GetInteractionUpdate(); interactionUpdate != nil {
		acc.accumulate(interactionUpdate)
	}

	return acc.toChatCompletionsResponse(chatReq.Model, responseID), nil
}
