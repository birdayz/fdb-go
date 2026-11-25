// Package wire implements FDB's custom FlatBuffers-inspired binary serialization format.
//
// FDB protocol messages are serialized using a vtable-based layout defined in
// flow/flat_buffers.h in the FDB C++ source. This is NOT Google FlatBuffers —
// it's FDB's own format with the same core concepts: vtables for field offsets,
// relative offsets for variable-length data, and alignment padding.
//
// The vtable determines where each field sits within the serialized object.
// Fields are sorted by size descending for optimal packing, and each offset
// includes a +4 adjustment for the vtable pointer at the start of the object.
package wire

import (
	"fmt"
	"sort"
)

// VTable describes the serialized layout of an FDB protocol message.
//
// Layout:
//
//	vtable[0] = vtable byte size on wire (2 bytes per field + 4)
//	vtable[1] = object byte size (includes 4-byte vtable-pointer prefix)
//	vtable[2+i] = byte offset of field i within the object
//
// A field offset of 0 means the field has zero size and is not present.
// Non-zero offsets include the +4 adjustment for the vtable pointer
// that occupies bytes [0,4) of the serialized object.
type VTable []uint16

// GenerateVTable computes the FDB FlatBuffers vtable layout for a message
// with the given field sizes and alignments.
//
// This is a direct port of detail::generate_vtable() from
// foundationdb/flow/flat_buffers.cpp. The output must be byte-identical
// to what the C++ implementation produces — any divergence means wire
// incompatibility.
//
// The algorithm:
//  1. Pair each field with its original index
//  2. Filter out zero-size fields
//  3. Stable-sort by size descending (largest fields first for packing)
//  4. Assign offsets with alignment, adding +4 for the vtable pointer
func GenerateVTable(sizes, alignments []uint32) VTable {
	n := len(sizes)
	if n == 0 {
		return VTable{4, 4}
	}
	if len(alignments) != n {
		panic("sizes and alignments must have equal length")
	}

	// Build (original_index, size) pairs for non-zero-size fields.
	type indexedField struct {
		index uint32
		size  uint32
	}
	fields := make([]indexedField, 0, n)
	for i := 0; i < n; i++ {
		if sizes[i] > 0 {
			fields = append(fields, indexedField{uint32(i), sizes[i]})
		}
	}

	// Stable sort by size descending — matches C++ std::stable_sort.
	sort.SliceStable(fields, func(i, j int) bool {
		return fields[i].size > fields[j].size
	})

	result := make(VTable, n+2)

	// vtable[0] = vtable byte size: 2 bytes per field + 2 for vtable size + 2 for object size
	result[0] = uint16(2*n + 4)

	// Assign offsets to each field, respecting alignment.
	offset := uint32(0)
	for _, f := range fields {
		align := alignments[f.index]
		if align == 0 {
			panic(fmt.Sprintf("wire: field %d has zero alignment with non-zero size %d", f.index, f.size))
		}
		// Round up to alignment boundary.
		if offset%align != 0 {
			offset = ((offset / align) + 1) * align
		}
		// Store offset + 4 (the vtable pointer occupies the first 4 bytes of the object).
		result[f.index+2] = uint16(offset + 4)
		offset += f.size
	}

	// vtable[1] = total object byte size (data + 4-byte vtable pointer).
	result[1] = uint16(offset + 4)

	return result
}
