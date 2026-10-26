package client

import (
	"context"
	"net"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
)

func guardTestConn(t *testing.T) *transport.Conn {
	t.Helper()
	peer := pipePeer(t, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := transport.Dial(ctx, "reply-guard", nil, func(context.Context, string, string) (net.Conn, error) { return peer, nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestReplyGuardOwnership(t *testing.T) {
	t.Parallel()
	conn := guardTestConn(t)
	for _, mode := range []string{"panic", "release", "transfer"} {
		t.Run(mode, func(t *testing.T) {
			token, _, handle := conn.PrepareReply()
			if !conn.HasPendingReply(token) {
				t.Fatal("positive control: no reply registration")
			}
			guard := replyGuard{handle: handle}
			func() {
				defer func() {
					if r := recover(); (r != nil) != (mode == "panic") {
						t.Errorf("panic = %v for mode %s", r, mode)
					}
				}()
				defer guard.close()
				switch mode {
				case "panic":
					panic("request builder")
				case "release":
					handle.Cancel()
					guard.release()
				case "transfer":
					guard.transfer()
				}
			}()
			if pending := conn.HasPendingReply(token); pending != (mode == "transfer") {
				t.Fatalf("pending after %s = %t", mode, pending)
			}
			if mode == "transfer" {
				next := replyGuard{handle: handle}
				next.close()
			}
			// A closed/disarmed guard must not touch a recycled handle's new owner.
			freshToken, _, fresh := conn.PrepareReply()
			guard.close()
			if !conn.HasPendingReply(freshToken) {
				t.Fatal("old guard removed a new registration")
			}
			fresh.Cancel()
			fresh.Release()
		})
	}
}

func TestSecondaryPanicCleansPrimaryReply(t *testing.T) {
	t.Parallel()
	conn := guardTestConn(t)
	token, replies, handle := conn.PrepareReply()
	func() {
		defer func() {
			if recover() == nil {
				t.Error("secondary did not panic")
			}
		}()
		sendFrameWithHedge(context.Background(), budgetModel(1), 0,
			func() inFlightRPC { return inFlightRPC{replyCh: replies, replyHandle: handle} },
			func() inFlightRPC { panic("secondary builder") }, time.Second)
	}()
	if conn.HasPendingReply(token) {
		t.Fatal("panicking secondary left the transferred primary reply registered")
	}
	if conn.IsClosed() {
		t.Fatal("panic cleanup closed the connection")
	}
}
