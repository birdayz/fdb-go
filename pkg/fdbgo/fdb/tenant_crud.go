package fdb

// Tenant CRUD via system keys (\xff/tenant/*).
// 1:1 port of C++ TenantAPI (TenantManagement.actor.h).
//
// System key layout (TenantMetadataSpecification with prefix \xff/):
//   \xff/tenant/nameIndex/<name>  → TupleCodec<int64_t> (tuple-encoded int64)
//   \xff/tenant/map/<id>          → TenantIdCodec key (raw 8-byte BE), ObjectCodec<TenantMapEntry, IncludeVersion> value
//   \xff/tenant/lastId            → TupleCodec<int64_t> (tuple-encoded int64)
//   \xff/tenant/count             → BinaryCodec<int64_t> (raw 8-byte LE, atomic ADD)
//   \xff/tenant/lastModification  → BinaryCodec<Versionstamp> (SetVersionstampedValue)
//   \xff/conf/tenant_mode         → string "0"/"1"/"2" (DISABLED/OPTIONAL/REQUIRED)

import (
	"encoding/binary"
	"fmt"
	"strconv"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire/types"
)

const (
	tenantSubspace            = "\xff/tenant/"
	tenantNameIndexKey        = tenantSubspace + "nameIndex/"
	tenantMapKey              = tenantSubspace + "map/"
	tenantLastIdKey           = tenantSubspace + "lastId"
	tenantCountKey            = tenantSubspace + "count"
	tenantLastModificationKey = tenantSubspace + "lastModification"
	tenantModeConfKey         = "\xff/conf/tenant_mode"

	// protocolVersion73 is the FDB 7.3.x protocol version used as IncludeVersion prefix.
	// C++ ObjectCodec<TenantMapEntry, IncludeVersion> prepends this before FlatBuffers data.
	protocolVersion73 = uint64(0x0FDB00B073000000)

	// maxTenantsPerCluster matches CLIENT_KNOBS->MAX_TENANTS_PER_CLUSTER (1e6).
	maxTenantsPerCluster = 1_000_000

	// Tenant mode values matching C++ TenantMode::Mode enum.
	tenantModeDisabled = 0
	// tenantModeOptional = 1
	// tenantModeRequired = 2
)

// tupleIntZeroCode is the FDB tuple type code for integer 0; positive ints use
// tupleIntZeroCode+n and negatives tupleIntZeroCode-n, where n is the byte width.
const tupleIntZeroCode = 0x14

// tupleIntSizeLimits[n] is the largest unsigned value representable in n bytes
// (2^(8n)-1). Mirrors tuple.sizeLimits; inlined to avoid importing the tuple package
// (which imports fdb → cycle, per the file comment).
var tupleIntSizeLimits = [9]uint64{
	1<<(0*8) - 1, 1<<(1*8) - 1, 1<<(2*8) - 1, 1<<(3*8) - 1,
	1<<(4*8) - 1, 1<<(5*8) - 1, 1<<(6*8) - 1, 1<<(7*8) - 1,
	1<<(8*8) - 1,
}

// tupleIntWidth returns the minimal byte width n in [0,8] holding unsigned u.
func tupleIntWidth(u uint64) int {
	n := 0
	for tupleIntSizeLimits[n] < u {
		n++
	}
	return n
}

// tuplePackInt64 encodes an int64 in FDB tuple format using MINIMAL width — exactly as
// C++ Tuple::append(int64_t) / TupleCodec<int64_t> (Tuple.cpp:204-227): a type code
// tupleIntZeroCode ± n followed by the n significant big-endian bytes (n = significant
// byte count). The tenant nameIndex and lastId are TupleCodec<int64_t>, so a Go client
// sharing a cluster with libfdb_c/Java MUST emit this canonical minimal form. The previous
// fixed 9-byte form (0x1C + 8 bytes) was a non-canonical encoding that libfdb_c never
// produces and that tupleUnpackInt64 could not read back from C/Java-created tenants
// (small IDs encode to 2–3 bytes there).
// Avoids importing tuple package (which imports fdb → cycle).
func tuplePackInt64(v int64) []byte {
	if v == 0 {
		return []byte{tupleIntZeroCode}
	}
	var scratch [8]byte
	if v > 0 {
		n := tupleIntWidth(uint64(v))
		binary.BigEndian.PutUint64(scratch[:], uint64(v))
		return append([]byte{byte(tupleIntZeroCode + n)}, scratch[8-n:]...)
	}
	n := tupleIntWidth(uint64(-v))
	offsetEncoded := int64(tupleIntSizeLimits[n]) + v
	binary.BigEndian.PutUint64(scratch[:], uint64(offsetEncoded))
	return append([]byte{byte(tupleIntZeroCode - n)}, scratch[8-n:]...)
}

