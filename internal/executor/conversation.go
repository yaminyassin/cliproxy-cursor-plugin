package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/router-for-me/cliproxy-cursor-plugin/internal/cursorpb/gen"
)

// Cursor's Run protocol is conversation-scoped: a request carries a
// conversation_id, and the blob store is a content-addressed CACHE that
// Cursor pulls from over the KV handshake. gajae-code keeps both per
// conversation (conversationBlobStores / conversationStateCache in
// streamCursor) and refreshes the state from each
// ConversationCheckpointUpdate.
//
// This plugin previously created a fresh blobStore for every request and
// never set conversation_id at all, so Cursor could not correlate turns
// and had to re-fetch every blob for the entire history on each turn. The
// KV round trips therefore grew with conversation length, which matches
// the escalating per-turn latency observed live on 2026-08-19 (20s -> 43s
// -> two turns pinned at 2m0s late in one session).
//
// Requests are stateless at the ABI boundary, so the conversation id is
// derived deterministically from the conversation's opening messages
// (which never change as history grows) rather than carried by the
// client.

const (
	// conversationTTL bounds how long an idle conversation's cached blobs
	// and state are retained.
	conversationTTL = 30 * time.Minute
	// maxCachedConversations bounds total retained conversations so a long
	// running host cannot grow this cache without limit.
	maxCachedConversations = 64
)

// conversationEntry holds the per-conversation state reused across turns.
type conversationEntry struct {
	blobs *blobStore
	// checkpoint is the most recent ConversationStateStructure Cursor
	// acknowledged for this conversation. Reusing it preserves the
	// non-history state Cursor accumulated (todos, pending tool calls,
	// file state, summaries) instead of resetting it every turn.
	checkpoint *gen.ConversationStateStructure
	lastAccess time.Time
}

// conversationCache is a TTL + LRU bounded store of conversation state.
type conversationCache struct {
	mu      sync.Mutex
	entries map[string]*conversationEntry
}

func newConversationCache() *conversationCache {
	return &conversationCache{entries: make(map[string]*conversationEntry)}
}

// acquire returns the entry for a conversation, creating it when absent,
// and evicts TTL-stale and overflowing entries.
func (c *conversationCache) acquire(id string) *conversationEntry {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	for key, entry := range c.entries {
		if key != id && now.Sub(entry.lastAccess) > conversationTTL {
			delete(c.entries, key)
		}
	}

	entry, ok := c.entries[id]
	if !ok {
		entry = &conversationEntry{blobs: newBlobStore()}
		c.entries[id] = entry
	}
	entry.lastAccess = now

	// Bound total size by dropping the least recently used entries.
	for len(c.entries) > maxCachedConversations {
		var oldestKey string
		var oldest time.Time
		for key, candidate := range c.entries {
			if key == id {
				continue
			}
			if oldest.IsZero() || candidate.lastAccess.Before(oldest) {
				oldestKey, oldest = key, candidate.lastAccess
			}
		}
		if oldestKey == "" {
			break
		}
		delete(c.entries, oldestKey)
	}

	return entry
}

// storeCheckpoint records the latest state Cursor acknowledged so the next
// turn can build on it instead of starting from scratch.
func (c *conversationCache) storeCheckpoint(id string, checkpoint *gen.ConversationStateStructure) {
	if checkpoint == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[id]
	if !ok {
		return
	}
	// Clone: the checkpoint came off the wire and must not be mutated by
	// later turns that adjust turns/root-prompt on the reused base.
	cloned, ok2 := proto.Clone(checkpoint).(*gen.ConversationStateStructure)
	if !ok2 {
		return
	}
	entry.checkpoint = cloned
	entry.lastAccess = time.Now()
}

// conversationIDFor derives a stable id for a conversation from its
// opening messages. The first system prompt and first user message do not
// change as a conversation grows, so every turn of the same conversation
// maps to the same id, letting Cursor correlate them and letting the blob
// cache stay warm. A conversation with no usable opening content falls
// back to a per-request id, which simply means no reuse (correct, just
// slower) rather than colliding with an unrelated conversation.
func conversationIDFor(messages []chatMessage) string {
	hash := sha256.New()
	wroteAny := false
	for _, role := range []string{"system", "user"} {
		for _, msg := range messages {
			if msg.Role != role || msg.Content == "" {
				continue
			}
			hash.Write([]byte(role))
			hash.Write([]byte{0})
			hash.Write([]byte(msg.Content))
			wroteAny = true
			break
		}
	}
	if !wroteAny {
		generated, err := newMessageID()
		if err != nil {
			return "cursor-conversation-fallback"
		}
		return "cursor-conversation-" + generated
	}
	return "cursor-conversation-" + hex.EncodeToString(hash.Sum(nil)[:16])
}
