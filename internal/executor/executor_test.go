package executor

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/protobuf/proto"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/router-for-me/cliproxy-cursor-plugin/internal/account"
	"github.com/router-for-me/cliproxy-cursor-plugin/internal/cursorpb/gen"
)

// fakeCursorRunServer is a real HTTP/2 (cleartext h2c, so it runs over
// plain httptest.NewServer without TLS certs) test server speaking the
// actual Connect streaming wire framing (flags byte + 4-byte length +
// payload) gajae-code's real backend uses for
// /agent.v1.AgentService/Run. Genuine HTTP/2 is required (not
// httptest's default HTTP/1.1): Cursor's KV blob handshake needs true
// duplex request/response streaming, which HTTP/1.1 cannot provide (the
// standard library fully sends the request body before reading any
// response) - this is exactly the terminal-critic-verified defect that
// motivated stream.go's rework. These tests exercise runCursorStream's
// real framing/read-loop/KV-handshake code over the real transport it
// will use in production, not a substituted unary RPC client.
type fakeCursorRunServer struct {
	server *httptest.Server
	client *http.Client

	scriptedUpdates            []*gen.InteractionUpdate
	kvGetBlobIDs               [][]byte
	receivedKVResponses        [][]byte
	execRequestsRequiringReply []*gen.ExecServerMessage
	receivedExecReplies        [][]byte
	failMidStream              bool
}

func newFakeCursorRunServer(t *testing.T) *fakeCursorRunServer {
	t.Helper()
	f := &fakeCursorRunServer{}
	h2s := &http2.Server{}
	f.server = httptest.NewServer(h2c.NewHandler(http.HandlerFunc(f.handle), h2s))
	t.Cleanup(f.server.Close)

	f.client = &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, addr)
			},
		},
	}
	return f
}

func (f *fakeCursorRunServer) handle(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "flushing not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("content-type", "application/connect+proto")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Read and discard the client's initial framed AgentClientMessage
	// (the AgentRunRequest); we don't need its content for these tests.
	if _, err := readOneFrame(r.Body); err != nil {
		return
	}

	// Send any scripted getBlobArgs requests and wait for the client's
	// KvClientMessage response frame - this is the real bidirectional
	// exchange: the server writes mid-stream while the request body is
	// still open, and reads the client's reply before continuing. HTTP/2
	// duplex makes this actually work, unlike HTTP/1.1.
	for _, blobID := range f.kvGetBlobIDs {
		kvReq := &gen.AgentServerMessage{
			Message: &gen.AgentServerMessage_KvServerMessage{
				KvServerMessage: &gen.KvServerMessage{
					Id:      1,
					Message: &gen.KvServerMessage_GetBlobArgs{GetBlobArgs: &gen.GetBlobArgs{BlobId: blobID}},
				},
			},
		}
		raw, _ := proto.Marshal(kvReq)
		_, _ = w.Write(frameConnectMessage(raw, 0))
		flusher.Flush()

		respFrame, err := readOneFrame(r.Body)
		if err != nil {
			return
		}
		var clientMsg gen.AgentClientMessage
		if err := proto.Unmarshal(respFrame, &clientMsg); err == nil {
			if kv, ok := clientMsg.GetMessage().(*gen.AgentClientMessage_KvClientMessage); ok {
				if result, ok := kv.KvClientMessage.GetMessage().(*gen.KvClientMessage_GetBlobResult); ok {
					f.receivedKVResponses = append(f.receivedKVResponses, result.GetBlobResult.GetBlobData())
				}
			}
		}
	}

	// Send any scripted ExecServerMessages that require a synchronous
	// reply before the turn can proceed, and wait for the client's
	// ExecClientMessage response on the same stream - this is the
	// terminal-critic-verified requestContextArgs handshake. If the
	// client never answers, this read blocks until the test's -timeout
	// fires, correctly failing a test that doesn't implement the fix.
	for _, execReq := range f.execRequestsRequiringReply {
		serverMsg := &gen.AgentServerMessage{
			Message: &gen.AgentServerMessage_ExecServerMessage{ExecServerMessage: execReq},
		}
		raw, _ := proto.Marshal(serverMsg)
		_, _ = w.Write(frameConnectMessage(raw, 0))
		flusher.Flush()

		respFrame, err := readOneFrame(r.Body)
		if err != nil {
			return
		}
		f.receivedExecReplies = append(f.receivedExecReplies, respFrame)
	}

	for _, update := range f.scriptedUpdates {
		serverMsg := &gen.AgentServerMessage{
			Message: &gen.AgentServerMessage_InteractionUpdate{InteractionUpdate: update},
		}
		raw, _ := proto.Marshal(serverMsg)
		_, _ = w.Write(frameConnectMessage(raw, 0))
		flusher.Flush()
	}

	if f.failMidStream {
		// Close the connection abruptly without an end-stream frame,
		// simulating a dropped HTTP/2 stream after partial content.
		if hijacker, ok := w.(http.Hijacker); ok {
			conn, _, _ := hijacker.Hijack()
			_ = conn.Close()
		}
		return
	}

	endStream := []byte(`{}`)
	_, _ = w.Write(frameConnectMessage(endStream, connectEndStreamFlag))
	flusher.Flush()
}

