package recordlayer

import (
	"math/big"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// A FormerIndex's SubspaceKey is `any`, so copying the struct copies an
// interface header and shares whatever it points at. Build copies former
// indexes precisely so a later builder mutation cannot rewrite what Build
// returned; a shallow copy of this field silently exempts it.
//
// THE SHAPES ARE THE DECODER'S, and each arm below mutates the ORIGINAL after
// the copy and asserts the copy is unmoved. That direction matters: asserting
// only that the values are equal immediately after copying passes for a shared
// pointer, which is the whole defect.
//
// The `default` arm is the stated limit, exercised rather than described:
// SetSubspaceKey accepts `any`, so a type no decoder produces stays shared.
func TestDeepCopySubspaceKeyIsolatesEveryMutableShape(t *testing.T) {
	t.Parallel()

	t.Run("[]byte", func(t *testing.T) {
		t.Parallel()
		orig := []byte{1, 2, 3}
		cp, ok := deepCopySubspaceKey(orig).([]byte)
		if !ok {
			t.Fatalf("copy changed the dynamic type: %T", deepCopySubspaceKey(orig))
		}
		orig[0] = 99
		if cp[0] != 1 {
			t.Fatalf("mutating the original moved the copy: got %v, want [1 2 3]", cp)
		}
	})

	t.Run("*big.Int", func(t *testing.T) {
		t.Parallel()
		// fastDecodeBigInt returns a *big.Int for values outside int64, and
		// big.Int has in-place mutators, so this is a live sharing hazard and
		// not a theoretical one.
		orig := new(big.Int).SetUint64(1 << 63)
		cp, ok := deepCopySubspaceKey(orig).(*big.Int)
		if !ok {
			t.Fatalf("copy changed the dynamic type: %T", deepCopySubspaceKey(orig))
		}
		if cp == orig {
			t.Fatal("deepCopySubspaceKey returned the SAME *big.Int; Set/SetBytes/Add on the " +
				"builder's key would rewrite the built metadata's")
		}
		want := new(big.Int).Set(cp)
		orig.SetInt64(7)
		if cp.Cmp(want) != 0 {
			t.Fatalf("mutating the original moved the copy: got %v, want %v", cp, want)
		}
	})

	t.Run("tuple.Tuple, flat", func(t *testing.T) {
		t.Parallel()
		orig := tuple.Tuple{int64(1), "two"}
		cp, ok := deepCopySubspaceKey(orig).(tuple.Tuple)
		if !ok {
			t.Fatalf("copy changed the dynamic type: %T", deepCopySubspaceKey(orig))
		}
		orig[0] = int64(99)
		if cp[0] != int64(1) {
			t.Fatalf("mutating the original moved the copy: got %v", cp)
		}
	})

	t.Run("tuple.Tuple holding []byte", func(t *testing.T) {
		t.Parallel()
		// The shape a one-level `append(tuple.Tuple(nil), raw...)` gets WRONG:
		// the backing array is fresh, and every element still points at the
		// original's memory.
		inner := []byte{1, 2, 3}
		orig := tuple.Tuple{inner}
		cp := deepCopySubspaceKey(orig).(tuple.Tuple)
		inner[0] = 99
		if got := cp[0].([]byte); got[0] != 1 {
			t.Fatalf("the nested []byte is still shared: got %v, want [1 2 3]", got)
		}
	})

	t.Run("nested tuple.Tuple", func(t *testing.T) {
		t.Parallel()
		inner := tuple.Tuple{int64(1)}
		orig := tuple.Tuple{inner}
		cp := deepCopySubspaceKey(orig).(tuple.Tuple)
		inner[0] = int64(99)
		if got := cp[0].(tuple.Tuple); got[0] != int64(1) {
			t.Fatalf("the nested tuple is still shared: got %v", got)
		}
	})

	t.Run("nested *big.Int", func(t *testing.T) {
		t.Parallel()
		inner := new(big.Int).SetUint64(1 << 63)
		orig := tuple.Tuple{inner}
		cp := deepCopySubspaceKey(orig).(tuple.Tuple)
		if cp[0].(*big.Int) == inner {
			t.Fatal("the nested *big.Int is still the same pointer")
		}
	})
}

// The value-typed shapes, pinned so that "they copy with the struct" is a
// checked statement rather than an assumption: each must come back through
// deepCopySubspaceKey's `default` arm unchanged and still compare equal.
//
// WHAT THIS CANNOT DETECT, stated because an earlier version of this comment
// claimed the opposite. If one of these types grows a POINTER field it stays
// comparable, the `default` arm hands it straight back, both sides hold the
// same pointer, and every arm below still passes -- the very regression the old
// wording promised to surface. Only a non-comparable addition (a slice, map or
// func field) shows up here, and then as a runtime panic on `!=` rather than as
// the message written in the failure. The real guard against a type gaining
// mutable state is the enumeration in deepCopySubspaceKey's doc comment being
// re-derived from the decoder, not this table.
func TestDeepCopySubspaceKeyPassesValueShapesThrough(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		key  any
	}{
		{"nil", nil},
		{"string", "abc"},
		{"int64", int64(7)},
		// uint64 and float32 are the two the decoder produces that an earlier
		// version of this table omitted -- the same two that were missing from
		// deepCopySubspaceKey's shape enumeration. A list of value shapes that
		// does not cover every value shape the decoder emits is the same defect
		// as the enumeration it is supposed to corroborate.
		{"uint64", uint64(1) << 63},
		{"float32", float32(1.5)},
		{"float64", float64(1.5)},
		{"bool", true},
		{"tuple.UUID", tuple.UUID{1, 2, 3}},
		{"tuple.Versionstamp", tuple.Versionstamp{TransactionVersion: [10]byte{1}, UserVersion: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := deepCopySubspaceKey(tc.key); got != tc.key {
				t.Fatalf("deepCopySubspaceKey(%v) = %v; a value shape must pass through unchanged, "+
					"and a copy here would mean the shape is not the value type this claims", tc.key, got)
			}
		})
	}
}

// The end-to-end pin: Build's former-index copy must isolate the key, which is
// what the helper exists for. Without this the helper could be correct and
// unwired.
func TestBuildIsolatesAFormerIndexSubspaceKey(t *testing.T) {
	t.Parallel()

	// A former index is produced by removing a registered one: RemoveIndex
	// records the departing index's subspace key, which SetSubspaceKey stores
	// by reference.
	raw := []byte{1, 2, 3}
	b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	b.AddIndex("Order", NewIndex("doomed_idx", Field("price")).SetSubspaceKey(raw))
	b.RemoveIndex("doomed_idx")

	md, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(md.formerIndexes) != 1 {
		t.Fatalf("built metadata has %d former indexes, want 1; the rest of this test would be "+
			"asserting nothing", len(md.formerIndexes))
	}

	raw[0] = 99
	got, ok := md.formerIndexes[0].SubspaceKey.([]byte)
	if !ok {
		t.Fatalf("former index subspace key is %T, want []byte", md.formerIndexes[0].SubspaceKey)
	}
	if got[0] != 1 {
		t.Fatalf("mutating the slice handed to the builder rewrote the BUILT metadata's former "+
			"index subspace key: got %v, want [1 2 3]", got)
	}
}
