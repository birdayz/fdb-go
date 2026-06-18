package client

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire/types"
)

func zeroSpan(sc types.SpanContext) bool {
	return sc.TraceID == [16]byte{} && sc.SpanID == 0 && sc.Flags == 0
}

// TestNewSpanContext: a generated span is non-zero and unique per call, unsampled by
// default (rate 0.0, matching C++ TRACING_SAMPLE_RATE) and sampled at rate 1.0 — the
// RFC-115 §4 fix: C++ never sends the all-zero span the Go client used to.
func TestNewSpanContext(t *testing.T) {
	t.Parallel()
	a := newSpanContext(0.0)
	b := newSpanContext(0.0)
	if zeroSpan(a) {
		t.Fatal("newSpanContext produced an all-zero span (the pre-RFC-115 divergence)")
	}
	if a.TraceID == b.TraceID || a.SpanID == b.SpanID {
		t.Fatalf("two spans must be distinct: a=%x/%d b=%x/%d", a.TraceID, a.SpanID, b.TraceID, b.SpanID)
	}
	if a.Flags != traceFlagUnsampled || b.Flags != traceFlagUnsampled {
		t.Fatalf("rate 0.0 must be unsampled, got flags %d/%d", a.Flags, b.Flags)
	}
	if s := newSpanContext(1.0); s.Flags != traceFlagSampled {
		t.Fatalf("rate 1.0 must be sampled, got flags %d", s.Flags)
	}
}

// TestChildSpanContext: a child inherits the parent's traceID + flags but gets a fresh
// spanID (C++ Span::setParent, Tracing.h:237).
func TestChildSpanContext(t *testing.T) {
	t.Parallel()
	parent := types.SpanContext{TraceID: [16]byte{9, 8, 7, 6, 5, 4, 3, 2, 1}, SpanID: 0xABCD, Flags: traceFlagSampled}
	child := childSpanContext(parent)
	if child.TraceID != parent.TraceID {
		t.Errorf("child must inherit parent traceID: got %x, want %x", child.TraceID, parent.TraceID)
	}
	if child.Flags != parent.Flags {
		t.Errorf("child must inherit parent flags: got %d, want %d", child.Flags, parent.Flags)
	}
	if child.SpanID == parent.SpanID {
		t.Errorf("child must get a fresh spanID, got the parent's %d", child.SpanID)
	}
}

// TestParseSpanParent: the 33-byte SPAN_PARENT (8-byte version header + 16 traceID + 8
// spanID + 1 flags, little-endian) decodes to the right SpanContext; wrong length errors.
func TestParseSpanParent(t *testing.T) {
	t.Parallel()
	var buf [spanParentSize]byte
	binary.LittleEndian.PutUint64(buf[0:8], transport.ProtocolVersion73) // version header
	traceID := [16]byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x01}
	copy(buf[8:24], traceID[:])
	binary.LittleEndian.PutUint64(buf[24:32], 0xCAFEBABE)
	buf[32] = traceFlagSampled

	sc, err := parseSpanParent(buf[:])
	if err != nil {
		t.Fatalf("parseSpanParent: %v", err)
	}
	if sc.TraceID != traceID {
		t.Errorf("traceID: got %x, want %x", sc.TraceID, traceID)
	}
	if sc.SpanID != 0xCAFEBABE {
		t.Errorf("spanID: got %x, want %x", sc.SpanID, 0xCAFEBABE)
	}
	if sc.Flags != traceFlagSampled {
		t.Errorf("flags: got %d, want %d", sc.Flags, traceFlagSampled)
	}

	for _, n := range []int{0, 25, 32, 34} {
		if _, err := parseSpanParent(make([]byte, n)); err == nil {
			t.Errorf("parseSpanParent(%d bytes) should error (want %d)", n, spanParentSize)
		}
	}
}

// TestTransactionSpanLifecycle (white-box, real FDB): every transaction carries a real
// span, distinct per transaction; Reset() re-anchors a fresh span; SetSpanParent links
// the span to an injected parent's trace, and that linkage survives reset() (an OnError
// retry keeps the same traceID, fresh spanID). RFC-115 §4.
func TestTransactionSpanLifecycle(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	tx1 := db.CreateTransaction()
	tx2 := db.CreateTransaction()
	if zeroSpan(tx1.spanContext) {
		t.Fatal("a fresh transaction must carry a non-zero span")
	}
	if tx1.spanContext.TraceID == tx2.spanContext.TraceID {
		t.Fatal("two transactions must have distinct trace IDs")
	}

	before := tx1.spanContext
	tx1.Reset()
	if tx1.spanContext.TraceID == before.TraceID {
		t.Fatal("Reset() must re-anchor a fresh span")
	}

	// SPAN_PARENT injection → child of the parent's trace.
	var pbuf [spanParentSize]byte
	binary.LittleEndian.PutUint64(pbuf[0:8], transport.ProtocolVersion73)
	parentTrace := [16]byte{1, 1, 2, 3, 5, 8, 13, 21, 34, 55, 89, 144, 233, 99, 100, 7}
	copy(pbuf[8:24], parentTrace[:])
	binary.LittleEndian.PutUint64(pbuf[24:32], 0xFEED)
	if err := tx1.SetSpanParent(pbuf[:]); err != nil {
		t.Fatalf("SetSpanParent: %v", err)
	}
	if tx1.spanContext.TraceID != parentTrace {
		t.Fatalf("SetSpanParent must adopt the parent traceID: got %x, want %x", tx1.spanContext.TraceID, parentTrace)
	}
	// The parent linkage survives an attempt reset (regenerateSpan honors spanParent).
	childSpanBefore := tx1.spanContext.SpanID
	tx1.reset()
	if tx1.spanContext.TraceID != parentTrace {
		t.Fatal("parent traceID must survive reset() (retry keeps the trace)")
	}
	if tx1.spanContext.SpanID == childSpanBefore {
		t.Error("reset() should still assign a fresh child spanID")
	}
}