func readOneFrame(r io.Reader) ([]byte, error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}
	msgLen := binary.BigEndian.Uint32(header[1:5])
	payload := make([]byte, msgLen)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

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

func testAgentClient(fake *fakeCursorRunServer, accounts *account.Store) *AgentClient {
	return NewAgentClient(accounts, fake.client, fake.server.URL, "")
}

// --- text-only translation round trip, against the real framed protocol ---

func TestExecute_TextOnlyRoundTrip(t *testing.T) {
	fake := newFakeCursorRunServer(t)
	fake.scriptedUpdates = []*gen.InteractionUpdate{
		{Message: &gen.InteractionUpdate_TextDelta{TextDelta: &gen.TextDeltaUpdate{Text: "hello back"}}},
		{Message: &gen.InteractionUpdate_TurnEnded{TurnEnded: &gen.TurnEndedUpdate{}}},
	}

	client := testAgentClient(fake, activeAccountStore())
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
}

// --- multi-message turn: proves the real fix for "only first message
// consumed" (terminal critic finding #4) ---

func TestExecute_MultiMessageTurn_AccumulatesAllUpdates(t *testing.T) {
	fake := newFakeCursorRunServer(t)
	fake.scriptedUpdates = []*gen.InteractionUpdate{
		{Message: &gen.InteractionUpdate_TextDelta{TextDelta: &gen.TextDeltaUpdate{Text: "part one "}}},
		{Message: &gen.InteractionUpdate_TextDelta{TextDelta: &gen.TextDeltaUpdate{Text: "part two"}}},
		{
			Message: &gen.InteractionUpdate_ToolCallCompleted{
				ToolCallCompleted: &gen.ToolCallCompletedUpdate{
					CallId:   "call-1",
					ToolCall: &gen.ToolCall{Tool: &gen.ToolCall_LsToolCall{LsToolCall: &gen.LsToolCall{}}},
				},
			},
		},
		{Message: &gen.InteractionUpdate_TurnEnded{TurnEnded: &gen.TurnEndedUpdate{}}},
	}

	client := testAgentClient(fake, activeAccountStore())
	exec := NewExecutor(client)

	resp, err := exec.Execute(context.Background(), pluginapi.ExecutorRequest{
		AuthID: "cursor",
		Model:  "cursor-fast",
		Payload: chatRequestPayload(t, chatCompletionsRequest{
			Model:    "cursor-fast",
			Messages: []chatMessage{{Role: "user", Content: "do multiple things"}},
		}),
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	var chatResp chatCompletionsResponse
	_ = json.Unmarshal(resp.Payload, &chatResp)

	if chatResp.Choices[0].Message.Content != "part one part two" {
		t.Errorf("content = %q, want the concatenation of both text-delta messages, not just the first", chatResp.Choices[0].Message.Content)
	}
	if len(chatResp.Choices[0].Message.ToolCalls) != 1 {
		t.Errorf("expected the tool call from the 3rd top-level message to also be captured, got %d tool calls", len(chatResp.Choices[0].Message.ToolCalls))
	}
}

// --- KV blob handshake: proves the real bidirectional exchange works ---

func TestExecute_AnswersServerInitiatedBlobRequest(t *testing.T) {
	fake := newFakeCursorRunServer(t)
	blobs := newBlobStore()
	knownBlobID := blobs.put([]byte("known-blob-content"))
	fake.kvGetBlobIDs = [][]byte{knownBlobID}
	fake.scriptedUpdates = []*gen.InteractionUpdate{
		{Message: &gen.InteractionUpdate_TextDelta{TextDelta: &gen.TextDeltaUpdate{Text: "ok"}}},
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
	_, err := client.runCursorStream(context.Background(), fake.server.URL, "test-token", runReq, blobs)
	if err != nil {
		t.Fatalf("runCursorStream failed: %v", err)
	}

	if len(fake.receivedKVResponses) != 1 {
		t.Fatalf("expected the server to receive exactly 1 KV response, got %d", len(fake.receivedKVResponses))
	}
	if string(fake.receivedKVResponses[0]) != "known-blob-content" {
		t.Errorf("KV response content = %q, want %q", fake.receivedKVResponses[0], "known-blob-content")
	}
}

// --- multi-tool-call-in-one-turn round trip ---

// TestResponseAccumulator_BatchesMultipleToolCallsIntoOneArray directly
// exercises the per-turn accumulator (Architect Finding 3): several
// ToolCallCompleted updates within one turn must flush as a single
// tool_calls array, not one response per event. CallId is set on the
// input fixtures to prove batching/ordering is independent of Cursor's
// raw call_id value: toChatToolCall now always generates a fresh,
// clean, OpenAI-compatible id (see translate_response.go's 2026-08-19
// live-E2E-driven fix - a real run observed Cursor sending a call_id
// containing an embedded newline), so this test asserts on the
// generated ids' shape/uniqueness/order, not on the raw CallId being
// echoed through.
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
	if toolCalls[0].Function.Name != "ls_tool_call" || toolCalls[1].Function.Name != "grep_tool_call" {
		t.Errorf("unexpected tool call order/content: %+v", toolCalls)
	}
	if toolCalls[0].ID == "" || toolCalls[1].ID == "" {
		t.Errorf("expected every tool call to have a generated, non-empty id, got %+v", toolCalls)
	}
	if toolCalls[0].ID == toolCalls[1].ID {
		t.Errorf("expected distinct generated ids for the two batched tool calls, both were %q", toolCalls[0].ID)
	}
	if strings.ContainsAny(toolCalls[0].ID, "\n\r\t ") || strings.ContainsAny(toolCalls[1].ID, "\n\r\t ") {
		t.Errorf("expected clean whitespace-free OpenAI-compatible tool call ids, got %+v", toolCalls)
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", resp.Choices[0].FinishReason)
	}
}

// --- mid-stream failure: dropped connection surfaces a terminal error,
// preserving any content already accumulated (not silent truncation) ---

func TestExecuteStream_MidStreamDrop_PreservesPartialContentAndSurfacesError(t *testing.T) {
	fake := newFakeCursorRunServer(t)
	fake.scriptedUpdates = []*gen.InteractionUpdate{
		{Message: &gen.InteractionUpdate_TextDelta{TextDelta: &gen.TextDeltaUpdate{Text: "partial content before drop"}}},
	}
	fake.failMidStream = true

	client := testAgentClient(fake, activeAccountStore())
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

	var gotChunk bool
	for chunk := range streamResp.Chunks {
		gotChunk = true
		if chunk.Err != nil {
			continue
		}
		var chatResp chatCompletionsResponse
		if errUnmarshal := json.Unmarshal(chunk.Payload, &chatResp); errUnmarshal == nil {
			if chatResp.Error == nil {
				t.Errorf("expected the chunk to carry a terminal error alongside any partial content, got a clean success with content %q", chatResp.Choices[0].Message.Content)
			}
			if len(chatResp.Choices) > 0 && chatResp.Choices[0].Message.Content != "partial content before drop" {
				t.Errorf("expected partial content to be preserved (never silently truncated), got %q", chatResp.Choices[0].Message.Content)
			}
		}
	}
	if !gotChunk {
		t.Fatalf("expected at least one chunk (error or partial-with-error), got none")
	}
}

// --- degraded-account fast-fail ---

func TestExecute_DegradedAccount_FailsFastWithoutCallingCursor(t *testing.T) {
	fake := newFakeCursorRunServer(t)
	var called bool
	baseHandler := fake.server.Config.Handler
	fake.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		baseHandler.ServeHTTP(w, r)
	})

	degradedStore := account.NewStore()
	degradedStore.Set("cursor", account.State{Status: account.StatusActive})
	degradedStore.MarkDegraded("cursor", account.StatusNeedsReauth, "refresh token rejected")

	client := testAgentClient(fake, degradedStore)
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
		t.Errorf("expected Cursor Run endpoint to never be called for a degraded account, but it was called")
	}
}
