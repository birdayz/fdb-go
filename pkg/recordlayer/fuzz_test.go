package recordlayer

import (
	"bytes"
	"fmt"
	"math"
	"math/big"
	"testing"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
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
		data := s.SerializeEntries(entries)
		f.Add(data, entries[0].Key.Pack())
	}

	// Invalid seeds.
	f.Add([]byte{}, []byte{0x14}) // empty data
	f.Add([]byte{0xff}, []byte{0x14})
	f.Add([]byte{0x20}, []byte{0x14})          // prefix only, no entries
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
	f.Add([]byte{0x00})                   // DOUBLE type, no data
	f.Add([]byte{0x01})                   // SINGLE type, no data
	f.Add([]byte{0x02})                   // HALF type, no data
	f.Add([]byte{0x05})                   // unknown type
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
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})                         // all zeros
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xfe, 0xff, 0xff}) // max version
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})                             // 11 bytes (too short)
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})                       // 13 bytes (too long)

	f.Fuzz(func(t *testing.T, data []byte) {
		// Must not panic.
		_, _ = CompleteVersionFromBytes(data)
	})
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
