package client

import (
	"encoding/binary"
	"fmt"
	"math/rand/v2"

	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire/types"
)

// Distributed-tracing span context (RFC-115 §4). The pure-Go client historically
// serialized an all-zero SpanContext into every request; C++ generates a random,
// default-UNSAMPLED span per transaction and stamps it on every request
// (NativeAPI.actor.cpp generateSpanID:3458, stamped at :986/:3677/:6169). Sending
// zero is a behavioral divergence — these helpers close it.
//
// C++ SpanContext (flow/Tracing.h:46): { UID traceID (2×uint64), uint64 spanID,
// TraceFlags m_Flags (uint8) }. The flatbuffers request field (wire/types.SpanContext)
// carries TraceID as a 16-byte UID (first uint64 LE ‖ second uint64 LE), SpanID, Flags.

// TraceFlags bit values (C++ flow/Tracing.h:39).
const (
	traceFlagUnsampled uint8 = 0b0
	traceFlagSampled   uint8 = 0b1
)

// spanParentSize is the exact byte length of a SPAN_PARENT option value, which C++
// validates at NativeAPI.actor.cpp:7128: an 8-byte IncludeVersion protocol-version
// header + the serialized SpanContext (16-byte traceID + 8-byte spanID + 1-byte
// flags) = 33 bytes.
const spanParentSize = 33

// newSpanContext generates a fresh per-transaction span, matching C++ generateSpanID
// (NativeAPI.actor.cpp:3458-3471): a random 128-bit traceID + random 64-bit spanID,
// with the sampled flag set iff a sample draw falls under sampleRate. The default
// sampleRate is 0.0 (FLOW_KNOBS->TRACING_SAMPLE_RATE, flow/Knobs.cpp:88) ⇒ a default
// client emits a real, UNSAMPLED span on every request — never the all-zero one.
// rand/v2's top-level generator is safe for concurrent use.
func newSpanContext(sampleRate float64) types.SpanContext {
	var sc types.SpanContext
	binary.LittleEndian.PutUint64(sc.TraceID[0:8], rand.Uint64())
	binary.LittleEndian.PutUint64(sc.TraceID[8:16], rand.Uint64())
	sc.SpanID = rand.Uint64()
	if sampleRate > 0 && rand.Float64() < sampleRate {
		sc.Flags = traceFlagSampled
	}
	return sc
}

// childSpanContext links a child span to an injected parent, matching C++
// Span::setParent (flow/Tracing.h:237): inherit the parent's traceID + flags, assign
// a fresh random spanID.
func childSpanContext(parent types.SpanContext) types.SpanContext {
	return types.SpanContext{TraceID: parent.TraceID, SpanID: rand.Uint64(), Flags: parent.Flags}
}

// parseSpanParent decodes a SPAN_PARENT option value (FDBTransactionOptions::SPAN_PARENT,
// NativeAPI.actor.cpp:7126-7133): an 8-byte IncludeVersion protocol-version header
// followed by a little-endian SpanContext — 16-byte traceID, 8-byte spanID, 1-byte
// flags — the format C++ reads via BinaryReader::fromStringRef<SpanContext>(value,
// IncludeVersion()). Input-only: the Go client PARSES a parent handed in by a caller
// (cross-process trace propagation); it never emits this format. The 8-byte version
// header is consumed but not validated — SpanContext has a fixed 3-field layout with
// no version-conditional serialization, so a compatible parent from any FDB build
// decodes identically. We don't reject on the version header — being laxer than C++'s
// BinaryReader here can't cause a wire divergence (the field layout is version-independent).
func parseSpanParent(b []byte) (types.SpanContext, error) {
	if len(b) != spanParentSize {
		return types.SpanContext{}, fmt.Errorf("SPAN_PARENT: got %d bytes, want %d (8-byte version header + 16+8+1 SpanContext)", len(b), spanParentSize)
	}
	var sc types.SpanContext
	copy(sc.TraceID[:], b[8:24]) // skip the 8-byte protocol-version header
	sc.SpanID = binary.LittleEndian.Uint64(b[24:32])
	sc.Flags = b[32]
	return sc, nil
}

// isSampled reports whether a wire SpanContext has the sampled flag set.
func isSampled(sc types.SpanContext) bool { return sc.Flags&traceFlagSampled != 0 }

