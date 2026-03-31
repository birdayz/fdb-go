package types

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

// CommitTransactionRequest — fdbclient/CommitProxyInterface.h
// serialize: serializer(ar, transaction, reply, flags, debugID, commitCostEstimation, tagSet, spanContext, tenantInfo, idempotencyId)
//
// VTable slot mapping (using [N+2] convention):
//   vt[0+2]:  transaction (serialize_member, CommitTransactionRef)
//   vt[1+2]:  reply (serialize_member, ReplyPromise)
//   vt[2+2]:  flags (uint32)
//   vt[3+2]:  debugID.type (union, absent)
//   vt[4+2]:  debugID.value
//   vt[5+2]:  commitCostEstimation.type (union, absent)
//   vt[6+2]:  commitCostEstimation.value
//   vt[7+2]:  tagSet.type (union, absent)
//   vt[8+2]:  tagSet.value
//   vt[9+2]:  spanContext (serialize_member)
//   vt[10+2]: tenantInfo (serialize_member)
//   vt[11+2]: idempotencyId (dynamic_size)

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
	return w.WriteMessage(CommitTransactionRequestFileID, vt, 4, func(obj *wire.ObjectWriter) {
		WriteTenantInfo(obj, int(vt[10+2]), tenantId)
		obj.WriteStruct(int(vt[9+2]), SpanContextVTable, 8, func(inner *wire.ObjectWriter) {})
		WriteReplyPromise(obj, int(vt[1+2]), replyFirst, replySecond)

		// CommitTransactionRef
		obj.WriteStruct(int(vt[0+2]), ctVT, 8, func(inner *wire.ObjectWriter) {
			inner.WriteInt64(int(ctVT[5]), readVersion)  // read_snapshot
			inner.WriteRawOOL(int(ctVT[2]), readCRData)  // read_conflict_ranges
			inner.WriteRawOOL(int(ctVT[3]), writeCRData) // write_conflict_ranges
			inner.WriteRawOOL(int(ctVT[4]), mutData)     // mutations
		})

		obj.WriteUint32(int(vt[2+2]), 0) // flags
	})
}
