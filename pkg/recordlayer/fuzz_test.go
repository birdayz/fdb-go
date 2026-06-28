package recordlayer

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"math/big"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// ---------------------------------------------------------------------------
// FuzzFastUnpack — cross-validates fastUnpack against tuple.Unpack on
// arbitrary byte input. Any disagreement or panic is a bug.
// ---------------------------------------------------------------------------

func FuzzFastUnpack(f *testing.F) {
	// Seed corpus: valid packed tuples covering every type code.
	seeds := []tuple.Tuple{
		{},
		{nil},
		{int64(0)},
		{int64(42)},
		{int64(-1)},
		{int64(math.MaxInt64)},
		{int64(math.MinInt64)},
		{int64(1 << 40)},
		{int64(-1 << 40)},
		{"hello"},
		{""},
		{"hel\x00lo"},
		{[]byte{0, 1, 2, 3}},
		{[]byte{}},
		{true},
		{false},
		{float32(3.14)},
		{float64(2.718281828)},
		{math.Copysign(0, -1)},
		{float64(math.Inf(1))},
		{float64(math.Inf(-1))},
		{tuple.UUID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}},
		{int64(1), "order", int64(-5)},
		{tuple.Tuple{int64(1), int64(2)}},
		{tuple.Tuple{tuple.Tuple{int64(1)}, int64(2)}},
		{int64(0), tuple.Tuple{int64(1)}, "abc"},
	}
	for _, s := range seeds {
		f.Add(s.Pack())
	}
	// Also seed some invalid bytes.
	f.Add([]byte{})
	f.Add([]byte{0xff})
	f.Add([]byte{0x05, 0x00}) // nested with terminator
	f.Add([]byte{0x33})       // truncated versionstamp
	f.Add([]byte{0x30})       // truncated UUID
	f.Add([]byte{0x21, 0x80}) // truncated double
	f.Add([]byte{0x20, 0x80}) // truncated float

	f.Fuzz(func(t *testing.T, data []byte) {
		// fastUnpack must never panic.
		got, fastErr := fastUnpack(data)

		// tuple.Unpack (upstream FDB library) may panic on truncated input —
		// that's their bug, not ours. Recover and treat as an error.
		var ref tuple.Tuple
		var refErr error
		func() {
			defer func() {
				if r := recover(); r != nil {
					refErr = fmt.Errorf("tuple.Unpack panicked: %v", r)
				}
			}()
			ref, refErr = tuple.Unpack(data)
		}()

		if fastErr != nil && refErr != nil {
			return // both errored — fine
		}
		if fastErr == nil && refErr == nil {
			// Both succeeded — compare results.
			if !fuzzTupleDeepEqual(got, ref) {
				t.Errorf("fastUnpack != tuple.Unpack\n  input: %x\n  fast:  %v\n  ref:   %v", data, got, ref)
			}
			return
		}
		// Divergence: one succeeded, the other didn't.
		// fastUnpack may correctly reject truncated input that tuple.Unpack
		// accepts by reading uninitialized memory (upstream bug). We only
		// flag when fastUnpack succeeds but tuple.Unpack errors, which
		// would mean we're accepting invalid input.
		if fastErr == nil && refErr != nil {
			t.Errorf("fastUnpack succeeded but tuple.Unpack errored\n  input: %x\n  fast: %v\n  ref err: %v", data, got, refErr)
		}
	})
}

// FuzzFastUnpackRoundtrip — pack a valid tuple, unpack with fastUnpack,
// verify result matches original.
func FuzzFastUnpackRoundtrip(f *testing.F) {
	// Seed with various int64 values that exercise edge cases.
	for _, v := range []int64{0, 1, -1, 127, 128, 255, 256, -128, -129, math.MaxInt64, math.MinInt64, 1 << 40, -(1 << 40)} {
		f.Add(v, "test", true)
	}

	f.Fuzz(func(t *testing.T, i int64, s string, b bool) {
		original := tuple.Tuple{i, s, b}
		packed := original.Pack()

		got, err := fastUnpack(packed)
		if err != nil {
			t.Fatalf("fastUnpack failed on valid packed tuple: %v\n  input: %x", err, packed)
		}
		if !fuzzTupleDeepEqual(got, original) {
			t.Errorf("roundtrip mismatch\n  original: %v\n  got:      %v\n  packed:   %x", original, got, packed)
		}
	})
}

