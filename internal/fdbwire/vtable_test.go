package wire

import (
	"fmt"
	"strings"
	"testing"
)

// These tests verify our GenerateVTable produces identical output to
// detail::generate_vtable() in foundationdb/flow/flat_buffers.cpp.
//
// Expected values are computed by hand-tracing the C++ algorithm and
// cross-checked against assertions in the C++ unit tests.

func TestGenerateVTable_Empty(t *testing.T) {
	t.Parallel()
	// C++ test: flow/FlatBuffers/emptyVtable
	//   auto* vtable = detail::get_vtable<>();
	//   ASSERT((*vtable)[0] == 4);
	//   ASSERT((*vtable)[1] == 4);
	vt := GenerateVTable(nil, nil)
	assertVTable(t, vt, VTable{4, 4})
}

func TestGenerateVTable_SingleInt(t *testing.T) {
	t.Parallel()
	// C++ test: get_vtable<int>()  — size=4, align=4
	// Hand trace:
	//   result[0] = 2*1 + 4 = 6
	//   field (0, size=4): align=4, offset=0, 0%4==0 → res=0, offset=4, res+=4 → result[2]=4
	//   result[1] = 4 + 4 = 8
	vt := GenerateVTable(
		[]uint32{4},
		[]uint32{4},
	)
	assertVTable(t, vt, VTable{6, 8, 4})
}

func TestGenerateVTable_FiveFields(t *testing.T) {
	t.Parallel()
	// C++ test: get_vtable<uint8_t, uint8_t, int, int64_t, int>()
	// sizes:  [1, 1, 4, 8, 4]
	// aligns: [1, 1, 4, 8, 4]
	//
	// C++ asserts:
	//   vtable2->size() == 7
	//   (*vtable2)[0] == 14
	//   (*vtable2)[1] == 22
	//   ((*vtable2)[4] - 4) % 4 == 0  → field 2 (int) aligned to 4
	//   ((*vtable2)[5] - 4) % 8 == 0  → field 3 (int64) aligned to 8
	//   ((*vtable2)[6] - 4) % 4 == 0  → field 4 (int) aligned to 4
	//
	// Hand trace:
	//   sorted by size desc (stable): [(3,8), (2,4), (4,4), (0,1), (1,1)]
	//   result[0] = 2*5 + 4 = 14
	//
	//   (3, 8): align=8, offset=0, 0%8==0, res=0, offset=8,  result[5]=0+4=4
	//   (2, 4): align=4, offset=8, 8%4==0, res=8, offset=12, result[4]=8+4=12
	//   (4, 4): align=4, offset=12, 12%4==0, res=12, offset=16, result[6]=12+4=16
	//   (0, 1): align=1, offset=16, 16%1==0, res=16, offset=17, result[2]=16+4=20
	//   (1, 1): align=1, offset=17, 17%1==0, res=17, offset=18, result[3]=17+4=21
	//
	//   result[1] = 18 + 4 = 22
	vt := GenerateVTable(
		[]uint32{1, 1, 4, 8, 4},
		[]uint32{1, 1, 4, 8, 4},
	)
	assertVTable(t, vt, VTable{14, 22, 20, 21, 12, 4, 16})

	// Verify the alignment assertions from C++.
	if (vt[4]-4)%4 != 0 {
		t.Errorf("field 2 (int) not 4-byte aligned: offset %d", vt[4]-4)
	}
	if (vt[5]-4)%8 != 0 {
		t.Errorf("field 3 (int64) not 8-byte aligned: offset %d", vt[5]-4)
	}
	if (vt[6]-4)%4 != 0 {
		t.Errorf("field 4 (int) not 4-byte aligned: offset %d", vt[6]-4)
	}
}

func TestGenerateVTable_WithZeroSizeField(t *testing.T) {
	t.Parallel()
	// A zero-size field (e.g., Arena) should get vtable entry = 0 and not
	// affect the layout of other fields.
	//
	// Fields: [int64(8,8), zero(0,0), int32(4,4)]
	// Only non-zero indexed: [(0,8), (2,4)]
	// Sorted: [(0,8), (2,4)]
	//
	// result[0] = 2*3 + 4 = 10
	// (0, 8): align=8, offset=0, res=0, offset=8,  result[2]=4
	// (2, 4): align=4, offset=8, res=8, offset=12, result[4]=12
	// result[1] = 12 + 4 = 16
	// result[3] = 0 (zero-size field, default)
	vt := GenerateVTable(
		[]uint32{8, 0, 4},
		[]uint32{8, 0, 4},
	)
	assertVTable(t, vt, VTable{10, 16, 4, 0, 12})
}