// spanContextValid mirrors C++ SpanContext::isValid (flow Tracing.h:56): a span
// is valid iff BOTH 64-bit traceID halves are non-zero AND the spanID is non-zero.
// The GRV batcher's fresh-root span starts INVALID (zero traceID, random spanID)
// and is assigned a real traceID only when a sampled link promotes it — see
// batchGRVSpanContext.
func spanContextValid(sc types.SpanContext) bool {
	return binary.LittleEndian.Uint64(sc.TraceID[0:8]) != 0 &&
		binary.LittleEndian.Uint64(sc.TraceID[8:16]) != 0 &&
		sc.SpanID != 0
}

// batchGRVSpanContext folds a GRV batch's per-transaction span contexts into the
// readVersionBatcher's span and returns the getConsistentReadVersion CHILD context
// to stamp on the GetReadVersionRequest wire. This is a 1:1 port of C++
// readVersionBatcher (NativeAPI.actor.cpp:7334 fresh-root span, :7345 per-request
// addLink, :7385 the getConsistentReadVersion call) + getConsistentReadVersion's
// child span (:7238).
//
// The batcher span is a FRESH ROOT (NativeAPI.actor.cpp:7334
// `Span("NAPI:readVersionBatcher")`), built via the no-parent ctor (Tracing.h:160)
// from a default-zero parent SpanContext (Tracing.h:50): traceID 0, random spanID,
// UNSAMPLED — hence spanContextValid==false. Each transaction is connected by a
// LOCAL link (addLink, Tracing.h:198-211), NOT by parenting and NOT on the wire
// (GetReadVersionRequest carries a single SpanContext, no links). addLink mutates
// the batcher span ONLY when the link is sampled and the batch is not yet sampled:
// it flips the batch to sampled and, since it is still invalid (zero traceID),
// assigns a fresh random traceID + spanID. So the wire context is:
//   - all-unsampled batch → {traceID 0, random spanID, unsampled}
//   - ≥1 sampled tx       → {fresh-random traceID, random spanID, sampled}
//     — a brand-new root, NOT any transaction's traceID.
//
// childSpanContext then derives the getConsistentReadVersion child (Tracing.h:147-148:
// inherit traceID+flags, fresh spanID).
func batchGRVSpanContext(txSpans []types.SpanContext) types.SpanContext {
	batch := types.SpanContext{SpanID: rand.Uint64()}
	for _, s := range txSpans {
		if !isSampled(batch) && isSampled(s) {
			batch.Flags = traceFlagSampled
			if !spanContextValid(batch) {
				binary.LittleEndian.PutUint64(batch.TraceID[0:8], rand.Uint64())
				binary.LittleEndian.PutUint64(batch.TraceID[8:16], rand.Uint64())
				batch.SpanID = rand.Uint64()
			}
		}
	}
	return childSpanContext(batch)
}

// otelSpanContext maps a wire SpanContext (Go) onto an OpenTelemetry SpanContext so the
// otel span tree shares the SAME 16-byte traceID that goes on the FDB wire — FDB
// server-side spans (under that traceID) then land in the same trace (RFC-115 §4
// Layer 2). The sampled flag maps to otel TraceFlags so the consumer's ParentBased
// sampler honors WithTracingSampleRate. Marked Remote: this seeds a parent the client
// did not itself start. (Per-op otel spanIDs are otel-minted under this traceID; Go
// owns the wire IDs unconditionally — the single-ID-authority invariant.)
func otelSpanContext(sc types.SpanContext) oteltrace.SpanContext {
	var tid oteltrace.TraceID
	var sid oteltrace.SpanID
	copy(tid[:], sc.TraceID[:])
	// Little-endian to stay CONSISTENT with the traceID, whose 16 bytes are the wire
	// UID's two little-endian uint64 halves copied verbatim above (@claude #303). The
	// otel IDs only need to be deterministic for correlation; LE keeps the spanID's byte
	// order matching the wire's uint64 encoding rather than mixing endiannesses.
	binary.LittleEndian.PutUint64(sid[:], sc.SpanID)
	var flags oteltrace.TraceFlags
	if isSampled(sc) {
		flags = flags.WithSampled(true)
	}
	return oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
		TraceID:    tid,
		SpanID:     sid,
		TraceFlags: flags,
		Remote:     true,
	})
}