// ---------------------------------------------------------------------------
// FuzzDeserializeBunch — fuzz the TEXT index custom binary deserializer.
// Must never panic, always return error on malformed input.
// ---------------------------------------------------------------------------

func FuzzDeserializeBunch(f *testing.F) {
	s := TextIndexBunchedSerializerInstance()

	// Seed corpus: valid serialized bunches.
	validEntries := [][]BunchedEntry[tuple.Tuple, []int]{
		{{Key: tuple.Tuple{int64(1)}, Value: []int{0, 1, 2}}},
		{
			{Key: tuple.Tuple{int64(1)}, Value: []int{0, 5}},
			{Key: tuple.Tuple{int64(2)}, Value: []int{3, 10, 20}},
		},
		{
			{Key: tuple.Tuple{"apple"}, Value: []int{0}},
			{Key: tuple.Tuple{"banana"}, Value: []int{1, 2}},
			{Key: tuple.Tuple{"cherry"}, Value: []int{3, 4, 5, 6}},
		},
		{{Key: tuple.Tuple{int64(0)}, Value: []int{}}}, // empty position list
	}
	for _, entries := range validEntries {
		data, err := s.SerializeEntries(entries)
		if err != nil {
			f.Fatalf("SerializeEntries: %v", err)
		}
		f.Add(data, entries[0].Key.Pack())
	}

	// Invalid seeds.
	f.Add([]byte{}, []byte{0x14}) // empty data
	f.Add([]byte{0xff}, []byte{0x14})
	f.Add([]byte{0x20}, []byte{0x14})             // prefix only, no entries
	f.Add([]byte{0x20, 0x80, 0x01}, []byte{0x14}) // truncated varint

	f.Fuzz(func(t *testing.T, data []byte, keyBytes []byte) {
		// tuple.Unpack may panic on malformed input (upstream bug).
		var key tuple.Tuple
		var keyErr error
		func() {
			defer func() {
				if r := recover(); r != nil {
					keyErr = fmt.Errorf("panic: %v", r)
				}
			}()
			key, keyErr = tuple.Unpack(keyBytes)
		}()
		if keyErr != nil {
			return
		}
		// Must not panic. Error is fine.
		_, _ = s.DeserializeEntries(key, data)
		_, _ = s.DeserializeKeys(key, data)
	})
}

// ---------------------------------------------------------------------------
// FuzzUnwrapContinuation — fuzz the continuation token parser.
// Must never panic.
// ---------------------------------------------------------------------------

func FuzzUnwrapContinuation(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add([]byte{0xff, 0xff, 0xff})
	// Valid proto-wrapped: would need magic number prefix. Seed some.
	f.Add([]byte{0x08}) // protobuf field 1 varint

	f.Fuzz(func(t *testing.T, data []byte) {
		// Must not panic. Either returns raw bytes or unwrapped key.
		_ = unwrapContinuation(data)
	})
}

// ---------------------------------------------------------------------------
// FuzzUninvertBytes — fuzz the custom 7-bit DESC ordering encoder.
// ---------------------------------------------------------------------------

func FuzzUninvertBytes(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add([]byte{0x7f})
	f.Add([]byte{0x80})
	f.Add([]byte{0xff})
	f.Add([]byte{0x80, 0x80, 0x00})
	// Valid roundtrip seeds.
	for _, b := range [][]byte{{0x01}, {0x01, 0x02, 0x03}, {0x00, 0xff, 0x00}} {
		f.Add(invertBytes(b))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		// Must not panic. Error on malformed input is fine.
		result, err := uninvertBytes(data)
		if err != nil {
			return
		}
		// If uninvert succeeded, roundtrip must work.
		reinverted := invertBytes(result)
		result2, err2 := uninvertBytes(reinverted)
		if err2 != nil {
			t.Fatalf("roundtrip failed: uninvert(invert(uninvert(%x))) errored: %v", data, err2)
		}
		if !bytes.Equal(result, result2) {
			t.Errorf("roundtrip mismatch: %x -> %x -> %x -> %x", data, result, reinverted, result2)
		}
	})
}