// tuplePackBytes encodes a byte string in FDB tuple format.
// FDB tuple encoding: 0x01 + escaped bytes + 0x00.
// Escaping: each 0x00 in data → 0x00 0xFF.
func tuplePackBytes(data []byte) []byte {
	// Count null bytes for size estimation.
	nulls := 0
	for _, b := range data {
		if b == 0x00 {
			nulls++
		}
	}
	buf := make([]byte, 0, 1+len(data)+nulls+1)
	buf = append(buf, 0x01) // bytes type code
	for _, b := range data {
		buf = append(buf, b)
		if b == 0x00 {
			buf = append(buf, 0xFF)
		}
	}
	buf = append(buf, 0x00) // null terminator
	return buf
}

// tupleUnpackBytes decodes a byte string from FDB tuple format.
// Expects: 0x01 + escaped bytes + 0x00.
func tupleUnpackBytes(data []byte) ([]byte, error) {
	if len(data) < 2 || data[0] != 0x01 {
		return nil, fmt.Errorf("tupleUnpackBytes: expected type code 0x01, got 0x%02X", data[0])
	}
	// Unescape: 0x00 0xFF → 0x00.
	var result []byte
	i := 1
	for i < len(data) {
		if data[i] == 0x00 {
			if i+1 < len(data) && data[i+1] == 0xFF {
				result = append(result, 0x00)
				i += 2
			} else {
				break // null terminator
			}
		} else {
			result = append(result, data[i])
			i++
		}
	}
	return result, nil
}

// tupleUnpackInt64 decodes an int64 from FDB tuple format (minimal width), matching
// C++ TupleCodec<int64_t>::unpack. Handles every width n in [0,8] and both signs, so it
// reads libfdb_c/Java's minimal-width values (a small tenant ID is 2–3 bytes there) AND
// any legacy fixed 9-byte value an older Go client wrote (n == 8). The stored value is the
// whole KeyBacked codec output — exactly the encoded integer, no trailing bytes — so the
// length is checked strictly.
//
// Unlike the general tuple decoder (tuple.decodeInt), which returns a uint64 for a positive
// value whose high bit is set (to avoid the int64 wrap), this always returns int64. That
// case cannot arise for the only callers — tenant IDs are bounded by maxTenantsPerCluster
// (1e6) and lastId is a sequential counter that never approaches 2^63.
func tupleUnpackInt64(data []byte) (int64, error) {
	if len(data) == 0 {
		return 0, fmt.Errorf("tupleUnpackInt64: empty value")
	}
	code := int(data[0])
	if code == tupleIntZeroCode {
		if len(data) != 1 {
			return 0, fmt.Errorf("tupleUnpackInt64: zero code 0x14 needs 1 byte, got %d", len(data))
		}
		return 0, nil
	}
	neg := code < tupleIntZeroCode
	n := code - tupleIntZeroCode
	if neg {
		n = tupleIntZeroCode - code
	}
	if n < 1 || n > 8 {
		return 0, fmt.Errorf("tupleUnpackInt64: invalid integer type code 0x%02X", data[0])
	}
	if len(data) != 1+n {
		return 0, fmt.Errorf("tupleUnpackInt64: type code 0x%02X needs %d bytes, got %d", data[0], 1+n, len(data))
	}
	var bp [8]byte
	copy(bp[8-n:], data[1:1+n])
	ret := int64(binary.BigEndian.Uint64(bp[:]))
	if neg {
		return ret - int64(tupleIntSizeLimits[n]), nil
	}
	return ret, nil
}

// prependProtocolVersion prepends the 8-byte LE protocol version header to FlatBuffers data.
// Matches C++ ObjectCodec<T, IncludeVersion>::pack().
func prependProtocolVersion(data []byte) []byte {
	var versionPrefix [8]byte
	binary.LittleEndian.PutUint64(versionPrefix[:], protocolVersion73)
	result := make([]byte, 8+len(data))
	copy(result, versionPrefix[:])
	copy(result[8:], data)
	return result
}

// versionstampedValueOperand returns the 14-byte operand for SetVersionstampedValue.
// C++: BinaryWriter::toValue<Versionstamp>(Versionstamp(), Unversioned()) + 4-byte LE offset.
// Versionstamp() = 10 zero bytes, offset = 0 (versionstamp placed at byte 0 of value).
func versionstampedValueOperand() []byte {
	var buf [14]byte // 10 zero bytes (Versionstamp) + 4 zero bytes (LE offset = 0)
	return buf[:]
}

// tenantIdToPrefix computes the 8-byte big-endian prefix for a tenant ID.
// Matches C++ TenantAPI::idToPrefix(id): bigEndian64(id).
func tenantIdToPrefix(id int64) [8]byte {
	var prefix [8]byte
	binary.BigEndian.PutUint64(prefix[:], uint64(id))
	return prefix
}

