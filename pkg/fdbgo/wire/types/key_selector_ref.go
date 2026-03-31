package types

// KeySelectorRef — fdbclient/include/fdbclient/FDBTypes.h:629
// C++ serialize: serializer(ar, key, orEqual, offset)
//   slot 0: key     — KeyRef/StringRef (dynamic_size, 4-byte RelOff at offset 4)
//   slot 1: orEqual — bool (scalar, 1 byte at offset 12)
//   slot 2: offset  — int (scalar, 4 bytes at offset 8)

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

type KeySelectorRef struct {
	Key     []byte
	OrEqual bool
	Offset  int32
}

// FirstGreaterOrEqual returns a KeySelectorRef for the first key >= k.
func FirstGreaterOrEqual(k []byte) KeySelectorRef {
	return KeySelectorRef{Key: k, OrEqual: false, Offset: 1}
}

// FirstGreaterThan returns a KeySelectorRef for the first key > k.
func FirstGreaterThan(k []byte) KeySelectorRef {
	return KeySelectorRef{Key: k, OrEqual: true, Offset: 1}
}

func (m *KeySelectorRef) TypeVTable() wire.VTable { return KeySelectorRefVTable }

func (m *KeySelectorRef) MarshalInto(obj *wire.ObjectWriter) {
	vt := KeySelectorRefVTable
	obj.WriteBytes(int(vt[2]), m.Key)
	obj.WriteInt32(int(vt[4]), m.Offset)
	obj.WriteBool(int(vt[3]), m.OrEqual)
}

func (m *KeySelectorRef) UnmarshalFrom(r *wire.Reader) error {
	if r.FieldPresent(0) {
		m.Key = r.ReadBytes(0)
	}
	if r.FieldPresent(1) {
		m.OrEqual = r.ReadBool(1)
	}
	if r.FieldPresent(2) {
		m.Offset = r.ReadInt32(2)
	}
	return nil
}
