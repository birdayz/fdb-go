package types

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

// TenantInfo — fdbclient/TenantInfo.h
// VTable {10, 17, 4, 16, 12}: tenantId (int64), tenantName (bytes), tenantGroup (bytes)
//
// WriteTenantInfo writes a TenantInfo nested struct at the given parent offset.
// tenantId=-1 means "no tenant" (the common case for non-tenant requests).
func WriteTenantInfo(obj *wire.ObjectWriter, parentOffset int, tenantId int64) {
	vt := TenantInfoVTable
	obj.WriteStruct(parentOffset, vt, 8, func(inner *wire.ObjectWriter) {
		inner.WriteInt64(int(vt[2]), tenantId) // field 0: tenantId
	})
}
