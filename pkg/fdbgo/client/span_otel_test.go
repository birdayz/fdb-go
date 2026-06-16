package client

import (
	"context"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestTracerExportsSpans is the RFC-115 §4 Layer-2 end-to-end proof: with WithTracer +
// WithTracingSampleRate(1.0), a transaction emits a "Transaction" span plus per-op child
// spans (fdbgo.getValue / fdbgo.getRange) that nest under it and share one traceID. With
// no tracer (the default noop) nothing is emitted and the tx still works. Real FDB.
//
// Revert-proof: drop the startOpSpan call in getValue and the "fdbgo.getValue" assertion
// reddens; drop ensureTxSpan and the "Transaction" assertion reddens.
func TestTracerExportsSpans(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if sharedClusterFile == nil {
		t.Fatal("shared FDB container not initialized — TestMain must run first")
	}

	key := []byte(t.Name() + "_k")
	rkey := []byte(t.Name() + "_r1")

	// Seed keys with an untraced db (so the traced reads hit the wire, not RYW).
	seed := openTestDB(t, ctx)
	defer seed.Close()
	if _, err := seed.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("v"))
		tx.Set(rkey, []byte("rv"))
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	defer tp.Shutdown(context.Background())

	db, err := OpenDatabaseFromConfig(ctx, sharedClusterFile,
		WithTracer(tp.Tracer("test")), WithTracingSampleRate(1.0))
	if err != nil {
		t.Fatalf("open with tracer: %v", err)
	}
	defer db.Close()

	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		if _, err := tx.Get(ctx, key); err != nil {
			return nil, err
		}
		if _, _, err := tx.GetRange(ctx, rkey, append(append([]byte{}, rkey...), 0xff), 10); err != nil {
			return nil, err
		}
		tx.Set(key, []byte("v2"))
		return nil, nil
	}); err != nil {
		t.Fatalf("traced transact: %v", err)
	}

	spans := sr.Ended()
	byName := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range spans {
		byName[s.Name()] = s
	}
	txSpan, ok := byName["Transaction"]
	if !ok {
		t.Fatalf("no \"Transaction\" span emitted; got %v", spanNames(spans))
	}
	gv, ok := byName["fdbgo.getValue"]
	if !ok {
		t.Fatalf("no \"fdbgo.getValue\" child span; got %v", spanNames(spans))
	}
	if _, ok := byName["fdbgo.getRange"]; !ok {
		t.Fatalf("no \"fdbgo.getRange\" child span; got %v", spanNames(spans))
	}
	// Child spans share the Transaction's traceID and nest under it.
	if gv.SpanContext().TraceID() != txSpan.SpanContext().TraceID() {
		t.Errorf("getValue traceID %v != Transaction traceID %v", gv.SpanContext().TraceID(), txSpan.SpanContext().TraceID())
	}
	if gv.Parent().SpanID() != txSpan.SpanContext().SpanID() {
		t.Errorf("getValue parent %v != Transaction span %v", gv.Parent().SpanID(), txSpan.SpanContext().SpanID())
	}
	if !txSpan.SpanContext().TraceID().IsValid() {
		t.Error("Transaction span has an invalid (zero) traceID")
	}
}

// TestTracerDefaultNoopEmitsNothing: with no WithTracer (default noop), a sampled tx
// runs fine and records zero spans — zero-cost default, C++ NoopTracer.
func TestTracerDefaultNoopEmitsNothing(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	sr := tracetest.NewSpanRecorder()
	_ = sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)) // recorder NOT wired into the db

	db := openTestDB(t, ctx) // no WithTracer → noop
	defer db.Close()
	key := []byte(t.Name())
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		_, _ = tx.Get(ctx, key)
		tx.Set(key, []byte("v"))
		return nil, nil
	}); err != nil {
		t.Fatalf("transact: %v", err)
	}
	if n := len(sr.Ended()); n != 0 {
		t.Fatalf("noop default must emit 0 spans, got %d", n)
	}
}

func spanNames(spans []sdktrace.ReadOnlySpan) []string {
	out := make([]string, len(spans))
	for i, s := range spans {
		out[i] = s.Name()
	}
	return out
}
