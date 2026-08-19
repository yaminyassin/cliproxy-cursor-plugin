package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const providerIdentifier = "cursor"

// Executor implements pluginapi.ProviderExecutor for Cursor: translates
// chat-completions requests into Cursor AgentRunRequest calls and Cursor
// responses back into chat-completions payloads, per the ralplan-approved
// plan Section 5 and the terminal-critic-driven rework that replaced a
// unary-only translation with the real bidirectional Run exchange (see
// stream.go).
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

// Execute implements pluginapi.ProviderExecutor: performs the full
// bidirectional Run exchange (see stream.go), consuming every
// InteractionUpdate until TurnEnded or stream close, and translates the
// accumulated result into a single non-streaming chat-completions
// response.
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

// ExecuteStream implements pluginapi.ProviderExecutor. The full Cursor
// exchange (including its own internal multi-message/KV-blob handshake)
// happens inside e.run before this returns; from the chat-completions
// client's perspective this still delivers one buffered terminal chunk,
// which is the documented v1 scope boundary for chat-completions-facing
// incremental streaming (see docs/E2E.md) - the exchange with Cursor
// itself is now the real bidirectional protocol, not a single unary
// call, even though the client-facing surface remains buffered. A
// mid-stream transport failure (the exchange with Cursor drops after
// partial InteractionUpdates were already accumulated) still surfaces a
// terminal chat-completions error chunk, never a silently truncated
// success, per fact-r4/Principle 4 - see runCursorStream's error
// handling in stream.go, which returns whatever was accumulated
// alongside the error rather than discarding it.
func (e *Executor) ExecuteStream(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorStreamResponse, error) {
	// Buffered for every chunk this method can emit without a reader
	// (content chunk + terminal chunk, or a single error chunk). The
	// channel is closed before returning, so the buffer must hold them
	// all or this would deadlock on its own send.
	chunks := make(chan pluginapi.ExecutorStreamChunk, 4)

	resp, errRun := e.run(ctx, req)
	if errRun != nil {
		chunks <- pluginapi.ExecutorStreamChunk{Err: errRun}
		close(chunks)
		return pluginapi.ExecutorStreamResponse{Chunks: chunks}, nil
	}

	// Emit the OpenAI *streaming* chunk sequence (object ==
	// "chat.completion.chunk" with choices[].delta), not the
	// non-streaming chat.completion object. A streaming client parses
	// delta and ignores message, so sending the non-streaming shape on an
	// SSE stream makes it accumulate nothing and report an empty response
	// (found in a live run on 2026-08-19). Each chunk is a separate
	// payload so the host frames them as separate SSE events.
	streamChunks := resp.toStreamChunks()
	for _, streamChunk := range streamChunks {
		payload, errMarshal := json.Marshal(streamChunk)
		if errMarshal != nil {
			chunks <- pluginapi.ExecutorStreamChunk{Err: fmt.Errorf("cursor: failed to marshal chat-completions chunk: %w", errMarshal)}
			close(chunks)
			return pluginapi.ExecutorStreamResponse{Chunks: chunks}, nil
		}
		chunks <- pluginapi.ExecutorStreamChunk{Payload: payload}
	}
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
// translation used by both Execute and ExecuteStream: builds the
// AgentRunRequest (registering every referenced blob in a fresh
// per-request blobStore first, per stream.go's KV handshake
// requirements), drives the real bidirectional exchange via
// runCursorStream, and folds every accumulated InteractionUpdate into
// the chat-completions response.
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

	blobs := newBlobStore()
	agentRunReq, err := buildAgentRunRequest(chatReq, blobs)
	if err != nil {
		return chatCompletionsResponse{}, fmt.Errorf("cursor: failed to build agent run request: %w", err)
	}

	streamResult, errStream := e.client.runCursorStream(ctx, e.client.baseURL, accountState.AccessToken, agentRunReq, blobs)

	responseID, errID := newResponseID()
	if errID != nil {
		return chatCompletionsResponse{}, errID
	}

	var acc responseAccumulator
	if streamResult != nil {
		for _, update := range streamResult.updates {
			acc.accumulate(update)
		}
	}

	if streamResult != nil {
		if errCaptured := acc.addCapturedToolRequests(streamResult.toolRequests); errCaptured != nil {
			return chatCompletionsResponse{}, errCaptured
		}
	}

	// promptText is used only to estimate prompt tokens for the usage
	// block (Cursor reports no input-token count); every message's text
	// contributes, matching what was actually sent upstream.
	var promptBuilder strings.Builder
	for _, m := range chatReq.Messages {
		promptBuilder.WriteString(m.Content)
	}
	promptText := promptBuilder.String()

	if errStream != nil {
		if len(acc.toolCalls) == 0 && acc.text.Len() == 0 {
			// Nothing was salvaged before the failure; surface it as a
			// hard error rather than an empty success.
			return chatCompletionsResponse{}, fmt.Errorf("cursor: agent run failed: %w", errStream)
		}
		// Partial content was accumulated before the exchange failed;
		// return it with the terminal error attached rather than
		// silently discarding what Cursor already sent, per the
		// fail-loud / never-silently-truncate principle.
		resp := acc.toChatCompletionsResponse(chatReq.Model, responseID, promptText)
		resp.Error = &chatErrorInfo{Message: errStream.Error(), Type: "cursor_stream_error"}
		return resp, nil
	}

	return acc.toChatCompletionsResponse(chatReq.Model, responseID, promptText), nil
}
