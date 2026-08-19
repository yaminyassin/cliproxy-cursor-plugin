package executor

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/router-for-me/cliproxy-cursor-plugin/internal/cursorpb/gen"
)

// Cursor's Run RPC is not a plain unary Connect-RPC call: the server can
// send a KvServerMessage (getBlobArgs/setBlobArgs) mid-exchange and
// expects a KvClientMessage response written back on the same open
// HTTP/2 request stream before it continues. connect-go's generated
// unary client (`genconnect.AgentServiceClient.Run`, built in G001/G004)
// cannot perform this - it is strictly request-then-response. This file
// ports gajae-code's own raw HTTP/2 duplex-stream approach
// (packages/ai/src/providers/cursor.ts: streamCursor, frameConnectMessage,
// parseConnectEndStream, handleKvServerMessage) so the real bidirectional
// blob handshake is honored, per the terminal-critic-verified finding
// that the previous unary-only implementation was wire-incompatible.
//
// The generated protobuf message *types* from internal/cursorpb/gen
// remain valid and are reused here; only the transport for this one RPC
// bypasses genconnect's unary client in favor of a raw HTTP/2 duplex
// stream using the Connect streaming wire framing directly.

const (
	connectEndStreamFlag = 0x02
	runProcedurePath     = "/agent.v1.AgentService/Run"
)

// frameConnectMessage wraps one protobuf message in Connect's streaming
// envelope: 1 flags byte + 4-byte big-endian length + payload. Ported
// from gajae-code's frameConnectMessage.
func frameConnectMessage(data []byte, flags byte) []byte {
	frame := make([]byte, 5+len(data))
	frame[0] = flags
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(data)))
	copy(frame[5:], data)
	return frame
}

// connectEndStreamPayload is the JSON envelope Connect sends on the
// end-of-stream frame; a non-empty "error" field signals a stream-level
// failure (see gajae-code's parseConnectEndStream).
type connectEndStreamPayload struct {
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func parseConnectEndStream(data []byte) error {
	var payload connectEndStreamPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("cursor: failed to parse Connect end-of-stream payload: %w", err)
	}
	if payload.Error != nil {
		code := payload.Error.Code
		if code == "" {
			code = "unknown"
		}
		msg := payload.Error.Message
		if msg == "" {
			msg = "unknown error"
		}
		return fmt.Errorf("cursor: connect error %s: %s", code, msg)
	}
	return nil
}

// runStreamResult accumulates every InteractionUpdate observed across the
// whole Run exchange (not just the first one, per the terminal-critic
// finding that only consuming one top-level AgentServerMessage silently
// truncates multi-message turns), until TurnEnded or the stream closes.
type runStreamResult struct {
	updates []*gen.InteractionUpdate
	// toolRequests holds client-declared tool invocations that Cursor
	// asked THIS process to execute inline via ExecServerMessage. Cursor
	// only carries the tool name and arguments on that exec request; the
	// ToolCallCompleted update that follows a decline carries just the
	// rejection result. Since this plugin never executes tools itself
	// (fact-r5-tool-roundtrip) and must instead surface them to the local
	// client, the request details are captured here before being declined
	// upstream - otherwise the tool name and arguments are lost entirely
	// and the client receives an undispatchable "mcp_tool_call" with only
	// a rejection payload (observed live on 2026-08-19).
	toolRequests []capturedToolRequest
	// checkpoint is the latest ConversationStateStructure Cursor
	// acknowledged during this exchange, cached for the next turn.
	checkpoint *gen.ConversationStateStructure
}

// capturedToolRequest is one client-declared tool invocation Cursor
// requested inline, preserved for translation into a chat-completions
// tool_calls entry.
type capturedToolRequest struct {
	// Field is Cursor's own exec oneof field name (e.g. "mcp_args",
	// "read_args"), used to map a NATIVE tool onto whichever equivalent
	// tool the client declared (see toolmap.go).
	Field string
	// Name/Args are set for mcp_args: a tool the client declared, whose
	// name and per-parameter values Cursor echoes back verbatim.
	Name string
	Args map[string][]byte
	// ArgsJSON is set for native tools: protojson of Cursor's own args
	// message, already unwrapped from its "args" envelope.
	ArgsJSON string
}