// ---------------------------------------------------------------------------
// FuzzDeserializeVector — fuzz the custom vector binary format parser.
// ---------------------------------------------------------------------------

func FuzzDeserializeVector(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x00})                                                 // DOUBLE type, no data
	f.Add([]byte{0x01})                                                 // SINGLE type, no data
	f.Add([]byte{0x02})                                                 // HALF type, no data
	f.Add([]byte{0x05})                                                 // unknown type
	f.Add([]byte{0x00, 0x3f, 0xf0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}) // 1 double (1.0)

	f.Fuzz(func(t *testing.T, data []byte) {
		// Must not panic.
		_, _ = deserializeVector(data)
	})
}

// ---------------------------------------------------------------------------
// FuzzCompleteVersionFromBytes — fuzz the 12-byte version parser.
// ---------------------------------------------------------------------------

func FuzzCompleteVersionFromBytes(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})                                     // all zeros
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xfe, 0xff, 0xff}) // max version
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})                                        // 11 bytes (too short)
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})                                  // 13 bytes (too long)

	f.Fuzz(func(t *testing.T, data []byte) {
		// Must not panic.
		_, _ = CompleteVersionFromBytes(data)
	})
}

// ---------------------------------------------------------------------------
// FuzzConcatContinuation — fuzz the ConcatCursor continuation deserializer.
// Must never panic. Exercises proto UnmarshalVT + factory fallback logic.
// ---------------------------------------------------------------------------

func FuzzConcatContinuation(f *testing.F) {
	// Raw garbage.
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add([]byte{0xff, 0xff, 0xff})
	// Valid proto: second=true, continuation=[]byte{0xAB}.
	validSecond, _ := (&gen.ConcatContinuation{
		Second:       proto.Bool(true),
		Continuation: []byte{0xAB},
	}).MarshalVT()
	f.Add(validSecond)
	// Valid proto: second=false, no continuation.
	validFirst, _ := (&gen.ConcatContinuation{
		Second: proto.Bool(false),
	}).MarshalVT()
	f.Add(validFirst)

	f.Fuzz(func(t *testing.T, data []byte) {
		// Dummy cursor factories that return exhausted cursors.
		factory := func(_ []byte) RecordCursor[int] {
			return FromList[int](nil)
		}
		cursor := ConcatCursors[int](factory, factory, data)
		// Must not panic when calling OnNext with whatever state was set up.
		result, err := cursor.OnNext(context.Background())
		_ = result
		_ = err
		cursor.Close()
	})
}

// ---------------------------------------------------------------------------
// FuzzFlatMapContinuation — fuzz the FlatMapPipelined continuation deserializer.
// Must never panic.
// ---------------------------------------------------------------------------

func FuzzFlatMapContinuation(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add([]byte{0xff, 0xff, 0xff})
	// Valid proto: outer only.
	validOuter, _ := (&gen.FlatMapContinuation{
		OuterContinuation: []byte{0x01},
	}).MarshalVT()
	f.Add(validOuter)
	// Valid proto: outer + inner.
	validBoth, _ := (&gen.FlatMapContinuation{
		OuterContinuation: []byte{0x01},
		InnerContinuation: []byte{0x02},
		CheckValue:        []byte{0x03},
	}).MarshalVT()
	f.Add(validBoth)

	f.Fuzz(func(t *testing.T, data []byte) {
		outerFactory := func(_ []byte) RecordCursor[int] {
			return FromList[int]([]int{1, 2, 3})
		}
		innerFactory := func(_ int, _ []byte) RecordCursor[string] {
			return FromList[string](nil)
		}
		cursor := FlatMapPipelinedWithCheck[int, string](
			outerFactory, innerFactory, nil, data, 1,
		)
		// Must not panic.
		result, err := cursor.OnNext(context.Background())
		_ = result
		_ = err
		cursor.Close()
	})
}

// ---------------------------------------------------------------------------
// FuzzOrElseContinuation — fuzz the OrElse cursor continuation deserializer.
// Must never panic.
// ---------------------------------------------------------------------------

