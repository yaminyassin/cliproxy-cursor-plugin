package executor

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/router-for-me/cliproxy-cursor-plugin/internal/account"
	"github.com/router-for-me/cliproxy-cursor-plugin/internal/cursorpb/gen"
	"github.com/router-for-me/cliproxy-cursor-plugin/internal/cursorpb/gen/genconnect"
)

// fakeAgentServiceClient implements genconnect.AgentServiceClient with a
// scriptable Run behavior, so these tests exercise the real
// request/response translation logic (buildAgentRunRequest,
// responseAccumulator, toChatToolCall) without a live Cursor backend or a
// wire-level Connect-RPC test server.
type fakeAgentServiceClient struct {
	runFunc func(ctx context.Context, req *connect.Request[gen.AgentClientMessage]) (*connect.Response[gen.AgentServerMessage], error)
}

func (f *fakeAgentServiceClient) Run(ctx context.Context, req *connect.Request[gen.AgentClientMessage]) (*connect.Response[gen.AgentServerMessage], error) {
	return f.runFunc(ctx, req)
}
func (f *fakeAgentServiceClient) RunSSE(ctx context.Context, req *connect.Request[gen.BidiRequestId]) (*connect.Response[gen.AgentServerMessage], error) {
	return nil, errors.New("not used in tests")
}
func (f *fakeAgentServiceClient) NameAgent(ctx context.Context, req *connect.Request[gen.NameAgentRequest]) (*connect.Response[gen.NameAgentResponse], error) {
	return nil, errors.New("not used in tests")
}
func (f *fakeAgentServiceClient) GetUsableModels(ctx context.Context, req *connect.Request[gen.GetUsableModelsRequest]) (*connect.Response[gen.GetUsableModelsResponse], error) {
	return nil, errors.New("not used in tests")
}
func (f *fakeAgentServiceClient) GetDefaultModelForCli(ctx context.Context, req *connect.Request[gen.GetDefaultModelForCliRequest]) (*connect.Response[gen.GetDefaultModelForCliResponse], error) {
	return nil, errors.New("not used in tests")
}
func (f *fakeAgentServiceClient) GetAllowedModelIntents(ctx context.Context, req *connect.Request[gen.GetAllowedModelIntentsRequest]) (*connect.Response[gen.GetAllowedModelIntentsResponse], error) {
	return nil, errors.New("not used in tests")
}

var _ genconnect.AgentServiceClient = (*fakeAgentServiceClient)(nil)

func activeAccountStore() *account.Store {
	store := account.NewStore()
	store.Set("cursor", account.State{
		AccessToken:  "test-access-token",
		RefreshToken: "test-refresh-token",
		ExpiresAt:    time.Now().Add(time.Hour),
		Status:       account.StatusActive,
	})
	return store
}

func chatRequestPayload(t *testing.T, req chatCompletionsRequest) []byte {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("failed to marshal chat request: %v", err)
	}
	return raw
}

// --- text-only translation round trip ---

func TestExecute_TextOnlyRoundTrip(t *testing.T) {
	fake := &fakeAgentServiceClient{
		runFunc: func(ctx context.Context, req *connect.Request[gen.AgentClientMessage]) (*connect.Response[gen.AgentServerMessage], error) {
			runReq := req.Msg.GetRunRequest()
			if runReq == nil {
				t.Fatalf("expected RunRequest oneof set")
			}
			userText := runReq.GetAction().GetUserMessageAction().GetUserMessage().GetText()
			if userText != "hello cursor" {
				t.Errorf("translated user text = %q, want %q", userText, "hello cursor")
			}
			return connect.NewResponse(&gen.AgentServerMessage{
				Message: &gen.AgentServerMessage_InteractionUpdate{
					InteractionUpdate: &gen.InteractionUpdate{
						Message: &gen.InteractionUpdate_TextDelta{
							TextDelta: &gen.TextDeltaUpdate{Text: "hello back"},
						},
					},
				},
			}), nil
		},
	}

	client := &AgentClient{Service: fake, Accounts: activeAccountStore()}
	exec := NewExecutor(client)

	resp, err := exec.Execute(context.Background(), pluginapi.ExecutorRequest{
		AuthID: "cursor",
		Model:  "cursor-fast",
		Payload: chatRequestPayload(t, chatCompletionsRequest{
			Model:    "cursor-fast",
			Messages: []chatMessage{{Role: "user", Content: "hello cursor"}},
		}),
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	var chatResp chatCompletionsResponse
	if errUnmarshal := json.Unmarshal(resp.Payload, &chatResp); errUnmarshal != nil {
		t.Fatalf("failed to decode chat-completions response: %v", errUnmarshal)
	}
	if len(chatResp.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(chatResp.Choices))
	}
	if chatResp.Choices[0].Message.Content != "hello back" {
		t.Errorf("content = %q, want %q", chatResp.Choices[0].Message.Content, "hello back")
	}
	if chatResp.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", chatResp.Choices[0].FinishReason)
	}
	if len(chatResp.Choices[0].Message.ToolCalls) != 0 {
		t.Errorf("expected no tool calls for a text-only response, got %d", len(chatResp.Choices[0].Message.ToolCalls))
	}
}

