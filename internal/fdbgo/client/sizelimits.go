package client

// Client-side key/value size limits, mirroring CLIENT_KNOBS in C++
// (fdbclient/ClientKnobs.cpp). RYW set/atomicOp throw key_too_large/value_too_large
// (ReadYourWrites.actor.cpp:2238-2241, 2345-2348), which libfdb_c keeps as the deferred
// error (ThreadSafeTransaction.cpp:589-617):
//   - set()/atomicOp() oversized key/value → the deferred error, reported by later reads
//     and the commit (deferIllegalLocked); the data never reaches the cluster.
//   - clear() oversized keys → CLAMP the range (or DROP a single-key clear),
//     exactly as C++ clear() does (NativeAPI.actor.cpp:6019-6047) — clear never
//     throws, it translates an oversized range to an equivalent smaller one.
const (
	keySizeLimit         = 10000      // CLIENT_KNOBS->KEY_SIZE_LIMIT
	systemKeySizeLimit   = 30000      // CLIENT_KNOBS->SYSTEM_KEY_SIZE_LIMIT
	valueSizeLimit       = 100000     // CLIENT_KNOBS->VALUE_SIZE_LIMIT
	tenantPrefixSize     = 8          // TenantAPI::PREFIX_SIZE (sizeof(int64_t))
	transactionSizeLimit = 10_000_000 // CLIENT_KNOBS->TRANSACTION_SIZE_LIMIT (1e7)
)

// getMaxWriteKeySize mirrors NativeAPI.actor.cpp:11629 getMaxWriteKeySize: system
// keys (\xff…) get SYSTEM_KEY_SIZE_LIMIT; others KEY_SIZE_LIMIT, plus the tenant
// prefix slack when raw access is enabled. C++ sets options.rawAccess = true for
// ANY of RAW_ACCESS, ACCESS_SYSTEM_KEYS, or READ_SYSTEM_KEYS (NativeAPI.actor.cpp:
// 7159-7170). We don't model bare RAW_ACCESS, but callers pass
// (writeSystemKeys || readSystemKeys) for the write path; clear always passes true
// (getMaxClearKeySize == getMaxKeySize == getMaxWriteKeySize(key, true)).
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
