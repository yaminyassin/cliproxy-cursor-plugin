package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
)

// blobStore is Cursor's content-addressed blob store: values are keyed
// by the SHA-256 hash of their bytes, matching gajae-code's
// createBlobId/storeCursorBlob/readCursorBlob. ConversationState fields
// such as turns[], root_prompt_messages_json[], and each turn's
// user_message/steps carry these blob IDs on the wire, not the raw bytes
// directly - Cursor resolves them via the KvServerMessage/KvClientMessage
// getBlobArgs/setBlobArgs handshake during the live Run exchange (see
// stream.go).
type blobStore struct {
	mu   sync.Mutex
	data map[string][]byte
}

func newBlobStore() *blobStore {
	return &blobStore{data: make(map[string][]byte)}
}

// put stores data and returns its content-addressed blob ID (the raw
// SHA-256 digest, matching gajae-code's createBlobId). Use this for
// blobs the client itself originates (turns, root prompt entries).
func (b *blobStore) put(data []byte) []byte {
	sum := sha256.Sum256(data)
	id := sum[:]
	b.putKnownID(id, data)
	return id
}

// putKnownID stores data under a blob ID supplied by the server (used
// when answering a setBlobArgs request mid-stream, where Cursor dictates
// the id rather than the client deriving it).
func (b *blobStore) putKnownID(id, data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data[hex.EncodeToString(id)] = append([]byte(nil), data...)
}

// get resolves a blob ID to its stored data, used both when answering a
// server-initiated getBlobArgs request and when a caller needs to read
// back a blob it stored earlier.
func (b *blobStore) get(id []byte) ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	data, ok := b.data[hex.EncodeToString(id)]
	return data, ok
}
