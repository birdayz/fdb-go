// fdb-diff-oracle — C++ serialization oracle for differential fuzzing.
//
// Reads binary-encoded requests from stdin, serializes them using FDB's
// C++ ObjectWriter, and writes the serialized bytes to stdout. The Go
// fuzz test compares Go MarshalFDB output against this oracle output.
//
// Binary protocol (stdin):
//   1 byte: message type enum
//   Then type-specific fields (see readXxx functions)
//
// Binary protocol (stdout):
//   4 bytes LE: response length (0 = error, skip this input)
//   N bytes: serialized FDB message (protocol version prefix stripped,
//            reply token zeroed)
//
// Build: inside FDB Docker image (see build.sh / BUILD.bazel)

#include "fdbclient/StorageServerInterface.h"
#include "fdbclient/CommitProxyInterface.h"
#include "fdbclient/GrvProxyInterface.h"
#include "fdbclient/ClusterInterface.h"
#include "fdbclient/CoordinationInterface.h"
#include "fdbclient/FDBTypes.h"
#include "fdbrpc/FlowTransport.h"
#include "fdbrpc/TenantInfo.h"
#include "flow/serialize.h"
#include "flow/TLSConfig.actor.h"

#include <cstdio>
#include <cstdint>
#include <cstring>
#include <string>
#include <vector>
#include <unistd.h>

static_assert(__BYTE_ORDER__ == __ORDER_LITTLE_ENDIAN__,
    "diff-oracle assumes little-endian byte order for binary protocol");

// Message type enum — must match Go's typeXxx constants.
enum MsgType : uint8_t {
    TYPE_GET_READ_VERSION_REQUEST = 0,
    TYPE_GET_VALUE_REQUEST = 1,
    TYPE_GET_KEY_REQUEST = 2,
    TYPE_GET_KEY_VALUES_REQUEST = 3,
    TYPE_GET_KEY_SERVER_LOCATIONS_REQUEST = 4,
    TYPE_COMMIT_TRANSACTION_REQUEST = 5,
    TYPE_GET_READ_VERSION_REPLY = 6,
    TYPE_GET_VALUE_REPLY = 7,
    TYPE_GET_KEY_REPLY = 8,
    TYPE_GET_KEY_VALUES_REPLY = 9,
    TYPE_GET_KEY_SERVER_LOCATIONS_REPLY = 10,
    TYPE_COMMIT_ID = 11,
    TYPE_ERROR = 12,
    TYPE_CLIENT_DB_INFO = 13,
    TYPE_OPEN_DATABASE_COORD_REQUEST = 14,
    TYPE_NETWORK_ADDRESS = 15,
    TYPE_ENDPOINT = 16,
    TYPE_REPLY_PROMISE = 17,
    TYPE_NETWORK_ADDRESS_V6 = 18,
};

// --- Buffered binary stdin reader ---

static bool readExact(uint8_t* buf, int n) {
    int got = 0;
    while (got < n) {
        ssize_t r = ::read(STDIN_FILENO, buf + got, n - got);
        if (r <= 0) return false;
        got += r;
    }
    return true;
}

static bool readU8(uint8_t& v) {
    return readExact(&v, 1);
}

static bool readU16(uint16_t& v) {
    uint8_t buf[2];
    if (!readExact(buf, 2)) return false;
    memcpy(&v, buf, 2);
    return true;
}

static bool readU32(uint32_t& v) {
    uint8_t buf[4];
    if (!readExact(buf, 4)) return false;
    memcpy(&v, buf, 4); // LE on x86
    return true;
}

static bool readI32(int32_t& v) {
    uint32_t u;
    if (!readU32(u)) return false;
    memcpy(&v, &u, sizeof(v));
    return true;
}

static bool readU64(uint64_t& v) {
    uint8_t buf[8];
    if (!readExact(buf, 8)) return false;
    memcpy(&v, buf, 8);
    return true;
}

static bool readI64(int64_t& v) {
    uint64_t u;
    if (!readU64(u)) return false;
    memcpy(&v, &u, sizeof(v));
    return true;
}

