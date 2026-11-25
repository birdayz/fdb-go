package simfdb

import (
	"bytes"
	"testing"
)

// b is a terse byte-slice literal helper. b() yields a non-nil empty slice
// (present empty value); a nil `existing` (absent key) is written as `nil`.
func b(vs ...byte) []byte {
	out := make([]byte, len(vs))
	copy(out, vs)
	return out
}

// TestApplyAtomic exhaustively pins the commit-time byte semantics of every atomic
// op against the C++ Atomic.h spec / the byte-identical pure-Go client. Each op
// covers: missing key, equal width, existing-shorter, existing-longer, and the
// op-specific edges (Add wrap, Min-missing->param, lexicographic winners, the 100 KB
// AppendIfFits boundary, CompareAndClear match/no-match).
func TestApplyAtomic(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		op        atomicOp
		existing  []byte // nil == absent key
		param     []byte
		wantVal   []byte // ignored when wantClear
		wantClear bool
	}{
		// ---- Add: little-endian, width = len(param) ------------------------
		{"add/missing->param", atomicAdd, nil, b(0x01, 0x00), b(0x01, 0x00), false},
		{"add/equal-width", atomicAdd, b(0x01, 0x00), b(0x01, 0x00), b(0x02, 0x00), false},
		{"add/existing-shorter (255+1=256)", atomicAdd, b(0xFF), b(0x01, 0x00), b(0x00, 0x01), false},
		{"add/existing-longer (extra byte dropped)", atomicAdd, b(0x01, 0x02, 0x03), b(0x01, 0x00), b(0x02, 0x02), false},
		{"add/overflow-wraps (65535+1=0)", atomicAdd, b(0xFF, 0xFF), b(0x01, 0x00), b(0x00, 0x00), false},
		{"add/carry-propagates (255+1 in 3B)", atomicAdd, b(0xFF, 0x00, 0x00), b(0x01, 0x00, 0x00), b(0x00, 0x01, 0x00), false},

		// ---- And (AndV2): absent -> param; else bitwise AND ----------------
		{"and/missing->param", atomicAnd, nil, b(0x0F, 0xF0), b(0x0F, 0xF0), false},
		{"and/equal-width", atomicAnd, b(0xFF, 0x0F), b(0x0F, 0xFF), b(0x0F, 0x0F), false},
		{"and/existing-shorter (high byte->0)", atomicAnd, b(0xFF), b(0xFF, 0xFF), b(0xFF, 0x00), false},
		{"and/existing-longer (extra byte dropped)", atomicAnd, b(0xFF, 0xFF, 0xFF), b(0x0F, 0x0F), b(0x0F, 0x0F), false},
		{"and/present-empty (V1 body -> zeros)", atomicAnd, b(), b(0xAB, 0xCD), b(0x00, 0x00), false},

		// ---- Or: absent/empty -> param; width = len(param) -----------------
		{"or/missing->param", atomicOr, nil, b(0x0F), b(0x0F), false},
		{"or/equal-width", atomicOr, b(0xF0), b(0x0F), b(0xFF), false},
		{"or/existing-shorter (tail=param)", atomicOr, b(0xF0), b(0x0F, 0xAB), b(0xFF, 0xAB), false},
		{"or/existing-longer (extra byte dropped)", atomicOr, b(0xF0, 0x0F, 0xFF), b(0x0F, 0xF0), b(0xFF, 0xFF), false},

		// ---- Xor: absent/empty -> param; width = len(param) ----------------
		{"xor/missing->param", atomicXor, nil, b(0xFF), b(0xFF), false},
		{"xor/equal-width", atomicXor, b(0xFF), b(0x0F), b(0xF0), false},
		{"xor/existing-shorter (tail=param)", atomicXor, b(0xFF), b(0x0F, 0xAB), b(0xF0, 0xAB), false},
		{"xor/existing-longer (extra byte dropped)", atomicXor, b(0xFF, 0xFF, 0x00), b(0x0F, 0xF0), b(0xF0, 0x0F), false},

		// ---- Max: unsigned little-endian, width = len(param) ---------------
		{"max/missing->param", atomicMax, nil, b(0x01), b(0x01), false},
		{"max/equal-width param-bigger", atomicMax, b(0x01, 0x00), b(0x02, 0x00), b(0x02, 0x00), false},
		{"max/equal-width existing-bigger", atomicMax, b(0x05, 0x00), b(0x02, 0x00), b(0x05, 0x00), false},
		{"max/existing-shorter (256>5)", atomicMax, b(0x05), b(0x00, 0x01), b(0x00, 0x01), false},
		{"max/existing-longer (trailing zeros dropped)", atomicMax, b(0x05, 0x00, 0x00), b(0x02, 0x00), b(0x05, 0x00), false},

		// ---- Min (MinV2): absent -> param; else unsigned little-endian min --
		{"min/missing->param", atomicMin, nil, b(0x05, 0x00), b(0x05, 0x00), false},
		{"min/equal-width param-smaller", atomicMin, b(0x05, 0x00), b(0x02, 0x00), b(0x02, 0x00), false},
		{"min/equal-width existing-smaller", atomicMin, b(0x02, 0x00), b(0x05, 0x00), b(0x02, 0x00), false},
		{"min/existing-shorter (5<256)", atomicMin, b(0x05), b(0x00, 0x01), b(0x05, 0x00), false},
		{"min/existing-longer (extra byte dropped)", atomicMin, b(0x02, 0x00, 0x00), b(0x05, 0x00), b(0x02, 0x00), false},
		{"min/present-empty (V1 body -> zeros)", atomicMin, b(), b(0x05, 0x00), b(0x00, 0x00), false},

		// ---- ByteMax: lexicographic (memcmp) winner, stored verbatim -------
		{"bytemax/missing->param", atomicByteMax, nil, b(0x01), b(0x01), false},
		{"bytemax/existing-bigger", atomicByteMax, b(0x02), b(0x01), b(0x02), false},
		{"bytemax/param-bigger", atomicByteMax, b(0x01), b(0x02), b(0x02), false},
		{"bytemax/longer-prefix-bigger", atomicByteMax, b(0x01, 0x00), b(0x01), b(0x01, 0x00), false},

		// ---- ByteMin: lexicographic (memcmp) winner, stored verbatim -------
		{"bytemin/missing->param", atomicByteMin, nil, b(0x02), b(0x02), false},
		{"bytemin/existing-smaller", atomicByteMin, b(0x01), b(0x02), b(0x01), false},
		{"bytemin/param-smaller", atomicByteMin, b(0x02), b(0x01), b(0x01), false},
		{"bytemin/shorter-prefix-smaller", atomicByteMin, b(0x01), b(0x01, 0x00), b(0x01), false},

		// ---- AppendIfFits: append iff <= 100 KB, else NO-OP ----------------
		{"append/missing->param", atomicAppendIfFits, nil, b(0x0A, 0x0B), b(0x0A, 0x0B), false},
		{"append/normal", atomicAppendIfFits, b(0x01, 0x02), b(0x03), b(0x01, 0x02, 0x03), false},
		{"append/empty-param->existing", atomicAppendIfFits, b(0x01), b(), b(0x01), false},

		// ---- CompareAndClear -----------------------------------------------
		{"cac/match->clear", atomicCompareAndClear, b(0x01, 0x02), b(0x01, 0x02), nil, true},
		{"cac/no-match->unchanged", atomicCompareAndClear, b(0x01, 0x02), b(0x09), b(0x01, 0x02), false},
		{"cac/both-absent->clear", atomicCompareAndClear, nil, b(0x01), nil, true},
		{"cac/present-empty==empty-param->clear", atomicCompareAndClear, b(), b(), nil, true},
		{"cac/present-empty!=nonempty-param->unchanged", atomicCompareAndClear, b(), b(0x01), b(), false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotVal, gotClear := applyAtomic(tc.op, tc.existing, tc.param)
			if gotClear != tc.wantClear {
				t.Fatalf("clear = %v, want %v", gotClear, tc.wantClear)
			}
			if tc.wantClear {
				return // newVal is irrelevant when the key is cleared
			}
			if !bytes.Equal(gotVal, tc.wantVal) {
				t.Fatalf("val = %x, want %x", gotVal, tc.wantVal)
			}
			// A non-clearing atomic result must never be nil — that would be a
			// spurious tombstone (mirrors resolveAtomics's nil->[]byte{} coercion).
			if gotVal == nil {
				t.Fatalf("non-clearing result must be non-nil (would tombstone the key)")
			}
		})
	}
}

