package types

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

// WriteTenantInfo writes a TenantInfo nested struct. tenantId=-1 = no tenant.
func WriteTenantInfo(obj *wire.ObjectWriter, parentOffset int, tenantId int64) {
	vt := TenantInfoVTable
	obj.WriteStruct(parentOffset, vt, 8, func(inner *wire.ObjectWriter) {
		inner.WriteInt64(int(vt[TenantInfoSlotField_0+2]), tenantId)
	})
}
