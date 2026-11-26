package transport

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"sync"
	"sync/atomic"
)

// replyChanPool pools chan Response to reduce allocations. A channel is
// returned to the pool only when no send can still race it: by the SUCCESS path
// (the caller received its reply, then calls ReplyHandle.Release / SendAndWait
// returns) or by Cancel when it won the delete race against readLoop. A channel
// whose delivery raced a cancel/timeout is deliberately NOT pooled — it may hold
// a stale buffered value, and a future request must never read it. readLoop does
// NOT return the channel (it only delivers); the earlier comment claiming it did
// was wrong and caused the success-path channel to leak.
var replyChanPool = sync.Pool{New: func() any { return make(chan Response, 1) }}

// ReplyHandle holds state for a pending reply. Pooled to avoid closure allocation
// in PrepareReply (the cancel closure captured conn+token+ch, causing a heap alloc
// on every RPC call — ~9% of total allocs in SaveRecord benchmarks).
type ReplyHandle struct {
	conn  *Conn
	token UID
	ch    chan Response
}

var replyHandlePool = sync.Pool{New: func() any { return &ReplyHandle{} }}

// Cancel removes the pending reply from the connection's pending map
// and returns the reply channel to the pool. Safe to call on a handle
// with a nil conn (no-op).
func (h *ReplyHandle) Cancel() {
	if h.conn == nil {
		return
	}
	s := h.conn.pendingShard(h.token)
	s.mu.Lock()
	if _, ok := s.m[h.token]; ok {
		// Won the race: the token is still pending, so readLoop has not
		// delivered and never will (we delete it here under the same lock).
		// The channel is clean — pool it.
		delete(s.m, h.token)
		h.conn.putReplyCh(h.ch)
	}
	// Else readLoop already delivered (or is mid-delivery): h.ch may hold a
	// buffered value, so we must NOT pool it — leave it to GC. This rare
	// race-loser leak is the price of never handing a stale reply to a future
	// request.
	s.mu.Unlock()
	h.ch = nil // mark the channel handled so Release does not re-pool it
}

// TakeReadyOrCancel chooses response publication or cancellation under the
// connection's reply lock. The caller must exclusively own this handle and
// channel (no concurrent receive/Cancel/Release). Release is still required.
func (h *ReplyHandle) TakeReadyOrCancel() (Response, bool) {
	if h.conn == nil {
		return Response{}, false
	}
	s := h.conn.pendingShard(h.token)
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case response := <-h.ch:
		delete(s.m, h.token)
		return response, true
	default:
		if _, pending := s.m[h.token]; pending {
			delete(s.m, h.token)
			h.conn.putReplyCh(h.ch)
		}
		h.ch = nil
		return Response{}, false
	}
}

// KeepReadyOrCancel leaves a published response for the caller's normal receive,
// or cancels an unpublished reply atomically. Like TakeReadyOrCancel, this needs
// exclusive handle ownership. It never adds a second channel producer.
func (h *ReplyHandle) KeepReadyOrCancel() bool {
	if h.conn == nil {
		return false
	}
	s := h.conn.pendingShard(h.token)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(h.ch) != 0 {
		return true
	}
	if _, pending := s.m[h.token]; pending {
		delete(s.m, h.token)
		h.conn.putReplyCh(h.ch)
	}
	h.ch = nil
	return false
}

// NotifyWait marks suspension only when the response is not already published.
// The caller owns the handle and has not received from its channel yet.
func (h *ReplyHandle) NotifyWait(ctx context.Context) {
	if len(h.ch) == 0 {
		NotifySuspend(ctx)
	}
}

// Release returns the handle to the pool. Call EITHER after a successful receive
// (no Cancel) OR after Cancel. On the success path h.ch is still set and is
// pooled here: response publication and token deletion share the token's
// pendingShard.mu lock, in that order. Once the caller receives the response,
// that publisher has no further channel send, and the lock excludes another
// publisher until deletion. Cancel nils h.ch, so the cancel path never double-pools.
func (h *ReplyHandle) Release() {
	if h.ch != nil {
		h.conn.putReplyCh(h.ch)
	}
	h.conn = nil
	h.ch = nil
	replyHandlePool.Put(h)
}

// cancelPending removes a still-pending reply token and returns its channel to
// the pool iff it won the race against readLoop's delivery — the same discipline
// as ReplyHandle.Cancel, for the raw token+channel SendAndWait holds.
func (c *Conn) cancelPending(token UID, ch chan Response) {
	s := c.pendingShard(token)
	s.mu.Lock()
	if _, ok := s.m[token]; ok {
		delete(s.m, token)
		c.putReplyCh(ch)
	}
	s.mu.Unlock()
}

// PrepareReply allocates a reply token and registers it for response routing.
// Use this when you need the token BEFORE building the request body
// (e.g., to embed it in the FDB request's Reply field).
//
// Returns a ReplyHandle whose Cancel() removes the pending token. Call
// handle.Release() when done to return it to the pool.
func (c *Conn) PrepareReply() (UID, <-chan Response, *ReplyHandle) {
	return c.PrepareReplyTo(UID{})
}