// TestApplyAtomic_AppendIfFitsBoundary pins the exact 100 KB VALUE_SIZE_LIMIT edge:
// a concatenation of exactly the limit fits (stored), one byte over is a NO-OP.
func TestApplyAtomic_AppendIfFitsBoundary(t *testing.T) {
	t.Parallel()

	// Exactly at the limit: 99_999 + 1 == 100_000, appends.
	existAt := bytes.Repeat([]byte{0xAA}, atomicValueSizeLimit-1)
	gotAt, clr := applyAtomic(atomicAppendIfFits, existAt, []byte{0xBB})
	if clr {
		t.Fatalf("append at boundary should not clear")
	}
	wantAt := append(append([]byte(nil), existAt...), 0xBB)
	if len(gotAt) != atomicValueSizeLimit || !bytes.Equal(gotAt, wantAt) {
		t.Fatalf("append at boundary: len=%d want=%d, bytes match=%v", len(gotAt), atomicValueSizeLimit, bytes.Equal(gotAt, wantAt))
	}

	// One over the limit: 100_000 + 1 == 100_001, NO-OP (existing unchanged).
	existOver := bytes.Repeat([]byte{0xAA}, atomicValueSizeLimit)
	gotOver, clr := applyAtomic(atomicAppendIfFits, existOver, []byte{0xBB})
	if clr {
		t.Fatalf("append over boundary should not clear")
	}
	if len(gotOver) != atomicValueSizeLimit || !bytes.Equal(gotOver, existOver) {
		t.Fatalf("append over boundary must be a no-op: len=%d want=%d unchanged=%v", len(gotOver), atomicValueSizeLimit, bytes.Equal(gotOver, existOver))
	}
}
