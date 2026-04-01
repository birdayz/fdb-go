package types

import (
	"encoding/binary"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
)

// emptyVersionVector is the serialized form of an empty VersionVector.
// C++ VersionVector::getEncodedSize() = sizeof(size_t) + sizeof(Version) = 16.
var emptyVersionVector = make([]byte, 16)

// GetValueRequest — fdbclient/StorageServerInterface.h
// serialize: serializer(ar, key, version, tags, reply, spanContext, options, ssLatestCommitVersions, tenantInfo)
//
// VTable slot mapping:
//
//	vt[2]:  key (dynamic_size)
//	vt[3]:  version (int64)
//	vt[4]:  tags.type (union, absent)
//	vt[5]:  tags.value
//	vt[6]:  reply (serialize_member)
//	vt[7]:  spanContext (serialize_member)
//	vt[8]:  tenantInfo (serialize_member)
//	vt[9]:  options.type (union, absent)
//	vt[10]: options.value
//	vt[11]: ssLatestCommitVersions (dynamic_size)
type GetValueRequest struct {
	Key         []byte
	Version     int64
	ReplyFirst  uint64
	ReplySecond uint64
	TenantId    int64 // -1 = no tenant
}

// getValueRequestTemplate is pre-computed at init — zero vtable allocs at runtime.
var getValueRequestTemplate = wire.NewMessageTemplate(
	GetValueRequestFileID, GetValueRequestVTable, 8, GetValueRequestVTableClosure,
)

func (m *GetValueRequest) MarshalFDB() []byte {
	vt := GetValueRequestVTable
	w := wire.NewWriter(nil)
	return w.WriteMessagePacked(getValueRequestTemplate,
		func(obj *wire.ObjectWriter) {
			WriteTenantInfo(obj, int(vt[8]), m.TenantId)
			obj.WriteStruct(int(vt[7]), SpanContextVTable, 8, func(inner *wire.ObjectWriter) {})
			WriteReplyPromise(obj, int(vt[6]), m.ReplyFirst, m.ReplySecond)
			obj.WriteInt64(int(vt[3]), m.Version)
			obj.WriteBytes(int(vt[2]), m.Key)
			obj.WriteBytes(int(vt[11]), emptyVersionVector)
		})
}

var _ = binary.LittleEndian // used by other files in package
