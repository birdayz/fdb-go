package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire/types"
)

type grvFlagsRequest struct {
	flags uint32
	count uint32
	tags  map[string]uint32
}

type grvFlagsObserver struct {
	mu       sync.Mutex
	requests []grvFlagsRequest
	err      error
}

func (o *grvFlagsObserver) dial(ctx context.Context, network, address string) (net.Conn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	return &grvFlagsConn{Conn: conn, observer: o, handshake: transport.ConnectPacketSize}, nil
}

func (o *grvFlagsObserver) record(body []byte, frameErr error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if frameErr != nil {
		if o.err == nil {
			o.err = frameErr
		}
		return
	}
	// Identify the real outgoing request before decoding; pings and coordinator
	// traffic sharing this connection must not count as GRVs.
	if len(body) < 8 || binary.LittleEndian.Uint32(body[4:8]) != 838566 {
		return
	}
	var req types.GetReadVersionRequest
	if err := req.UnmarshalFDB(body); err != nil {
		if o.err == nil {
			o.err = err
		}
		return
	}
	tags := make(map[string]uint32, len(req.Tags))
	for _, tag := range req.Tags {
		tags[string(tag.Tag)] += tag.Count
	}
	o.requests = append(o.requests, grvFlagsRequest{flags: req.Flags, count: req.TransactionCount, tags: tags})
}

// Observe copies of complete plaintext frames without changing the real FDB
// connection. TCP writes may split or combine both the handshake and frames.
type grvFlagsConn struct {
	net.Conn
	observer  *grvFlagsObserver
	mu        sync.Mutex
	handshake int
	buffer    []byte
}

func (c *grvFlagsConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.observe(p)
	c.mu.Unlock()
	return c.Conn.Write(p)
}

func (c *grvFlagsConn) observe(p []byte) {
	if c.handshake > 0 {
		n := min(c.handshake, len(p))
		c.handshake -= n
		p = p[n:]
	}
	c.buffer = append(c.buffer, p...)
	for len(c.buffer) >= 4 {
		payload := int(binary.LittleEndian.Uint32(c.buffer[:4]))
		if payload < 16 || payload > 100<<20 {
			c.observer.record(nil, fmt.Errorf("invalid outgoing payload size %d", payload))
			c.buffer = nil
			return
		}
		frameSize := 12 + payload
		if len(c.buffer) < frameSize {
			return
		}
		var fr transport.FrameReader
		_, body, err := fr.Read(bytes.NewReader(c.buffer[:frameSize]), false)
		c.observer.record(body, err)
		c.buffer = c.buffer[frameSize:]
	}
}

// getReadVersion evaluates the caller's Done channel only after queue admission;
// the shared RPC uses db.ctx instead. This signals admission without a sleep.
type grvFlagsAdmissionContext struct {
	context.Context
	once     sync.Once
	admitted chan struct{}
}

func (c *grvFlagsAdmissionContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.admitted) })
	return c.Context.Done()
}

func TestGRVBatcherSeparatesFullFlags(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	observer := &grvFlagsObserver{}
	db := newTestDatabase(t, ctx, sharedClusterFile, observer.dial)
	defer db.Close()
	batcher := db.db.grvBatchers[grvBatcherDefault]
	batcher.mu.Lock()
	batcher.batchTime = time.Hour
	batcher.inFlight++ // keep ordinary admission queued behind a held RPC
	batcher.mu.Unlock()
	// Also release on a setup failure, so no hour-long callback retains db.
	defer batcher.finishBatch()
	defer func() {
		batcher.mu.Lock()
		if batcher.timer != nil {
			batcher.timer.Reset(0)
		}
		batcher.mu.Unlock()
	}()

	const normalTag, riskyTag = "flags-normal", "flags-risky"
	callers := []*grvFlagsAdmissionContext{
		{Context: ctx, admitted: make(chan struct{})},
		{Context: ctx, admitted: make(chan struct{})},
	}
	done := make(chan grvResult, len(callers))
	for i, caller := range callers {
		flags, tag := uint32(0x08000000), normalTag
		if i == 1 {
			flags, tag = 0x08000001, riskyTag
		}
		go func() {
			rv, locked, instant, err := batcher.getReadVersion(db.db, caller, flags, types.SpanContext{}, []string{tag}, false, false)
			done <- grvResult{version: rv, locked: locked, instant: instant, err: err}
		}()
	}
	for _, caller := range callers {
		select {
		case <-caller.admitted:
		case <-ctx.Done():
			t.Fatal("GRV caller never reached queue admission:", ctx.Err())
		}
	}

	batcher.mu.Lock()
	timer := batcher.timer
	if timer != nil {
		timer.Reset(0)
	}
	batcher.mu.Unlock()
	if timer == nil {
		t.Fatal("ordinary GRV did not arm the held root batch timer")
	}
	for range callers {
		select {
		case result := <-done:
			if result.err != nil || result.version <= 0 {
				t.Fatalf("GRV result: version=%d error=%v", result.version, result.err)
			}
		case <-ctx.Done():
			t.Fatal("GRV did not complete after releasing its timer:", ctx.Err())
		}
	}

	observer.mu.Lock()
	defer observer.mu.Unlock()
	if observer.err != nil {
		t.Fatal("GRV request observer:", observer.err)
	}
	seenNormal, seenRisky := false, false
	for _, req := range observer.requests {
		normal, risky := req.tags[normalTag], req.tags[riskyTag]
		if normal+risky == 0 {
			continue
		}
		if normal > 0 && risky > 0 {
			t.Errorf("GRV_FLAGS_MIXED: flags=%#x combined normal=%d and causal-risky=%d transactions", req.flags, normal, risky)
		}
		if req.count != normal+risky {
			t.Errorf("GRV transaction count=%d, tagged count=%d", req.count, normal+risky)
		}
		if normal > 0 {
			seenNormal = true
			if normal != 1 || req.flags != 0x08000000 {
				t.Errorf("ordinary GRV: tag count=%d flags=%#x, want 1 and 0x08000000", normal, req.flags)
			}
		}
		if risky > 0 {
			seenRisky = true
			if risky != 1 || req.flags != 0x08000001 {
				t.Errorf("causal-risky GRV: tag count=%d flags=%#x, want 1 and 0x08000001", risky, req.flags)
			}
		}
	}
	if !seenNormal || !seenRisky {
		t.Fatalf("missing tagged GRV wire population: normal=%t risky=%t", seenNormal, seenRisky)
	}
}