static bool readF64(double& v) {
    uint64_t u;
    if (!readU64(u)) return false;
    memcpy(&v, &u, sizeof(v));
    return true;
}

static bool readBool(bool& v) {
    uint8_t b;
    if (!readU8(b)) return false;
    v = (b != 0);
    return true;
}

static bool readBytes(std::string& out) {
    uint32_t len;
    if (!readU32(len)) return false;
    if (len > 10 * 1024 * 1024) return false; // sanity: 10MB max
    out.resize(len);
    if (len > 0 && !readExact((uint8_t*)out.data(), len)) return false;
    return true;
}

static bool readUID(UID& uid) {
    uint64_t a, b;
    if (!readU64(a)) return false;
    if (!readU64(b)) return false;
    uid = UID(a, b);
    return true;
}

// --- Binary stdout writer ---

static void writeExact(const void* data, int n) {
    const uint8_t* p = (const uint8_t*)data;
    int written = 0;
    while (written < n) {
        ssize_t w = ::write(STDOUT_FILENO, p + written, n - written);
        if (w <= 0) _exit(1);
        written += w;
    }
}

static void writeResponse(const uint8_t* data, int len) {
    uint32_t n = (uint32_t)len;
    writeExact(&n, 4);
    if (len > 0) writeExact(data, len);
}

static void writeErrorResponse() {
    uint32_t zero = 0;
    writeExact(&zero, 4);
}

// --- Serialization helpers ---

// Serialize a message, strip protocol version prefix (8 bytes), zero reply token.
// Returns the serialized bytes (without protocol version prefix).
template <class T>
std::vector<uint8_t> serializeMessage(T& msg) {
    ObjectWriter wr(IncludeVersion(currentProtocolVersion()));
    wr.serialize(T::file_identifier, msg);
    auto bytes = wr.toStringRef();

    const uint8_t* data = bytes.begin();
    int size = bytes.size();

    // Strip 8-byte protocol version prefix.
    if (size >= 8 && data[7] == 0x0F && data[6] == 0xDB) {
        data += 8;
        size -= 8;
    }

    std::vector<uint8_t> result(data, data + size);
    return result;
}

// Zero the endpoint token on a ReplyPromise.
template <class T>
void zeroReplyToken(std::vector<uint8_t>& buf, ReplyPromise<T>& rp) {
    auto& ep = rp.getEndpoint().token;
    uint8_t token[16];
    uint64_t first = ep.first(), second = ep.second();
    memcpy(token, &first, 8);
    memcpy(token + 8, &second, 8);

    // Scan for the token bytes in the buffer and zero them.
    // NOTE: This linear scan could theoretically match fuzz-generated payload
    // data that happens to equal the random 16-byte token (probability 2^-128).
    // A structural approach (computing the offset from the vtable) would be
    // more correct but requires per-type vtable knowledge. The scan is safe
    // in practice because the token is runtime-random, not fuzz-controlled.
    if (buf.size() < 16) return;
    for (size_t i = 0; i <= buf.size() - 16; i++) {
        if (memcmp(buf.data() + i, token, 16) == 0) {
            memset(buf.data() + i, 0, 16);
            return; // Only one reply token per message
        }
    }
}

// --- Request handlers ---

static bool handleGetReadVersionRequest() {
    uint32_t transactionCount, flags;
    int64_t maxVersion;
    bool hasTags, hasDebugID;
    std::string tags, debugID;

    if (!readU32(transactionCount)) return false;
    if (!readU32(flags)) return false;
    if (!readBool(hasTags)) return false;
    if (hasTags) {
        if (!readBytes(tags)) return false;
    }
    if (!readBool(hasDebugID)) return false;
    if (hasDebugID) {
        if (!readBytes(debugID)) return false;
    }
    if (!readI64(maxVersion)) return false;

    GetReadVersionRequest req;
    req.transactionCount = transactionCount;
    req.flags = flags;
    // tags: TransactionTagMap, complex structured type — skip
    // debugID: Optional<UID>, structured — skip
    req.maxVersion = maxVersion;

    auto buf = serializeMessage(req);
    zeroReplyToken(buf, req.reply);
    writeResponse(buf.data(), buf.size());
    return true;
}

