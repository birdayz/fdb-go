package types

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

// MarshalMutationRef produces a standalone FlatBuffers blob for a MutationRef.
// VTable {10, 13, 12, 4, 8}: type (uint8 at 12), param1/key (bytes at 4), param2/value (bytes at 8).
func MarshalMutationRef(mutType uint8, key, value []byte) []byte {
	vt := MutationRefVTable
	return wire.MarshalStructBlob(vt, func(obj *wire.ObjectWriter) {
		obj.WriteUint8(int(vt[2]), mutType)
		obj.WriteBytes(int(vt[3]), key)
		obj.WriteBytes(int(vt[4]), value)
	})
}