func TestGenerateVTable_AlignmentPadding(t *testing.T) {
	t.Parallel()
	// Fields: [uint8(1,1), int64(8,8)]
	// Sorted: [(1,8), (0,1)]
	//
	// result[0] = 2*2 + 4 = 8
	// (1, 8): align=8, offset=0, 0%8==0, res=0, offset=8,  result[3]=4
	// (0, 1): align=1, offset=8, 8%1==0, res=8, offset=9,  result[2]=12
	// result[1] = 9 + 4 = 13
	vt := GenerateVTable(
		[]uint32{1, 8},
		[]uint32{1, 8},
	)
	assertVTable(t, vt, VTable{8, 13, 12, 4})
}

func TestGenerateVTable_AlignmentGap(t *testing.T) {
	t.Parallel()
	// Fields: [uint8(1,1), uint8(1,1), int64(8,8)]
	// Sorted: [(2,8), (0,1), (1,1)]
	//
	// result[0] = 2*3 + 4 = 10
	// (2, 8): align=8, offset=0, 0%8==0, res=0, offset=8, result[4]=4
	// (0, 1): align=1, offset=8, res=8, offset=9, result[2]=12
	// (1, 1): align=1, offset=9, res=9, offset=10, result[3]=13
	// result[1] = 10 + 4 = 14
	vt := GenerateVTable(
		[]uint32{1, 1, 8},
		[]uint32{1, 1, 8},
	)
	assertVTable(t, vt, VTable{10, 14, 12, 13, 4})
}

func TestGenerateVTable_MixedTypesWithRelativeOffsets(t *testing.T) {
	t.Parallel()
	// Simulates a message with: int64 field, bytes field (RelativeOffset = uint32).
	// In FDB, variable-length fields (bytes, vectors, nested structs) occupy a
	// 4-byte RelativeOffset slot in the vtable object.
	//
	// Fields: [int64(8,8), RelativeOffset(4,4)]
	// Sorted: [(0,8), (1,4)]
	//
	// result[0] = 2*2 + 4 = 8
	// (0, 8): align=8, offset=0, res=0, offset=8, result[2]=4
	// (1, 4): align=4, offset=8, res=8, offset=12, result[3]=12
	// result[1] = 12 + 4 = 16
	vt := GenerateVTable(
		[]uint32{8, 4},
		[]uint32{8, 4},
	)
	assertVTable(t, vt, VTable{8, 16, 4, 12})
}

func TestGenerateVTable_StableSortPreservesOrder(t *testing.T) {
	t.Parallel()
	// Three fields of equal size — stable sort must preserve original order.
	// Fields: [int32(4,4), int32(4,4), int32(4,4)]
	// Sorted (stable): [(0,4), (1,4), (2,4)] — original order preserved
	//
	// result[0] = 2*3 + 4 = 10
	// (0, 4): align=4, offset=0, res=0, offset=4,  result[2]=4
	// (1, 4): align=4, offset=4, res=4, offset=8,  result[3]=8
	// (2, 4): align=4, offset=8, res=8, offset=12, result[4]=12
	// result[1] = 12 + 4 = 16
	vt := GenerateVTable(
		[]uint32{4, 4, 4},
		[]uint32{4, 4, 4},
	)
	assertVTable(t, vt, VTable{10, 16, 4, 8, 12})
}

func TestGenerateVTable_ComplexMix(t *testing.T) {
	t.Parallel()
	// C++ test: get_vtable<uint64_t, bool, std::string, int64_t, std::vector<uint16_t>, Table2, Table3>()
	//
	// In FDB FlatBuffers, types with serialize() method (Table2, Table3) and
	// dynamic_size_traits types (std::string, std::vector) all occupy a
	// RelativeOffset (uint32_t, size=4, align=4) in the vtable object.
	//
	// Field sizes/aligns:
	//   [0] uint64_t:            size=8, align=8
	//   [1] bool:                size=1, align=1
	//   [2] std::string:         size=4, align=4  (RelativeOffset)
	//   [3] int64_t:             size=8, align=8
	//   [4] std::vector<uint16>: size=4, align=4  (RelativeOffset)
	//   [5] Table2:              size=4, align=4  (RelativeOffset, has serialize())
	//   [6] Table3:              size=4, align=4  (RelativeOffset, has serialize())
	//
	// Sorted by size desc (stable): [(0,8),(3,8),(2,4),(4,4),(5,4),(6,4),(1,1)]
	//
	// result[0] = 2*7 + 4 = 18
	// (0, 8): align=8, offset=0,  res=0,  offset=8,  result[2]=4
	// (3, 8): align=8, offset=8,  res=8,  offset=16, result[5]=12
	// (2, 4): align=4, offset=16, res=16, offset=20, result[4]=20
	// (4, 4): align=4, offset=20, res=20, offset=24, result[6]=24
	// (5, 4): align=4, offset=24, res=24, offset=28, result[7]=28
	// (6, 4): align=4, offset=28, res=28, offset=32, result[8]=32
	// (1, 1): align=1, offset=32, res=32, offset=33, result[3]=36
	// result[1] = 33 + 4 = 37
	vt := GenerateVTable(
		[]uint32{8, 1, 4, 8, 4, 4, 4},
		[]uint32{8, 1, 4, 8, 4, 4, 4},
	)
	assertVTable(t, vt, VTable{18, 37, 4, 36, 20, 12, 24, 28, 32})

	// C++ assert: !(vtable[2] <= vtable[4] && vtable[4] < vtable[2] + 8)
	// This checks that field 2 (std::string offset) doesn't overlap with field 0 (uint64).
	if vt[2] <= vt[4] && vt[4] < vt[2]+8 {
		t.Error("field 2 (string offset) overlaps with field 0 (uint64)")
	}
}