static bool handleGetValueRequest() {
    std::string key;
    int64_t version, tenantId;
    bool hasTags, hasOptions;
    std::string tags, ssLatestCommitVersions;

    if (!readBytes(key)) return false;
    if (!readI64(version)) return false;
    if (!readBool(hasTags)) return false;
    if (hasTags) {
        if (!readBytes(tags)) return false;
    }
    if (!readI64(tenantId)) return false;
    if (!readBool(hasOptions)) return false;
    // If hasOptions, skip ReadOptions fields (just mark present)
    if (hasOptions) {
        // ReadOptions has: Type(i32), CacheResult(bool), LockAware(optional)
        // For simplicity, we just use default ReadOptions.
    }
    if (!readBytes(ssLatestCommitVersions)) return false;

    GetValueRequest req;
    req.key = KeyRef((uint8_t*)key.data(), key.size());
    req.version = version;
    req.tenantInfo.tenantId = tenantId;
    // hasTags: Optional<TagSet> - complex structured type, skip
    // hasOptions: Optional<ReadOptions> - skip
    // ssLatestCommitVersions: this is a VersionVector type, not raw bytes
    // in C++. The Go side treats it as raw bytes. Skip for exact match.

    auto buf = serializeMessage(req);
    zeroReplyToken(buf, req.reply);
    writeResponse(buf.data(), buf.size());
    return true;
}

static bool handleGetKeyRequest() {
    std::string selKey;
    bool selOrEqual;
    int32_t selOffset;
    int64_t version, tenantId;
    bool hasTags, hasOptions;
    std::string tags, ssLatestCommitVersions, field10;

    if (!readBytes(selKey)) return false;
    if (!readBool(selOrEqual)) return false;
    if (!readI32(selOffset)) return false;
    if (!readI64(version)) return false;
    if (!readBool(hasTags)) return false;
    if (hasTags) {
        if (!readBytes(tags)) return false;
    }
    if (!readI64(tenantId)) return false;
    if (!readBool(hasOptions)) return false;
    if (!readBytes(ssLatestCommitVersions)) return false;
    if (!readBytes(field10)) return false;

    GetKeyRequest req;
    req.sel = KeySelectorRef(KeyRef((uint8_t*)selKey.data(), selKey.size()), selOrEqual, selOffset);
    req.version = version;
    req.tenantInfo.tenantId = tenantId;

    auto buf = serializeMessage(req);
    zeroReplyToken(buf, req.reply);
    writeResponse(buf.data(), buf.size());
    return true;
}

static bool handleGetKeyValuesRequest() {
    std::string beginKey, endKey;
    bool beginOrEqual, endOrEqual;
    int32_t beginOffset, endOffset, limit, limitBytes;
    int64_t version, tenantId;
    bool hasTags, hasOptions;
    std::string tags, ssLatestCommitVersions, arena;

    if (!readBytes(beginKey)) return false;
    if (!readBool(beginOrEqual)) return false;
    if (!readI32(beginOffset)) return false;
    if (!readBytes(endKey)) return false;
    if (!readBool(endOrEqual)) return false;
    if (!readI32(endOffset)) return false;
    if (!readI64(version)) return false;
    if (!readI32(limit)) return false;
    if (!readI32(limitBytes)) return false;
    if (!readBool(hasTags)) return false;
    if (hasTags) {
        if (!readBytes(tags)) return false;
    }
    if (!readI64(tenantId)) return false;
    if (!readBool(hasOptions)) return false;
    if (!readBytes(ssLatestCommitVersions)) return false;

    GetKeyValuesRequest req;
    req.begin = KeySelectorRef(KeyRef((uint8_t*)beginKey.data(), beginKey.size()),
                                beginOrEqual, beginOffset);
    req.end = KeySelectorRef(KeyRef((uint8_t*)endKey.data(), endKey.size()),
                              endOrEqual, endOffset);
    req.version = version;
    req.limit = limit;
    req.limitBytes = limitBytes;
    req.tenantInfo.tenantId = tenantId;

    auto buf = serializeMessage(req);
    zeroReplyToken(buf, req.reply);
    writeResponse(buf.data(), buf.size());
    return true;
}