// maxStreamAttempts bounds how many times one logical Run exchange is
// re-attempted after a retryable connection-level failure.
const maxStreamAttempts = 3

// Deadlines for one Run exchange. These replace a blanket
// http.Client.Timeout, which covered the whole request INCLUDING the body
// read and therefore killed any turn longer than its value mid-stream
// (observed live on 2026-08-19 as exactly-2m0s requests once CLIProxyAPI
// retried the truncated turn).
//
// streamIdleTimeout is the real health signal for a duplex stream: a turn
// may legitimately run for minutes while Cursor works, but it should
// always be producing *something* periodically. No bytes at all for this
// long means the exchange is hung, not busy.
const (
	streamIdleTimeout = 90 * time.Second
	streamMaxDuration = 10 * time.Minute

	// clientHeartbeatInterval matches gajae-code's own
	// setInterval(sendHeartbeat, 5000) in streamCursor. Cursor expects the
	// client to heartbeat for the whole duration of a Run exchange;
	// without it Cursor stalls the turn until its own server-side timeout
	// (observed live 2026-08-19 as requests pinned at exactly 2m0s).
	clientHeartbeatInterval = 5 * time.Second
)

// runCursorStream performs the Run exchange, transparently re-attempting
// it when the connection dies in a way that produced no content yet.
//
// Cursor's servers routinely cycle HTTP/2 connections with GOAWAY. Go's
// http2 transport wants to retry such a request on a fresh connection,
// but it refuses once the request body has been written and no
// Request.GetBody is defined - surfacing:
//
//	http2: Transport received Server's graceful shutdown GOAWAY
//	cannot retry err [...] after Request.Body was written;
//	define Request.GetBody to avoid this error
//
// Defining GetBody is NOT a valid fix here: this exchange is duplex, so
// the transport would hand itself a fresh body reader while kvWriter
// still points at the old pipe, and every blob-handshake reply would be
// written to a pipe nobody reads. The exchange has to be rebuilt from
// scratch instead - which is safe, because the only unconditional write
// is the initial AgentRunRequest frame (reproducible from runReq) and all
// KV/exec replies are generated reactively against whatever the new
// stream asks for. The shared blobStore is a content-addressed cache, so
// it stays valid and warm across attempts.
//
// A retry is only attempted when nothing was accumulated yet; once any
// update or tool request has been observed, the error is returned with
// the partial result so a retry can never duplicate emitted content
// (fail loud, never silently replay half a turn).
func (c *AgentClient) runCursorStream(ctx context.Context, baseURL, accessToken string, runReq *gen.AgentRunRequest, blobs *blobStore) (*runStreamResult, error) {
	var (
		result *runStreamResult
		err    error
	)
	for attempt := 1; attempt <= maxStreamAttempts; attempt++ {
		result, err = c.runCursorStreamOnce(ctx, baseURL, accessToken, runReq, blobs)
		if err == nil {
			return result, nil
		}

		produced := result != nil && (len(result.updates) > 0 || len(result.toolRequests) > 0)
		if produced || attempt == maxStreamAttempts || !isRetryableStreamError(err) {
			return result, err
		}

		// Brief backoff before rebuilding the exchange, honoring ctx.
		backoff := time.Duration(attempt) * 250 * time.Millisecond
		select {
		case <-ctx.Done():
			return result, err
		case <-time.After(backoff):
		}
	}
	return result, err
}

// isRetryableStreamError reports whether a failed Run exchange died from
// a connection-level condition that a fresh connection is expected to
// resolve. Deliberately narrow: request/protocol errors and Cursor's own
// Connect-level errors must surface to the caller, not be retried.
func isRetryableStreamError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, signature := range []string{
		"GOAWAY",
		"graceful shutdown",
		"cannot retry err",
		"REFUSED_STREAM",
		"connection reset by peer",
		"use of closed network connection",
		"broken pipe",
		"unexpected EOF",
		"server closed idle connection",
		"http2: client connection lost",
	} {
		if strings.Contains(msg, signature) {
			return true
		}
	}
	return false
}