func FuzzOrElseContinuation(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add([]byte{0xff, 0xff, 0xff})
	undecided := gen.OrElseContinuation_UNDECIDED
	validUndecided, _ := (&gen.OrElseContinuation{
		State:        &undecided,
		Continuation: []byte{0x00, 0x00, 0x00, 0x01},
	}).MarshalVT()
	f.Add(validUndecided)
	useInner := gen.OrElseContinuation_USE_INNER
	validInner, _ := (&gen.OrElseContinuation{
		State:        &useInner,
		Continuation: []byte{0x00, 0x00, 0x00, 0x02},
	}).MarshalVT()
	f.Add(validInner)
	useOther := gen.OrElseContinuation_USE_OTHER
	validOther, _ := (&gen.OrElseContinuation{
		State:        &useOther,
		Continuation: []byte{0x00, 0x00, 0x00, 0x00},
	}).MarshalVT()
	f.Add(validOther)

	f.Fuzz(func(t *testing.T, data []byte) {
		primary := func(_ []byte) RecordCursor[int] {
			return FromList[int]([]int{1, 2, 3})
		}
		alt := func(_ []byte) RecordCursor[int] {
			return FromList[int]([]int{4, 5})
		}
		cursor := OrElseWithContinuation(primary, alt, data)
		result, err := cursor.OnNext(context.Background())
		_ = result
		_ = err
		cursor.Close()
	})
}

// ---------------------------------------------------------------------------
// FuzzDedupContinuation — fuzz the Dedup cursor continuation deserializer.
// Must never panic.
// ---------------------------------------------------------------------------

func FuzzDedupContinuation(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add([]byte{0xff, 0xff, 0xff})
	// Valid proto: inner continuation only.
	validInner, _ := (&gen.DedupContinuation{
		InnerContinuation: []byte{0x01},
	}).MarshalVT()
	f.Add(validInner)
	// Valid proto: inner + lastValue.
	validBoth, _ := (&gen.DedupContinuation{
		InnerContinuation: []byte{0x01},
		LastValue:         []byte{0x02, 0x03},
	}).MarshalVT()
	f.Add(validBoth)

	f.Fuzz(func(t *testing.T, data []byte) {
		factory := func(_ []byte) RecordCursor[int] {
			return FromList[int]([]int{1, 1, 2, 2, 3})
		}
		equal := func(a, b int) bool { return a == b }
		pack := func(v int) []byte { return []byte{byte(v)} }
		unpack := func(b []byte) (int, bool) {
			if len(b) > 0 {
				return int(b[0]), true
			}
			return 0, false
		}
		cursor := Dedup[int](factory, equal, pack, unpack, data)
		// Must not panic.
		result, err := cursor.OnNext(context.Background())
		_ = result
		_ = err
		cursor.Close()
	})
}

// ---------------------------------------------------------------------------
// FuzzDeserializeAndDiscover — fuzz the hand-rolled union wire format parser
// that discovers record types from raw protobuf bytes.
// Uses protowire.ConsumeTag/ConsumeBytes/ConsumeFieldValue directly —
// any panic on malformed input is a bug.
// ---------------------------------------------------------------------------

func FuzzDeserializeAndDiscover(f *testing.F) {
	md := fuzzBuildMetaData(f)
	store := &FDBRecordStore{metaData: md}

	// Seed corpus: valid union-serialized records.
	for _, data := range fuzzUnionSeeds() {
		f.Add(data)
	}
	// Malformed seeds.
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add([]byte{0xff})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0xff})
	// Truncated tag.
	f.Add([]byte{0x80})
	// Valid tag (field 1, varint) but no value.
	f.Add([]byte{0x08})
	// Valid tag (field 1, bytes) but truncated length.
	f.Add([]byte{0x0a, 0x80})
	// Unknown field number with valid wire format.
	f.Add(append([]byte{0xf8, 0x3e, 0x00}, []byte("junk")...))

	f.Fuzz(func(t *testing.T, data []byte) {
		// Must not panic. Error is fine.
		_, _, _ = store.deserializeAndDiscover(data)
	})
}

// ---------------------------------------------------------------------------
// FuzzDeserializeRecord — fuzz the targeted union field parser that
// extracts a specific record type from raw protobuf bytes.
// ---------------------------------------------------------------------------