static bool handleGetKeyServerLocationsRequest() {
    std::string begin;
    bool hasEnd;
    std::string end;
    int32_t limit;
    bool reverse;
    int64_t tenantId, minTenantVersion;
    std::string arena;

    if (!readBytes(begin)) return false;
    if (!readBool(hasEnd)) return false;
    if (hasEnd) {
        if (!readBytes(end)) return false;
    }
    if (!readI32(limit)) return false;
    if (!readBool(reverse)) return false;
    if (!readI64(tenantId)) return false;
    if (!readI64(minTenantVersion)) return false;

    GetKeyServerLocationsRequest req;
    req.begin = KeyRef((uint8_t*)begin.data(), begin.size());
    if (hasEnd) {
        req.end = Optional<KeyRef>(KeyRef((uint8_t*)end.data(), end.size()));
    }
    req.limit = limit;
    req.reverse = reverse;
    req.tenant.tenantId = tenantId;
    req.minTenantVersion = minTenantVersion;

    auto buf = serializeMessage(req);
    zeroReplyToken(buf, req.reply);
    writeResponse(buf.data(), buf.size());
    return true;
}

static bool handleCommitTransactionRequest() {
    int64_t readSnapshot, tenantId;
    uint32_t numMutations, numReadConflictRanges, numWriteConflictRanges;
    uint32_t flags;
    bool hasDebugID, hasCommitCostEstimation, hasTagSet;
    std::string debugID, commitCostEstimation, tagSet, idempotencyId;

    if (!readI64(readSnapshot)) return false;
    if (!readU32(numMutations)) return false;

    Arena arena;
    CommitTransactionRequest req;
    req.transaction.read_snapshot = readSnapshot;

    // Read mutations (capped at 16)
    for (uint32_t i = 0; i < numMutations && i < 16; i++) {
        uint8_t mutType;
        std::string param1, param2;
        if (!readU8(mutType)) return false;
        if (!readBytes(param1)) return false;
        if (!readBytes(param2)) return false;
        req.transaction.mutations.push_back(
            arena,
            MutationRef((MutationRef::Type)mutType,
                        KeyRef(arena, StringRef((uint8_t*)param1.data(), param1.size())),
                        ValueRef(arena, StringRef((uint8_t*)param2.data(), param2.size()))));
    }

    // Read conflict ranges
    if (!readU32(numReadConflictRanges)) return false;
    for (uint32_t i = 0; i < numReadConflictRanges && i < 16; i++) {
        std::string b, e;
        if (!readBytes(b)) return false;
        if (!readBytes(e)) return false;
        req.transaction.read_conflict_ranges.push_back(
            arena,
            KeyRangeRef(KeyRef(arena, StringRef((uint8_t*)b.data(), b.size())),
                        KeyRef(arena, StringRef((uint8_t*)e.data(), e.size()))));
    }

    if (!readU32(numWriteConflictRanges)) return false;
    for (uint32_t i = 0; i < numWriteConflictRanges && i < 16; i++) {
        std::string b, e;
        if (!readBytes(b)) return false;
        if (!readBytes(e)) return false;
        req.transaction.write_conflict_ranges.push_back(
            arena,
            KeyRangeRef(KeyRef(arena, StringRef((uint8_t*)b.data(), b.size())),
                        KeyRef(arena, StringRef((uint8_t*)e.data(), e.size()))));
    }

    if (!readU32(flags)) return false;
    req.flags = flags;

    if (!readBool(hasDebugID)) return false;
    if (hasDebugID) {
        if (!readBytes(debugID)) return false;
        // Optional<UID> - structured, skip setting
    }

    if (!readBool(hasCommitCostEstimation)) return false;
    if (hasCommitCostEstimation) {
        if (!readBytes(commitCostEstimation)) return false;
    }

    if (!readBool(hasTagSet)) return false;
    if (hasTagSet) {
        if (!readBytes(tagSet)) return false;
    }

    if (!readI64(tenantId)) return false;
    req.tenantInfo.tenantId = tenantId;

    if (!readBytes(idempotencyId)) return false;
    // IdempotencyIdRef: skip — complex Standalone<StringRef> wrapper

    req.arena = arena;

    auto buf = serializeMessage(req);
    zeroReplyToken(buf, req.reply);
    writeResponse(buf.data(), buf.size());
    return true;
}