// runCursorStreamOnce performs ONE real bidirectional Run exchange: opens an
// HTTP/2 duplex request, writes the initial AgentClientMessage carrying
// the AgentRunRequest, then loops reading framed AgentServerMessages from
// the response body. Any server-initiated KvServerMessage
// (getBlobArgs/setBlobArgs) is answered by writing a KvClientMessage
// frame back on the same request body before continuing to read - this
// is the bidirectional handshake a unary Connect-RPC call cannot perform.
// The loop ends on a chat-relevant terminal condition (TurnEnded) or
// stream close/error.
//
// The request-body writer goroutine's lifetime is scoped to this call
// (via the local release channel, closed by this function's own defer
// on every exit path), NOT to the caller's ctx: callers commonly pass
// context.Background() (see executor.go's run/dispatch.go's
// HandleMethod), which never cancels on its own, so waiting on
// ctx.Done() to release the writer would leak one goroutine per request
// forever.
func (c *AgentClient) runCursorStreamOnce(ctx context.Context, baseURL, accessToken string, runReq *gen.AgentRunRequest, blobs *blobStore) (result *runStreamResult, resultErr error) {
	clientMsg := &gen.AgentClientMessage{
		Message: &gen.AgentClientMessage_RunRequest{RunRequest: runReq},
	}
	initialBytes, err := proto.Marshal(clientMsg)
	if err != nil {
		return nil, fmt.Errorf("cursor: failed to marshal initial run request: %w", err)
	}

	// Overall cap plus an idle watchdog, in place of a total client
	// timeout (see streamIdleTimeout/streamMaxDuration). The idle timer is
	// reset on every read that yields bytes, so a long-but-progressing
	// turn survives while a silent stream is aborted promptly.
	ctx, cancelStream := context.WithTimeout(ctx, streamMaxDuration)
	defer cancelStream()
	idleTimer := time.AfterFunc(streamIdleTimeout, cancelStream)
	defer idleTimer.Stop()

	pr, pw := io.Pipe()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+runProcedurePath, pr)
	if err != nil {
		return nil, fmt.Errorf("cursor: failed to build streaming run request: %w", err)
	}
	httpReq.Header.Set("content-type", "application/connect+proto")
	httpReq.Header.Set("connect-protocol-version", "1")
	httpReq.Header.Set("te", "trailers")
	httpReq.Header.Set("authorization", "Bearer "+accessToken)
	httpReq.Header.Set("x-ghost-mode", "true")
	httpReq.Header.Set("x-cursor-client-type", "cli")
	httpReq.Header.Set("x-cursor-client-version", c.clientVersionOrDefault())
	httpReq.Header.Set("x-request-id", newRequestID())

	kv := &kvWriter{pw: pw}

	// Heartbeat for the whole exchange (see clientHeartbeatInterval). The
	// ticker goroutine exits with the call, and kv.close() below makes a
	// late tick a no-op instead of a write into a torn-down pipe.
	heartbeatDone := make(chan struct{})
	defer close(heartbeatDone)
	defer kv.close()
	go func() {
		ticker := time.NewTicker(clientHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatDone:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := kv.sendClientHeartbeat(); err != nil {
					return
				}
			}
		}
	}()

	// release is closed by this function's own defer on every exit
	// path, telling the writer goroutine it may stop waiting and close
	// pw; the second defer then waits for that goroutine to actually
	// exit before this function returns, so no goroutine outlives the
	// call. Defers run LIFO, so registering the "wait for exit" defer
	// BEFORE the "signal release" defer would deadlock (it would block
	// on writerExited before release is ever closed) - the order below
	// (signal first, wait second) is required, not incidental. This
	// scopes the goroutine's lifetime to this call, not to the caller's
	// ctx (see the function doc above for why that matters: ctx is
	// commonly context.Background() and never cancels on its own).
	release := make(chan struct{})
	writerExited := make(chan struct{})
	go func() {
		defer close(writerExited)
		defer pw.Close()
		if _, errWrite := pw.Write(frameConnectMessage(initialBytes, 0)); errWrite != nil {
			kv.recordWriteErr(errWrite)
			return
		}
		<-release
	}()
	// Defers run LIFO, so the effective order here is:
	//   1. close(release)  - tell the writer it may stop waiting
	//   2. pr.Close()      - unblock any write still parked on the pipe
	//   3. <-writerExited  - only then wait for the goroutine to finish
	//
	// Step 2 is required for correctness, not tidiness: if the transport
	// fails BEFORE consuming the request body (e.g. the connection is
	// refused, or a GOAWAY retry is rejected outright), nothing ever
	// drains the pipe, so the writer goroutine stays parked in pw.Write
	// and waiting on writerExited deadlocks the whole call. Closing the
	// read end makes that pending write fail immediately so the goroutine
	// can exit. Caught by TestRunCursorStream_RetriesGoawayThenSucceeds,
	// which injects a transport error before any body read.
	defer func() { <-writerExited }()
	defer pr.Close()
	defer close(release)

	resp, err := c.streamClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("cursor: streaming run request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("cursor: streaming run request returned status %d: %s", resp.StatusCode, string(body))
	}

	result = &runStreamResult{}
	var pending bytes.Buffer
	buf := make([]byte, 32*1024)

	for {
		n, errRead := resp.Body.Read(buf)
		if n > 0 {
			// Progress: the stream is alive, so restart the idle watchdog.
			idleTimer.Reset(streamIdleTimeout)
			pending.Write(buf[:n])
		}

		for {
			frame, ok := extractFrame(&pending)
			if !ok {
				break
			}

			if frame.flags&connectEndStreamFlag != 0 {
				if errEnd := parseConnectEndStream(frame.payload); errEnd != nil {
					return result, errEnd
				}
				return result, nil
			}

			var serverMsg gen.AgentServerMessage
			if errUnmarshal := proto.Unmarshal(frame.payload, &serverMsg); errUnmarshal != nil {
				return result, fmt.Errorf("cursor: failed to decode server message: %w", errUnmarshal)
			}

			switch msg := serverMsg.GetMessage().(type) {
			case *gen.AgentServerMessage_InteractionUpdate:
				if msg.InteractionUpdate != nil {
					result.updates = append(result.updates, msg.InteractionUpdate)
					if isTurnEnded(msg.InteractionUpdate) {
						return result, nil
					}
				}
			case *gen.AgentServerMessage_KvServerMessage:
				if errKV := handleKvServerMessage(msg.KvServerMessage, blobs, kv); errKV != nil {
					return result, fmt.Errorf("cursor: failed to answer KV handshake: %w", errKV)
				}
			case *gen.AgentServerMessage_ExecServerMessage:
				// Cursor's live exec-request channel requires a
				// synchronous reply on this same stream before it
				// continues (see execmessage.go doc comment) - unlike
				// the other default-branch cases below, silently
				// ignoring this can hang the exchange.
				if errExec := handleExecServerMessage(msg.ExecServerMessage, kv, result); errExec != nil {
					return result, fmt.Errorf("cursor: failed to answer exec message: %w", errExec)
				}
			case *gen.AgentServerMessage_ConversationCheckpointUpdate:
				// Cursor's acknowledged conversation state; cached so the
				// next turn builds on it instead of resetting Cursor's
				// accumulated non-history state (see conversation.go).
				if cp := msg.ConversationCheckpointUpdate; cp != nil {
					result.checkpoint = cp
				}
			default:
				// ExecServerControlMessage/
				// InteractionQuery: non-chat agent bookkeeping, safely
				// ignored per the plan's documented scope boundary -
				// these do not require a synchronous reply to avoid
				// hanging the exchange (unlike ExecServerMessage above).
			}
		}

		if errRead != nil {
			// A clean graceful close is signaled by Cursor's own
			// Connect end-stream frame (flags & connectEndStreamFlag,
			// handled above), never by the transport simply reaching
			// EOF/closing first. Reaching io.EOF (or any other read
			// error) here means the connection ended WITHOUT that
			// frame - a genuine mid-stream drop, not a graceful
			// completion. Per the fail-loud principle (never silently
			// truncate), this is always a terminal error; the caller
			// still receives whatever was accumulated in result
			// alongside it, so partial content is preserved rather than
			// discarded.
			return result, fmt.Errorf("cursor: streaming run ended without a Connect end-of-stream frame (mid-stream drop): %w", errRead)
		}
	}
}