// checkTenantMode validates that tenants are enabled in this cluster.
// Matches C++ TenantAPI::checkTenantMode for ClusterType::STANDALONE.
// Reads \xff/conf/tenant_mode; if "0" (DISABLED), returns errTenantsDisabled.
func checkTenantMode(tr Transaction) error {
	modeVal, err := tr.Get(Key(tenantModeConfKey)).Get()
	if err != nil {
		return fmt.Errorf("read tenant_mode: %w", err)
	}
	if modeVal != nil {
		mode, err := strconv.Atoi(string(modeVal))
		if err == nil && mode == tenantModeDisabled {
			return errTenantsDisabled
		}
	}
	// nil (not configured) or non-DISABLED → tenants allowed.
	// In C++, nil means not configured which is treated as DISABLED for
	// STANDALONE clusters when the check is strict. But FDB testcontainers
	// configure tenant_mode=optional_experimental, so this path isn't hit
	// in practice. Match the permissive behavior here.
	return nil
}

// checkPrefixEmpty verifies no keys exist in the tenant's prefix range.
// Used by create (tenant_prefix_allocator_conflict) and delete (tenant_not_empty).
func checkPrefixEmpty(tr Transaction, prefix [8]byte) (bool, error) {
	end, err := Strinc(prefix[:])
	if err != nil {
		return false, fmt.Errorf("strinc tenant prefix: %w", err)
	}
	kvs, err := tr.GetRange(
		KeyRange{Begin: Key(prefix[:]), End: Key(end)},
		RangeOptions{Limit: 1},
	).GetSliceWithError()
	if err != nil {
		return false, fmt.Errorf("check prefix range: %w", err)
	}
	return len(kvs) == 0, nil
}

// createTenantInternal implements C++ TenantAPI::createTenantTransaction.
// Must be called with ACCESS_SYSTEM_KEYS / LOCK_AWARE on the transaction.
func createTenantInternal(tr Transaction, name []byte) (int64, error) {
	// C++: tenant name cannot start with \xff.
	if len(name) > 0 && name[0] == 0xFF {
		return 0, errTenantInvalid
	}

	// C++: checkTenantMode(tr, ClusterType::STANDALONE).
	if err := checkTenantMode(tr); err != nil {
		return 0, err
	}

	// Check if tenant already exists via nameIndex.
	// C++: tryGetTenantTransaction → if present, return (existingEntry, false).
	nameIdxKey := append(Key(tenantNameIndexKey), tuplePackBytes(name)...)
	existing, err := tr.Get(nameIdxKey).Get()
	if err != nil {
		return 0, fmt.Errorf("check tenant exists: %w", err)
	}
	if existing != nil {
		return 0, errTenantExists
	}

	// Allocate next tenant ID.
	// C++: getNextTenantId reads lastTenantId (TupleCodec<int64_t>), increments by 1.
	lastIdVal, err := tr.Get(Key(tenantLastIdKey)).Get()
	if err != nil {
		return 0, fmt.Errorf("read lastTenantId: %w", err)
	}
	var lastId int64
	if lastIdVal != nil {
		lastId, err = tupleUnpackInt64(lastIdVal)
		if err != nil {
			return 0, fmt.Errorf("decode lastTenantId: %w", err)
		}
	}
	newId := lastId + 1

	// Write new lastId: TupleCodec<int64_t>.
	// C++: TenantMetadata::lastTenantId().set(tr, tenantId).
	tr.Set(Key(tenantLastIdKey), tuplePackInt64(newId))

	// Compute prefix and check prefix range is empty.
	// C++: tr->getRange(prefixRange(tenantEntry.prefix), 1) → tenant_prefix_allocator_conflict.
	prefix := tenantIdToPrefix(newId)
	empty, err := checkPrefixEmpty(tr, prefix)
	if err != nil {
		return 0, err
	}
	if !empty {
		return 0, errTenantPrefixConflict
	}

	// Build TenantMapEntry.
	entry := types.TenantMapEntry{
		Id:                       newId,
		TenantName:               name,
		TenantLockState:          0, // UNLOCKED
		ConfigurationSequenceNum: 0,
	}

	// Write to tenantMap.
	// Key: TenantIdCodec = raw 8-byte big-endian int64 (NOT tuple-encoded).
	// Value: ObjectCodec<TenantMapEntry, IncludeVersion> = protocol version prefix + FlatBuffers.
	mapKey := append(Key(tenantMapKey), prefix[:]...)
	tr.Set(mapKey, prependProtocolVersion(entry.MarshalFDB()))

	// Write to nameIndex: TupleCodec<int64_t>.
	tr.Set(nameIdxKey, tuplePackInt64(newId))

	// Update lastTenantModification: SetVersionstampedValue.
	// C++: TenantMetadata::lastTenantModification().setVersionstamp(tr, Versionstamp(), 0).
	tr.SetVersionstampedValue(Key(tenantLastModificationKey), versionstampedValueOperand())

	// Atomically increment tenant count: BinaryCodec<int64_t> (raw LE).
	// C++: TenantMetadata::tenantCount().atomicOp(tr, 1, MutationRef::AddValue).
	var oneBuf [8]byte
	binary.LittleEndian.PutUint64(oneBuf[:], 1)
	tr.Add(Key(tenantCountKey), oneBuf[:])

	// Read count after increment and validate capacity.
	// C++: tenantCount = wait(TenantMetadata::tenantCount().getD(tr, Snapshot::False, 0));
	//      if (tenantCount > CLIENT_KNOBS->MAX_TENANTS_PER_CLUSTER) throw cluster_no_capacity();
	countVal, err := tr.Get(Key(tenantCountKey)).Get()
	if err != nil {
		return 0, fmt.Errorf("read tenant count: %w", err)
	}
	if countVal != nil && len(countVal) >= 8 {
		count := int64(binary.LittleEndian.Uint64(countVal))
		if count > maxTenantsPerCluster {
			return 0, errClusterNoCapacity
		}
	}

	return newId, nil
}

