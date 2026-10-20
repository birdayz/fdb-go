package client

import (
	"context"

	"fdb.dev/pkg/fdbgo/internal/sendqueue"
	"fdb.dev/pkg/fdbgo/transport"
)

// getOrDialForSend reserves a cold peer's outbox position before its first
// suspension. The caller owns the permit until transport acceptance or abort.
func (db *database) getOrDialForSend(ctx context.Context, addr string) (*transport.Conn, *sendqueue.Permit, error) {
	conn, _, permit, err := db.getOrDialConnFor(ctx, addr, true)
	return conn, permit, err
}

func (db *database) getOrDialConnFor(ctx context.Context, addr string, sending bool) (conn *transport.Conn, owner bool, permit *sendqueue.Permit, err error) {
	if err := ctx.Err(); err != nil {
		return nil, false, nil, err
	}
	db.connMu.Lock()
	call := db.dialing[addr]
	if call != nil {
		select {
		case <-call.done:
			drained := call.sends.SealIfEmpty()
			if call.err != nil || call.conn == nil || call.conn.IsClosed() || drained {
				if db.dialing[addr] == call {
					delete(db.dialing, addr)
				}
				if call.conn != nil && drained {
					call.conn.ClearSendOrder(&call.sends)
				}
				call = nil
			}
		default: // running dials survive even when all current senders canceled
		}
	}
	// A retained cold generation takes precedence over the newly pooled socket:
	// returning that socket early would let a newcomer overtake its dial waiters.
	if !sending || call == nil {
		if c := db.connPool[addr]; c != nil {
			if !c.IsClosed() {
				db.connMu.Unlock()
				return c, false, nil, nil
			}
			delete(db.connPool, addr)
		}
	}
	if call == nil {
		call = &dialCall{done: make(chan struct{})}
		db.dialing[addr] = call
		owner = true
	}
	if sending {
		permit, _ = call.sends.Reserve()
	}
	db.connMu.Unlock()
	owned := permit
	transferred := false
	defer func() {
		if !transferred {
			owned.Release()
		}
	}()
	if owner {
		go db.dialAndPool(addr, call)
	}
	select {
	case <-call.done:
	default:
		notifyParked(ctx)
	}
	select {
	case <-call.done:
		if call.err != nil {
			return nil, false, nil, call.err
		}
	case <-ctx.Done():
		return nil, false, nil, ctx.Err()
	}
	if permit != nil {
		select {
		case <-permit.Ready():
		default:
			notifyParked(ctx)
		}
		select {
		case <-permit.Ready():
		case <-ctx.Done():
			return nil, false, nil, ctx.Err()
		case <-call.conn.Done():
			return nil, false, nil, &transport.ConnClosedError{}
		case <-db.ctx.Done():
			return nil, false, nil, db.ctx.Err()
		}
		if err := ctx.Err(); err != nil {
			return nil, false, nil, err
		}
	}
	transferred = true
	return call.conn, owner, permit, nil
}

type sendGuard struct{ permit *sendqueue.Permit }

func (g *sendGuard) close() {
	g.permit.Release()
	g.permit = nil
}