func newRequestID() string {
	id, err := newMessageID()
	if err != nil {
		return "unknown"
	}
	return id
}

func isTurnEnded(update *gen.InteractionUpdate) bool {
	_, ok := update.GetMessage().(*gen.InteractionUpdate_TurnEnded)
	return ok
}

type connectFrame struct {
	flags   byte
	payload []byte
}

// extractFrame pulls one complete Connect streaming envelope off the
// front of buf if enough bytes are available, consuming it from buf.
func extractFrame(buf *bytes.Buffer) (connectFrame, bool) {
	b := buf.Bytes()
	if len(b) < 5 {
		return connectFrame{}, false
	}
	flags := b[0]
	msgLen := binary.BigEndian.Uint32(b[1:5])
	if uint32(len(b)) < 5+msgLen {
		return connectFrame{}, false
	}
	payload := make([]byte, msgLen)
	copy(payload, b[5:5+msgLen])
	buf.Next(int(5 + msgLen))
	return connectFrame{flags: flags, payload: payload}, true
}

// kvWriter serializes writes back onto the shared request-body pipe used
// for the KV handshake, the exec-message handshake, and the periodic
// client heartbeat, so concurrent writers never interleave partial
// frames. lastWriteErr records the initial-write failure case so it is
// not silently discarded even if the read loop observes a different
// terminal condition first.
//
// The mutex is required, not defensive: the heartbeat ticker writes from
// its own goroutine while the read loop writes KV/exec replies, and two
// unsynchronized writes to the pipe would corrupt the framing.
type kvWriter struct {
	mu           sync.Mutex
	pw           *io.PipeWriter
	closed       bool
	lastWriteErr error
}

