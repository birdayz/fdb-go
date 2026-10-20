package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// BenchmarkWarmPeerReadSendPath measures one production GetValue RPC over a
// prewarmed loopback connection: lookup, encoding, send, reply wait and parsing.
// Its source also compiles against ff4b78d1b, whose unchanged sender API reaches
// getOrDial/SendFrameContext instead of getOrDialForSend/SendFrameContextOrdered.
// Allocations include both ends of the loopback RPC, not just send admission.
func BenchmarkWarmPeerReadSendPath(b *testing.B) {
	for _, queued := range []bool{false, true} {
		name := "tcp-direct-eligible"
		if queued {
			name = "tcp-queued"
		}
		b.Run(name, func(b *testing.B) { benchmarkWarmPeerReadSendPath(b, queued) })
	}
}

// This wrapper deliberately hides *net.TCPConn from transport.newRawWriter.
// The untimed probe holds its write until the sender reports its flush wait;
// the measured loop uses only the counter and the disabled hold flag.
type warmPeerQueuedConn struct {
	net.Conn
	writes atomic.Int64
	hold   atomic.Bool
	resume <-chan struct{}
}

func (c *warmPeerQueuedConn) Write(body []byte) (int, error) {
	c.writes.Add(1)
	if c.hold.Load() {
		<-c.resume
	}
	return c.Conn.Write(body)
}

// Observe the first real suspension without inspecting transport.Conn fields.
// Stack collection and the gates are used only before benchmark timing starts.
type warmPeerProbeContext struct {
	context.Context
	once   sync.Once
	stack  []string
	resume func()
}

func (c *warmPeerProbeContext) Value(key any) any {
	value := c.Context.Value(key)
	if _, ok := value.(*transport.SuspendSignal); ok {
		c.once.Do(func() {
			var pcs [32]uintptr
			n := runtime.Callers(2, pcs[:])
			frames := runtime.CallersFrames(pcs[:n])
			for {
				frame, more := frames.Next()
				c.stack = append(c.stack, frame.Function)
				if !more {
					break
				}
			}
			c.resume()
		})
	}
	return value
}

