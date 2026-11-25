// Portions derived from FoundationDB Record Layer (MessageHelpers.java,
// QueryResult.java, IndexOrphanBehavior.java, RecordCursor.java,
// and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package executor

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"reflect"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func executeDistinct(
	ctx context.Context,
	p *plans.RecordQueryDistinctPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	// Streaming adjacent-dedup path (resume-clean): the planner sets Streaming
	// only when the inner is ordered by the dedup key, so equal rows are
	// adjacent and only the last emitted key need ride the continuation
	// (gen.DedupContinuation). This is the fix for the fresh-per-page hash-set's
	// cross-page re-admission (TODO.md C5) — a Go-internal continuation, since
	// Java's SELECT DISTINCT does not deduplicate (DIVERGENCES.md).
	if p.IsStreaming() {
		var innerCont []byte
		var lastKey string
		var hasLast bool
		if len(continuation) > 0 {
			var dc gen.DedupContinuation
			if uerr := recordlayer.UnmarshalVTAsJava(&dc, continuation); uerr != nil {
				return nil, fmt.Errorf("invalid streaming-distinct continuation: %w", uerr)
			}
			innerCont = dc.GetInnerContinuation()
			// A nil (absent) lastValue means no prior key — the first resumed
			// row is never wrongly skipped (mirrors DedupCursor). Presence is
			// carried as non-nil bytes; proto3 drops an EMPTY byte string on the
			// wire, so an all-empty dedup key would decode back as "no prior
			// key". That key only arises from a 0-slot / nil-Positional row,
			// which the streaming path never sees — a real SELECT DISTINCT
			// projects at least one slot and a lone NULL packs to "\x00" (a
			// non-empty tuple), so every genuine key survives the round trip.
			if lv := dc.LastValue; lv != nil {
				lastKey = string(lv)
				hasLast = true
			}
		}
		innerCursor, err := ExecutePlan(ctx, p.GetInner(), store, evalCtx, innerCont, props.ClearSkipAndLimit())
		if err != nil {
			return nil, err
		}
		cursor := &distinctStreamCursor{inner: innerCursor, lastKey: lastKey, hasLast: hasLast}
		return applySkipLimit(cursor, props.Skip, props.ReturnedRowLimit), nil
	}

	// Hash path (UNORDERED input): dedup by the packed dedup key via a
	// memory-budget-charged set that SURVIVES THE RESUME (TODO.md C5), so a
	// duplicate whose run straddles a scanned-rows page boundary is not
	// re-admitted (the fresh-per-page set used to lose its state across the
	// internal-paging Execute boundary and return WRONG ROWS). This covers both
	// the unordered case and DISTINCT + ORDER-BY-on-a-non-output-column,
	// neither of which the ordering-detection streaming path (above) reaches.
	//
	// The set crosses the boundary BY REFERENCE, parked in the statement's
	// ExecutionScratch and named by a token in the continuation. Writing it into
	// the continuation instead — which is what a self-contained continuation
	// would require — makes page P carry every key emitted through page P, i.e.
	// O(pages^2) bytes written and re-parsed across a drain; that encoding
	// survives only for a scratch-less execution.
	//
	// Java's SQL layer never dedups SELECT DISTINCT (DIVERGENCES.md),
	// and Java's value-distinct HashSet is likewise un-carried across
	// continuations — so this continuation is Go-internal
	// (gen.DistinctHashContinuation) with no Java wire format to match. The set
	// is bounded by the statement memory budget; a high-cardinality DISTINCT
	// fails LOUDLY on the budget (never silent wrong rows), the signal that the
	// cost-based sort-distinct follow-up should have been chosen.
	return executeHashDistinct(
		ctx,
		p.GetInner(),
		store,
		evalCtx,
		continuation,
		props,
		distinctKey,
		narrowedDedupFor(p),
	)
}

// narrowedDedupFor reads the R3 residual off the plan. Nil for every distinct
// the planner did not narrow, which is every distinct not built over a
// qualifying secondary UNIQUE index — the full dedup, unchanged.
func narrowedDedupFor(p *plans.RecordQueryDistinctPlan) *narrowedDedup {
	if !p.IsNarrowedDedup() {
		return nil
	}
	return &narrowedDedup{exemptSlots: p.GetNarrowedExemptSlots()}
}

