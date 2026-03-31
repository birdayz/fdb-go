package types

// Error — flow/include/flow/Error.h
// C++ serialize: serializer(ar, error_code)
//   slot 0: error_code — int (scalar, 4 bytes at offset 4)
// Note: C++ Error.error_code is int (32-bit), not uint16.

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

type Error struct {
	Code int32
}

func (m *Error) TypeVTable() wire.VTable { return ErrorVTable }

func (m *Error) MarshalInto(obj *wire.ObjectWriter) {
	obj.WriteInt32(4, m.Code) // slot 0: error_code at offset 4
}

func (m *Error) UnmarshalFrom(r *wire.Reader) error {
	if r.FieldPresent(0) {
		m.Code = r.ReadInt32(0)
	}
	return nil
}
