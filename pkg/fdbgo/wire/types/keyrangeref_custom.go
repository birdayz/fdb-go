package types

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

func (m *KeyRangeRef) UnmarshalFDB(data []byte) error {
	r, err := wire.NewReader(data)
	if err != nil {
		return err
	}
	if r.FieldPresent(KeyRangeRefSlotBegin) {
		m.Begin = r.ReadBytes(KeyRangeRefSlotBegin)
	}
	if r.FieldPresent(KeyRangeRefSlotEnd) {
		m.End = r.ReadBytes(KeyRangeRefSlotEnd)
	}
	return nil
}

// MarshalKeyRangeRef produces a standalone FlatBuffers blob for a KeyRangeRef.
// Used for embedding in VectorRef (e.g., write conflict ranges).
func MarshalKeyRangeRef(begin, end []byte) []byte {
	vt := KeyRangeRefVTable
	return wire.MarshalStructBlob(vt, func(obj *wire.ObjectWriter) {
		obj.WriteBytes(int(vt[KeyRangeRefSlotBegin+2]), begin)
		obj.WriteBytes(int(vt[KeyRangeRefSlotEnd+2]), end)
	})
}