func TestGenerateVTable_AllZeroSizeFields(t *testing.T) {
	t.Parallel()
	// All fields have zero size — none participate in layout.
	// result[0] = 2*2 + 4 = 8, result[1] = 0 + 4 = 4, field entries = 0
	vt := GenerateVTable(
		[]uint32{0, 0},
		[]uint32{0, 0},
	)
	assertVTable(t, vt, VTable{8, 4, 0, 0})
}

func TestGenerateVTable_SingleZeroSizeField(t *testing.T) {
	t.Parallel()
	// Single zero-size field — filtered out, but n != 0.
	// result[0] = 2*1 + 4 = 6, result[1] = 0 + 4 = 4, field entry = 0
	vt := GenerateVTable(
		[]uint32{0},
		[]uint32{0},
	)
	assertVTable(t, vt, VTable{6, 4, 0})
}

func TestGenerateVTable_AlignmentLargerThanSize(t *testing.T) {
	t.Parallel()
	// Field with alignment > size: 1-byte field with 4-byte alignment.
	// Uncommon but legal in C struct layouts.
	//
	// Fields: [uint8(1,4), uint8(1,4)]
	// Sorted (stable, equal size): [(0,1), (1,1)]
	//
	// result[0] = 2*2 + 4 = 8
	// (0, 1): align=4, offset=0, 0%4==0, res=0, offset=1, result[2]=4
	// (1, 1): align=4, offset=1, 1%4!=0 → ((1/4)+1)*4 = 4, res=4, offset=5, result[3]=8
	// result[1] = 5 + 4 = 9
	vt := GenerateVTable(
		[]uint32{1, 1},
		[]uint32{4, 4},
	)
	assertVTable(t, vt, VTable{8, 9, 4, 8})

	// Verify alignment: both fields should be 4-byte aligned (offset - 4 divisible by 4).
	if (vt[2]-4)%4 != 0 {
		t.Errorf("field 0 not 4-byte aligned: offset %d", vt[2]-4)
	}
	if (vt[3]-4)%4 != 0 {
		t.Errorf("field 1 not 4-byte aligned: offset %d", vt[3]-4)
	}
}

func TestGenerateVTable_PanicOnZeroAlignment(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Error("expected panic on zero alignment for non-zero-size field")
		}
		msg, ok := r.(string)
		if !ok || !contains(msg, "zero alignment") {
			t.Errorf("unexpected panic message: %v", r)
		}
	}()
	GenerateVTable([]uint32{4}, []uint32{0})
}

func TestGenerateVTable_PanicOnMismatchedLengths(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on mismatched sizes/alignments lengths")
		}
	}()
	GenerateVTable([]uint32{4}, []uint32{4, 4})
}

func assertVTable(t *testing.T, got, want VTable) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("vtable length: got %d, want %d\n  got:  %s\n  want: %s",
			len(got), len(want), formatVTable(got), formatVTable(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("vtable mismatch at [%d]: got %d, want %d\n  got:  %s\n  want: %s",
				i, got[i], want[i], formatVTable(got), formatVTable(want))
		}
	}
}

func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}

func formatVTable(vt VTable) string {
	if len(vt) < 2 {
		return fmt.Sprintf("%v", []uint16(vt))
	}
	s := fmt.Sprintf("[vtableSize=%d, objectSize=%d", vt[0], vt[1])
	for i := 2; i < len(vt); i++ {
		s += fmt.Sprintf(", field%d=@%d", i-2, vt[i])
	}
	s += "]"
	return s
}