// executeUnorderedPrimaryKeyDistinct removes duplicate records by primary key.
// The input need not be ordered, so the seen-key set is the operator's whole
// memory and must survive a resume; executeHashDistinct carries it across the
// page boundary by reference through the statement's ExecutionScratch. Java's
// RecordQueryUnorderedPrimaryKeyDistinctPlan (:100-104) instead mints a fresh
// HashSet per execution and passes the inner continuation through, so a
// duplicate spanning a resume is re-admitted there.
func executeUnorderedPrimaryKeyDistinct(
	ctx context.Context,
	p *plans.RecordQueryUnorderedPrimaryKeyDistinctPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	return executeHashDistinct(
		ctx,
		p.GetInner(),
		store,
		evalCtx,
		continuation,
		props,
		primaryKeyDistinctKey,
		// The primary-key distinct is never narrowed: a PK is a storage
		// invariant with no exempt set and no index state to rest on.
		nil,
	)
}

// executeHashDistinct is the common resume-clean unordered-distinct path. Its
// continuation contains the inner position plus the IDENTITY of the seen-set:
// a scratch token when the execution carries an ExecutionScratch (the live set
// is handed forward untouched — O(1) bytes, linear total work), and otherwise
// every key seen so far in deterministic insertion order (self-contained, and
// quadratic across pages). Each page re-charges the set against its own
// statement budget and releases that charge at cursor teardown.
func executeHashDistinct(
	ctx context.Context,
	inner plans.RecordQueryPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
	keyer queryResultDistinctKeyer,
	narrowed *narrowedDedup,
) (recordlayer.RecordCursor[QueryResult], error) {
	seen := newBoundedSet[string](props.State)
	var order []string
	var adoptedToken int64
	// base is the committed set carried by reference. A scratch-bearing
	// execution always has one — empty on the first page — so every page after
	// it publishes a delta OVER something rather than replacing it.
	var base *seenLayer
	// resumeBytes/resumeInner arm the no-progress re-emit (see distinctHashCursor).
	var resumeBytes, resumeInner []byte
	if evalCtx.Scratch() != nil {
		base = newSeenLayer()
	}
	innerCont := continuation
	if len(continuation) > 0 {
		var dc gen.DistinctHashContinuation
		if uerr := recordlayer.UnmarshalVTAsJava(&dc, continuation); uerr != nil {
			return nil, fmt.Errorf("invalid distinct-hash continuation: %w", uerr)
		}
		innerCont = dc.GetInnerContinuation()
		if token := dc.GetStateToken(); token != 0 {
			// The set was carried BY REFERENCE: take the committed one back
			// from the statement scratch rather than re-parsing it out of the
			// bytes, which is what makes a P-page drain linear instead of
			// quadratic. A token the scratch does not hold is an error, never a
			// silent resume against an empty set.
			//
			// The layer that comes back is READ-ONLY: this page accumulates
			// into its own fresh delta (seen/order, still zero here) so that a
			// retryable failure mid-page leaves the committed set exactly as
			// the retry's re-adoption of this same token expects to find it.
			adopted, aerr := evalCtx.Scratch().adoptDistinct(
				token, int(dc.GetStateDeltaN()), props.State,
			)
			if aerr != nil {
				return nil, aerr
			}
			adoptedToken = token
			base = adopted
			// Kept so a page that consumes NOTHING and leaves the inner where
			// it found it can hand these exact bytes back rather than mint a
			// token — see distinctHashContinuation.ToBytes.
			resumeBytes = continuation
			resumeInner = innerCont
		} else {
			for _, key := range dc.GetSeenKeys() {
				packedKey := string(key)
				added, addErr := seen.Add(packedKey, int64(len(key)))
				if addErr != nil {
					// No cursor exists yet, so release the partially rebuilt set
					// directly instead of relying on a close hook.
					props.State.ReleaseMemory(seen.Charged())
					return nil, addErr
				}
				if added {
					order = append(order, packedKey)
				}
			}
		}
	}

	innerCursor, err := ExecutePlan(
		ctx,
		inner,
		store,
		evalCtx,
		innerCont,
		props.ClearSkipAndLimit(),
	)
	if err != nil {
		props.State.ReleaseMemory(seen.Charged())
		return nil, err
	}
	cursor := &distinctHashCursor{
		inner:        innerCursor,
		base:         base,
		seen:         seen,
		order:        order,
		keyer:        keyer,
		scratch:      evalCtx.Scratch(),
		adoptedToken: adoptedToken,
		resumeBytes:  resumeBytes,
		resumeInner:  resumeInner,
		// RFC-210 R3: the residual narrowing is orthogonal to WHERE the
		// seen-set lives. It decides WHICH rows enter the set (only those in
		// the proving index's exempt subset); the scratch decides how the set
		// crosses a page boundary. Both survive the merge.
		narrowed: narrowed,
	}
	return newCloseHookCursor(
		applySkipLimit(cursor, props.Skip, props.ReturnedRowLimit),
		func() {
			// Release exactly what this cursor still OWNS. A cursor that PARKED
			// handed part of its delta charge to the scratch entry, which
			// outlives it — releasing THAT would tell the budget a live set is
			// free and an accumulating shape would grow invisibly; the entry's
			// own retirement returns it (SweepAfterPage). The rest — the keys
			// sighted after the last continuation was serialized — is held by
			// nothing once this cursor closes, and used to stay charged to the
			// statement forever. A cursor that never parked owns all of it, so
			// this is the same release it always did.
			props.State.ReleaseMemory(cursor.releaseUnhanded())
		},
	), nil
}

