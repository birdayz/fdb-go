package types

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

// OpenDatabaseCoordRequest — fdbclient/CoordinationInterface.h
// serialize: serializer(ar, issues, supportedVersions, traceLogGroup, knownClientInfoID, clusterKey, coordinators, reply, hostnames, internal)
//
// VTable slot mapping:
//
//	vt[2]:  issues (serialize_member)
//	vt[3]:  supportedVersions (serialize_member)
//	vt[4]:  traceLogGroup (serialize_member)
//	vt[5]:  knownClientInfoID (UID, 16 bytes inline)
//	vt[6]:  clusterKey (serialize_member)
//	vt[7]:  coordinators (vector_like)
//	vt[8]:  reply (serialize_member)
//	vt[9]:  hostnames (vector_like)
//	vt[10]: internal (bool)
type OpenDatabaseCoordRequest struct {
	ClusterKey  string
	ReplyFirst  uint64
	ReplySecond uint64
	Internal    bool
}

func (m *OpenDatabaseCoordRequest) MarshalFDB() []byte {
	vt := OpenDatabaseCoordRequestVTable
	w := wire.NewWriter(nil)
	return w.WriteMessage(OpenDatabaseCoordRequestFileID, vt, 8, func(obj *wire.ObjectWriter) {
		// knownClientInfoID: UID all zeros (field 3, inline)
		obj.WriteUint64(int(vt[5]), 0)
		obj.WriteUint64(int(vt[5])+8, 0)
		WriteReplyPromise(obj, int(vt[8]), m.ReplyFirst, m.ReplySecond)
		obj.WriteBytes(int(vt[6]), []byte(m.ClusterKey))
		obj.WriteBool(int(vt[10]), m.Internal)
	})
}
