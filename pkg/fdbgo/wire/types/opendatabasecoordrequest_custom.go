package types

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

func (m *OpenDatabaseCoordRequest) UnmarshalFDB(data []byte) error {
	panic("OpenDatabaseCoordRequest.UnmarshalFDB not implemented")
}

func (m *OpenDatabaseCoordRequest) MarshalFDB() []byte {
	vt := OpenDatabaseCoordRequestVTable
	w := wire.NewWriter(nil)
	return w.WriteMessagePacked(OpenDatabaseCoordRequestTemplate,
		func(obj *wire.ObjectWriter) {
			// knownClientInfoID: UID all zeros (inline 16 bytes)
			obj.WriteUint64(int(vt[OpenDatabaseCoordRequestSlotKnownClientInfoID+2]), 0)
			obj.WriteUint64(int(vt[OpenDatabaseCoordRequestSlotKnownClientInfoID+2])+8, 0)
			WriteReplyPromise(obj, int(vt[OpenDatabaseCoordRequestSlotReply+2]), m.ReplyFirst, m.ReplySecond)
			obj.WriteBytes(int(vt[OpenDatabaseCoordRequestSlotClusterKey+2]), []byte(m.ClusterKey))
			obj.WriteBool(int(vt[OpenDatabaseCoordRequestSlotInternal+2]), m.Internal)
		})
}
