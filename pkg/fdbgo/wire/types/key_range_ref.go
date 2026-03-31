package types

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

// MarshalKeyRangeRef produces a standalone FlatBuffers blob for a KeyRangeRef.
// VTable {8, 12, 4, 8}: begin (bytes at 4), end (bytes at 8).
func MarshalKeyRangeRef(begin, end []byte) []byte {
	vt := KeyRangeRefVTable
	return wire.MarshalStructBlob(vt, func(obj *wire.ObjectWriter) {
		obj.WriteBytes(int(vt[2]), begin)
		obj.WriteBytes(int(vt[3]), end)
	})
}