// --- single tool-call round trip ---

func TestExecute_SingleToolCallRoundTrip(t *testing.T) {
	fake := &fakeAgentServiceClient{
		runFunc: func(ctx context.Context, req *connect.Request[gen.AgentClientMessage]) (*connect.Response[gen.AgentServerMessage], error) {
			return connect.NewResponse(&gen.AgentServerMessage{
				Message: &gen.AgentServerMessage_InteractionUpdate{
					InteractionUpdate: &gen.InteractionUpdate{
						Message: &gen.InteractionUpdate_ToolCallCompleted{
							ToolCallCompleted: &gen.ToolCallCompletedUpdate{
								CallId: "call-1",
								ToolCall: &gen.ToolCall{
									Tool: &gen.ToolCall_ShellToolCall{
										ShellToolCall: &gen.ShellToolCall{
											Args: &gen.ShellArgs{Command: "ls -la"},
										},
									},
								},
							},
						},
					},
				},
			}), nil
		},
	}

	client := &AgentClient{Service: fake, Accounts: activeAccountStore()}
	exec := NewExecutor(client)

	resp, err := exec.Execute(context.Background(), pluginapi.ExecutorRequest{
		AuthID: "cursor",
		Model:  "cursor-fast",
		Payload: chatRequestPayload(t, chatCompletionsRequest{
			Model:    "cursor-fast",
			Messages: []chatMessage{{Role: "user", Content: "list files"}},
		}),
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	var chatResp chatCompletionsResponse
	if errUnmarshal := json.Unmarshal(resp.Payload, &chatResp); errUnmarshal != nil {
		t.Fatalf("failed to decode chat-completions response: %v", errUnmarshal)
	}
	toolCalls := chatResp.Choices[0].Message.ToolCalls
	if len(toolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(toolCalls))
	}
	if toolCalls[0].ID != "call-1" {
		t.Errorf("tool call id = %q, want call-1", toolCalls[0].ID)
	}
	if toolCalls[0].Function.Name != "shell_tool_call" {
		t.Errorf("tool call name = %q, want shell_tool_call", toolCalls[0].Function.Name)
	}
	if chatResp.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", chatResp.Choices[0].FinishReason)
	}
	// The plugin itself never executes tools (fact-r5-tool-roundtrip):
	// verify the response is purely the surfaced tool_calls entry with
	// well-formed JSON arguments, no execution result field.
	var argsCheck map[string]any
	if errUnmarshal := json.Unmarshal([]byte(toolCalls[0].Function.Arguments), &argsCheck); errUnmarshal != nil {
		t.Fatalf("tool call arguments are not valid JSON: %v", errUnmarshal)
	}
}

// --- multi-tool-call-in-one-turn round trip ---

