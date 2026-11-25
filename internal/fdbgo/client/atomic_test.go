package client

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// le64 encodes a uint64 as 8 little-endian bytes.
func le64(v uint64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, v)
	return b
}

func TestApplyAtomic_SetValue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		base  []byte
		param []byte
		want  []byte
	}{
		{"nil_base", nil, []byte{1, 2, 3}, []byte{1, 2, 3}},
		{"existing_base_overwritten", []byte{9, 9, 9}, []byte{1, 2, 3}, []byte{1, 2, 3}},
		{"empty_param", []byte{1, 2}, []byte{}, []byte{}},
		{"nil_param", nil, nil, []byte{}}, // append(nil, nil...) = []byte{}
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result, cleared := applyAtomic(MutSetValue, tt.base, tt.param)
			if cleared {
				t.Fatal("SetValue must never clear")
			}
			if !bytes.Equal(result, tt.want) {
				t.Fatalf("got %v, want %v", result, tt.want)
			}
		})
	}
}

func TestApplyAtomic_Add(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		base  []byte
		param []byte
		want  []byte
	}{
		// Basic 8-byte fast path.
		{"8byte_simple", le64(100), le64(200), le64(300)},
		// Overflow wraps (unsigned).
		{"8byte_overflow", le64(^uint64(0)), le64(1), le64(0)},
		// Nil base treated as zero.
		{"nil_base_8byte", nil, le64(42), le64(42)},
		// Empty base treated as zero.
		{"empty_base_8byte", []byte{}, le64(42), le64(42)},
		// Result length = len(param). Base longer than param: extra bytes dropped.
		{"base_longer_truncated", []byte{0xFF, 0xFF, 0xFF, 0xFF}, []byte{1, 0}, []byte{0, 0}},
		// Base shorter than param: missing base bytes are zero.
		{"base_shorter_zero_padded", []byte{1}, []byte{0, 0, 5}, []byte{1, 0, 5}},
		// Carry propagation through non-8-byte path.
		{"carry_propagation_3byte", []byte{0xFF, 0x00, 0x00}, []byte{0x01, 0x00, 0x00}, []byte{0x00, 0x01, 0x00}},
		{"carry_chain", []byte{0xFF, 0xFF, 0x00}, []byte{0x01, 0x00, 0x00}, []byte{0x00, 0x00, 0x01}},
		// Empty param returns empty.
		{"empty_param", le64(99), []byte{}, []byte{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result, cleared := applyAtomic(MutAddValue, tt.base, tt.param)
			if cleared {
				t.Fatal("Add must never clear")
			}
			if !bytes.Equal(result, tt.want) {
				t.Fatalf("got %v, want %v", result, tt.want)
			}
		})
	}
}

func TestApplyAtomic_And(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		base  []byte
		param []byte
		want  []byte
	}{
		// Nil base: AND with 0x00 = all zeros.
		{"nil_base_zeros_out", nil, []byte{0xFF, 0xAA}, []byte{0x00, 0x00}},
		// Empty base: same as nil (existing() returns []byte{}).
		{"empty_base_zeros_out", []byte{}, []byte{0xFF, 0xAA}, []byte{0x00, 0x00}},
		// Normal AND.
		{"basic", []byte{0xFF, 0x0F}, []byte{0xF0, 0xFF}, []byte{0xF0, 0x0F}},
		// Base longer: extra base bytes ignored, result = len(param).
		{"base_longer", []byte{0xFF, 0xFF, 0xFF}, []byte{0x0F, 0xF0}, []byte{0x0F, 0xF0}},
		// Param longer: extra positions are 0 (AND with missing base bytes = 0).
		{"param_longer_zero_padded", []byte{0xFF}, []byte{0xFF, 0xFF, 0xFF}, []byte{0xFF, 0x00, 0x00}},
		// Empty param.
		{"empty_param", []byte{0xFF}, []byte{}, []byte{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result, cleared := applyAtomic(MutAnd, tt.base, tt.param)
			if cleared {
				t.Fatal("And must never clear")
			}
			if !bytes.Equal(result, tt.want) {
				t.Fatalf("got %v, want %v", result, tt.want)
			}
		})
	}
}