// PrepareReplyTo is PrepareReply for a request that will be sent to dest, so an
// ENDPOINT_NOT_FOUND for dest fails this reply instead of leaving it to time out.
func (c *Conn) PrepareReplyTo(dest UID) (UID, <-chan Response, *ReplyHandle) {
	h := replyHandlePool.Get().(*ReplyHandle)
	token := c.nextReplyToken()
	ch := getReplyChannel()
	h.conn = c
	h.token = token
	h.ch = ch
	c.putPending(token, pendingEntry{ch: ch, dest: dest})
	return token, ch, h
}

// HasPendingReply reports whether token still owns a reply registration.
func (c *Conn) HasPendingReply(token UID) bool {
	s := c.pendingShard(token)
	s.mu.Lock()
	_, pending := s.m[token]
	s.mu.Unlock()
	return pending
}

// getReplyChannel gets a reply channel from the pool.
func getReplyChannel() chan Response {
	return replyChanPool.Get().(chan Response)
}

// putReplyCh returns a reply channel to the pool after draining it,
// routing through connTestHooks.putReplyChannel when installed in tests so
// pool-discipline tests can observe exactly which channels are returned to the
// pool, on which path, without depending on sync.Pool's non-deterministic reuse.
func (c *Conn) putReplyCh(ch chan Response) {
	if c != nil {
		if h := c.hooks.Load(); h != nil && h.putReplyChannel != nil {
			h.putReplyChannel(ch)
			return
		}
	}
	putReplyChannelImpl(ch)
}

func putReplyChannelImpl(ch chan Response) {
	// Drain any buffered value (shouldn't normally happen).
	select {
	case <-ch:
	default:
	}
	replyChanPool.Put(ch)
}

// fastRNGState is the shared, atomically advanced PRNG state for UID generation.
// Uses SplitMix64 (same as java.util.SplittableRandom). Seeded from crypto/rand.
// Reply tokens only need uniqueness within a connection, not crypto security.
var fastRNGState atomic.Uint64

func init() {
	var buf [8]byte
	rand.Read(buf[:])
	fastRNGState.Store(binary.LittleEndian.Uint64(buf[:]))
}

// numPendingShards is the number of reply-map shards per connection (power of two).
const numPendingShards = 32

type pendingShard struct {
	mu sync.Mutex
	m  map[UID]pendingEntry
	_  [48]byte // cache-line padding (64B total on 64-bit architectures)
}

func (c *Conn) pendingShard(token UID) *pendingShard {
	return &c.pendingShards[(token.First^(token.First>>17))&(numPendingShards-1)]
}

func (c *Conn) putPending(token UID, entry pendingEntry) {
	s := c.pendingShard(token)
	s.mu.Lock()
	if s.m == nil {
		s.m = make(map[UID]pendingEntry, 4)
	}
	s.m[token] = entry
	s.mu.Unlock()
}

func (c *Conn) deliverPending(token UID, resp Response) {
	s := c.pendingShard(token)
	s.mu.Lock()
	if entry, ok := s.m[token]; ok {
		entry.ch <- resp
		delete(s.m, token)
	}
	s.mu.Unlock()
}

func (c *Conn) removePending(token UID) {
	s := c.pendingShard(token)
	s.mu.Lock()
	delete(s.m, token)
	s.mu.Unlock()
}

func (c *Conn) clearAllPending() {
	for i := range c.pendingShards {
		s := &c.pendingShards[i]
		s.mu.Lock()
		s.m = nil
		s.mu.Unlock()
	}
}

func (c *Conn) hasAnyPending() bool {
	for i := range c.pendingShards {
		s := &c.pendingShards[i]
		s.mu.Lock()
		n := len(s.m)
		s.mu.Unlock()
		if n > 0 {
			return true
		}
	}
	return false
}

func (c *Conn) pendingCount() int {
	n := 0
	for i := range c.pendingShards {
		s := &c.pendingShards[i]
		s.mu.Lock()
		n += len(s.m)
		s.mu.Unlock()
	}
	return n
}

// nextReplyToken generates a connection-unique 128-bit reply token using one
// atomic increment and SplitMix64 finalizer (spreading uniformly across shards).
func (c *Conn) nextReplyToken() UID {
	if c.nextToken.Load() == 0 {
		c.nextToken.CompareAndSwap(0, splitmix64()|1)
	}
	s := c.nextToken.Add(0x9e3779b97f4a7c15)
	a := mix64(s)
	if a == wellKnownFirst {
		a ^= 1
	}
	return UID{First: a, Second: s}
}

// splitmix64 is a fast 64-bit PRNG. Period: 2^64.
func splitmix64() uint64 {
	// Atomic add gives each caller a unique state progression.
	s := fastRNGState.Add(0x9e3779b97f4a7c15)
	return mix64(s)
}

func mix64(s uint64) uint64 {
	s = (s ^ (s >> 30)) * 0xbf58476d1ce4e5b9
	s = (s ^ (s >> 27)) * 0x94d049bb133111eb
	return s ^ (s >> 31)
}

// NewUID generates a random 128-bit UID for endpoint tokens.
// Uses a single atomic progression on fastRNGState per 128-bit UID.
func NewUID() UID {
	s := fastRNGState.Add(0x9e3779b97f4a7c15)
	return UID{
		First:  mix64(s),
		Second: mix64(s ^ 0x6a09e667f3bcc909),
	}
}

func newConnectionID() uint64 {
	// Connection IDs use crypto/rand for true uniqueness across processes.
	var buf [8]byte
	rand.Read(buf[:])
	return binary.LittleEndian.Uint64(buf[:])
}
