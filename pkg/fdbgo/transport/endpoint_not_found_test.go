package transport

import (
	"context"
	"encoding/binary"
	"errors"
	mathrand "math/rand/v2"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/wire"
)

// endpointNotFoundBody is the SerializeSource<UID> body: a FakeRoot table whose
// field 0 is the UID inline (vtable {6, 24, 8}).
func endpointNotFoundBody(tok UID) []byte {
	b := make([]byte, 40)
	binary.LittleEndian.PutUint32(b[0:], 16) // root object offset
	binary.LittleEndian.PutUint32(b[4:], uidFileID)
	binary.LittleEndian.PutUint16(b[8:], 6)   // vtable size
	binary.LittleEndian.PutUint16(b[10:], 24) // object size
	binary.LittleEndian.PutUint16(b[12:], 8)  // field 0 offset
	binary.LittleEndian.PutUint32(b[16:], 8)  // soffset back to the vtable
	binary.LittleEndian.PutUint64(b[24:], tok.First)
	binary.LittleEndian.PutUint64(b[32:], tok.Second)
	return b
}

func TestDecodeEndpointNotFound(t *testing.T) {
	t.Parallel()
	tok := UID{First: 0x1122334455667701, Second: 0x8899aabbccddeeff}
	valid := endpointNotFoundBody(tok)
	mutate := func(f func(b []byte)) []byte {
		b := append([]byte(nil), valid...)
		f(b)
		return b
	}
	cases := []struct {
		name string
		body []byte
		ok   bool
	}{
		{"valid", valid, true},
		{"nil", nil, false},
		{"short", valid[:7], false},
		{"truncated uid", valid[:32], false},
		{"wrong file id", mutate(func(b []byte) { binary.LittleEndian.PutUint32(b[4:], 4707015) }), false},
		{"root out of range", mutate(func(b []byte) { binary.LittleEndian.PutUint32(b[0:], 0xFFFF) }), false},
		{"field absent", mutate(func(b []byte) { binary.LittleEndian.PutUint16(b[8:], 4) }), false},
		{"field zero", mutate(func(b []byte) { binary.LittleEndian.PutUint16(b[12:], 0) }), false},
		{"field past object", mutate(func(b []byte) { binary.LittleEndian.PutUint16(b[12:], 20) }), false},
		{"vtable out of range", mutate(func(b []byte) { binary.LittleEndian.PutUint32(b[16:], 0x7FFFFFFF) }), false},
	}
	for _, tc := range cases {
		got, ok := decodeEndpointNotFound(tc.body)
		if ok != tc.ok {
			t.Errorf("%s: ok=%v, want %v", tc.name, ok, tc.ok)
		}
		if ok && got != tok {
			t.Errorf("%s: token %v, want %v", tc.name, got, tok)
		}
	}
	rng := mathrand.New(mathrand.NewPCG(1, 2))
	for i := 0; i < 10000; i++ {
		b := make([]byte, rng.IntN(64))
		for j := range b {
			b[j] = byte(rng.Uint32())
		}
		decodeEndpointNotFound(b) // must not panic
	}
}

func TestEndpointNotFoundErrorIdentity(t *testing.T) {
	t.Parallel()
	var err error = &EndpointNotFoundError{Token: UID{First: 3}}
	var fdbErr *wire.FDBError
	if !errors.Is(err, ErrEndpointNotFound) || !errors.As(err, &fdbErr) || fdbErr.Code != codeRequestMaybeDelivered {
		t.Fatalf("want ErrEndpointNotFound + 1030, got %v", err)
	}
	if errors.Is(err, ErrConnClosed) {
		t.Fatal("endpoint-not-found must not read as a connection teardown")
	}
}

// TestConn_EndpointNotFoundFailsOnlyThatDestination drives the readLoop: the
// not-found packet fails the replies bound to that destination only, reports
// every token (even with no waiter) and leaves the connection usable.
func TestConn_EndpointNotFoundFailsOnlyThatDestination(t *testing.T) {
	t.Parallel()
	s := newSimServer(t)
	destA := UID{First: 0xA1, Second: 0xA2}
	destB := UID{First: 0xB1, Second: 0xB2}
	frames := make(chan [2]any)
	go func() {
		if err := s.handshake(); err != nil {
			return
		}
		for f := range frames {
			if WriteFrame(s.srv, f[0].(UID), f[1].([]byte), false) != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { close(frames) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := dialWith(ctx, "sim", s.dialFunc(), nil, withMonitorCadence(time.Hour, time.Hour))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	seen := make(chan UID, 8)
	c.SetEndpointNotFoundHandler(func(tok UID) { seen <- tok })

	tokA, chA, hA := c.PrepareReplyTo(destA)
	defer hA.Release()
	_, _, hA2 := c.PrepareReplyTo(destA)
	hA2.Cancel() // abandoned: the handler must still see destA
	hA2.Release()
	tokB, chB, hB := c.PrepareReplyTo(destB)
	defer hB.Release()

	frames <- [2]any{endpointNotFoundToken, []byte("garbage")}
	frames <- [2]any{endpointNotFoundToken, endpointNotFoundBody(UID{First: wellKnownFirst, Second: 7})}
	frames <- [2]any{endpointNotFoundToken, endpointNotFoundBody(destA)}

	select {
	case resp := <-chA:
		var enf *EndpointNotFoundError
		if !errors.As(resp.Err, &enf) || enf.Token != destA {
			t.Fatalf("destA reply: got %+v, want EndpointNotFoundError{%v}", resp, destA)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("destA reply was not failed by ENDPOINT_NOT_FOUND")
	}
	if got := <-seen; got != destA {
		t.Fatalf("handler saw %v, want %v (well-known and malformed must be ignored)", got, destA)
	}

	// A late reply to the failed token is dropped; destB still gets its reply.
	frames <- [2]any{tokA, []byte("late")}
	frames <- [2]any{tokB, []byte("ok")}
	select {
	case resp := <-chB:
		if resp.Err != nil || string(resp.Body) != "ok" {
			t.Fatalf("destB reply: %+v", resp)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("destB reply never arrived")
	}
	if c.IsClosed() {
		t.Fatal("ENDPOINT_NOT_FOUND must not tear down the connection")
	}
	if len(chA) != 0 || len(seen) != 0 {
		t.Fatalf("unexpected extra delivery: chA=%d seen=%d", len(chA), len(seen))
	}
}