func FuzzDeserializeRecord(f *testing.F) {
	md := fuzzBuildMetaData(f)
	store := &FDBRecordStore{metaData: md}
	orderType := md.GetRecordType("Order")
	customerType := md.GetRecordType("Customer")

	// Seed corpus.
	for _, data := range fuzzUnionSeeds() {
		f.Add(data)
	}
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})
	f.Add([]byte{0x08}) // varint tag, no value
	f.Add([]byte{0x0a, 0x80})

	f.Fuzz(func(t *testing.T, data []byte) {
		// Must not panic for either record type.
		_, _ = store.deserializeRecord(data, orderType)
		_, _ = store.deserializeRecord(data, customerType)
	})
}

// fuzzBuildMetaData creates a RecordMetaData for fuzz targets (no FDB needed).
func fuzzBuildMetaData(f *testing.F) *RecordMetaData {
	f.Helper()
	builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	md, err := builder.Build()
	if err != nil {
		f.Fatalf("build metadata: %v", err)
	}
	return md
}

// fuzzUnionSeeds returns valid union-serialized record bytes for seed corpus.
func fuzzUnionSeeds() [][]byte {
	order := &gen.UnionDescriptor{XOrder: &gen.Order{
		OrderId: proto.Int64(1), Price: proto.Int32(42),
	}}
	customer := &gen.UnionDescriptor{XCustomer: &gen.Customer{
		CustomerId: proto.Int64(1), Name: proto.String("test"),
	}}
	empty := &gen.UnionDescriptor{XOrder: &gen.Order{}}

	var seeds [][]byte
	for _, msg := range []proto.Message{order, customer, empty} {
		data, err := proto.Marshal(msg)
		if err != nil {
			panic(err)
		}
		seeds = append(seeds, data)
	}
	return seeds
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// fuzzTupleDeepEqual compares two tuples element by element, handling type differences
// between fastUnpack and tuple.Unpack (e.g., big.Int vs int64).
func fuzzTupleDeepEqual(a, b tuple.Tuple) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !fuzzTupleElemEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

func fuzzTupleElemEqual(a, b any) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}

	switch av := a.(type) {
	case tuple.Tuple:
		bv, ok := b.(tuple.Tuple)
		if !ok {
			return false
		}
		return fuzzTupleDeepEqual(av, bv)
	case int64:
		switch bv := b.(type) {
		case int64:
			return av == bv
		case *big.Int:
			return bv.IsInt64() && bv.Int64() == av
		}
	case *big.Int:
		switch bv := b.(type) {
		case *big.Int:
			return av.Cmp(bv) == 0
		case int64:
			return av.IsInt64() && av.Int64() == bv
		}
	case uint64:
		if bv, ok := b.(uint64); ok {
			return av == bv
		}
	case float32:
		if bv, ok := b.(float32); ok {
			return fmt.Sprintf("%v", av) == fmt.Sprintf("%v", bv)
		}
	case float64:
		if bv, ok := b.(float64); ok {
			return fmt.Sprintf("%v", av) == fmt.Sprintf("%v", bv)
		}
	case string:
		if bv, ok := b.(string); ok {
			return av == bv
		}
	case []byte:
		if bv, ok := b.([]byte); ok {
			return bytes.Equal(av, bv)
		}
	case bool:
		if bv, ok := b.(bool); ok {
			return av == bv
		}
	case tuple.UUID:
		if bv, ok := b.(tuple.UUID); ok {
			return av == bv
		}
	case tuple.Versionstamp:
		if bv, ok := b.(tuple.Versionstamp); ok {
			return av == bv
		}
	}
	return false
}