static bool handleGetReadVersionReply() {
    int32_t processBusyTime;
    int64_t version;
    bool locked;
    bool hasMetadataVersion;
    std::string metadataVersion;
    std::string tagThrottleInfo;
    int64_t midShardSize;
    bool rkDefaultThrottled, rkBatchThrottled;
    std::string ssVersionVectorDelta;
    UID proxyId;
    double proxyTagThrottledDuration;

    if (!readI32(processBusyTime)) return false;
    if (!readI64(version)) return false;
    if (!readBool(locked)) return false;
    if (!readBool(hasMetadataVersion)) return false;
    if (hasMetadataVersion) {
        if (!readBytes(metadataVersion)) return false;
    }
    if (!readBytes(tagThrottleInfo)) return false;
    if (!readI64(midShardSize)) return false;
    if (!readBool(rkDefaultThrottled)) return false;
    if (!readBool(rkBatchThrottled)) return false;
    if (!readBytes(ssVersionVectorDelta)) return false;
    if (!readUID(proxyId)) return false;
    if (!readF64(proxyTagThrottledDuration)) return false;

    GetReadVersionReply rep;
    rep.processBusyTime = processBusyTime;
    rep.version = version;
    rep.locked = locked;
    if (hasMetadataVersion) {
        rep.metadataVersion = Key(StringRef((uint8_t*)metadataVersion.data(), metadataVersion.size()));
    }
    // tagThrottleInfo: structured, skip for raw bytes mismatch
    rep.midShardSize = midShardSize;
    rep.rkDefaultThrottled = rkDefaultThrottled;
    rep.rkBatchThrottled = rkBatchThrottled;
    // ssVersionVectorDelta: structured VersionVector, skip
    rep.proxyId = proxyId;
    rep.proxyTagThrottledDuration = proxyTagThrottledDuration;

    auto buf = serializeMessage(rep);
    writeResponse(buf.data(), buf.size());
    return true;
}

static bool handleGetValueReply() {
    double penalty;
    bool hasError, hasValue, cached;
    std::string error, value;

    if (!readF64(penalty)) return false;
    if (!readBool(hasError)) return false;
    if (hasError) {
        if (!readBytes(error)) return false;
    }
    if (!readBool(hasValue)) return false;
    if (hasValue) {
        if (!readBytes(value)) return false;
    }
    if (!readBool(cached)) return false;

    GetValueReply rep;
    rep.penalty = penalty;
    if (hasError) {
        uint16_t code = 0;
        if (error.size() >= 2) memcpy(&code, error.data(), 2);
        rep.error = Error(code);
    }
    if (hasValue) {
        rep.value = Value(StringRef((uint8_t*)value.data(), value.size()));
    }
    rep.cached = cached;

    auto buf = serializeMessage(rep);
    writeResponse(buf.data(), buf.size());
    return true;
}

static bool handleGetKeyReply() {
    double penalty;
    bool hasError, cached;
    std::string error;

    if (!readF64(penalty)) return false;
    if (!readBool(hasError)) return false;
    if (hasError) {
        if (!readBytes(error)) return false;
    }
    if (!readBool(cached)) return false;

    GetKeyReply rep;
    rep.penalty = penalty;
    if (hasError) {
        uint16_t code = 0;
        if (error.size() >= 2) memcpy(&code, error.data(), 2);
        rep.error = Error(code);
    }
    rep.cached = cached;

    auto buf = serializeMessage(rep);
    writeResponse(buf.data(), buf.size());
    return true;
}

