package types

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

// MutationRef fields: slot 0 = type (uint8), slot 1 = param1/key, slot 2 = param2/value.

// MarshalMutationRef produces a standalone FlatBuffers blob for a MutationRef.
func MarshalMutationRef(mutType uint8, key, value []byte) []byte {
	vt := MutationRefVTable
	return wire.MarshalStructBlob(vt, func(obj *wire.ObjectWriter) {
		obj.WriteUint8(int(vt[MutationRefSlotField_0+2]), mutType)
		obj.WriteBytes(int(vt[MutationRefSlotField_1+2]), key)
		obj.WriteBytes(int(vt[MutationRefSlotField_2+2]), value)
	})
}
