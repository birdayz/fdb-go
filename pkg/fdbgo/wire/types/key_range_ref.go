package types

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

// MarshalKeyRangeRef produces a standalone FlatBuffers blob for a KeyRangeRef.
// VTable {8, 12, 4, 8}: begin (bytes at 4), end (bytes at 8).
func MarshalKeyRangeRef(begin, end []byte) []byte {
	return wire.MarshalStructBlob(KeyRangeRefVTable, func(obj *wire.ObjectWriter) {
		obj.WriteBytes(4, begin)
		obj.WriteBytes(8, end)
	})
}