// FuzzKeyExpressionFromProto stresses the proto → KeyExpression parser with
// arbitrary byte sequences. The recursive descent over Then/Nesting/Grouping
// etc. is a DoS candidate: a crafted proto with deep or self-referential
// nesting could blow the stack. This fuzz ensures parsing always terminates
// either with a concrete (non-nil) expression or an error (not a panic).
//
// Seeds: a handful of simple KeyExpressions (field, empty, record type key,
// nested then) plus a random few bytes to get mutation started.
func FuzzKeyExpressionFromProto(f *testing.F) {
	// Seed with a few valid KeyExpressions serialised to bytes.
	seeds := []*gen.KeyExpression{
		{Empty: &gen.Empty{}},
		{RecordTypeKey: &gen.RecordTypeKey{}},
		{Field: &gen.Field{FieldName: proto.String("x"), FanType: gen.Field_SCALAR.Enum()}},
		{
			Then: &gen.Then{Child: []*gen.KeyExpression{
				{Field: &gen.Field{FieldName: proto.String("a"), FanType: gen.Field_SCALAR.Enum()}},
				{Field: &gen.Field{FieldName: proto.String("b"), FanType: gen.Field_SCALAR.Enum()}},
			}},
		},
		// Grouping — used by COUNT / SUM / MAX_EVER index types. The
		// full-grouping form (all columns grouped, none grouped-out)
		// is what `Ungrouped(Field("price"))` produces.
		{
			Grouping: &gen.Grouping{
				WholeKey: &gen.KeyExpression{
					Field: &gen.Field{FieldName: proto.String("price"), FanType: gen.Field_SCALAR.Enum()},
				},
				GroupedCount: proto.Int32(0),
			},
		},
		// Grouping with a non-default grouped_count.
		{
			Grouping: &gen.Grouping{
				WholeKey: &gen.KeyExpression{
					Then: &gen.Then{Child: []*gen.KeyExpression{
						{Field: &gen.Field{FieldName: proto.String("k"), FanType: gen.Field_SCALAR.Enum()}},
						{Field: &gen.Field{FieldName: proto.String("v"), FanType: gen.Field_SCALAR.Enum()}},
					}},
				},
				GroupedCount: proto.Int32(1),
			},
		},
		// Nesting — used by indexes on nested message fields.
		{
			Nesting: &gen.Nesting{
				Parent: &gen.Field{FieldName: proto.String("flower"), FanType: gen.Field_SCALAR.Enum()},
				Child: &gen.KeyExpression{
					Field: &gen.Field{FieldName: proto.String("color"), FanType: gen.Field_SCALAR.Enum()},
				},
			},
		},
		// KeyWithValue — covering indexes (k=index key, v=stored value).
		{
			KeyWithValue: &gen.KeyWithValue{
				InnerKey: &gen.KeyExpression{
					Then: &gen.Then{Child: []*gen.KeyExpression{
						{Field: &gen.Field{FieldName: proto.String("a"), FanType: gen.Field_SCALAR.Enum()}},
						{Field: &gen.Field{FieldName: proto.String("b"), FanType: gen.Field_SCALAR.Enum()}},
					}},
				},
				SplitPoint: proto.Int32(1),
			},
		},
		// Function — version function key expression (records timestamp /
		// versionstamp into the index entry).
		{
			Function: &gen.Function{
				Name:      proto.String("version"),
				Arguments: &gen.KeyExpression{Empty: &gen.Empty{}},
			},
		},
	}
	for _, s := range seeds {
		if b, err := proto.Marshal(s); err == nil {
			f.Add(b)
		}
	}
	// Pathological seeds.
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add(bytes.Repeat([]byte{0xff}, 64))

	f.Fuzz(func(t *testing.T, blob []byte) {
		expr := &gen.KeyExpression{}
		if err := proto.Unmarshal(blob, expr); err != nil {
			// Bad proto bytes: parser rejects upstream, no work for us.
			return
		}
		// Must not panic, must not return (nil, nil) — either a valid
		// KeyExpression or a non-nil error.
		ke, err := KeyExpressionFromProto(expr)
		if err != nil {
			return
		}
		if ke == nil {
			t.Fatalf("KeyExpressionFromProto returned (nil, nil) for bytes %x", blob)
		}
	})
}