func benchmarkWarmPeerReadSendPath(b *testing.B, queued bool) {
	b.Helper()
	b.StopTimer()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	b.Cleanup(cancel)
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = listener.Close() })
	type acceptedTCP struct {
		conn *net.TCPConn
		err  error
	}
	accepted := make(chan acceptedTCP, 1)
	go func() {
		conn, err := listener.AcceptTCP()
		accepted <- acceptedTCP{conn, err}
	}()
	client, err := net.DialTCP("tcp4", nil, listener.Addr().(*net.TCPAddr))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = client.Close() })
	peer := <-accepted
	if peer.err != nil {
		b.Fatal(peer.err)
	}
	_ = listener.Close()
	b.Cleanup(func() { _ = peer.conn.Close() })
	if deadline, ok := ctx.Deadline(); ok {
		if err := peer.conn.SetDeadline(deadline); err != nil {
			b.Fatal(err)
		}
	}

	key, value := []byte("warm-peer-key"), []byte("warm-peer-value")
	reply := warmPeerValueReply(value)
	decoded, _, err := parseGetValueReply(reply)
	if err != nil || !bytes.Equal(decoded, value) {
		b.Fatalf("fixture reply = %q, %v; want %q", decoded, err, value)
	}
	endpoint := transport.UID{First: 0x262, Second: 0x730}
	server := ServerInfo{Address: client.RemoteAddr().String(), Token: endpoint}
	writeGate, replyGate := make(chan struct{}), make(chan struct{})
	resumeWrite := sync.OnceFunc(func() { close(writeGate) })
	resumeReply := sync.OnceFunc(func() { close(replyGate) })
	var requests atomic.Int64
	peerDone := make(chan error, 1)
	go func() {
		peerDone <- serveWarmPeerReadBench(peer.conn, endpoint, key, reply, replyGate, &requests)
	}()
	var live *transport.Conn
	b.Cleanup(func() {
		resumeWrite()
		resumeReply()
		if live != nil {
			_ = live.Close()
		}
		_ = client.Close()
		_ = peer.conn.Close()
		select {
		case <-peerDone:
		case <-time.After(5 * time.Second):
			b.Error("loopback benchmark peer did not stop")
		}
	})

	var wrapped *warmPeerQueuedConn
	var socket net.Conn = client
	if queued {
		wrapped = &warmPeerQueuedConn{Conn: client, resume: writeGate}
		socket = wrapped
	} else if _, err := client.SyscallConn(); err != nil {
		b.Fatalf("direct-eligible TCP socket has no raw capability: %v", err)
	}
	var dials atomic.Int64
	db := &database{
		ctx:        ctx,
		connPool:   make(map[string]*transport.Conn),
		dialing:    make(map[string]*dialCall),
		failMon:    newFailureMonitor(),
		queueModel: newQueueModel(),
		dialOpts:   []transport.DialOption{transport.WithCoalesceDelay(0, 0)},
		dialFn: func(context.Context, string, string) (net.Conn, error) {
			if n := dials.Add(1); n != 1 {
				return nil, fmt.Errorf("warm benchmark unexpectedly dialed %d times", n)
			}
			return socket, nil
		},
	}
	live, err = db.getOrDial(ctx, server.Address)
	if err != nil {
		b.Fatal(err)
	}
	minDelay, maxDelay, waits := live.CoalesceDelay()
	if minDelay != 0 || maxDelay != 0 || waits {
		b.Fatalf("coalescing was not disabled: %v, %v, %t", minDelay, maxDelay, waits)
	}
	tx := &Transaction{db: db}
	send := func(ctx context.Context) ([]byte, error) {
		return tx.sendGetValueToServer(ctx, key, server, 1, false, -1, readTagSample{})
	}

	// The peer withholds the first reply. The wrapper additionally withholds
	// the queued flush, making its writeContext notification unavoidable.
	var probeWrites int64
	if wrapped != nil {
		probeWrites = wrapped.writes.Load()
		wrapped.hold.Store(true)
	}
	probeDeadline, cancelProbe := context.WithTimeout(ctx, 5*time.Second)
	defer cancelProbe()
	signaled, signal := transport.WithSuspendSignal(probeDeadline)
	defer signal.Fire()
	probe := &warmPeerProbeContext{Context: signaled, resume: func() { resumeWrite(); resumeReply() }}
	got, err := send(probe)
	signal.Fire() // no signal/probe remains live during the measured loop
	cancelProbe()
	if err != nil || !bytes.Equal(got, value) {
		b.Fatalf("untimed production send = %q, %v; want %q", got, err, value)
	}
	stack := strings.Join(probe.stack, "\n")
	flushWait := strings.Contains(stack, "transport.(*Conn).writeContext")
	replyWait := strings.Contains(stack, "transport.(*ReplyHandle).NotifyWait")
	if queued {
		wrapped.hold.Store(false)
		if !flushWait || wrapped.writes.Load() <= probeWrites {
			b.Fatalf("queued positive control did not execute a blocked socket flush:\n%s", stack)
		}
	} else if flushWait || !replyWait {
		b.Fatalf("idle TCP positive control suspended before the reply wait:\n%s", stack)
	}
	if n := requests.Load(); n != 1 {
		b.Fatalf("untimed probe reached the peer %d times, want 1", n)
	}
	b.Logf("positive control: queued=%t coalesce=%v/%v flushWait=%t replyWait=%t dials=%d peerRequests=%d", queued, minDelay, maxDelay, flushWait, replyWait, dials.Load(), requests.Load())
	// A reply-only notification establishes a non-suspending idle TCP send,
	// not an all-iterations direct-write counter: a flush already acknowledged
	// before its readiness check can also avoid a flush notification.

	// Warm request/reply pools and the model independently of the probe.
	for i := 0; i < 64; i++ {
		got, err := send(ctx)
		if err != nil || !bytes.Equal(got, value) {
			b.Fatalf("warmup send = %q, %v", got, err)
		}
	}
	beforeRequests := requests.Load()
	b.ReportAllocs()
	b.ResetTimer()
	b.StartTimer()
	for i := 0; i < b.N; i++ {
		got, err := send(ctx)
		if err != nil || !bytes.Equal(got, value) {
			b.Fatalf("production send = %q, %v", got, err)
		}
	}
	b.StopTimer()
	if n := requests.Load() - beforeRequests; n != int64(b.N) {
		b.Fatalf("measured peer requests = %d, want %d", n, b.N)
	}
	if n := dials.Load(); n != 1 {
		b.Fatalf("warm production sends used %d dials, want 1", n)
	}
	db.connMu.Lock()
	pooled := db.connPool[server.Address]
	db.connMu.Unlock()
	if pooled != live || live.IsClosed() {
		b.Fatal("measured sends did not retain the prewarmed connection")
	}
	b.ReportMetric(float64(requests.Load()-beforeRequests)/float64(b.N), "peer-requests/op")
}

