package types

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

// GetKeyValuesRequest — fdbclient/StorageServerInterface.h
// serialize: serializer(ar, begin, end, version, limit, limitBytes, tags, reply, spanContext, tenantInfo, options, ssLatestCommitVersions)
//
// VTable slot mapping:
//
//	vt[2]:  begin (serialize_member, KeySelectorRef)
//	vt[3]:  end (serialize_member, KeySelectorRef)
//	vt[4]:  version (int64)
//	vt[5]:  limit (int32)
//	vt[6]:  limitBytes (int32)
//	vt[7]:  tags.type (union, absent)
//	vt[8]:  tags.value
//	vt[9]:  reply (serialize_member)
//	vt[10]: spanContext (serialize_member)
//	vt[11]: tenantInfo (serialize_member)
//	vt[12]: options.type (union, absent)
//	vt[13]: options.value
//	vt[14]: ssLatestCommitVersions (dynamic_size)
type GetKeyValuesRequest struct {
	BeginKey     []byte
	BeginOffset  int32
	BeginOrEqual bool
	EndKey       []byte
	EndOffset    int32
	EndOrEqual   bool
	Version      int64
	Limit        int32
	LimitBytes   int32
	ReplyFirst   uint64
	ReplySecond  uint64
	TenantId     int64
}

func (m *GetKeyValuesRequest) MarshalFDB() []byte {
	vt := GetKeyValuesRequestVTable
	w := wire.NewWriter(nil)
	return w.WriteMessageWithVTables(GetKeyValuesRequestFileID, vt, 8, GetKeyValuesRequestVTableClosure,
		func(obj *wire.ObjectWriter) {
			WriteTenantInfo(obj, int(vt[11]), m.TenantId)
			obj.WriteStruct(int(vt[10]), SpanContextVTable, 8, func(inner *wire.ObjectWriter) {})
			WriteReplyPromise(obj, int(vt[9]), m.ReplyFirst, m.ReplySecond)
			writeKeySelectorRef(obj, int(vt[3]), m.EndKey, m.EndOffset, m.EndOrEqual)
			writeKeySelectorRef(obj, int(vt[2]), m.BeginKey, m.BeginOffset, m.BeginOrEqual)
			obj.WriteInt64(int(vt[4]), m.Version)
			obj.WriteInt32(int(vt[5]), m.Limit)
			obj.WriteInt32(int(vt[6]), m.LimitBytes)
			obj.WriteBytes(int(vt[14]), emptyVersionVector)
		})
}

// writeKeySelectorRef writes a KeySelectorRef nested struct.
// VTable {10, 13, 4, 12, 8}: key (bytes), orEqual (bool), offset (int32)
func writeKeySelectorRef(obj *wire.ObjectWriter, parentOffset int, key []byte, offset int32, orEqual bool) {
	ksVT := KeySelectorRefVTable
	obj.WriteStruct(parentOffset, ksVT, 4, func(inner *wire.ObjectWriter) {
		inner.WriteBytes(int(ksVT[2]), key)
		if orEqual {
			inner.WriteUint8(int(ksVT[3]), 1)
		}
		inner.WriteInt32(int(ksVT[4]), offset)
	})
}