func TestApplyAtomic_AndV2(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		base  []byte
		param []byte
		want  []byte
	}{
		// Critical difference from And: nil base returns param unchanged.
		{"nil_base_returns_param", nil, []byte{0xAB, 0xCD}, []byte{0xAB, 0xCD}},
		// Empty base (present): normal AND behavior (zeros out).
		{"empty_base_is_present", []byte{}, []byte{0xFF, 0xAA}, []byte{0x00, 0x00}},
		// Normal present base: same as And.
		{"present_base", []byte{0xFF, 0x0F}, []byte{0xF0, 0xFF}, []byte{0xF0, 0x0F}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result, cleared := applyAtomic(MutAndV2, tt.base, tt.param)
			if cleared {
				t.Fatal("AndV2 must never clear")
			}
			if !bytes.Equal(result, tt.want) {
				t.Fatalf("got %v, want %v", result, tt.want)
			}
		})
	}
}

func TestApplyAtomic_Or(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		base  []byte
		param []byte
		want  []byte
	}{
		// Nil base: OR with 0x00 = param.
		{"nil_base_returns_param", nil, []byte{0xAB, 0xCD}, []byte{0xAB, 0xCD}},
		// Empty base: same.
		{"empty_base_returns_param", []byte{}, []byte{0xAB, 0xCD}, []byte{0xAB, 0xCD}},
		// Normal OR.
		{"basic", []byte{0xF0, 0x0F}, []byte{0x0F, 0xF0}, []byte{0xFF, 0xFF}},
		// Param longer: extra bytes are param (OR with 0 = param).
		{"param_longer", []byte{0xAA}, []byte{0x55, 0xBB}, []byte{0xFF, 0xBB}},
		// Base longer: result truncated to param length, extra base bytes dropped.
		// OR loops only up to min(len(e), len(param)), then copies param[minLen:].
		// Since base is longer, minLen=len(param), no extra param bytes to copy.
		{"base_longer", []byte{0xFF, 0xFF, 0xFF}, []byte{0x0F, 0xF0}, []byte{0xFF, 0xFF}},
		// Empty param.
		{"empty_param", []byte{0xFF}, []byte{}, []byte{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result, cleared := applyAtomic(MutOr, tt.base, tt.param)
			if cleared {
				t.Fatal("Or must never clear")
			}
			if !bytes.Equal(result, tt.want) {
				t.Fatalf("got %v, want %v", result, tt.want)
			}
		})
	}
}

func TestApplyAtomic_Xor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		base  []byte
		param []byte
		want  []byte
	}{
		// Nil base: XOR with 0x00 = param.
		{"nil_base_returns_param", nil, []byte{0xAB}, []byte{0xAB}},
		// Empty base: same.
		{"empty_base_returns_param", []byte{}, []byte{0xAB}, []byte{0xAB}},
		// Self-XOR = zeros.
		{"self_xor_zeros", []byte{0xFF, 0xAA}, []byte{0xFF, 0xAA}, []byte{0x00, 0x00}},
		// Normal XOR.
		{"basic", []byte{0xF0, 0x0F}, []byte{0xFF, 0xFF}, []byte{0x0F, 0xF0}},
		// Param longer: extra bytes = param (XOR with 0).
		{"param_longer", []byte{0xAA}, []byte{0x55, 0xBB}, []byte{0xFF, 0xBB}},
		// Empty param.
		{"empty_param", []byte{0xFF}, []byte{}, []byte{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result, cleared := applyAtomic(MutXor, tt.base, tt.param)
			if cleared {
				t.Fatal("Xor must never clear")
			}
			if !bytes.Equal(result, tt.want) {
				t.Fatalf("got %v, want %v", result, tt.want)
			}
		})
	}
}