// distinctKey builds a DISTINCT dedup key by packing the row's ordinal slot
// values into an FDB tuple, mirroring Java's RecordQueryUnorderedDistinctPlan,
// which dedups via Set<Key.Evaluated> — structured value equality, never a
// delimiter-joined string. Tuple encoding is length-prefixed and type-tagged,
// so no slot value can forge an inter-column boundary (a string containing the
// old "|NAME=type:" separator no longer collides two distinct rows) and SQL
// NULL has its own tuple code, distinct from any string. Names are not part of
// the key: a physical distinct's inner produces ONE schema, so every row shares
// the same slot names in the same positions — the value tuple alone is the
// comparison key, exactly as Java's comparison-key VALUES are. A nil-Positional
// row yields the empty key.
func distinctKey(qr QueryResult) (string, error) {
	if qr.Positional == nil || qr.Positional.Type == nil {
		return "", nil
	}
	return packedDedupKey(qr.Positional.Slots)
}

// primaryKeyDistinctKey packs exactly the physical primary key carried by a
// QueryResult. A missing key is a plan/executor contract violation: treating
// nil as the empty tuple would collapse unrelated computed rows silently.
func primaryKeyDistinctKey(qr QueryResult) (string, error) {
	if qr.PrimaryKey == nil {
		return "", fmt.Errorf(
			"unordered primary-key distinct: input row has no primary key",
		)
	}
	return string(qr.PrimaryKey.Pack()), nil
}

