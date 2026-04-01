package types

// Error — flow/include/flow/Error.h
// C++ serialize: serializer(ar, error_code)
//   slot 0: error_code — uint16_t (scalar, 2 bytes at offset 4)
// Note: C++ Error.error_code is declared as int, but serialized as uint16_t
// on the wire. Confirmed by schema extraction + ground-truth test vector.

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

type Error struct {
	Code int32
}

func (m *Error) TypeVTable() wire.VTable { return ErrorVTable }

func (m *Error) MarshalInto(obj *wire.ObjectWriter) {
	vt := ErrorVTable
	obj.WriteUint16(int(vt[2]), uint16(m.Code))
}

func (m *Error) UnmarshalFrom(r *wire.Reader) error {
	if r.FieldPresent(0) {
		// Wire format is uint16, but we store as int32 for convenience.
		// ReadInt32 reads 4 bytes (2 value + 2 zero-padding), which is
		// safe because FDB zero-initializes object buffers.
		m.Code = r.ReadInt32(0)
	}
	return nil
}
