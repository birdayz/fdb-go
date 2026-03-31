package types

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

// MarshalMutationRef produces a standalone FlatBuffers blob for a MutationRef.
// VTable {10, 13, 12, 4, 8}: type (uint8 at 12), param1/key (bytes at 4), param2/value (bytes at 8).
func MarshalMutationRef(mutType uint8, key, value []byte) []byte {
	return wire.MarshalStructBlob(MutationRefVTable, func(obj *wire.ObjectWriter) {
		obj.WriteUint8(12, mutType)
		obj.WriteBytes(4, key)
		obj.WriteBytes(8, value)
	})
}