// packedDedupKey encodes a row's slot values into an unambiguous FDB-tuple key
// for DISTINCT / UNION-DISTINCT deduplication — the single encoder every dedup
// path routes through (distinctKey, cteDedupKeyer.key, queryResultKey). Tuple
// encoding is typed and length-prefixed, so no value can forge an inter-column
// boundary (the defect of a '|'-joined %v string, where a value containing the
// delimiter collapsed two different rows) and NULL has its own tuple code
// distinct from any string (the "\x00NULL\x00"-sentinel collision). Java dedups
// via Set<Key.Evaluated> structured value equality; this is the Go equivalent.
// The [16]byte→UUID arm mirrors intersectionCompKeyFunc's tuple
// canonicalization. Composite values use a DISTINCT-only typed recursive
// encoding. It deliberately does not reuse the continuation encoding as-is:
// continuations must preserve raw floating-point bits so they can reconstruct
// the original row, while DISTINCT must canonicalize NaNs at every nesting
// level. (The merge-sort union dedups via compareValues on evaluated keys
// instead — Java UnionCursor's advance-all-equal — so it no longer packs a
// dedup key at all.)
//
// A zero-valued FLOAT/DOUBLE slot is packed VERBATIM, sign bit and all, so
// -0.0 and +0.0 dedup as two distinct values. NaNs are the one normalization:
// every payload/sign representation is replaced with one quiet NaN per width,
// matching Java Float.equals/Double.equals (via floatToIntBits/doubleToLongBits)
// and the in-memory total comparator, both of which make all NaNs equal. Raw
// FDB tuple keys preserve NaN payloads, which is why a FLOAT/DOUBLE UNIQUE
// index cannot prove logical DISTINCT even though signed-zero keys can remain
// separate. Unlike cmpAny's IEEE `=`, Java's structured value equality and
// tuple encoding preserve the zero sign bit. Canonicalizing only this encoder
// would make query results depend on the plan:
//
//   - The ORDERED dedup path (distinctStreamCursor) compares a row against the
//     PREVIOUS one only, which is sound because equal keys are adjacent in
//     index order — and index order keeps the sign. Canonicalizing the key
//     while the order stays signed breaks that adjacency, so
//     `SELECT DISTINCT d, a ORDER BY d, a` over rows (-0.0,1) (-0.0,2) (+0.0,1)
//     emitted (-0.0,1) and (+0.0,1) as two rows — a duplicate under the very
//     equality the canonicalization was asserting.
//   - GROUP BY cannot follow. A maintained aggregate index stores each group
//     as its own index entry keyed by the grouping prefix, so two signed zeros
//     are two physical entries that Java also reads; merging them would mean
//     either writing key bytes Java does not, or reporting a different group
//     count than Java does from the same index.
//   - A metadata key can prove logical DISTINCT only when its physical bytes
//     are globally congruent with this value identity. FLOAT/DOUBLE keys fail
//     that proof because raw NaN encodings remain physically distinct.
//
// Splitting needs no guard on any of those paths; merging needs one on each
// and still ends in a divergence. CockroachDB merges because it normalizes
// zero inside its key encoder (EncodeFloatAscending) — the same principle,
// applied to an encoding it controls and we do not.
//
// The float32 arm below is reached only by DIRECT callers, never by a query. A
// 32-bit FLOAT column arrives at this layer already WIDENED to float64, so SQL
// DISTINCT on a FLOAT is served by the float64 arm. MEASURED: a panic at the
// top of the float32 arm passes the whole SQL-level gate and fails only
// TestPackedDedupKeyCanonicalizesNaNsButPreservesSignedZero, which calls this
// function directly. The arm is kept because this takes []any and its contract
// is over Go values rather than over what today's SQL layer happens to hand
// it — but it must not be cited as evidence that a float32 path is exercised,
// and the group-key encoder deliberately has no such arm for that reason.
func packedDedupKey(slots []any) (string, error) {
	t := make(tuple.Tuple, len(slots))
	for i, v := range slots {
		switch tv := v.(type) {
		case float32:
			if math.IsNaN(float64(tv)) {
				tv = math.Float32frombits(distinctCanonicalNaN32Bits)
			}
			t[i] = tv
		case float64:
			if math.IsNaN(tv) {
				tv = math.Float64frombits(distinctCanonicalNaN64Bits)
			}
			t[i] = tv
		case nil, int64, int, uint, uint64, string, []byte, bool:
			t[i] = tv
		case int32:
			t[i] = int64(tv)
		case [16]byte:
			t[i] = tuple.UUID(tv)
		default:
			// Composite/nested slot (struct, array, message): the tuple layer
			// cannot pack it. Encode with the DISTINCT-only type-tagged,
			// length-prefixed recursive codec as one []byte tuple slot —
			// boundary-safe and collision-free, with recursive NaN
			// canonicalization but verbatim signed-zero bits. The retired %T:%v
			// rendering collided on composites ([]any{"a b"} vs
			// []any{"a","b"} both rendered "[a b]") and split equal protos
			// across generated/dynamicpb representations (RFC-180 C1).
			b, err := appendDistinctValue(nil, v)
			if err != nil {
				return "", fmt.Errorf("dedup key slot %d: %w", i, err)
			}
			// Keep the composite's outer type distinct from a user BYTES value
			// containing these exact encoder bytes. A nested FDB tuple has its own
			// type code, while b retains the recursive ARRAY/STRUCT type tags.
			t[i] = tuple.Tuple{b}
		}
	}
	return string(t.Pack()), nil
}

const (
	distinctCanonicalNaN32Bits uint32 = 0x7fc00000
	distinctCanonicalNaN64Bits uint64 = 0x7ff8000000000000
)

// appendDistinctValue is the composite-value encoder used only by logical
// value deduplication. Its framing intentionally mirrors appendContValue's
// proven type-tagged, length-prefixed format, but its floating-point contract is
// different: every NaN sign/payload is one value, while both signed-zero bit
// patterns remain distinct. Keeping this as a separate recursive entry point is
// essential — appendContValue is a reversible continuation codec and therefore
// must retain every raw bit of the value it will later reconstruct.
func appendDistinctValue(buf []byte, v any) ([]byte, error) {
	switch t := v.(type) {
	case float32:
		if math.IsNaN(float64(t)) {
			t = math.Float32frombits(distinctCanonicalNaN32Bits)
		}
		return appendContValue(buf, t)
	case float64:
		if math.IsNaN(t) {
			t = math.Float64frombits(distinctCanonicalNaN64Bits)
		}
		return appendContValue(buf, t)
	case []any:
		buf = append(buf, contValList)
		buf = binary.AppendUvarint(buf, uint64(len(t)))
		for _, element := range t {
			var err error
			buf, err = appendDistinctValue(buf, element)
			if err != nil {
				return nil, err
			}
		}
		return buf, nil
	case proto.Message:
		return appendDistinctProtoMessage(buf, t)
	default:
		// Scalars without floating payloads already have the exact typed,
		// collision-free representation DISTINCT needs. Delegating those arms
		// avoids a second implementation drifting from the continuation codec;
		// only the recursive and protobuf arms above intentionally diverge.
		return appendContValue(buf, v)
	}
}

