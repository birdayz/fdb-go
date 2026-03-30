package protocol

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

// UID constants for the FDB UID type (flow/include/flow/IRandom.h).
const UID_FileIdentifier uint32 = 15597147

// UID_VTable: vtable_size=8, object_size=20, field0(part[0])@4, field1(part[1])@12
var UID_VTable = wire.VTable{8, 20, 4, 12}
