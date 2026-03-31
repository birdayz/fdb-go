package types

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

// GetKeyRequest — fdbclient/StorageServerInterface.h
// serialize: serializer(ar, sel, version, tags, reply, spanContext, options, ssLatestCommitVersions, tenantInfo)
//
// VTable slot mapping:
//
//	vt[2]:  sel (serialize_member, KeySelectorRef)
//	vt[3]:  version (int64)
//	vt[4]:  tags.type (union, absent)
//	vt[5]:  tags.value
//	vt[6]:  reply (serialize_member)
//	vt[7]:  spanContext (serialize_member)
//	vt[8]:  tenantInfo (serialize_member)
//	vt[9]:  options.type (union, absent)
//	vt[10]: options.value
//	vt[11]: ssLatestCommitVersions (dynamic_size)
type GetKeyRequest struct {
	SelectorKey     []byte
	SelectorOrEqual bool
	SelectorOffset  int32
	Version         int64
	ReplyFirst      uint64
	ReplySecond     uint64
	TenantId        int64
}

func (m *GetKeyRequest) MarshalFDB() []byte {
	vt := GetKeyRequestVTable
	w := wire.NewWriter(nil)
	return w.WriteMessageWithVTables(GetKeyRequestFileID, vt, 8, GetKeyRequestVTableClosure,
		func(obj *wire.ObjectWriter) {
			WriteTenantInfo(obj, int(vt[8]), m.TenantId)
			obj.WriteStruct(int(vt[7]), SpanContextVTable, 8, func(inner *wire.ObjectWriter) {})
			WriteReplyPromise(obj, int(vt[6]), m.ReplyFirst, m.ReplySecond)
			writeKeySelectorRef(obj, int(vt[2]), m.SelectorKey, m.SelectorOffset, m.SelectorOrEqual)
			obj.WriteInt64(int(vt[3]), m.Version)
			obj.WriteBytes(int(vt[11]), emptyVersionVector)
		})
}
