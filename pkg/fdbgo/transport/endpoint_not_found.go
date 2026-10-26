package transport

import (
	"errors"
	"fmt"

	"fdb.dev/pkg/fdbgo/wire"
)

// pendingEntry is a registered reply channel plus the request's destination
// token, so an ENDPOINT_NOT_FOUND for that destination can fail it.
type pendingEntry struct {
	ch   chan Response
	dest UID // zero when the destination was not bound
}

// WLTokenEndpointNotFound is WLTOKEN_ENDPOINT_NOT_FOUND (FlowTransport.h:39).
const WLTokenEndpointNotFound = 0

var endpointNotFoundToken = WellKnownToken(WLTokenEndpointNotFound)

const wellKnownFirst = ^uint64(0)

// uidFileID is UID::file_identifier (flow/include/flow/IRandom.h:68); the
// packet body is SerializeSource<UID> (FlowTransport.actor.cpp:1229-1236).
const uidFileID uint32 = 15597147

// ErrEndpointNotFound identifies a reply failed because the server no longer
// has the request's endpoint. It is NOT a connection teardown.
var ErrEndpointNotFound = errors.New("endpoint not found")

// EndpointNotFoundError is delivered to every pending reply whose request went
// to Token. It matches ErrEndpointNotFound and FDB code 1030: C++ tryGetReply
// completes such a request with request_maybe_delivered
// (genericactors.actor.h:375-378) and leaves the connection alone.
type EndpointNotFoundError struct {
	Token UID
}

func (e *EndpointNotFoundError) Error() string {
	return fmt.Sprintf("endpoint %016x:%016x not found (request_maybe_delivered)", e.Token.First, e.Token.Second)
}

func (e *EndpointNotFoundError) Unwrap() []error {
	return []error{ErrEndpointNotFound, &wire.FDBError{Code: codeRequestMaybeDelivered}}
}

// SetEndpointNotFoundHandler installs fn to observe every ENDPOINT_NOT_FOUND
// token this connection receives, whether or not a reply is still waiting.
func (c *Conn) SetEndpointNotFoundHandler(fn func(UID)) {
	c.onEndpointNotFound.Store(&fn)
}

// decodeEndpointNotFound reads the UID from the packet body: a FakeRoot table
// whose field 0 is the 16-byte UID inline (flat_buffers.h:1332-1341).
func decodeEndpointNotFound(body []byte) (UID, bool) {
	r, err := wire.ReaderAtRootObject(body)
	if err != nil || r.FileIdentifier() != uidFileID {
		return UID{}, false
	}
	off := r.FieldOffset(0)
	if off < 4 || off+16 > len(r.ObjectBytes()) {
		return UID{}, false
	}
	first, second := r.ReadUIDPair(0)
	return UID{First: first, Second: second}, true
}

// handleEndpointNotFound fails every pending reply sent to the missing
// endpoint. Rare, so a scan of pending beats indexing by destination on the
// per-RPC hot path.
func (c *Conn) handleEndpointNotFound(body []byte) {
	token, ok := decodeEndpointNotFound(body)
	// C++ ignores well-known tokens (FailureMonitor.actor.cpp:119-126).
	if !ok || token.First == wellKnownFirst || token == (UID{}) {
		return
	}
	if fn := c.onEndpointNotFound.Load(); fn != nil {
		(*fn)(token)
	}
	var err error
	for i := range c.pendingShards {
		s := &c.pendingShards[i]
		s.mu.Lock()
		for reply, entry := range s.m {
			if entry.dest != token {
				continue
			}
			if err == nil {
				err = &EndpointNotFoundError{Token: token}
			}
			// Same single-producer discipline as readLoop's delivery.
			entry.ch <- Response{Err: err}
			delete(s.m, reply)
		}
		s.mu.Unlock()
	}
}
