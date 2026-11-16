package sync

import (
	stdsync "sync"
	"time"

	"github.com/fx/health-agent-go/models"
)

const (
	bufferMaxEntries = 60            // ~10 min at 10 s/interval
	bufferMaxAge     = 10 * time.Minute
)

type bufferedEntry struct {
	payload    models.AgentPayload
	bufferedAt time.Time
}

type callbackBuffer struct {
	mu      stdsync.Mutex
	entries []bufferedEntry
}

// package-level singleton — one per agent process.
var _cbBuffer = &callbackBuffer{}

// bufferPayload enqueues a payload that could not be delivered to the manager.
// Entries older than bufferMaxAge are evicted; the oldest is dropped when the
// buffer is full so memory stays bounded.
func bufferPayload(p models.AgentPayload) {
	_cbBuffer.add(p)
}

// drainBuffer returns all buffered payloads oldest-first and clears the buffer.
func drainBuffer() []models.AgentPayload {
	return _cbBuffer.drain()
}

func (b *callbackBuffer) add(p models.AgentPayload) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-bufferMaxAge)
	// evict stale entries in-place
	n := 0
	for _, e := range b.entries {
		if e.bufferedAt.After(cutoff) {
			b.entries[n] = e
			n++
		}
	}
	b.entries = b.entries[:n]
	// drop oldest when at capacity
	if len(b.entries) >= bufferMaxEntries {
		b.entries = b.entries[1:]
	}
	b.entries = append(b.entries, bufferedEntry{payload: p, bufferedAt: now})
}

func (b *callbackBuffer) drain() []models.AgentPayload {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.entries) == 0 {
		return nil
	}
	out := make([]models.AgentPayload, len(b.entries))
	for i, e := range b.entries {
		out[i] = e.payload
	}
	b.entries = b.entries[:0]
	return out
}