// TestResponseAccumulator_BatchesMultipleToolCallsIntoOneArray directly
// exercises the per-turn accumulator (Architect Finding 3): several
// ToolCallCompleted updates within one turn must flush as a single
// tool_calls array, not one response per event.
func TestResponseAccumulator_BatchesMultipleToolCallsIntoOneArray(t *testing.T) {
	var acc responseAccumulator
	acc.accumulate(&gen.InteractionUpdate{
		Message: &gen.InteractionUpdate_ToolCallCompleted{
			ToolCallCompleted: &gen.ToolCallCompletedUpdate{
				CallId:   "call-a",
				ToolCall: &gen.ToolCall{Tool: &gen.ToolCall_LsToolCall{LsToolCall: &gen.LsToolCall{}}},
			},
		},
	})
	acc.accumulate(&gen.InteractionUpdate{
		Message: &gen.InteractionUpdate_ToolCallCompleted{
			ToolCallCompleted: &gen.ToolCallCompletedUpdate{
				CallId:   "call-b",
				ToolCall: &gen.ToolCall{Tool: &gen.ToolCall_GrepToolCall{GrepToolCall: &gen.GrepToolCall{}}},
			},
		},
	})

	resp := acc.toChatCompletionsResponse("cursor-fast", "resp-1")
	toolCalls := resp.Choices[0].Message.ToolCalls
	if len(toolCalls) != 2 {
		t.Fatalf("expected 2 batched tool calls in one array, got %d", len(toolCalls))
	}
	if toolCalls[0].ID != "call-a" || toolCalls[1].ID != "call-b" {
		t.Errorf("unexpected tool call ids/order: %+v", toolCalls)
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", resp.Choices[0].FinishReason)
	}
}

// --- mid-stream failure: dropped stream surfaces a terminal error ---

func TestExecuteStream_TransportFailure_SurfacesTerminalError(t *testing.T) {
	fake := &fakeAgentServiceClient{
		runFunc: func(ctx context.Context, req *connect.Request[gen.AgentClientMessage]) (*connect.Response[gen.AgentServerMessage], error) {
			return nil, errors.New("simulated dropped HTTP/2 stream")
		},
	}

	client := &AgentClient{Service: fake, Accounts: activeAccountStore()}
	exec := NewExecutor(client)

	streamResp, err := exec.ExecuteStream(context.Background(), pluginapi.ExecutorRequest{
		AuthID: "cursor",
		Model:  "cursor-fast",
		Payload: chatRequestPayload(t, chatCompletionsRequest{
			Model:    "cursor-fast",
			Stream:   true,
			Messages: []chatMessage{{Role: "user", Content: "hello"}},
		}),
	})
	if err != nil {
		t.Fatalf("ExecuteStream returned a transport-level error instead of a chunk-level one: %v", err)
	}

	var sawTerminalErr bool
	for chunk := range streamResp.Chunks {
		if chunk.Err != nil {
			sawTerminalErr = true
		} else if len(chunk.Payload) > 0 {
			t.Errorf("expected no successful payload chunk after a transport failure (would be silent truncation), got %q", chunk.Payload)
		}
	}
	if !sawTerminalErr {
		t.Fatalf("expected a terminal error chunk on transport failure, got none (silent failure)")
	}
}

// --- degraded-account fast-fail ---

func TestExecute_DegradedAccount_FailsFastWithoutCallingCursor(t *testing.T) {
	called := false
	fake := &fakeAgentServiceClient{
		runFunc: func(ctx context.Context, req *connect.Request[gen.AgentClientMessage]) (*connect.Response[gen.AgentServerMessage], error) {
			called = true
			return nil, errors.New("should not be called")
		},
	}

	degradedStore := account.NewStore()
	degradedStore.Set("cursor", account.State{Status: account.StatusActive})
	degradedStore.MarkDegraded("cursor", account.StatusNeedsReauth, "refresh token rejected")

	client := &AgentClient{Service: fake, Accounts: degradedStore}
	exec := NewExecutor(client)

	_, err := exec.Execute(context.Background(), pluginapi.ExecutorRequest{
		AuthID: "cursor",
		Model:  "cursor-fast",
		Payload: chatRequestPayload(t, chatCompletionsRequest{
			Model:    "cursor-fast",
			Messages: []chatMessage{{Role: "user", Content: "hello"}},
		}),
	})
	if err == nil {
		t.Fatalf("expected Execute to fail fast on a degraded account")
	}
	if called {
		t.Errorf("expected Cursor Run RPC to never be called for a degraded account, but it was called")
	}
}
