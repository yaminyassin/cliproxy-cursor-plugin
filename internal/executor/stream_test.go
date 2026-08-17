package executor

import (
	"bytes"
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/router-for-me/cliproxy-cursor-plugin/internal/cursorpb/gen"
)

// TestRunCursorStream_NoGoroutineLeak_WithBackgroundContext regressions
// the writer-goroutine lifecycle bug found during rework: runCursorStream
// is always called with context.Background() in production
// (executor.go's run, invoked from dispatch.go's HandleMethod), which
// never cancels on its own. An earlier version of the write goroutine
// blocked on <-ctx.Done() to know when to stop, which would never fire
// for context.Background() and leaked one goroutine per request forever.
// This test calls runCursorStream many times with context.Background()
// and asserts the goroutine count returns to baseline afterward, ignoring
// a small fixed margin for the HTTP/2 transport's own long-lived
// connection-management goroutines (readLoop/writeLoop per pooled
// connection), which are normal and not per-request.
func TestRunCursorStream_NoGoroutineLeak_WithBackgroundContext(t *testing.T) {
	fake := newFakeCursorRunServer(t)
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

	// Warm up the connection once first, so the HTTP/2 transport's own
	// pooled-connection goroutines (readLoop/writeLoop) are already
	// running before baseline is captured - those are per-connection,
	// not per-request, and must not be counted as a request-driven leak.
	blobsWarm := newBlobStore()
	if _, err := client.runCursorStream(context.Background(), fake.server.URL, "test-token", runReq, blobsWarm); err != nil {
		t.Fatalf("warm-up runCursorStream failed: %v", err)
	}

	runtime.GC()
	baseline := runtime.NumGoroutine()

	const iterations = 20
	for i := 0; i < iterations; i++ {
		blobs := newBlobStore()
		// context.Background() deliberately, matching production usage -
		// this is the exact condition that leaked before the fix.
		if _, err := client.runCursorStream(context.Background(), fake.server.URL, "test-token", runReq, blobs); err != nil {
			t.Fatalf("iteration %d: runCursorStream failed: %v", i, err)
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	var after int
	for {
		runtime.GC()
		after = runtime.NumGoroutine()
		if after <= baseline+1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if after > baseline+1 {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Errorf(
			"goroutine count after %d runCursorStream calls = %d, baseline (post warm-up) = %d (a leak scaling with call count, not a fixed connection-pool overhead, would indicate the writer-goroutine lifecycle bug). Stacks:\n%s",
			iterations, after, baseline, string(bytes.TrimSpace(buf[:n])),
		)
	}
}