static bool handleGetKeyValuesReply() {
    double penalty;
    bool hasError;
    std::string error, data;
    int64_t version;
    bool more, cached;
    std::string arena;

    if (!readF64(penalty)) return false;
    if (!readBool(hasError)) return false;
    if (hasError) {
        if (!readBytes(error)) return false;
    }
    if (!readBytes(data)) return false;
    if (!readI64(version)) return false;
    if (!readBool(more)) return false;
    if (!readBool(cached)) return false;

    GetKeyValuesReply rep;
    rep.penalty = penalty;
    if (hasError) {
        uint16_t code = 0;
        if (error.size() >= 2) memcpy(&code, error.data(), 2);
        rep.error = Error(code);
    }
    // Data: VectorRef<KeyValueRef, VecSerStrategy::String> — complex nested type.
    // We pass through the raw bytes from Go but can't construct the C++ type.
    // The Go side treats it as []byte, which matches the VecSerStrategy::String
    // serialization (raw byte blob). Leave empty for now.
    rep.version = version;
    rep.more = more;
    rep.cached = cached;

    auto buf = serializeMessage(rep);
    writeResponse(buf.data(), buf.size());
    return true;
}

static bool handleGetKeyServerLocationsReply() {
    std::string results, resultsTssMapping, resultsTagMapping, arena;

    if (!readBytes(results)) return false;
    if (!readBytes(resultsTssMapping)) return false;
    if (!readBytes(resultsTagMapping)) return false;

    GetKeyServerLocationsReply rep;
    // All fields are structured vectors, not raw bytes in C++
    // Skip setting - just use defaults to test structure/scaffolding

    auto buf = serializeMessage(rep);
    writeResponse(buf.data(), buf.size());
    return true;
}

static bool handleCommitID() {
    int64_t version;
    uint16_t txnBatchId;
    bool hasMetadataVersion, hasConflictingKRIndices;
    std::string metadataVersion, conflictingKRIndices;

    if (!readI64(version)) return false;
    if (!readU16(txnBatchId)) return false;
    if (!readBool(hasMetadataVersion)) return false;
    if (hasMetadataVersion) {
        if (!readBytes(metadataVersion)) return false;
    }
    if (!readBool(hasConflictingKRIndices)) return false;
    if (hasConflictingKRIndices) {
        if (!readBytes(conflictingKRIndices)) return false;
    }

    CommitID rep;
    rep.version = version;
    rep.txnBatchId = txnBatchId;
    if (hasMetadataVersion) {
        rep.metadataVersion = Value(StringRef((uint8_t*)metadataVersion.data(), metadataVersion.size()));
    }
    // conflictingKRIndices: Optional<VectorRef<int>> complex, skip

    auto buf = serializeMessage(rep);
    writeResponse(buf.data(), buf.size());
    return true;
}

static bool handleError() {
    uint16_t errorCode;
    if (!readU16(errorCode)) return false;

    Error err = Error::fromCode(errorCode);

    auto buf = serializeMessage(err);
    writeResponse(buf.data(), buf.size());
    return true;
}

static bool handleClientDBInfo() {
    std::string grvProxies, commitProxies;
    UID id;
    bool hasForward;
    std::string forward, history;
    bool hasEncryptKeyProxy;
    std::string encryptKeyProxy;
    UID clusterId;
    int32_t clusterType;
    bool hasMetaclusterName;
    std::string metaclusterName;

    if (!readBytes(grvProxies)) return false;
    if (!readBytes(commitProxies)) return false;
    if (!readUID(id)) return false;
    if (!readBool(hasForward)) return false;
    if (hasForward) {
        if (!readBytes(forward)) return false;
    }
    if (!readBytes(history)) return false;
    if (!readBool(hasEncryptKeyProxy)) return false;
    if (hasEncryptKeyProxy) {
        if (!readBytes(encryptKeyProxy)) return false;
    }
    if (!readUID(clusterId)) return false;
    if (!readI32(clusterType)) return false;
    if (!readBool(hasMetaclusterName)) return false;
    if (hasMetaclusterName) {
        if (!readBytes(metaclusterName)) return false;
    }

    ClientDBInfo info;
    info.id = id;
    if (hasForward) {
        info.forward = StringRef((uint8_t*)forward.data(), forward.size());
    }
    info.clusterId = clusterId;
    info.clusterType = static_cast<decltype(info.clusterType)>(clusterType);
    if (hasMetaclusterName) {
        info.metaclusterName = Standalone<StringRef>(StringRef((uint8_t*)metaclusterName.data(), metaclusterName.size()));
    }
    // grvProxies, commitProxies, history, encryptKeyProxy: structured vectors, skip

    auto buf = serializeMessage(info);
    writeResponse(buf.data(), buf.size());
    return true;
}

