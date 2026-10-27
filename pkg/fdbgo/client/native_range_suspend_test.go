package client

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
)

// pipePeer speaks the real transport handshake, then can hold all socket reads.
// The writer therefore blocks on a flush without blocking an independent peer.
func pipePeer(t *testing.T, handshake, reads <-chan struct{}) net.Conn {
	t.Helper()
	cli, srv := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer srv.Close()
		if _, err := transport.ReadConnectPacket(srv); err != nil {
			return
		}
		if handshake != nil {
			<-handshake
		}
		packet := transport.ConnectPacket{ProtocolVersion: transport.ProtocolVersion73, ConnectionID: 262}
		if _, err := srv.Write(packet.Marshal()); err != nil {
			return
		}
		if reads != nil {
			<-reads
		}
		_, _ = io.Copy(io.Discard, srv)
	}()
	t.Cleanup(func() { cli.Close(); srv.Close(); <-done })
	return cli
}

func TestFallbackStartsEndAtTransportSuspension(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"cold-dial", "handshake", "flush"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			release := make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			var handshake, reads <-chan struct{}
			if mode == "handshake" {
				handshake = release
			} else if mode == "flush" {
				reads = release
			}
			beginPeer, endPeer := pipePeer(t, handshake, reads), pipePeer(t, nil, nil)
			db := &database{ctx: ctx, connPool: map[string]*transport.Conn{}, dialing: map[string]*dialCall{}, failMon: newFailureMonitor()}
			if mode == "flush" {
				// The flush variant tests suspension on a blocked socket write acknowledgment.
				db.dialOpts = []transport.DialOption{transport.WithLegacyWriter()}
			}
			db.dialFn = func(ctx context.Context, _, addr string) (net.Conn, error) {
				if addr == "end" {
					return endPeer, nil
				}
				if mode == "cold-dial" {
					select {
					case <-release:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
				return beginPeer, nil
			}
			endConn, err := db.getOrDial(ctx, "end") // independently warm end
			if err != nil {
				t.Fatal(err)
			}
			defer endConn.Close()
			if mode == "flush" {
				beginConn, err := db.getOrDial(ctx, "begin")
				if err != nil {
					t.Fatal(err)
				}
				defer beginConn.Close()
			}
			endSent, endJoined := make(chan struct{}), make(chan struct{})
			failure := errors.New("begin resolved after release")
			tx := &Transaction{db: db}
			tx.resolveKeyFn = func(ctx context.Context, s KeySelector, _ int64) ([]byte, error) {
				conn, err := db.getOrDial(ctx, string(s.Key))
				if err != nil {
					return nil, err
				}
				if _, err := conn.SendFrameContext(ctx, transport.UID{First: 262}, []byte("resolve")); err != nil {
					return nil, err
				}
				if string(s.Key) == "end" {
					defer close(endJoined)
					close(endSent)
					if ctx.Err() == nil {
						notifyParked(ctx)
					}
					<-ctx.Done()
					return nil, ctx.Err()
				}
				return nil, failure
			}
			done := make(chan error, 1)
			go func() {
				_, err := tx.nativeGetRangeFallback(ctx, sel("begin", false, 2), sel("end", false, 2), newRangeLimits(1, -1), 1, false)
				done <- err
			}()
			select {
			case <-endSent:
			case <-time.After(3 * time.Second):
				t.Error("warm end was serialized behind begin's held " + mode)
			}
			close(release)
			select {
			case err := <-done:
				if !errors.Is(err, failure) {
					t.Fatalf("fallback = %v, want begin failure", err)
				}
			case <-ctx.Done():
				t.Fatal("fallback did not join its resolvers")
			}
			select {
			case <-endJoined:
			default:
				t.Fatal("fallback returned without cancelling and joining end")
			}
			db.connMu.Lock()
			beginConn := db.connPool["begin"]
			db.connMu.Unlock()
			if beginConn != nil {
				beginConn.Close()
			}
		})
	}
}

func TestResolveKeyPanicPublishesAndJoins(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx := &Transaction{}
	endStarted, endJoined := make(chan struct{}), make(chan struct{})
	tx.resolveKeyFn = func(ctx context.Context, s KeySelector, _ int64) ([]byte, error) {
		if string(s.Key) == "begin" {
			panic("resolver-local panic")
		}
		defer close(endJoined)
		close(endStarted)
		if ctx.Err() == nil {
			notifyParked(ctx)
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	done := make(chan error, 1)
	go func() {
		_, err := tx.nativeGetRangeFallback(ctx, sel("begin", false, 2), sel("end", false, 2), newRangeLimits(1, -1), 1, false)
		done <- err
	}()
	select {
	case err := <-done:
		if code, _ := fdbErrorCode(err); code != ErrInternalError {
			t.Fatalf("resolver panic = %v, want internal_error (4100)", err)
		}
	case <-ctx.Done():
		t.Fatal("resolver panic stranded fallback")
	}
	select {
	case <-endStarted:
	default:
		t.Fatal("begin panic did not publish parked")
	}
	select {
	case <-endJoined:
	default:
		t.Fatal("fallback did not join peer after resolver panic")
	}
}
