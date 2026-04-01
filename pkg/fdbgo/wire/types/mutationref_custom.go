package types

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

// MutationRef fields: slot 0 = type (uint8), slot 1 = param1/key, slot 2 = param2/value.

func (m *MutationRef) UnmarshalFDB(data []byte) error {
	r, err := wire.NewReader(data)
	if err != nil {
		return err
	}
	if r.FieldPresent(MutationRefSlotField_0) {
		m.Field_0 = r.ReadUint8(MutationRefSlotField_0)
	}
	if r.FieldPresent(MutationRefSlotField_1) {
		m.Field_1 = r.ReadBytes(MutationRefSlotField_1)
	}
	if r.FieldPresent(MutationRefSlotField_2) {
		m.Field_2 = r.ReadBytes(MutationRefSlotField_2)
	}
	return nil
}

// MarshalMutationRef produces a standalone FlatBuffers blob for a MutationRef.
func MarshalMutationRef(mutType uint8, key, value []byte) []byte {
	vt := MutationRefVTable
	return wire.MarshalStructBlob(vt, func(obj *wire.ObjectWriter) {
		obj.WriteUint8(int(vt[MutationRefSlotField_0+2]), mutType)
		obj.WriteBytes(int(vt[MutationRefSlotField_1+2]), key)
		obj.WriteBytes(int(vt[MutationRefSlotField_2+2]), value)
	})
}
