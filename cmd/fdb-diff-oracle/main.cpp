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

// Message type enum — must match Go's typeXxx constants.
enum MsgType : uint8_t {
    TYPE_GET_READ_VERSION_REQUEST = 0,
    TYPE_GET_VALUE_REQUEST = 1,
    TYPE_GET_KEY_REQUEST = 2,
    TYPE_GET_KEY_VALUES_REQUEST = 3,
    TYPE_GET_KEY_SERVER_LOCATIONS_REQUEST = 4,
    TYPE_COMMIT_TRANSACTION_REQUEST = 5,
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

static bool readU32(uint32_t& v) {
    uint8_t buf[4];
    if (!readExact(buf, 4)) return false;
    memcpy(&v, buf, 4); // LE on x86
    return true;
}

static bool readI32(int32_t& v) {
    return readU32(*(uint32_t*)&v);
}

static bool readU64(uint64_t& v) {
    uint8_t buf[8];
    if (!readExact(buf, 8)) return false;
    memcpy(&v, buf, 8);
    return true;
}

static bool readI64(int64_t& v) {
    return readU64(*(uint64_t*)&v);
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

    // Zero the reply token (16 bytes inside the Endpoint's UID).
    // The ReplyPromise contains an Endpoint which has a UID token.
    // We can't control it from C++, so we zero it after serialization.
    // The token is located by scanning for the Endpoint vtable pattern.
    // However, simpler approach: we zero ALL 16-byte UIDs that look like
    // Endpoint tokens. The Endpoint vtable is {6, 20, 4} with a UID at
    // offset 4 in the object (20 bytes: 4 soffset + 16 UID).
    //
    // Actually, simplest: the reply token position is deterministic per type.
    // We find it by looking at the serialized bytes for a non-zero 16-byte
    // region at the known offset within the ReplyPromise object.
    //
    // Even simpler: we set the reply token to zero BEFORE serialization.
    // Can we do that? The ReplyPromise is initialized by C++ FlowTransport
    // with a random token. We need to zero it on the instance.
    //
    // This is handled by the caller: set reply.getEndpoint() token to zero
    // before calling this function. See handleXxx functions.

    return result;
}

// Zero the endpoint token on a ReplyPromise.
// This requires accessing FlowTransport internals which is tricky.
// Instead, we'll zero the token bytes post-serialization.
// The ReplyPromise object's vtable is [6, 20, 4].
// vtable_size=6, obj_size=20. The object has:
//   bytes 0-3: soffset (to vtable)
//   bytes 4-19: UID token (16 bytes)
// So we need to find where the ReplyPromise object lives in the buffer.
//
// Alternative: find the token bytes by checking what token the runtime assigned,
// then zero those specific bytes in the output.
template <class T>
void zeroReplyToken(std::vector<uint8_t>& buf, ReplyPromise<T>& rp) {
    auto& ep = rp.getEndpoint().token;
    uint8_t token[16];
    uint64_t first = ep.first(), second = ep.second();
    memcpy(token, &first, 8);
    memcpy(token + 8, &second, 8);

    // Scan for the token bytes in the buffer and zero them.
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
    uint32_t flags, transactionCount;
    int64_t maxVersion;
    if (!readU32(flags)) return false;
    if (!readU32(transactionCount)) return false;
    if (!readI64(maxVersion)) return false;

    GetReadVersionRequest req;
    req.flags = flags;
    req.transactionCount = transactionCount;
    // maxVersion is handled via the .maxVersion field
    // C++ GetReadVersionRequest has maxVersion as a regular field.
    req.maxVersion = maxVersion;

    auto buf = serializeMessage(req);
    zeroReplyToken(buf, req.reply);
    writeResponse(buf.data(), buf.size());
    return true;
}

static bool handleGetValueRequest() {
    std::string key;
    int64_t version, tenantId;
    if (!readBytes(key)) return false;
    if (!readI64(version)) return false;
    if (!readI64(tenantId)) return false;

    GetValueRequest req;
    req.key = KeyRef((uint8_t*)key.data(), key.size());
    req.version = version;
    req.tenantInfo.tenantId = tenantId;

    auto buf = serializeMessage(req);
    zeroReplyToken(buf, req.reply);
    writeResponse(buf.data(), buf.size());
    return true;
}

static bool handleGetKeyRequest() {
    std::string key;
    bool orEqual;
    int32_t offset;
    int64_t version, tenantId;
    if (!readBytes(key)) return false;
    if (!readBool(orEqual)) return false;
    if (!readI32(offset)) return false;
    if (!readI64(version)) return false;
    if (!readI64(tenantId)) return false;

    GetKeyRequest req;
    req.sel = KeySelectorRef(KeyRef((uint8_t*)key.data(), key.size()), orEqual, offset);
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

    if (!readBytes(beginKey)) return false;
    if (!readBool(beginOrEqual)) return false;
    if (!readI32(beginOffset)) return false;
    if (!readBytes(endKey)) return false;
    if (!readBool(endOrEqual)) return false;
    if (!readI32(endOffset)) return false;
    if (!readI64(version)) return false;
    if (!readI32(limit)) return false;
    if (!readI32(limitBytes)) return false;
    if (!readI64(tenantId)) return false;

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

    if (!readI64(readSnapshot)) return false;
    if (!readI64(tenantId)) return false;
    if (!readU32(numMutations)) return false;

    Arena arena;
    CommitTransactionRequest req;
    req.transaction.read_snapshot = readSnapshot;
    req.tenantInfo.tenantId = tenantId;

    // Read mutations
    for (uint32_t i = 0; i < numMutations && i < 1000; i++) {
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
    for (uint32_t i = 0; i < numReadConflictRanges && i < 1000; i++) {
        std::string b, e;
        if (!readBytes(b)) return false;
        if (!readBytes(e)) return false;
        req.transaction.read_conflict_ranges.push_back(
            arena,
            KeyRangeRef(KeyRef(arena, StringRef((uint8_t*)b.data(), b.size())),
                        KeyRef(arena, StringRef((uint8_t*)e.data(), e.size()))));
    }

    if (!readU32(numWriteConflictRanges)) return false;
    for (uint32_t i = 0; i < numWriteConflictRanges && i < 1000; i++) {
        std::string b, e;
        if (!readBytes(b)) return false;
        if (!readBytes(e)) return false;
        req.transaction.write_conflict_ranges.push_back(
            arena,
            KeyRangeRef(KeyRef(arena, StringRef((uint8_t*)b.data(), b.size())),
                        KeyRef(arena, StringRef((uint8_t*)e.data(), e.size()))));
    }

    req.arena = arena;

    auto buf = serializeMessage(req);
    zeroReplyToken(buf, req.reply);
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