func TestApplyAtomic_Max(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		base  []byte
		param []byte
		want  []byte
	}{
		// Nil base: returns param (existing() = empty, early return).
		{"nil_base_returns_param", nil, le64(42), le64(42)},
		// Empty base: returns param.
		{"empty_base_returns_param", []byte{}, le64(42), le64(42)},
		// param > base: returns param.
		{"param_greater", le64(10), le64(20), le64(20)},
		// param < base: returns base truncated to param length.
		{"base_greater", le64(20), le64(10), le64(20)},
		// Equal: returns param (the >= case).
		{"equal", le64(42), le64(42), le64(42)},
		// Little-endian comparison: 0x0100 > 0xFF00 as LE integers.
		// LE: 0x0100 = [0x00, 0x01], 0xFF00 = [0x00, 0xFF]. Compare MSB: param[1]=0x01 < base[1]=0xFF => base wins.
		{"le_compare_msb_matters", []byte{0x00, 0xFF}, []byte{0x00, 0x01}, []byte{0x00, 0xFF}},
		// Mismatched lengths: param longer, extra byte nonzero => param wins.
		{"param_longer_nonzero_high", []byte{0xFF}, []byte{0x00, 0x01}, []byte{0x00, 0x01}},
		// Mismatched lengths: param longer, extra bytes all zero => compare overlapping.
		{"param_longer_zero_high_base_wins", []byte{0xFF}, []byte{0x01, 0x00}, []byte{0xFF, 0x00}},
		// Empty param.
		{"empty_param", le64(42), []byte{}, []byte{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result, cleared := applyAtomic(MutMax, tt.base, tt.param)
			if cleared {
				t.Fatal("Max must never clear")
			}
			if !bytes.Equal(result, tt.want) {
				t.Fatalf("got %v, want %v", result, tt.want)
			}
		})
	}
}

func TestApplyAtomic_Min(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		base  []byte
		param []byte
		want  []byte
	}{
		// Nil base (existing()=empty): for Min, compare from MSB down.
		// param[i] for i >= len(e) checked against 0: if nonzero => base wins.
		// All nonzero? base (empty, zero-padded) wins.
		{"nil_base_8byte", nil, le64(42), le64(0)},
		// Empty base: same as nil.
		{"empty_base", []byte{}, []byte{0x01}, []byte{0x00}},
		// But if param is also all zeros, returns param (equal case).
		{"nil_base_zero_param", nil, []byte{0x00, 0x00}, []byte{0x00, 0x00}},
		// param < base: returns param.
		{"param_smaller", le64(20), le64(10), le64(10)},
		// param > base: returns base (truncated to param length).
		{"base_smaller", le64(10), le64(20), le64(10)},
		// Equal: returns param.
		{"equal", le64(42), le64(42), le64(42)},
		// Mismatched lengths: param longer, extra high byte nonzero => base (zero-padded) wins.
		{"param_longer_high_nonzero", []byte{0xFF}, []byte{0x01, 0x01}, []byte{0xFF, 0x00}},
		// Empty param returns empty.
		{"empty_param", le64(42), []byte{}, []byte{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result, cleared := applyAtomic(MutMin, tt.base, tt.param)
			if cleared {
				t.Fatal("Min must never clear")
			}
			if !bytes.Equal(result, tt.want) {
				t.Fatalf("got %v, want %v", result, tt.want)
			}
		})
	}
}

func TestApplyAtomic_MinV2(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		base  []byte
		param []byte
		want  []byte
	}{
		// Critical difference from Min: nil base returns param unchanged (not zero).
		{"nil_base_returns_param", nil, le64(42), le64(42)},
		// Empty base (present): normal Min behavior.
		{"empty_base_is_present", []byte{}, []byte{0x05}, []byte{0x00}},
		// Normal present base.
		{"param_smaller", le64(20), le64(10), le64(10)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result, cleared := applyAtomic(MutMinV2, tt.base, tt.param)
			if cleared {
				t.Fatal("MinV2 must never clear")
			}
			if !bytes.Equal(result, tt.want) {
				t.Fatalf("got %v, want %v", result, tt.want)
			}
		})
	}
}

func TestApplyAtomic_ByteMax(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		base  []byte
		param []byte
		want  []byte
	}{
		// Nil base: returns param.
		{"nil_base", nil, []byte{0x01, 0x02}, []byte{0x01, 0x02}},
		// Lexicographic: base > param => base.
		{"base_greater", []byte{0x02}, []byte{0x01}, []byte{0x02}},
		// Lexicographic: param > base => param.
		{"param_greater", []byte{0x01}, []byte{0x02}, []byte{0x02}},
		// Equal: returns param (bytes.Compare == 0 is not > 0).
		{"equal", []byte{0xAB}, []byte{0xAB}, []byte{0xAB}},
		// Longer string is greater when prefix matches.
		{"longer_param", []byte{0x01}, []byte{0x01, 0x00}, []byte{0x01, 0x00}},
		{"longer_base", []byte{0x01, 0x00}, []byte{0x01}, []byte{0x01, 0x00}},
		// Both empty: returns param.
		{"both_empty", []byte{}, []byte{}, []byte{}},
		// Empty base (present): compare with param. Empty < anything.
		{"empty_base_present", []byte{}, []byte{0x01}, []byte{0x01}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result, cleared := applyAtomic(MutByteMax, tt.base, tt.param)
			if cleared {
				t.Fatal("ByteMax must never clear")
			}
			if !bytes.Equal(result, tt.want) {
				t.Fatalf("got %v, want %v", result, tt.want)
			}
		})
	}
}

