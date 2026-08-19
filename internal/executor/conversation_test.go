package executor

import (
	"testing"

	"github.com/router-for-me/cliproxy-cursor-plugin/internal/cursorpb/gen"
)

// TestConversationIDFor_StableAsHistoryGrows is the property the whole
// conversation cache depends on: every turn of one conversation must map to
// the same id, so Cursor can correlate them and the blob cache stays warm.
// Requests are stateless at the ABI boundary, so the id is derived from the
// opening messages, which do not change as history accumulates.
func TestConversationIDFor_StableAsHistoryGrows(t *testing.T) {
	turn1 := []chatMessage{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: "read go.mod"},
	}
	turn2 := append(append([]chatMessage{}, turn1...),
		chatMessage{Role: "assistant", Content: "sure"},
		chatMessage{Role: "user", Content: "now read main.go"},
	)
	turn3 := append(append([]chatMessage{}, turn2...),
		chatMessage{Role: "assistant", Content: "done"},
		chatMessage{Role: "user", Content: "and the Makefile"},
	)

	id1, id2, id3 := conversationIDFor(turn1), conversationIDFor(turn2), conversationIDFor(turn3)
	if id1 != id2 || id2 != id3 {
		t.Errorf("conversation id must be stable across turns, got %q / %q / %q", id1, id2, id3)
	}
	if id1 == "" {
		t.Errorf("expected a non-empty conversation id")
	}
}

// TestConversationIDFor_DistinguishesDifferentConversations verifies two
// unrelated conversations do not share a cache entry (which would leak one
// conversation's blobs and acknowledged state into the other).
func TestConversationIDFor_DistinguishesDifferentConversations(t *testing.T) {
	a := conversationIDFor([]chatMessage{{Role: "user", Content: "read go.mod"}})
	b := conversationIDFor([]chatMessage{{Role: "user", Content: "delete everything"}})
	if a == b {
		t.Errorf("different conversations must not share an id, both were %q", a)
	}

	// Same user message but a different system prompt is a different
	// conversation too.
	c := conversationIDFor([]chatMessage{
		{Role: "system", Content: "You are terse."},
		{Role: "user", Content: "read go.mod"},
	})
	if c == a {
		t.Errorf("differing system prompts must produce different ids, both were %q", a)
	}
}

// TestConversationCache_ReusesBlobsAcrossTurns verifies the blob store is
// genuinely shared across turns of one conversation. Without this, Cursor
// re-fetches every blob for the whole history on every turn, which is what
// made per-turn latency escalate live on 2026-08-19.
func TestConversationCache_ReusesBlobsAcrossTurns(t *testing.T) {
	cache := newConversationCache()

	first := cache.acquire("conv-a")
	id := first.blobs.put([]byte("turn-1 payload"))

	second := cache.acquire("conv-a")
	if second.blobs != first.blobs {
		t.Fatalf("expected the same blobStore instance across turns")
	}
	if _, ok := second.blobs.get(id); !ok {
		t.Errorf("expected a blob stored on turn 1 to still be resolvable on turn 2")
	}

	other := cache.acquire("conv-b")
	if _, ok := other.blobs.get(id); ok {
		t.Errorf("a different conversation must not see another's blobs")
	}
}

// TestConversationCache_StoresAndReusesCheckpoint verifies Cursor's
// acknowledged state is retained for the next turn, and that it is cloned
// rather than aliased so a later turn cannot mutate the cached copy.
func TestConversationCache_StoresAndReusesCheckpoint(t *testing.T) {
	cache := newConversationCache()
	cache.acquire("conv-a")

	checkpoint := &gen.ConversationStateStructure{
		Turns:                  [][]byte{[]byte("turn-blob-id")},
		RootPromptMessagesJson: [][]byte{[]byte("root-blob-id")},
	}
	cache.storeCheckpoint("conv-a", checkpoint)

	entry := cache.acquire("conv-a")
	if entry.checkpoint == nil {
		t.Fatalf("expected the checkpoint to be retained for the next turn")
	}
	if entry.checkpoint == checkpoint {
		t.Errorf("checkpoint must be cloned, not aliased to the wire message")
	}
	if len(entry.checkpoint.GetTurns()) != 1 {
		t.Errorf("expected the cloned checkpoint to preserve contents, got %+v", entry.checkpoint)
	}

	// Storing for an unknown conversation must not panic or create state.
	cache.storeCheckpoint("conv-unknown", checkpoint)
}

// TestBuildAgentRunRequest_SetsConversationIDAndReusesBaseState verifies the
// request actually carries conversation_id (so Cursor can correlate turns)
// and that a cached checkpoint's non-history fields survive while the
// history fields are replaced with this turn's encoding.
func TestBuildAgentRunRequest_SetsConversationIDAndReusesBaseState(t *testing.T) {
	blobs := newBlobStore()
	req := chatCompletionsRequest{
		Model:    "cursor-fast",
		Messages: []chatMessage{{Role: "user", Content: "hello"}},
	}
	base := &gen.ConversationStateStructure{
		Turns:                  [][]byte{[]byte("stale-turn")},
		RootPromptMessagesJson: [][]byte{[]byte("stale-root")},
		Todos:                  [][]byte{[]byte("todo-blob-id")},
		PendingToolCalls:       []string{"pending-1"},
	}

	agentReq, err := buildAgentRunRequest(req, blobs, "conv-xyz", base)
	if err != nil {
		t.Fatalf("buildAgentRunRequest failed: %v", err)
	}

	if agentReq.GetConversationId() != "conv-xyz" {
		t.Errorf("conversation_id = %q, want %q", agentReq.GetConversationId(), "conv-xyz")
	}

	state := agentReq.GetConversationState()
	if len(state.GetTodos()) != 1 || string(state.GetTodos()[0]) != "todo-blob-id" {
		t.Errorf("expected non-history state from the checkpoint to survive, got %+v", state.GetTodos())
	}
	if len(state.GetPendingToolCalls()) != 1 || state.GetPendingToolCalls()[0] != "pending-1" {
		t.Errorf("expected pending tool calls to survive, got %+v", state.GetPendingToolCalls())
	}
	// History fields must be this turn's encoding, not the stale base.
	for _, turn := range state.GetTurns() {
		if string(turn) == "stale-turn" {
			t.Errorf("expected turns to be replaced with this turn's encoding, found the stale base value")
		}
	}
	for _, root := range state.GetRootPromptMessagesJson() {
		if string(root) == "stale-root" {
			t.Errorf("expected root prompt to be replaced with this turn's encoding, found the stale base value")
		}
	}

	// The base must not be mutated in place.
	if len(base.GetTurns()) != 1 || string(base.GetTurns()[0]) != "stale-turn" {
		t.Errorf("the cached base state was mutated: %+v", base.GetTurns())
	}
}