// appendDistinctProtoMessage encodes a STRUCT value independently of its Go
// representation. Generated and dynamicpb messages with the same protobuf type
// and contents are the same logical STRUCT value, so the key contains the full
// protobuf name and deterministic payload, not appendContProtoMessage's
// continuation-only generated/dynamic representation flag.
//
// The input is cloned before canonicalization. DISTINCT is observational only;
// computing a key must never rewrite a QueryResult that may still be consumed by
// another operator. Descriptor-driven traversal reaches populated ordinary,
// oneof, and extension fields, including repeated/map/nested message values.
// Unknown wire fields are retained verbatim: without a descriptor their fixed32
// or fixed64 bytes cannot safely be classified as floating point.
func appendDistinctProtoMessage(buf []byte, msg proto.Message) ([]byte, error) {
	if msg == nil || isNilProtoMessage(msg) {
		return nil, fmt.Errorf("distinct: cannot encode a nil proto message")
	}
	cloned := proto.Clone(msg)
	if cloned == nil || isNilProtoMessage(cloned) {
		return nil, fmt.Errorf("distinct: cannot clone proto message of type %T", msg)
	}
	reflection := cloned.ProtoReflect()
	if !reflection.IsValid() {
		return nil, fmt.Errorf("distinct: cannot encode invalid proto message of type %T", msg)
	}
	canonicalizeDistinctProtoNaNs(reflection)
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(cloned)
	if err != nil {
		return nil, fmt.Errorf("distinct: cannot encode proto message %q: %w", reflection.Descriptor().FullName(), err)
	}
	fullName := string(reflection.Descriptor().FullName())
	buf = append(buf, contValProtoMsg)
	buf = binary.AppendUvarint(buf, uint64(len(fullName)))
	buf = append(buf, fullName...)
	buf = binary.AppendUvarint(buf, uint64(len(payload)))
	return append(buf, payload...), nil
}

func isNilProtoMessage(msg proto.Message) bool {
	v := reflect.ValueOf(msg)
	return v.Kind() == reflect.Pointer && v.IsNil()
}

// canonicalizeDistinctProtoNaNs rewrites only the cloned message passed by
// appendDistinctProtoMessage. Message.Range covers populated regular, oneof,
// and extension fields. Map updates are collected and applied after Range so
// traversal never mutates the map being iterated.
func canonicalizeDistinctProtoNaNs(message protoreflect.Message) {
	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		switch {
		case field.IsMap():
			protoMap := value.Map()
			type mapUpdate struct {
				key   protoreflect.MapKey
				value protoreflect.Value
			}
			var updates []mapUpdate
			protoMap.Range(func(key protoreflect.MapKey, mapValue protoreflect.Value) bool {
				canonical, changed := canonicalDistinctProtoValue(field.MapValue(), mapValue)
				if changed {
					updates = append(updates, mapUpdate{key: key, value: canonical})
				}
				return true
			})
			for _, update := range updates {
				protoMap.Set(update.key, update.value)
			}
		case field.IsList():
			list := value.List()
			for i := 0; i < list.Len(); i++ {
				canonical, changed := canonicalDistinctProtoValue(field, list.Get(i))
				if changed {
					list.Set(i, canonical)
				}
			}
		default:
			canonical, changed := canonicalDistinctProtoValue(field, value)
			if changed {
				message.Set(field, canonical)
			}
		}
		return true
	})
}

func canonicalDistinctProtoValue(
	field protoreflect.FieldDescriptor,
	value protoreflect.Value,
) (protoreflect.Value, bool) {
	switch field.Kind() {
	case protoreflect.FloatKind:
		floating := float32(value.Float())
		if math.IsNaN(float64(floating)) {
			return protoreflect.ValueOfFloat32(
				math.Float32frombits(distinctCanonicalNaN32Bits),
			), true
		}
	case protoreflect.DoubleKind:
		floating := value.Float()
		if math.IsNaN(floating) {
			return protoreflect.ValueOfFloat64(
				math.Float64frombits(distinctCanonicalNaN64Bits),
			), true
		}
	case protoreflect.MessageKind, protoreflect.GroupKind:
		canonicalizeDistinctProtoNaNs(value.Message())
	}
	return value, false
}