func TestApplyAtomic_ByteMin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		base  []byte
		param []byte
		want  []byte
	}{
		// Nil base: returns param (matches ByteMax nil behavior).
		{"nil_base", nil, []byte{0x01, 0x02}, []byte{0x01, 0x02}},
		// Lexicographic: base < param => base.
		{"base_smaller", []byte{0x01}, []byte{0x02}, []byte{0x01}},
		// Lexicographic: param < base => param.
		{"param_smaller", []byte{0x02}, []byte{0x01}, []byte{0x01}},
		// Equal: returns param (bytes.Compare == 0 is not < 0).
		{"equal", []byte{0xAB}, []byte{0xAB}, []byte{0xAB}},
		// Shorter string is less when prefix matches.
		{"shorter_base", []byte{0x01}, []byte{0x01, 0x00}, []byte{0x01}},
		{"shorter_param", []byte{0x01, 0x00}, []byte{0x01}, []byte{0x01}},
		// Empty base (present): empty < anything.
		{"empty_base_present", []byte{}, []byte{0x01}, []byte{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result, cleared := applyAtomic(MutByteMin, tt.base, tt.param)
			if cleared {
				t.Fatal("ByteMin must never clear")
			}
			if !bytes.Equal(result, tt.want) {
				t.Fatalf("got %v, want %v", result, tt.want)
			}
		})
	}
}

func TestApplyAtomic_AppendIfFits(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		base  []byte
		param []byte
		want  []byte
	}{
		// Nil base: returns param.
		{"nil_base", nil, []byte{1, 2, 3}, []byte{1, 2, 3}},
		// Empty base: returns param.
		{"empty_base", []byte{}, []byte{1, 2, 3}, []byte{1, 2, 3}},
		// Normal append.
		{"basic_append", []byte{1, 2}, []byte{3, 4}, []byte{1, 2, 3, 4}},
		// Empty param: returns base.
		{"empty_param", []byte{1, 2}, []byte{}, []byte{1, 2}},
		// Exactly at limit (100000 bytes): fits.
		{"exactly_at_limit", make100KB(50000), make100KB(50000), append(make100KB(50000), make100KB(50000)...)},
		// Over limit: returns base unchanged.
		{"over_limit", make100KB(50001), make100KB(50000), make100KB(50001)},
		// Way over: returns base.
		{"base_already_at_limit", make100KB(100000), []byte{1}, make100KB(100000)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result, cleared := applyAtomic(MutAppendIfFits, tt.base, tt.param)
			if cleared {
				t.Fatal("AppendIfFits must never clear")
			}
			if !bytes.Equal(result, tt.want) {
				t.Fatalf("len(got)=%d, len(want)=%d", len(result), len(tt.want))
			}
		})
	}
}

// make100KB creates a byte slice of the given size filled with 0xAA.
func make100KB(size int) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = 0xAA
	}
	return b
}

func TestApplyAtomic_CompareAndClear(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		base        []byte
		param       []byte
		want        []byte
		wantCleared bool
	}{
		// Nil base: always clears (key absent = clear).
		{"nil_base_clears", nil, []byte{1, 2, 3}, nil, true},
		// Equal: clears.
		{"equal_clears", []byte{1, 2, 3}, []byte{1, 2, 3}, nil, true},
		// Empty base equals empty param: clears.
		{"both_empty_clears", []byte{}, []byte{}, nil, true},
		// Not equal: no change, returns base.
		{"not_equal_keeps", []byte{1, 2, 3}, []byte{4, 5, 6}, []byte{1, 2, 3}, false},
		// Different lengths: not equal, keeps.
		{"different_length_keeps", []byte{1, 2}, []byte{1, 2, 3}, []byte{1, 2}, false},
		// Nil base with empty param: nil != empty? No: base==nil => always clears.
		{"nil_base_empty_param_clears", nil, []byte{}, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result, cleared := applyAtomic(MutCompareAndClear, tt.base, tt.param)
			if cleared != tt.wantCleared {
				t.Fatalf("cleared=%v, want %v", cleared, tt.wantCleared)
			}
			if !bytes.Equal(result, tt.want) {
				t.Fatalf("got %v, want %v", result, tt.want)
			}
		})
	}
}

