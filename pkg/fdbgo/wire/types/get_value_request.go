package types

// GetValueRequest — fdbclient/StorageServerInterface.h
// C++ serialize: serializer(ar, key, version, tags, reply, spanContext, options, ssLatestCommitVersions, tenantInfo)
//
// VTable field mapping (12 entries):
//   vt[2]=12:  key (dynamic_size, RelOff)
//   vt[3]=4:   version (scalar int64)
//   vt[4]=40:  tags.type (union_like)
//   vt[5]=16:  tags.value
//   vt[6]=20:  reply (serialize_member, RelOff)
//   vt[7]=24:  spanContext (serialize_member, RelOff)
//   vt[8]=28:  tenantInfo (serialize_member, RelOff) — absent for client requests
//   vt[9]=41:  options.type (union_like)
//   vt[10]=32: options.value
//   vt[11]=36: ssLatestCommitVersions (dynamic_size, RelOff)

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

// ReplyPromise vtable: just a UID (16 bytes inline).
// C++ ReplyPromise<T> serialize: serializer(ar, token) where token is UID.
var replyPromiseVTable = wire.VTable{6, 20, 4}

// emptyVersionVector is the serialized form of an empty VersionVector.
// C++ VersionVector::getEncodedSize() returns sizeof(size_t) + sizeof(Version) = 16
// for an empty vector (utlCount=0, maxVersion).
var emptyVersionVector = make([]byte, 16)

type GetValueRequest struct {
	Key              []byte
	Version          int64
	ReplyToken       [16]byte // UID for ReplyPromise
	SpanCtx          SpanContext
	SSLatestVersions []byte // VersionVector — empty = 16 zero bytes
}

func (m *GetValueRequest) TypeVTable() wire.VTable { return GetValueRequestVTable }

func (m *GetValueRequest) MarshalFDB() []byte {
	vt := GetValueRequestVTable
	w := wire.NewWriter(nil)
	return w.WriteMessageWithVTables(
		GetValueRequestFileID, vt, 8, GetValueRequestVTableClosure,
		func(obj *wire.ObjectWriter) {
			// Nested structs in REVERSE serialization order (matches C++ end-offset allocation).
			obj.WriteStruct(int(vt[7]), SpanContextVTable, 8, func(inner *wire.ObjectWriter) {
				m.SpanCtx.MarshalInto(inner)
			})
			obj.WriteStruct(int(vt[6]), replyPromiseVTable, 8, func(inner *wire.ObjectWriter) {
				inner.WriteUID(4, m.ReplyToken)
			})
			obj.WriteInt64(int(vt[3]), m.Version)
			obj.WriteBytes(int(vt[2]), m.Key)
			// tenantInfo at vt[8] — absent (zero RelOff = null pointer)
			data := m.SSLatestVersions
			if len(data) == 0 {
				data = emptyVersionVector
			}
			obj.WriteBytes(int(vt[11]), data)
		})
}