func (k *kvWriter) write(clientMsg *gen.AgentClientMessage) error {
	raw, err := proto.Marshal(clientMsg)
	if err != nil {
		return fmt.Errorf("cursor: failed to marshal client message: %w", err)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		return fmt.Errorf("cursor: stream writer is closed")
	}
	_, err = k.pw.Write(frameConnectMessage(raw, 0))
	if err != nil {
		k.lastWriteErr = err
	}
	return err
}

// close marks the writer unusable so a late heartbeat tick can never
// write into a torn-down exchange.
func (k *kvWriter) close() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.closed = true
}

func (k *kvWriter) recordWriteErr(err error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.lastWriteErr = err
}

// sendClientHeartbeat writes one ClientHeartbeat frame on the open
// stream. Cursor expects the client to heartbeat for the whole duration
// of a Run exchange (gajae-code does this on a 5s interval in
// streamCursor); without it Cursor stalls the turn until its own
// server-side timeout, which showed up live on 2026-08-19 as requests
// pinned at exactly 2m0s before returning or being abandoned.
func (k *kvWriter) sendClientHeartbeat() error {
	return k.write(&gen.AgentClientMessage{
		Message: &gen.AgentClientMessage_ClientHeartbeat{
			ClientHeartbeat: &gen.ClientHeartbeat{},
		},
	})
}

// handleKvServerMessage answers a server-initiated getBlobArgs/
// setBlobArgs request by reading/writing the shared blobStore and
// writing the KvClientMessage response frame back on the open stream,
// ported from gajae-code's handleKvServerMessage.
func handleKvServerMessage(kvMsg *gen.KvServerMessage, blobs *blobStore, kv *kvWriter) error {
	switch req := kvMsg.GetMessage().(type) {
	case *gen.KvServerMessage_GetBlobArgs:
		data, _ := blobs.get(req.GetBlobArgs.GetBlobId())
		resp := &gen.KvClientMessage{
			Id: kvMsg.GetId(),
			Message: &gen.KvClientMessage_GetBlobResult{
				GetBlobResult: &gen.GetBlobResult{BlobData: data},
			},
		}
		return kv.write(&gen.AgentClientMessage{Message: &gen.AgentClientMessage_KvClientMessage{KvClientMessage: resp}})

	case *gen.KvServerMessage_SetBlobArgs:
		blobs.putKnownID(req.SetBlobArgs.GetBlobId(), req.SetBlobArgs.GetBlobData())
		resp := &gen.KvClientMessage{
			Id:      kvMsg.GetId(),
			Message: &gen.KvClientMessage_SetBlobResult{SetBlobResult: &gen.SetBlobResult{}},
		}
		return kv.write(&gen.AgentClientMessage{Message: &gen.AgentClientMessage_KvClientMessage{KvClientMessage: resp}})

	default:
		// Unknown KV request kind: no response we can construct; safely
		// ignored rather than failing the whole exchange.
		return nil
	}
}
