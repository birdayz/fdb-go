package types

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

func (m *CommitTransactionRequest) MarshalFDB() []byte {
	panic("CommitTransactionRequest.MarshalFDB not implemented — use MarshalCommitTransactionRequest function")
}

// MarshalCommitTransactionRequest builds the full request from pre-serialized
// mutation and conflict range vectors.
func MarshalCommitTransactionRequest(
	readVersion int64,
	mutData, readCRData, writeCRData []byte,
	replyFirst, replySecond uint64,
	tenantId int64,
) []byte {
	vt := CommitTransactionRequestVTable
	ctVT := CommitTransactionRefVTable

	w := wire.NewWriter(nil)
	return w.WriteMessagePacked(CommitTransactionRequestTemplate,
		func(obj *wire.ObjectWriter) {
			WriteTenantInfo(obj, int(vt[CommitTransactionRequestSlotTenantInfo+2]), tenantId)
			obj.WriteStruct(int(vt[CommitTransactionRequestSlotSpanContext+2]), SpanContextVTable, 8, func(inner *wire.ObjectWriter) {})
			WriteReplyPromise(obj, int(vt[CommitTransactionRequestSlotReply+2]), wire.UIDFromParts(replyFirst, replySecond))

			// CommitTransactionRef
			obj.WriteStruct(int(vt[CommitTransactionRequestSlotTransaction+2]), ctVT, 8, func(inner *wire.ObjectWriter) {
				inner.WriteInt64(int(ctVT[CommitTransactionRefSlotField_3+2]), readVersion)  // read_snapshot
				inner.WriteRawOOL(int(ctVT[CommitTransactionRefSlotField_0+2]), readCRData)  // read_conflict_ranges
				inner.WriteRawOOL(int(ctVT[CommitTransactionRefSlotField_1+2]), writeCRData) // write_conflict_ranges
				inner.WriteRawOOL(int(ctVT[CommitTransactionRefSlotField_2+2]), mutData)     // mutations
			})

			obj.WriteUint32(int(vt[CommitTransactionRequestSlotFlags+2]), 0) // flags
		})
}
