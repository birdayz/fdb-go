package types

// ReadOptions — fdbclient/include/fdbclient/FDBTypes.h:1644
// C++ serialize: serializer(ar, type, cacheResult, debugID, consistencyCheckStartVersion, lockAware)
//   slot 0: type                          — ReadType/enum/int (scalar, 4 bytes at offset 4)
//   slot 1: cacheResult                   — bool (scalar, 1 byte at offset 16)
//   slot 2: debugID                       — Optional<UID> (union_like: type@17, value@8)
//   slot 3: consistencyCheckStartVersion  — Optional<Version> (union_like: type@18, value@12)
//   slot 4: lockAware                     — bool (scalar, 1 byte at offset 19)
// VTable: {18, 20, 4, 16, 17, 8, 18, 12, 19}

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

type ReadOptions struct {
	Type        int32 // ReadType enum (NORMAL=3)
	CacheResult bool
	LockAware   bool
	// Optional fields — not needed for client requests (always absent).
}

func (m *ReadOptions) TypeVTable() wire.VTable { return ReadOptionsVTable }

func (m *ReadOptions) MarshalInto(obj *wire.ObjectWriter) {
	obj.WriteInt32(4, m.Type)        // slot 0: type at offset 4
	obj.WriteBool(16, m.CacheResult) // slot 1: cacheResult at offset 16
	// slot 2: debugID — absent (Optional type byte = 0 at offset 17)
	// slot 3: consistencyCheckStartVersion — absent (type byte = 0 at offset 18)
	obj.WriteBool(19, m.LockAware) // slot 4: lockAware at offset 19
}

func (m *ReadOptions) UnmarshalFrom(r *wire.Reader) error {
	if r.FieldPresent(0) {
		m.Type = r.ReadInt32(0)
	}
	if r.FieldPresent(1) {
		m.CacheResult = r.ReadBool(1)
	}
	if r.FieldPresent(4) {
		m.LockAware = r.ReadBool(4)
	}
	return nil
}