static bool handleOpenDatabaseRequest() {
    std::string issues, supportedVersions, traceLogGroup;
    UID knownClientInfoID;
    std::string clusterKey, coordinators, hostnames;
    bool internal;

    if (!readBytes(issues)) return false;
    if (!readBytes(supportedVersions)) return false;
    if (!readBytes(traceLogGroup)) return false;
    if (!readUID(knownClientInfoID)) return false;
    if (!readBytes(clusterKey)) return false;
    if (!readBytes(coordinators)) return false;
    if (!readBytes(hostnames)) return false;
    if (!readBool(internal)) return false;

    OpenDatabaseCoordRequest req;
    req.knownClientInfoID = knownClientInfoID;
    req.internal = internal;
    // issues, supportedVersions, coordinators, hostnames: structured vectors, skip

    auto buf = serializeMessage(req);
    zeroReplyToken(buf, req.reply);
    writeResponse(buf.data(), buf.size());
    return true;
}

static bool handleNetworkAddress() {
    uint32_t ipAddr;
    uint16_t port, flags;
    bool fromHostname;

    if (!readU32(ipAddr)) return false;
    if (!readU16(port)) return false;
    if (!readU16(flags)) return false;
    if (!readBool(fromHostname)) return false;

    // Set fields DIRECTLY — NOT NetworkAddress(ip, port, flags, fromHostname): that 4-arg
    // form binds the (IPAddress, port, bool isPublic, bool isTLS, fromHostname=False) ctor,
    // so `flags`→isPublic and `fromHostname`→isTLS, and the on-wire flags become
    // (isPublic?0:FLAG_PRIVATE)|(isTLS?FLAG_TLS:0) — NOT the requested flags. The oracle
    // must serialize the EXACT (ip, port, flags, fromHostname) it was given.
    NetworkAddress addr;
    addr.ip = IPAddress(ipAddr);
    addr.port = port;
    addr.flags = flags;
    addr.fromHostname = fromHostname;

    auto buf = serializeMessage(addr);
    writeResponse(buf.data(), buf.size());
    return true;
}

static bool handleNetworkAddressV6() {
    std::string v6;
    uint16_t port, flags;
    bool fromHostname;

    if (!readBytes(v6)) return false;
    if (v6.size() != 16) return false;
    if (!readU16(port)) return false;
    if (!readU16(flags)) return false;
    if (!readBool(fromHostname)) return false;

    // IPv6 NetworkAddress: build an IPAddress from the 16-byte store, then set the
    // NetworkAddress fields directly (the 4-arg ctor misbinds flags→isPublic — see
    // handleNetworkAddress). This exercises the Go IPAddress variant tag=2 (IPv6) marshal:
    // a count-prefixed 16-byte vector behind the union RelativeOffset.
    IPAddress::IPAddressStore store;
    for (size_t i = 0; i < 16; i++)
        store[i] = (uint8_t)v6[i];

    NetworkAddress addr;
    addr.ip = IPAddress(store);
    addr.port = port;
    addr.flags = flags;
    addr.fromHostname = fromHostname;

    auto buf = serializeMessage(addr);
    writeResponse(buf.data(), buf.size());
    return true;
}