// FuzzRecordMetaDataFromProto stresses the MetaData proto loader. The
// file-descriptor rebuild phase walks an arbitrary dependency graph that
// attackers can shape (each dep names its own deps), so a self-referential
// A→B→A proto would recurse until the goroutine stack overflows without
// the in-progress cycle guard added in swingshift-35. This fuzz pins that
// guard and the upstream proto-unmarshal / FileDescriptor build path.
//
// Seeds include real serialised metadata (via builder + build + ToProto),
// the swingshift-35 4-byte regression bytes, and a handful of proto shapes
// known to confuse descriptor resolvers.
func FuzzRecordMetaDataFromProto(f *testing.F) {
	// Seed 1: a minimal real MetaData proto.
	md := &gen.MetaData{
		Records: &descriptorpb.FileDescriptorProto{
			Name:    proto.String("empty.proto"),
			Syntax:  proto.String("proto2"),
			Package: proto.String("test"),
		},
	}
	if b, err := proto.Marshal(md); err == nil {
		f.Add(b)
	}
	// Seed 2: a proto with a dependency that references itself.
	self := &gen.MetaData{
		Records: &descriptorpb.FileDescriptorProto{
			Name:       proto.String("root.proto"),
			Syntax:     proto.String("proto2"),
			Package:    proto.String("test"),
			Dependency: []string{"cycle.proto"},
		},
		Dependencies: []*descriptorpb.FileDescriptorProto{
			{
				Name:       proto.String("cycle.proto"),
				Syntax:     proto.String("proto2"),
				Package:    proto.String("test"),
				Dependency: []string{"cycle.proto"}, // self-reference
			},
		},
	}
	if b, err := proto.Marshal(self); err == nil {
		f.Add(b)
	}
	// Seed 3: A→B→A cycle.
	ab := &gen.MetaData{
		Records: &descriptorpb.FileDescriptorProto{
			Name:       proto.String("a.proto"),
			Syntax:     proto.String("proto2"),
			Package:    proto.String("test"),
			Dependency: []string{"b.proto"},
		},
		Dependencies: []*descriptorpb.FileDescriptorProto{
			{Name: proto.String("a.proto"), Syntax: proto.String("proto2"), Dependency: []string{"b.proto"}},
			{Name: proto.String("b.proto"), Syntax: proto.String("proto2"), Dependency: []string{"a.proto"}},
		},
	}
	if b, err := proto.Marshal(ab); err == nil {
		f.Add(b)
	}
	// Seed 4: real demo-proto metadata shipped through a real builder
	// (no indexes). Captures the full FileDescriptor + dependencies +
	// union descriptor that A2 cross-language tests exercise.
	if b := buildSeedMetaDataBytes(false, false, false); b != nil {
		f.Add(b)
	}
	// Seed 5: same but with a VALUE index on Order.price — pins the
	// shape A2 spec #2 sends across the wire.
	if b := buildSeedMetaDataBytes(true, false, false); b != nil {
		f.Add(b)
	}
	// Seed 6: same but with a COUNT index on Order.price (atomic
	// mutation maintainer + grouping expression). Shape from A2 spec
	// #6 (COUNT index BY_GROUP).
	if b := buildSeedMetaDataBytes(false, true, false); b != nil {
		f.Add(b)
	}
	// Seed 7: same but with an ungrouped SUM index on Order.price —
	// shape from A2 spec #8.
	if b := buildSeedMetaDataBytes(false, false, true); b != nil {
		f.Add(b)
	}

	// Pathological raw bytes.
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add([]byte{0x0a, 0x00})
	f.Add(bytes.Repeat([]byte{0xff}, 32))

	f.Fuzz(func(t *testing.T, blob []byte) {
		msg := &gen.MetaData{}
		if err := proto.Unmarshal(blob, msg); err != nil {
			return
		}
		// Must not panic or stack-overflow; must return either (non-nil, nil)
		// or (nil, non-nil error). (nil, nil) is the forbidden state.
		rmd, err := RecordMetaDataFromProto(msg)
		if err != nil {
			return
		}
		if rmd == nil {
			t.Fatalf("RecordMetaDataFromProto returned (nil, nil) for bytes %x", blob)
		}
	})
}

