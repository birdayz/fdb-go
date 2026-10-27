package client

import (
	"context"
	"errors"
	"sync"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
)

// errReplyTimeout is the INTERNAL signal that an RPC reply did not arrive
// within DefaultRPCTimeout. It is deliberately distinct from caller-context
// cancellation (ctx.Err()): libfdb_c's loadBalance imposes NO per-read client
// timeout — a slow-but-alive storage server is simply re-sent (across
// alternatives, with backoff) until it replies or the transaction's read
// version ages out and the server itself returns transaction_too_old
// (NativeAPI.actor.cpp getValue/getKeyValues catch only wrong_shard_server /
// all_alternatives_failed and retry the read loop; all_alternatives_failed is
// handled internally and never reaches the application). This sentinel must
// therefore NEVER escape the client to the application: the READ paths convert
// it to a bounded re-send and, on exhaustion, to a RETRYABLE
// transaction_too_old. A caller's own deadline/cancellation still propagates as
// ctx.Err(). The commit path keeps its own (commit_unknown_result) semantics
// and does not use this sentinel.
var errReplyTimeout = errors.New("fdbgo: rpc reply timeout (internal; retry the read)")

// isReplyTimeout reports whether err is the internal RPC reply-timeout signal.
func isReplyTimeout(err error) bool { return errors.Is(err, errReplyTimeout) }

var timerPool = sync.Pool{New: func() any { return time.NewTimer(0) }}

func getTimer(d time.Duration) *time.Timer {
	t := timerPool.Get().(*time.Timer)
	t.Reset(d)
	return t
}

func putTimer(t *time.Timer) {
	t.Stop()
	timerPool.Put(t)
}

// waitReply waits for an RPC response with timeout, avoiding context.WithTimeout allocation.
// Returns (response, nil) on success, or (zero, error) on timeout/cancellation.
func waitReply(replyCh <-chan transport.Response, ctx context.Context, timeout time.Duration) (transport.Response, error) {
	select {
	case resp := <-replyCh:
		return resp, nil
	default:
	}
	timer := getTimer(timeout)
	select {
	case resp := <-replyCh:
		putTimer(timer)
		return resp, nil
	case <-timer.C:
		putTimer(timer)
		// Internal reply timeout, not a caller deadline. Storage-read paths
		// re-send on this; it must never escape as a terminal error.
		return transport.Response{}, errReplyTimeout
	case <-ctx.Done():
		putTimer(timer)
		select {
		case resp := <-replyCh:
			return resp, nil
		default:
			return transport.Response{}, ctx.Err()
		}
	}
}

// C++ tryCommit has no reply timer (NativeAPI.actor.cpp:6646–6651).
func waitReplyOrProxiesChanged(replyCh <-chan transport.Response, ctx context.Context, proxiesChanged <-chan struct{}) (transport.Response, error) {
	select {
	case resp := <-replyCh:
		return resp, nil
	case <-proxiesChanged:
		return transport.Response{}, &wire.FDBError{Code: ErrCommitUnknownResult}
	case <-ctx.Done():
		return transport.Response{}, ctx.Err()
	}
}