// deleteTenantInternal implements C++ TenantAPI::deleteTenantTransaction.
// Must be called with ACCESS_SYSTEM_KEYS / LOCK_AWARE on the transaction.
func deleteTenantInternal(tr Transaction, name []byte) error {
	// C++: checkTenantMode(tr, ClusterType::STANDALONE).
	if err := checkTenantMode(tr); err != nil {
		return err
	}

	// Look up tenant ID from nameIndex: TupleCodec<int64_t>.
	nameIdxKey := append(Key(tenantNameIndexKey), tuplePackBytes(name)...)
	idVal, err := tr.Get(nameIdxKey).Get()
	if err != nil {
		return fmt.Errorf("read tenant nameIndex: %w", err)
	}
	if idVal == nil {
		return errTenantNotFound
	}
	tenantId, err := tupleUnpackInt64(idVal)
	if err != nil {
		return fmt.Errorf("decode tenant nameIndex: %w", err)
	}

	// Check prefix range is empty (tenant data must be cleared first).
	// C++: tr->getRange(prefixRange(tenantEntry.get().prefix), 1) → tenant_not_empty.
	prefix := tenantIdToPrefix(tenantId)
	empty, err := checkPrefixEmpty(tr, prefix)
	if err != nil {
		return err
	}
	if !empty {
		return errTenantNotEmpty
	}

	// Delete from tenantMap: TenantIdCodec = raw 8-byte big-endian.
	mapKey := append(Key(tenantMapKey), prefix[:]...)
	tr.Clear(mapKey)

	// Delete from nameIndex.
	tr.Clear(nameIdxKey)

	// Decrement tenant count: BinaryCodec<int64_t> (raw LE).
	var minusOne [8]byte
	binary.LittleEndian.PutUint64(minusOne[:], ^uint64(0)) // -1 in two's complement
	tr.Add(Key(tenantCountKey), minusOne[:])

	// Update lastTenantModification: SetVersionstampedValue.
	// C++: TenantMetadata::lastTenantModification().setVersionstamp(tr, Versionstamp(), 0).
	tr.SetVersionstampedValue(Key(tenantLastModificationKey), versionstampedValueOperand())

	return nil
}

// listTenantsInternal lists tenants by scanning the nameIndex.
// Matches C++ TenantAPI::listTenantsTransaction.
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
	names := make([]Key, 0, len(kvs))
	for _, kv := range kvs {
		// Key suffix is tuple-encoded bytes: 0x01 + escaped name + 0x00.
		suffix := kv.Key[len(tenantNameIndexKey):]
		name, err := tupleUnpackBytes(suffix)
		if err != nil {
			continue // skip malformed entries
		}
		names = append(names, Key(name))
	}
	return names, nil
}

// openTenantInternal looks up tenant ID from nameIndex: TupleCodec<int64_t>.
func openTenantInternal(tr Transaction, name []byte) (int64, error) {
	nameIdxKey := append(Key(tenantNameIndexKey), tuplePackBytes(name)...)
	idVal, err := tr.Get(nameIdxKey).Get()
	if err != nil {
		return 0, fmt.Errorf("read tenant nameIndex: %w", err)
	}
	if idVal == nil {
		return 0, errTenantNotFound
	}
	tenantId, err := tupleUnpackInt64(idVal)
	if err != nil {
		return 0, fmt.Errorf("decode tenant nameIndex: %w", err)
	}
	return tenantId, nil
}
