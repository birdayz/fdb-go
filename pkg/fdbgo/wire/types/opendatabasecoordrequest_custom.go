package types

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

// MarshalOpenDatabaseCoordRequest builds an OpenDatabaseCoordRequest from parameters.
func MarshalOpenDatabaseCoordRequest(
	clusterKey string,
	replyFirst, replySecond uint64,
	internal bool,
) []byte {
	vt := OpenDatabaseCoordRequestVTable
	w := wire.NewWriter(nil)
	return w.WriteMessagePacked(OpenDatabaseCoordRequestTemplate,
		func(obj *wire.ObjectWriter) {
			// knownClientInfoID: UID all zeros (inline 16 bytes)
			obj.WriteUint64(int(vt[OpenDatabaseCoordRequestSlotKnownClientInfoID+2]), 0)
			obj.WriteUint64(int(vt[OpenDatabaseCoordRequestSlotKnownClientInfoID+2])+8, 0)
			WriteReplyPromise(obj, int(vt[OpenDatabaseCoordRequestSlotReply+2]), wire.UIDFromParts(replyFirst, replySecond))
			obj.WriteBytes(int(vt[OpenDatabaseCoordRequestSlotClusterKey+2]), []byte(clusterKey))
			obj.WriteBool(int(vt[OpenDatabaseCoordRequestSlotInternal+2]), internal)
		})
}