func TestApplyAtomic_UnknownOp(t *testing.T) {
	t.Parallel()
	// Unknown ops (like versionstamped mutations) can't be resolved client-side.
	// With a present base: returns base unchanged.
	// With nil base: returns nil.
	tests := []struct {
		name string
		base []byte
		want []byte
	}{
		{"present_base_returned", []byte{1, 2, 3}, []byte{1, 2, 3}},
		{"empty_base_returned", []byte{}, []byte{}},
		{"nil_base_returns_nil", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result, cleared := applyAtomic(MutSetVersionstampedKey, tt.base, []byte{0xDE, 0xAD})
			if cleared {
				t.Fatal("unknown op must never clear")
			}
			if !bytes.Equal(result, tt.want) {
				t.Fatalf("got %v, want %v", result, tt.want)
			}
		})
	}
}

// TestApplyAtomic_NilVsEmpty verifies the critical nil/empty distinction
// across all ops that handle them differently.
func TestApplyAtomic_NilVsEmpty(t *testing.T) {
	t.Parallel()

	// And: nil base => zeros. Empty base => zeros. (Same behavior.)
	t.Run("And_nil_and_empty_same", func(t *testing.T) {
		t.Parallel()
		param := []byte{0xFF}
		rNil, _ := applyAtomic(MutAnd, nil, param)
		rEmpty, _ := applyAtomic(MutAnd, []byte{}, param)
		if !bytes.Equal(rNil, rEmpty) {
			t.Fatalf("And nil=%v vs empty=%v (should be same)", rNil, rEmpty)
		}
		if !bytes.Equal(rNil, []byte{0x00}) {
			t.Fatalf("And nil base: got %v, want [0x00]", rNil)
		}
	})

	// AndV2: nil base => param. Empty base => zeros. (DIFFERENT.)
	t.Run("AndV2_nil_and_empty_differ", func(t *testing.T) {
		t.Parallel()
		param := []byte{0xFF}
		rNil, _ := applyAtomic(MutAndV2, nil, param)
		rEmpty, _ := applyAtomic(MutAndV2, []byte{}, param)
		if bytes.Equal(rNil, rEmpty) {
			t.Fatal("AndV2 nil and empty must differ")
		}
		if !bytes.Equal(rNil, []byte{0xFF}) {
			t.Fatalf("AndV2 nil base: got %v, want [0xFF]", rNil)
		}
		if !bytes.Equal(rEmpty, []byte{0x00}) {
			t.Fatalf("AndV2 empty base: got %v, want [0x00]", rEmpty)
		}
	})

	// Min: nil base (existing=empty) => zero-padded to param length.
	// MinV2: nil base => param unchanged.
	t.Run("Min_vs_MinV2_nil_base", func(t *testing.T) {
		t.Parallel()
		param := []byte{0x05, 0x0A}
		rMin, _ := applyAtomic(MutMin, nil, param)
		rMinV2, _ := applyAtomic(MutMinV2, nil, param)
		// Min: existing()=[] => zero-padded. param[1]=0x0A != 0 => base wins => [0x00, 0x00].
		if !bytes.Equal(rMin, []byte{0x00, 0x00}) {
			t.Fatalf("Min nil base: got %v, want [0x00, 0x00]", rMin)
		}
		// MinV2: nil => return param.
		if !bytes.Equal(rMinV2, []byte{0x05, 0x0A}) {
			t.Fatalf("MinV2 nil base: got %v, want [0x05, 0x0A]", rMinV2)
		}
	})
}

// TestApplyAtomic_Add_ResultNotAliased verifies the result doesn't alias
// the input slices (mutation safety).
func TestApplyAtomic_Add_ResultNotAliased(t *testing.T) {
	t.Parallel()
	base := le64(100)
	param := le64(200)
	baseCopy := append([]byte(nil), base...)
	paramCopy := append([]byte(nil), param...)

	result, _ := applyAtomic(MutAddValue, base, param)

	// Mutate result — inputs must be unaffected.
	result[0] = 0xFF
	if !bytes.Equal(base, baseCopy) {
		t.Fatal("base was mutated via result alias")
	}
	if !bytes.Equal(param, paramCopy) {
		t.Fatal("param was mutated via result alias")
	}
}
