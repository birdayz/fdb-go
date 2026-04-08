package fdb

// Tenant CRUD via system keys (\xff/tenant/*).
// 1:1 port of C++ TenantAPI::createTenantTransaction (TenantManagement.actor.h).
//
// System key layout (TenantMetadataSpecification with prefix \xff/):
//   \xff/tenant/nameIndex/<name>  → int64 tenant ID (tuple-encoded key, little-endian value)
//   \xff/tenant/map/<id>          → ObjectWriter(TenantMapEntry) with IncludeVersion
//   \xff/tenant/lastId            → int64 (little-endian)
//   \xff/tenant/count             → int64 (little-endian, atomic ADD)
//   \xff/tenant/lastModification  → versionstamped value

import (
	"encoding/binary"
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire/types"
)

const (
	tenantSubspace     = "\xff/tenant/"
	tenantNameIndexKey = tenantSubspace + "nameIndex/"
	tenantMapKey       = tenantSubspace + "map/"
	tenantLastIdKey    = tenantSubspace + "lastId"
	tenantCountKey     = tenantSubspace + "count"
)

// createTenantInternal implements C++ TenantAPI::createTenantTransaction.
// Must be called with ACCESS_SYSTEM_KEYS enabled on the transaction.
func createTenantInternal(tr Transaction, name []byte) (int64, error) {
	// Check if tenant already exists via nameIndex.
	nameIdxKey := Key(tenantNameIndexKey + string(name))
	existing, err := tr.Get(nameIdxKey).Get()
	if err != nil {
		return 0, fmt.Errorf("check tenant exists: %w", err)
	}
	if existing != nil {
		return 0, errTenantExists
	}

	// Allocate next tenant ID.
	// C++: getNextTenantId reads lastTenantId, increments by 1 (or random in BUGGIFY).
	lastIdVal, err := tr.Get(Key(tenantLastIdKey)).Get()
	if err != nil {
		return 0, fmt.Errorf("read lastTenantId: %w", err)
	}
	var lastId int64
	if lastIdVal != nil && len(lastIdVal) >= 8 {
		lastId = int64(binary.LittleEndian.Uint64(lastIdVal))
	}
	newId := lastId + 1

	// Write new lastId.
	var idBuf [8]byte
	binary.LittleEndian.PutUint64(idBuf[:], uint64(newId))
	tr.Set(Key(tenantLastIdKey), idBuf[:])

	// Compute prefix: tenant ID as big-endian 8 bytes.
	// C++: TenantMapEntry constructor calls computePrefix which does bigEndian64(id).
	var prefix [8]byte
	binary.BigEndian.PutUint64(prefix[:], uint64(newId))

	// Build TenantMapEntry.
	entry := types.TenantMapEntry{
		Id:                       newId,
		TenantName:               name,
		TenantLockState:          0, // UNLOCKED
		ConfigurationSequenceNum: 0,
	}

	// Write to tenantMap: key = tuple-encoded int64 ID, value = ObjectWriter(entry)
	// FDB tuple int64 encoding: 0x1C (positive 8-byte int) + big-endian bytes
	mapKey := Key(tenantMapKey)
	mapKey = append(mapKey, packInt64ForTuple(newId)...)
	tr.Set(mapKey, entry.MarshalFDB())

	// Write to nameIndex: key = name bytes, value = little-endian int64 ID
	tr.Set(nameIdxKey, idBuf[:])

	// Atomically increment tenant count.
	var oneBuf [8]byte
	binary.LittleEndian.PutUint64(oneBuf[:], 1)
	tr.Add(Key(tenantCountKey), oneBuf[:])

	return newId, nil
}

// deleteTenantInternal implements C++ TenantAPI::deleteTenantTransaction.
func deleteTenantInternal(tr Transaction, name []byte) error {
	// Look up tenant ID from nameIndex.
	nameIdxKey := Key(tenantNameIndexKey + string(name))
	idVal, err := tr.Get(nameIdxKey).Get()
	if err != nil {
		return fmt.Errorf("read tenant nameIndex: %w", err)
	}
	if idVal == nil || len(idVal) < 8 {
		return errTenantNotFound
	}
	tenantId := int64(binary.LittleEndian.Uint64(idVal))

	// Delete from tenantMap.
	mapKey := Key(tenantMapKey)
	mapKey = append(mapKey, packInt64ForTuple(tenantId)...)
	tr.Clear(mapKey)

	// Delete from nameIndex.
	tr.Clear(nameIdxKey)

	// Decrement tenant count.
	var minusOne [8]byte
	binary.LittleEndian.PutUint64(minusOne[:], ^uint64(0)) // -1 in two's complement
	tr.Add(Key(tenantCountKey), minusOne[:])

	return nil
}

// listTenantsInternal lists tenants by scanning the nameIndex.
func listTenantsInternal(tr Transaction) ([]Key, error) {
	begin := Key(tenantNameIndexKey)
	end, err := Strinc(begin)
	if err != nil {
		return nil, err
	}
	kvs, err := tr.GetRange(KeyRange{Begin: begin, End: Key(end)}, RangeOptions{}).GetSliceWithError()
	if err != nil {
		return nil, err
	}
	names := make([]Key, len(kvs))
	for i, kv := range kvs {
		names[i] = kv.Key[len(tenantNameIndexKey):]
	}
	return names, nil
}

// packInt64ForTuple encodes an int64 in FDB tuple format.
// Positive values: 0x1C + 8 bytes big-endian.
// This avoids importing the tuple package (which imports fdb → cycle).
func packInt64ForTuple(v int64) []byte {
	buf := make([]byte, 9)
	buf[0] = 0x1C // intZeroCode + 8
	binary.BigEndian.PutUint64(buf[1:], uint64(v))
	return buf
}

// openTenantInternal looks up tenant ID from nameIndex.
func openTenantInternal(tr Transaction, name []byte) (int64, error) {
	nameIdxKey := Key(tenantNameIndexKey + string(name))
	idVal, err := tr.Get(nameIdxKey).Get()
	if err != nil {
		return 0, fmt.Errorf("read tenant nameIndex: %w", err)
	}
	if idVal == nil || len(idVal) < 8 {
		return 0, errTenantNotFound
	}
	return int64(binary.LittleEndian.Uint64(idVal)), nil
}
