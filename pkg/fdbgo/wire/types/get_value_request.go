package types

// GetValueRequest — fdbclient/include/fdbclient/StorageServerInterface.h:215
// C++ serialize: serializer(ar, key, version, tags, reply, spanContext, options, ssLatestCommitVersions)
//   slot 0: key                      — Key/StringRef (dynamic_size, RelOff at offset 12)
//   slot 1: version                  — Version/int64 (scalar, 8 bytes at offset 4)
//   slot 2: tags                     — Optional<TagSet> (union_like: type@36, value@16)
//   slot 3: reply                    — ReplyPromise<GetValueReply> (serialize_member, RelOff at offset 20)
//   slot 4: spanContext              — SpanContext (serialize_member, RelOff at offset 24)
//   slot 5: options                  — Optional<ReadOptions> (union_like: type@37, value@28)
//   slot 6: ssLatestCommitVersions   — VersionVector (dynamic_size, RelOff at offset 32)
// VTable: {22, 38, 12, 4, 36, 16, 20, 24, 37, 28, 32}

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
			// slot 4: spanContext at offset 24
			obj.WriteStruct(int(vt[6]), SpanContextVTable, 8, func(inner *wire.ObjectWriter) {
				m.SpanCtx.MarshalInto(inner)
			})
			// slot 3: reply at offset 20
			obj.WriteStruct(int(vt[5]), replyPromiseVTable, 8, func(inner *wire.ObjectWriter) {
				inner.WriteUID(4, m.ReplyToken)
			})
			// slot 1: version at offset 4
			obj.WriteInt64(int(vt[3]), m.Version)
			// slot 0: key at offset 12
			obj.WriteBytes(int(vt[2]), m.Key)
			// slot 6: ssLatestCommitVersions at offset 32
			data := m.SSLatestVersions
			if len(data) == 0 {
				data = emptyVersionVector
			}
			obj.WriteBytes(int(vt[10]), data)
		})
}