static bool handleEndpoint() {
    // Addresses: treat as NetworkAddress for primary, no secondary
    uint32_t ipAddr;
    uint16_t port, flags;
    bool fromHostname;
    UID token;

    if (!readU32(ipAddr)) return false;
    if (!readU16(port)) return false;
    if (!readU16(flags)) return false;
    if (!readBool(fromHostname)) return false;
    if (!readUID(token)) return false;

    // Construct Endpoint by setting fields directly (avoids choosePrimaryAddress
    // which depends on g_network local address TLS state). Set the NetworkAddress fields
    // directly too — the 4-arg NetworkAddress(ip, port, flags, fromHostname) ctor misbinds
    // flags→isPublic / fromHostname→isTLS (see handleNetworkAddress).
    Endpoint ep;
    NetworkAddress na;
    na.ip = IPAddress(ipAddr);
    na.port = port;
    na.flags = flags;
    na.fromHostname = fromHostname;
    ep.addresses.address = na;
    ep.token = token;

    auto buf = serializeMessage(ep);
    writeResponse(buf.data(), buf.size());
    return true;
}

static bool handleReplyPromise() {
    UID token;
    if (!readUID(token)) return false;

    // ReplyPromise in C++ always gets a random token from FlowTransport.
    // We can't control it, and we zero it post-serialization.
    // So test with zero token on both sides.
    ReplyPromise<Void> rp;

    auto buf = serializeMessage(rp);
    zeroReplyToken(buf, rp);
    writeResponse(buf.data(), buf.size());
    return true;
}

// --- Main loop ---

int main() {
    // Initialize FDB networking (required for FlowTransport / ReplyPromise).
    TLSConfig tlsConfig;
    g_network = newNet2(tlsConfig, false, false);
    FlowTransport::createInstance(false, 1, WLTOKEN_FIRST_AVAILABLE, nullptr);

    // Process requests in a loop.
    while (true) {
        uint8_t msgType;
        if (!readU8(msgType)) break; // EOF

        bool ok;
        switch (msgType) {
        case TYPE_GET_READ_VERSION_REQUEST:
            ok = handleGetReadVersionRequest();
            break;
        case TYPE_GET_VALUE_REQUEST:
            ok = handleGetValueRequest();
            break;
        case TYPE_GET_KEY_REQUEST:
            ok = handleGetKeyRequest();
            break;
        case TYPE_GET_KEY_VALUES_REQUEST:
            ok = handleGetKeyValuesRequest();
            break;
        case TYPE_GET_KEY_SERVER_LOCATIONS_REQUEST:
            ok = handleGetKeyServerLocationsRequest();
            break;
        case TYPE_COMMIT_TRANSACTION_REQUEST:
            ok = handleCommitTransactionRequest();
            break;
        case TYPE_GET_READ_VERSION_REPLY:
            ok = handleGetReadVersionReply();
            break;
        case TYPE_GET_VALUE_REPLY:
            ok = handleGetValueReply();
            break;
        case TYPE_GET_KEY_REPLY:
            ok = handleGetKeyReply();
            break;
        case TYPE_GET_KEY_VALUES_REPLY:
            ok = handleGetKeyValuesReply();
            break;
        case TYPE_GET_KEY_SERVER_LOCATIONS_REPLY:
            ok = handleGetKeyServerLocationsReply();
            break;
        case TYPE_COMMIT_ID:
            ok = handleCommitID();
            break;
        case TYPE_ERROR:
            ok = handleError();
            break;
        case TYPE_CLIENT_DB_INFO:
            ok = handleClientDBInfo();
            break;
        case TYPE_OPEN_DATABASE_COORD_REQUEST:
            ok = handleOpenDatabaseRequest();
            break;
        case TYPE_NETWORK_ADDRESS:
            ok = handleNetworkAddress();
            break;
        case TYPE_NETWORK_ADDRESS_V6:
            ok = handleNetworkAddressV6();
            break;
        case TYPE_ENDPOINT:
            ok = handleEndpoint();
            break;
        case TYPE_REPLY_PROMISE:
            ok = handleReplyPromise();
            break;
        default:
            ok = false;
            break;
        }

        if (!ok) {
            writeErrorResponse();
        }
    }

    return 0;
}
