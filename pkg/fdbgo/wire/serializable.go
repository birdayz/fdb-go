package wire

// FDBSerializable is implemented by every FDB wire type — both generated
// (from C++ schema extractor) and hand-written (for types with conditional
// serialization branches).
//
// The vtable, field sizes, and trait classifications are STATIC (from the
// C++ type system). The marshal/unmarshal logic may be DYNAMIC (data-dependent
// branches, protocol version checks). See docs/wire-format-static-vs-logic.md.
type FDBSerializable interface {
	// MarshalInto writes this type's fields into the parent ObjectWriter.
	// The caller has already set up the vtable and allocated the object.
	// MarshalInto writes field values at the correct vtable offsets.
	MarshalInto(obj *ObjectWriter)

	// UnmarshalFrom reads this type's fields from a Reader positioned
	// at this type's object (after FakeRoot/ErrorOr navigation).
	UnmarshalFrom(r *Reader) error

	// TypeVTable returns the vtable for this type.
	// Static — same for all instances regardless of field values.
	TypeVTable() VTable
}
