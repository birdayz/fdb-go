package client

import (
	"context"
	"errors"
	"math"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
)

type hedgeSenderFrame struct {
	addr  string
	token transport.UID
	at    time.Time
}

// These peers complete the real handshake and record requests without replying.
func hedgeSenderPeer(addr string, frames chan<- hedgeSenderFrame) (net.Conn, func()) {
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer server.Close()
		if _, err := transport.ReadConnectPacket(server); err != nil {
			return
		}
		packet := transport.ConnectPacket{ProtocolVersion: transport.ProtocolVersion73, ConnectionID: 262}
		if _, err := server.Write(packet.Marshal()); err != nil {
			return
		}
		for {
			token, _, err := transport.ReadFrame(server, false)
			if err != nil {
				return
			}
			frames <- hedgeSenderFrame{addr: addr, token: token, at: time.Now()}
		}
	}()
	return client, func() {
		client.Close()
		server.Close()
		<-done
	}
}

// Exercise the production senders, not a delay supplied directly to the hedge helper.
func TestHedgeSenderDelay(t *testing.T) {
	// Transport completion pools must stay in one fake-clock domain; this target
	// has no other production transport senders outside this bubble.
	hedgeTimerBubble(t, func(t *testing.T, _ *[]*time.Timer) {
		const primaryAddr, backupAddr = "primary:4500", "backup:4500"
		token := transport.UID{First: 0x77, Second: 0x88}
		servers := []ServerInfo{{Address: primaryAddr, Token: token}, {Address: backupAddr, Token: token}}
		for _, sender := range []struct {
			name     string
			endpoint int
			send     func(context.Context, *Transaction) error
		}{
			{"get-value", EndpointGetValue, func(ctx context.Context, tx *Transaction) error {
				_, err := tx.sendGetValue(ctx, []byte("k"), servers, readTagSample{})
				return err
			}},
			{"get-key", EndpointGetKey, func(ctx context.Context, tx *Transaction) error {
				_, _, _, err := tx.sendGetKey(ctx, 1, []byte("k"), false, 1, servers, readTagSample{}, nil)
				return err
			}},
			{"get-range", EndpointGetKeyValues, func(ctx context.Context, tx *Transaction) error {
				_, _, err := sendRangeRPC(tx, ctx, firstGreaterOrEqual([]byte("k")), firstGreaterOrEqual([]byte("l")), newRangeLimits(10, ByteLimitUnlimited), 1, false, servers, readTagSample{}, plainRangeRPCOps)
				return err
			}},
		} {
			for _, timing := range []struct {
				name           string
				primaryLatency float64
				wantDelay      time.Duration
			}{
				{"adaptive", 0.004, 24500 * time.Microsecond},
				{"threshold-equality", 0.049, 24500 * time.Microsecond},
				{"instant", 0.050, 0},
			} {
				t.Logf("sender delay: %s/%s", sender.name, timing.name)
				func() {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					frames := make(chan hedgeSenderFrame, 8)
					peers := make(map[string]net.Conn)
					for _, server := range servers {
						peer, closePeer := hedgeSenderPeer(server.Address, frames)
						defer closePeer()
						peers[server.Address] = peer
					}
					q := newQueueModel()
					db := &database{
						ctx: ctx, connPool: make(map[string]*transport.Conn), dialing: make(map[string]*dialCall),
						failMon: newFailureMonitor(), queueModel: q, proxiesChanged: make(chan struct{}),
						dialFn: func(_ context.Context, _, addr string) (net.Conn, error) { return peers[addr], nil },
					}
					q.failMon = db.failMon
					db.hedgeEnabled.Store(true)
					defer func() {
						cancel()
						for _, conn := range db.connPool {
							conn.Close()
						}
					}()
					for _, server := range servers {
						if _, err := db.getOrDial(ctx, server.Address); err != nil {
							t.Fatalf("prewarm %s: %v", server.Address, err)
						}
					}
					tx := &Transaction{db: db, readVersionState: &readVersionState{}, txOptions: txOptions{tenantId: NoTenantID}}
					tx.readVersion = 1
					tx.rpcTimeoutOverride = time.Minute
					sentinel := q.startRequest(backupAddr)
					q.mu.Lock()
					q.getOrCreate(primaryAddr).latency = timing.primaryLatency
					q.getOrCreate(backupAddr).latency = 0.008
					q.getOrCreate(backupAddr).smoothOutstanding.estimate = 1
					q.secondMultiplier, q.secondBudget = 3, 1
					q.mu.Unlock()
					defer q.endRequest(backupAddr, sentinel, 0, false)
					if best, next := q.chooseTopTwo(servers); best != 0 || next != 1 {
						t.Fatalf("selection = %d,%d, want primary then backup", best, next)
					}

					opCtx, stop := context.WithCancel(ctx)
					done := make(chan struct{})
					var readErr error
					start := time.Now()
					go func() {
						defer close(done)
						readErr = sender.send(opCtx, tx)
					}()
					defer func() { stop(); <-done }()
					synctest.Wait()
					seen := make(map[string]hedgeSenderFrame)
					collect := func() {
						for {
							select {
							case frame := <-frames:
								if _, duplicate := seen[frame.addr]; duplicate {
									t.Fatalf("duplicate request to %s", frame.addr)
								}
								seen[frame.addr] = frame
							default:
								return
							}
						}
					}
					collect()
					if primary, ok := seen[primaryAddr]; !ok || !primary.at.Equal(start) {
						t.Fatalf("primary frame = %+v, want at %v", primary, start)
					}
					if timing.wantDelay > 0 {
						if len(seen) != 1 {
							t.Errorf("requests at start = %+v, want only primary", seen)
							return
						}
						time.Sleep(timing.wantDelay - time.Nanosecond)
						synctest.Wait()
						collect()
						if len(seen) != 1 {
							t.Errorf("backup sent before %v: %+v", timing.wantDelay, seen)
							return
						}
						time.Sleep(time.Nanosecond)
						synctest.Wait()
						collect()
					}
					if backup, ok := seen[backupAddr]; !ok || backup.at.Sub(start) != timing.wantDelay || len(seen) != 2 {
						t.Errorf("requests = %+v, want backup at %v", seen, timing.wantDelay)
						return
					}
					for _, frame := range seen {
						if frame.token != getAdjustedEndpoint(token, sender.endpoint) {
							t.Fatalf("%s token = %v, want endpoint %d", frame.addr, frame.token, sender.endpoint)
						}
					}
					stop()
					<-done
					if !errors.Is(readErr, context.Canceled) {
						t.Fatalf("cancel read: %v", readErr)
					}
					if budget, multiplier := secondState(q); budget != 0 || math.Abs(multiplier-3.01) > 1e-12 {
						t.Fatalf("budget=%v multiplier=%v, want 0 and 3.01", budget, multiplier)
					}
					q.mu.Lock()
					primaryOutstanding := q.getOrCreate(primaryAddr).smoothOutstanding.total
					backupOutstanding := q.getOrCreate(backupAddr).smoothOutstanding.total
					q.mu.Unlock()
					if primaryOutstanding != 0 || backupOutstanding != sentinel {
						t.Fatalf("outstanding = %v,%v, want 0,%v", primaryOutstanding, backupOutstanding, sentinel)
					}
				}()
			}
		}
	})
}
