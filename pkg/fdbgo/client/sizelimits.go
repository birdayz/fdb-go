package client

// Client-side key/value size limits, mirroring CLIENT_KNOBS in C++
// (fdbclient/ClientKnobs.cpp). The official C binding ABORTS the process when a
// write exceeds these: fdb_transaction_set/atomic_op are void and wrap the call
// in CATCH_AND_DIE (bindings/c/fdb_c.cpp), so the key_too_large/value_too_large
// thrown by ReadYourWrites.actor.cpp::set/atomicOp (:2237-2241, :2344-2348)
// terminates the process. We never abort in library code (design principle #4),
// so instead:
//   - set()/atomicOp() oversized key/value → reject the COMMIT with the same error
//     code, deferred like the other RYW write checks (our Set/Atomic are void too).
//     The oversized data never reaches the shared cluster, which is the point.
//   - clear() oversized keys → CLAMP the range (or DROP a single-key clear),
//     exactly as C++ clear() does (NativeAPI.actor.cpp:6019-6047) — clear never
//     throws, it translates an oversized range to an equivalent smaller one.
const (
	keySizeLimit       = 10000  // CLIENT_KNOBS->KEY_SIZE_LIMIT
	systemKeySizeLimit = 30000  // CLIENT_KNOBS->SYSTEM_KEY_SIZE_LIMIT
	valueSizeLimit     = 100000 // CLIENT_KNOBS->VALUE_SIZE_LIMIT
	tenantPrefixSize   = 8      // TenantAPI::PREFIX_SIZE (sizeof(int64_t))
)

// getMaxWriteKeySize mirrors NativeAPI.actor.cpp getMaxWriteKeySize: system keys
// (\xff…) get SYSTEM_KEY_SIZE_LIMIT; others KEY_SIZE_LIMIT, plus the tenant prefix
// length when raw access is enabled (the caller supplies an already-prefixed key,
// so the extra 8 bytes are allowed).
func getMaxWriteKeySize(key []byte, rawAccess bool) int {
	if len(key) > 0 && key[0] == 0xff {
		return systemKeySizeLimit
	}
	if rawAccess {
		return keySizeLimit + tenantPrefixSize
	}
	return keySizeLimit
}

// getMaxClearKeySize mirrors NativeAPI.actor.cpp getMaxClearKeySize, which is
// getMaxKeySize = getMaxWriteKeySize(key, /*hasRawAccess=*/true).
func getMaxClearKeySize(key []byte) int {
	return getMaxWriteKeySize(key, true)
}
