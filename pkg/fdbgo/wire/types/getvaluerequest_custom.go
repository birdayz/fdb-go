package types

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

// emptyVersionVector is the serialized form of an empty VersionVector.
// C++ VersionVector::getEncodedSize() = sizeof(size_t) + sizeof(Version) = 16.
var emptyVersionVector = make([]byte, 16)

// MarshalGetValueRequest builds a GetValueRequest from parameters.
func MarshalGetValueRequest(
	key []byte, version int64,
	replyFirst, replySecond uint64,
	tenantId int64,
) []byte {
	vt := GetValueRequestVTable
	w := wire.NewWriter(nil)
	return w.WriteMessagePacked(GetValueRequestTemplate,
		func(obj *wire.ObjectWriter) {
			WriteTenantInfo(obj, int(vt[GetValueRequestSlotTenantInfo+2]), tenantId)
			obj.WriteStruct(int(vt[GetValueRequestSlotSpanContext+2]), SpanContextVTable, 8, func(inner *wire.ObjectWriter) {})
			WriteReplyPromise(obj, int(vt[GetValueRequestSlotReply+2]), replyFirst, replySecond)
			obj.WriteInt64(int(vt[GetValueRequestSlotVersion+2]), version)
			obj.WriteBytes(int(vt[GetValueRequestSlotKey+2]), key)
			obj.WriteBytes(int(vt[GetValueRequestSlotSsLatestCommitVersions+2]), emptyVersionVector)
		})
}