func serveWarmPeerReadBench(conn *net.TCPConn, endpoint transport.UID, key, reply []byte, firstReply <-chan struct{}, requests *atomic.Int64) error {
	if _, err := transport.ReadConnectPacket(conn); err != nil {
		return err
	}
	if err := transport.WriteConnectPacket(conn, conn.LocalAddr(), 262); err != nil {
		return err
	}
	voidReply := (&types.VoidReply{}).MarshalFDB()
	in := bufio.NewReaderSize(conn, 16<<10)
	var reader transport.FrameReader
	for {
		token, body, err := reader.Read(in, false)
		if err != nil {
			return err
		}
		var replyToken transport.UID
		response := reply
		if token == transport.WellKnownToken(transport.WLTokenPingPacket) {
			r, err := wire.NewReader(body)
			if err != nil {
				return err
			}
			rp, err := r.ReadNestedReader(0)
			if err != nil {
				return err
			}
			replyToken.First, replyToken.Second = rp.ReadUIDPair(types.ReplyPromiseSlotToken)
			response = voidReply
		} else {
			if token != endpoint {
				return fmt.Errorf("unexpected benchmark endpoint: %+v", token)
			}
			var request types.GetValueRequest
			if err := request.UnmarshalFDB(body); err != nil {
				return err
			}
			if !bytes.Equal(request.Key, key) || request.Version != 1 || request.TenantInfo.TenantId != -1 {
				return fmt.Errorf("production GetValue request lost its arguments: %+v", request)
			}
			replyToken.First = binary.LittleEndian.Uint64(request.Reply.Token[:8])
			replyToken.Second = binary.LittleEndian.Uint64(request.Reply.Token[8:])
			if requests.Add(1) == 1 {
				<-firstReply
			}
		}
		if err := transport.WriteFrame(conn, replyToken, response, false); err != nil {
			return err
		}
	}
}

// Preserve the generated GetValueReply payload and its relative offsets, and
// prepend a success ErrorOr root. The original FakeRoot becomes unreachable.
func warmPeerValueReply(value []byte) []byte {
	payload := (&types.GetValueReply{Penalty: 1, HasValue: true, Value: value}).MarshalFDB()
	fakeRoot := int(binary.LittleEndian.Uint32(payload[:4]))
	replyObject := fakeRoot + 4 + int(binary.LittleEndian.Uint32(payload[fakeRoot+4:]))
	const prefix = 32
	body := make([]byte, prefix+len(payload))
	copy(body[prefix:], payload)
	binary.LittleEndian.PutUint32(body[:4], 16) // root object
	copy(body[4:8], payload[4:8])               // reply file identifier
	for i, field := range [...]uint16{8, 9, 8, 4} {
		binary.LittleEndian.PutUint16(body[8+2*i:], field)
	}
	binary.LittleEndian.PutUint32(body[16:], 8) // root soffset -> vtable
	binary.LittleEndian.PutUint32(body[20:], uint32(prefix+replyObject-20))
	body[24] = 2 // ErrorOr success tag
	return body
}