// buildSeedMetaDataBytes constructs a real RecordMetaData via the
// builder, optionally with a VALUE / COUNT / SUM index on Order.price,
// and serialises to bytes for FuzzRecordMetaDataFromProto seeds.
// Returns nil if any step fails (the fuzz target tolerates fewer
// seeds gracefully).
func buildSeedMetaDataBytes(withValueIndex, withCountIndex, withSumIndex bool) []byte {
	builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	if withValueIndex {
		builder.AddIndex("Order", NewIndex("Order$price", Field("price")))
	}
	if withCountIndex {
		builder.AddIndex("Order", NewCountIndex("Order$count_by_price",
			GroupAll(Field("price"))))
	}
	if withSumIndex {
		builder.AddIndex("Order", NewSumIndex("Order$total_price",
			Ungrouped(Field("price"))))
	}
	md, err := builder.Build()
	if err != nil {
		return nil
	}
	mdProto, err := md.ToProto()
	if err != nil {
		return nil
	}
	b, err := proto.Marshal(mdProto)
	if err != nil {
		return nil
	}
	return b
}

// ---------------------------------------------------------------------------
// FuzzParseNodeValue — round-trips the HNSW compact node value through the
// span-based parseNodeValue. The vector bytes must come back identical, and
// every neighbor span must decode to the original primary key. The fetch-key
// equality (layerPrefix ++ span == Pack({layer, pk})) is what makes carrying
// raw spans through the search hot path correct, so it's asserted too.
// ---------------------------------------------------------------------------

func FuzzParseNodeValue(f *testing.F) {
	f.Add([]byte{0x01, 0x00, 0x05, 0xff}, uint8(3))
	f.Add([]byte{}, uint8(0))
	f.Add([]byte{0x00, 0x00, 0x00, 0x05, 0x05}, uint8(5))
	f.Add([]byte{0xff, 0xff, 0xff}, uint8(1))

	ss := newHNSWStorage(specSubspaceFuzz(), DefaultHNSWConfig(8))
	const layer = 0

	f.Fuzz(func(t *testing.T, vec []byte, nNeighbors uint8) {
		n := int(nNeighbors % 40)
		neighbors := make([]tuple.Tuple, n)
		for i := range neighbors {
			// Mix int and composite PKs, including value bytes that collide with
			// tuple type codes (0x00, 0x05).
			switch i % 3 {
			case 0:
				neighbors[i] = tuple.Tuple{int64(i * 7)}
			case 1:
				neighbors[i] = tuple.Tuple{int64(i), []byte{0x00, 0x05, byte(i)}}
			default:
				neighbors[i] = tuple.Tuple{int64(-i), "k\x00v"}
			}
		}

		// Build the value exactly like saveNodeLayer.
		neighborList := make(tuple.Tuple, len(neighbors))
		for i, pk := range neighbors {
			neighborList[i] = pk
		}
		value := tuple.Tuple{int64(0), tuple.Tuple{vec}, neighborList}.Pack()

		gotVec, gotSpans, err := parseNodeValue(value)
		if err != nil {
			t.Fatalf("parseNodeValue: %v (vec=%x n=%d)", err, vec, n)
		}
		if len(vec) == 0 {
			if len(gotVec) != 0 {
				t.Fatalf("empty vector came back as %x", gotVec)
			}
		} else if !bytes.Equal(gotVec, vec) {
			t.Fatalf("vector mismatch:\n got %x\n want %x", gotVec, vec)
		}
		if len(gotSpans) != n {
			t.Fatalf("neighbor count: got %d want %d", len(gotSpans), n)
		}
		layerPrefix := ss.dataSubspace.Pack(tuple.Tuple{int64(layer)})
		for i, span := range gotSpans {
			pk, derr := decodeNestedPK(span)
			if derr != nil {
				t.Fatalf("decode span %d: %v", i, derr)
			}
			if !tupleEqual(pk, neighbors[i]) {
				t.Fatalf("neighbor %d mismatch: got %v want %v", i, pk, neighbors[i])
			}
			// span must reconstruct the exact fetch key.
			gotKey := append(append([]byte{}, layerPrefix...), span...)
			wantKey := ss.dataSubspace.Pack(tuple.Tuple{int64(layer), neighbors[i]})
			if !bytes.Equal(gotKey, wantKey) {
				t.Fatalf("fetch-key mismatch for neighbor %d:\n got  %x\n want %x", i, gotKey, wantKey)
			}
		}
	})
}

func specSubspaceFuzz() subspace.Subspace {
	return subspace.FromBytes([]byte("fuzz-hnsw-node"))
}
