package client

import (
	"context"
	"sync"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
)

var timerPool = sync.Pool{New: func() any { return time.NewTimer(0) }}

func getTimer(d time.Duration) *time.Timer {
	t := timerPool.Get().(*time.Timer)
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
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
	timer := getTimer(timeout)
	select {
	case resp := <-replyCh:
		putTimer(timer)
		return resp, nil
	case <-timer.C:
		putTimer(timer)
		return transport.Response{}, context.DeadlineExceeded
	case <-ctx.Done():
		putTimer(timer)
		return transport.Response{}, ctx.Err()
	}
}

// waitReplyOrProxiesChanged is like waitReply but also wakes on proxy list
// changes. Used by commit to detect mid-commit topology changes (C++
// onProxiesChanged). If the proxy set changes, the commit result is unknown
// — the proxy may have been removed before processing our commit.
func waitReplyOrProxiesChanged(replyCh <-chan transport.Response, ctx context.Context, timeout time.Duration, proxiesChanged <-chan struct{}) (transport.Response, error) {
	timer := getTimer(timeout)
	select {
	case resp := <-replyCh:
		putTimer(timer)
		return resp, nil
	case <-proxiesChanged:
		putTimer(timer)
		return transport.Response{}, context.DeadlineExceeded
	case <-timer.C:
		putTimer(timer)
		return transport.Response{}, context.DeadlineExceeded
	case <-ctx.Done():
		putTimer(timer)
		return transport.Response{}, ctx.Err()
	}
}
