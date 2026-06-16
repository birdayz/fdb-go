#pragma once
// extract.h — Shared infrastructure for FDB type extraction and Go code generation.
// Extracted from main.cpp to allow multiple generator backends (v4, v5, etc.)

#include "fdbclient/StorageServerInterface.h"
#include "fdbclient/CommitProxyInterface.h"
#include "fdbclient/GrvProxyInterface.h"
#include "fdbclient/CoordinationInterface.h"
#include "fdbclient/ClusterInterface.h"
#include "fdbclient/FDBTypes.h"
#include "fdbclient/GlobalConfig.h"
#include "fdbrpc/FlowTransport.h"
#include "fdbclient/Tenant.h"
#include "fdbrpc/TenantInfo.h"
#include "flow/serialize.h"
#include "flow/TLSConfig.actor.h"

#include <cstdio>
#include <cstdint>
#include <cstring>
#include <string>
#include <vector>
#include <map>
#include <algorithm>
#include <sys/stat.h>
#include <sys/wait.h>
#include <unistd.h>

// ============================================================
// 1. GoTypeName — compile-time verified type name registry
// ============================================================

template <class T>
struct GoTypeName {
    static constexpr bool registered = false;
    static const char* name() { return ""; } // Empty = not registered
};

#define REGISTER_GO_TYPE(CppType, GoName) \
    template<> struct GoTypeName<CppType> { \
        static constexpr bool registered = true; \
        static const char* name() { return GoName; } \
    }

// Register ALL nested struct types used in serialize() methods.
REGISTER_GO_TYPE(SpanContext, "SpanContext");
REGISTER_GO_TYPE(TenantInfo, "TenantInfo");
REGISTER_GO_TYPE(KeySelectorRef, "KeySelectorRef");
REGISTER_GO_TYPE(KeyRangeRef, "KeyRangeRef");
REGISTER_GO_TYPE(MutationRef, "MutationRef");
REGISTER_GO_TYPE(CommitTransactionRef, "CommitTransactionRef");
REGISTER_GO_TYPE(ReadOptions, "ReadOptions");
REGISTER_GO_TYPE(StorageMetrics, "StorageMetrics");
REGISTER_GO_TYPE(TenantMapEntry, "TenantMapEntry");
REGISTER_GO_TYPE(SplitRangeReply, "SplitRangeReply");
REGISTER_GO_TYPE(NetworkAddress, "NetworkAddress");
REGISTER_GO_TYPE(NetworkAddressList, "NetworkAddressList");
REGISTER_GO_TYPE(IPAddress, "IPAddress");
REGISTER_GO_TYPE(Endpoint, "Endpoint");

// ReplyPromise<T> — all instantiations share same vtable.
REGISTER_GO_TYPE(ReplyPromise<GetValueReply>, "ReplyPromise");
REGISTER_GO_TYPE(ReplyPromise<GetKeyValuesReply>, "ReplyPromise");
REGISTER_GO_TYPE(ReplyPromise<GetKeyReply>, "ReplyPromise");
REGISTER_GO_TYPE(ReplyPromise<GetReadVersionReply>, "ReplyPromise");
REGISTER_GO_TYPE(ReplyPromise<GetKeyServerLocationsReply>, "ReplyPromise");
REGISTER_GO_TYPE(ReplyPromise<CommitID>, "ReplyPromise");
REGISTER_GO_TYPE(ReplyPromise<Void>, "ReplyPromise");
REGISTER_GO_TYPE(ReplyPromise<CachedSerialization<ClientDBInfo>>, "ReplyPromise");
REGISTER_GO_TYPE(ReplyPromise<StorageMetrics>, "ReplyPromise");
REGISTER_GO_TYPE(ReplyPromise<SplitRangeReply>, "ReplyPromise");

// ============================================================
// 2. FieldNames — explicit per-type, indexed by field position
// ============================================================

template <class T>
struct FieldNames {
    static const char* get(int index) { return nullptr; } // No names = use field_N
};

#define REGISTER_FIELD_NAMES(CppType, ...) \
    template<> struct FieldNames<CppType> { \
        static const char* get(int index) { \
            static const char* names[] = { __VA_ARGS__ }; \
            int n = sizeof(names)/sizeof(names[0]); \
            return (index < n) ? names[index] : nullptr; \
        } \
    }

// Field names for all extracted types.
REGISTER_FIELD_NAMES(GetValueRequest, "key", "version", "tags", "reply", "spanContext", "tenantInfo", "options", "ssLatestCommitVersions");
REGISTER_FIELD_NAMES(GetValueReply, "penalty", "error", "value", "cached");
REGISTER_FIELD_NAMES(GetKeyValuesRequest, "begin", "end", "version", "limit", "limitBytes", "tags", "reply", "spanContext", "tenantInfo", "options", "ssLatestCommitVersions", "arena");
REGISTER_FIELD_NAMES(GetKeyValuesReply, "penalty", "error", "data", "version", "more", "cached", "arena");
REGISTER_FIELD_NAMES(GetKeyRequest, "sel", "version", "tags", "reply", "spanContext", "tenantInfo", "options", "ssLatestCommitVersions");
REGISTER_FIELD_NAMES(GetKeyReply, "penalty", "error", "sel", "cached");
REGISTER_FIELD_NAMES(GetReadVersionRequest, "transactionCount", "flags", "tags", "debugID", "reply", "spanContext", "maxVersion");
REGISTER_FIELD_NAMES(GetReadVersionReply, "processBusyTime", "version", "locked", "metadataVersion", "tagThrottleInfo", "midShardSize", "rkDefaultThrottled", "rkBatchThrottled", "ssVersionVectorDelta", "proxyId", "proxyTagThrottledDuration");
REGISTER_FIELD_NAMES(GetKeyServerLocationsRequest, "begin", "end", "limit", "reverse", "reply", "spanContext", "tenant", "minTenantVersion", "arena");
REGISTER_FIELD_NAMES(GetKeyServerLocationsReply, "results", "resultsTssMapping", "resultsTagMapping", "arena");
REGISTER_FIELD_NAMES(CommitTransactionRequest, "transaction", "reply", "flags", "debugID", "commitCostEstimation", "tagSet", "spanContext", "tenantInfo", "idempotencyId", "arena");
REGISTER_FIELD_NAMES(CommitID, "version", "txnBatchId", "metadataVersion", "conflictingKRIndices");
REGISTER_FIELD_NAMES(OpenDatabaseCoordRequest, "issues", "supportedVersions", "traceLogGroup", "knownClientInfoID", "clusterKey", "coordinators", "reply", "hostnames", "internal");
REGISTER_FIELD_NAMES(CommitTransactionRef, "readConflictRanges", "writeConflictRanges", "mutations", "readSnapshot", "report_conflicting_keys", "lock_aware", "read_conflict_ranges_disabled", "write_conflict_ranges_disabled");
REGISTER_FIELD_NAMES(KeyValueRef, "key", "value");
REGISTER_FIELD_NAMES(TenantInfo, "tenantId", "token", "arena");
REGISTER_FIELD_NAMES(MutationRef, "mutType", "param1", "param2");
REGISTER_FIELD_NAMES(Error, "errorCode");
REGISTER_FIELD_NAMES(NetworkAddress, "ip", "port", "flags", "fromHostname");
REGISTER_FIELD_NAMES(Endpoint, "addresses", "token");
REGISTER_FIELD_NAMES(ReplyPromise<GetValueReply>, "token");
REGISTER_FIELD_NAMES(NetworkAddressList, "address", "secondaryAddress");
REGISTER_FIELD_NAMES(IPAddress, "addr");
REGISTER_FIELD_NAMES(KeyRangeRef, "begin", "end");
REGISTER_FIELD_NAMES(KeySelectorRef, "key", "orEqual", "offset");
REGISTER_FIELD_NAMES(SpanContext, "traceID", "spanID", "flags");
REGISTER_FIELD_NAMES(ReadOptions, "type", "cacheResult", "lockAware");
REGISTER_FIELD_NAMES(StorageMetrics, "bytes", "bytesWrittenPerKSecond", "iosPerKSecond", "bytesReadPerKSecond", "opsReadPerKSecond");
REGISTER_FIELD_NAMES(TenantMapEntry, "id", "tenantName", "tenantLockState", "tenantLockId", "tenantGroup", "configurationSequenceNum");
REGISTER_FIELD_NAMES(WaitMetricsRequest, "keys", "min", "max", "reply", "tenantInfo", "minVersion");
REGISTER_FIELD_NAMES(SplitRangeRequest, "keys", "chunkSize", "reply", "tenantInfo");
REGISTER_FIELD_NAMES(SplitRangeReply, "splitPoints");
REGISTER_FIELD_NAMES(ClientDBInfo, "grvProxies", "commitProxies", "id", "forward", "history", "tenantMode", "encryptKeyProxy", "clusterId", "clusterType", "metaclusterName");
REGISTER_FIELD_NAMES(GrvProxyInterface, "processId", "provisional", "getConsistentReadVersion");
REGISTER_FIELD_NAMES(CommitProxyInterface, "processId", "provisional", "commit");
REGISTER_FIELD_NAMES(StorageServerInterface, "watchValue");

// LocationPair — registered via template specialization (macro can't handle template commas).
using LocationPair = std::pair<KeyRangeRef, std::vector<StorageServerInterface>>;
template<> struct GoTypeName<LocationPair> {
    static constexpr bool registered = true;
    static const char* name() { return "LocationPair"; }
};
template<> struct FieldNames<LocationPair> {
    static const char* get(int index) {
        static const char* names[] = { "keyRange", "servers" };
        return (index < 2) ? names[index] : nullptr;
    }
};

// ============================================================
// 3. Field Classification — compile-time from FDB traits
// ============================================================

enum class FieldKind { Scalar, DynamicSize, VectorLike, VectorOfStruct, Optional, NestedStruct, Variant };

struct ScalarInfo {
    const char* goType;
    const char* reader;
    const char* writer;
};

template <class T> ScalarInfo scalarInfoFor() {
    if constexpr (std::is_same_v<T, bool>)     return {"bool", "ReadBool", "WriteBool"};
    else if constexpr (std::is_same_v<T, int8_t>)   return {"int8", "ReadInt8", "WriteInt8"};
    else if constexpr (std::is_same_v<T, uint8_t>)  return {"uint8", "ReadUint8", "WriteUint8"};
    else if constexpr (std::is_same_v<T, int16_t>)  return {"int16", "ReadInt16", "WriteInt16"};
    else if constexpr (std::is_same_v<T, uint16_t>) return {"uint16", "ReadUint16", "WriteUint16"};
    else if constexpr (std::is_same_v<T, int32_t>)  return {"int32", "ReadInt32", "WriteInt32"};
    else if constexpr (std::is_same_v<T, uint32_t>) return {"uint32", "ReadUint32", "WriteUint32"};
    else if constexpr (std::is_same_v<T, int64_t>)  return {"int64", "ReadInt64", "WriteInt64"};
    else if constexpr (std::is_same_v<T, uint64_t>) return {"uint64", "ReadUint64", "WriteUint64"};
    else if constexpr (std::is_same_v<T, double>)   return {"float64", "ReadFloat64", "WriteFloat64"};
    else if constexpr (std::is_same_v<T, UID>)      return {"[16]byte", "ReadUID", "WriteUID"};
    // Enums: resolve underlying type.
    else if constexpr (std::is_enum_v<T>)       return scalarInfoFor<std::underlying_type_t<T>>();
    // Size-based fallback for unknown scalars.
    else if constexpr (detail::fb_size<T> == 1)  return {"uint8", "ReadUint8", "WriteUint8"};
    else if constexpr (detail::fb_size<T> == 2)  return {"uint16", "ReadUint16", "WriteUint16"};
    else if constexpr (detail::fb_size<T> == 4)  return {"int32", "ReadInt32", "WriteInt32"};
    else if constexpr (detail::fb_size<T> == 8)  return {"int64", "ReadInt64", "WriteInt64"};
    else return {"[]byte", "ReadBytes", "WriteBytes"};
}

// Standalone<T> detection.
template <class T> struct is_standalone : std::false_type {};
template <class T> struct is_standalone<Standalone<T>> : std::true_type {};

// std::variant detection.
template <class T> struct is_std_variant : std::false_type {};
template <class... Ts> struct is_std_variant<std::variant<Ts...>> : std::true_type {};

// Variant alternative info.
struct VariantAlt {
    const char* goType;
    const char* reader;
    FieldKind kind;
    int size;
};

template <class T>
VariantAlt makeVariantAlt() {
    using namespace detail;
    if constexpr (is_scalar<T>) {
        auto si = scalarInfoFor<T>();
        return {si.goType, si.reader, FieldKind::Scalar, (int)fb_size<T>};
    } else if constexpr (is_vector_like<T>) {
        return {"[]byte", "ReadBytes", FieldKind::VectorLike, (int)fb_size<T>};
    } else {
        return {"[]byte", "ReadBytes", FieldKind::DynamicSize, (int)fb_size<T>};
    }
}

template <class... Ts>
std::vector<VariantAlt> extractVariantAlts(std::variant<Ts...>*) {
    return { makeVariantAlt<Ts>()... };
}

template <class T>
std::vector<VariantAlt> getVariantAlts() {
    if constexpr (is_std_variant<T>::value) {
        return extractVariantAlts((T*)nullptr);
    }
    return {};
}

// Detect element type of VectorRef<T, S>.
template <class T> struct VectorElementGoType {
    static constexpr bool registered = false;
    static const char* name() { return ""; }
};
template <class T, VecSerStrategy S> struct VectorElementGoType<VectorRef<T, S>> {
    static constexpr bool registered = GoTypeName<T>::registered;
    static const char* name() { return GoTypeName<T>::name(); }
};

// Classify a field type into FieldKind.
template <class T>
FieldKind classifyField() {
    using namespace detail;
    if constexpr (is_scalar<T>) return FieldKind::Scalar;
    else if constexpr (is_dynamic_size<T>) return FieldKind::DynamicSize;
    else if constexpr (is_standalone<T>::value) {
        using Inner = typename T::RefType;
        if constexpr (is_vector_like<Inner>) {
            if constexpr (VectorElementGoType<Inner>::registered)
                return FieldKind::VectorOfStruct;
            return FieldKind::VectorLike;
        }
        else if constexpr (is_dynamic_size<Inner>) return FieldKind::DynamicSize;
        else return FieldKind::NestedStruct;
    }
    else if constexpr (is_vector_like<T>) {
        if constexpr (VectorElementGoType<T>::registered)
            return FieldKind::VectorOfStruct;
        return FieldKind::VectorLike;
    }
    else if constexpr (is_union_like<T>) {
        if constexpr (is_std_variant<T>::value) return FieldKind::Variant;
        else return FieldKind::Optional;
    }
    else return FieldKind::NestedStruct;
}

// Get the Go type name for the inner type of Optional<T>, if it's a serializable struct.
// Returns "" (empty) for bytes/scalar Optionals (e.g. Optional<KeyRef>).
// Returns the registered Go type name for struct Optionals (e.g. Optional<ReadOptions> → "ReadOptions").
// Detect Optional<T> where T is a registered Go struct type.
// Uses explicit specializations to avoid template metaprogramming issues.
template <class T> const char* optionalInnerGoType() { return ""; }
template <> inline const char* optionalInnerGoType<Optional<ReadOptions>>() { return "ReadOptions"; }
// RFC-115 §6: Optional<Error> (the inline LoadBalancedReply.error on read replies) is a
// flatbuffers UNION — a 1-byte present tag + a RelativeOffset to a nested Error table —
// NOT Optional<bytes>. Without this specialization the writer emitted a length-prefixed
// byte vector (mis-marshal); registering Error as the struct inner makes the generated
// writer emit the nested-Error-table-via-offset that the reader (wire.ReadInlineReplyError)
// and C++ expect. Error has uint16 errorCode at slot 0 (REGISTER_FIELD_NAMES(Error,...)).
template <> inline const char* optionalInnerGoType<Optional<Error>>() { return "Error"; }

// ============================================================
// 4. FieldDesc + FieldCollector
// ============================================================

struct FieldDesc {
    const char* name;
    FieldKind kind;
    ScalarInfo scalar;
    const char* nestedGoType;
    const char* elementGoType;  // For VectorOfStruct: the element's Go type name
    std::vector<VariantAlt> variantAlts;
    int vtableSlot;
    uint32_t size;
};

template <class ParentT>
struct FieldCollector {
    static constexpr bool isDeserializing = false;
    static constexpr bool isSerializing = false;
    static constexpr bool is_fb_visitor = true;

    std::vector<FieldDesc> fields;
    std::vector<uint16_t> vtable;
    int fieldIndex = 0;
    int slotIndex = 0;

    FieldCollector& context() { return *this; }
    ProtocolVersion protocolVersion() const { return currentProtocolVersion(); }

    template <class... Members>
    void operator()(Members&... members) {
        const auto* vt = detail::get_vtable<std::decay_t<Members>...>();
        vtable.assign(vt->begin(), vt->end());
        (pushField<std::decay_t<Members>>(), ...);
    }

private:
    template <class T>
    void pushField() {
        using namespace detail;
        FieldDesc fd{};
        fd.vtableSlot = slotIndex;
        fd.name = FieldNames<ParentT>::get(fieldIndex);
        fd.size = (uint32_t)fb_size<T>;
        fd.kind = classifyField<T>();

        switch (fd.kind) {
        case FieldKind::Scalar:
            fd.scalar = scalarInfoFor<T>();
            break;
        case FieldKind::NestedStruct:
            fd.nestedGoType = GoTypeName<T>::name();
            break;
        case FieldKind::Variant:
            fd.variantAlts = getVariantAlts<T>();
            break;
        case FieldKind::VectorOfStruct:
            if constexpr (is_standalone<T>::value) {
                using Inner = typename T::RefType;
                fd.elementGoType = VectorElementGoType<Inner>::name();
            } else {
                fd.elementGoType = VectorElementGoType<T>::name();
            }
            break;
        case FieldKind::Optional:
            // Check if inner type is a struct (has serialize method).
            // C++ Optional<T> via union_like_traits: alternatives = pack<T>.
            // If T is expect_serialize_member, it's serialized as EnsureTable<T> (nested object).
            fd.nestedGoType = optionalInnerGoType<T>();
            break;
        default:
            break;
        }

        fields.push_back(fd);
        fieldIndex++;
        slotIndex += (fd.kind == FieldKind::Optional) ? 2 : 1;
    }
};

// ============================================================
// 5. Helper functions
// ============================================================

static std::string sanitize(const char* s) {
    if (!s || !s[0]) return "";
    std::string r(s);
    // Strip v. prefix, :: prefix, const_cast wrapper.
    auto dot = r.rfind('.');
    if (dot != std::string::npos) r = r.substr(dot + 1);
    auto col = r.rfind("::");
    if (col != std::string::npos) r = r.substr(col + 2);
    r[0] = toupper(r[0]);
    return r;
}

static std::string safeParam(const std::string& goName) {
    std::string p = goName;
    if (!p.empty()) p[0] = tolower(p[0]);
    if (p == "type" || p == "range" || p == "error" || p == "func" || p == "map" || p == "select")
        p += "_";
    return p;
}

static std::string fieldGoName(const FieldDesc& fd) {
    if (fd.name) return sanitize(fd.name);
    return "Field_" + std::to_string(fd.vtableSlot);
}

// ============================================================
// 6. VTable extraction helpers
// ============================================================

inline std::vector<std::vector<uint16_t>> extractVTableClosure(const uint8_t* data, int size) {
    std::vector<std::vector<uint16_t>> result;
    if (size < 16) return result;
    int off = 0;
    if (size >= 16 && data[7] == 0x0F && data[6] == 0xDB) off = 8;
    if (off + 8 > size) return result;
    uint32_t rootOff; memcpy(&rootOff, data + off, 4);
    int vtEnd = off + (int)rootOff, pos = off + 8;
    while (pos < vtEnd && pos + 2 <= size) {
        uint16_t vtSize; memcpy(&vtSize, data + pos, 2);
        if (vtSize == 0) { pos += 2; continue; }
        if (vtSize < 6 || vtSize > 64 || vtSize % 2 != 0 || pos + vtSize > size) break;
        uint16_t objSize; memcpy(&objSize, data + pos + 2, 2);
        if (objSize < 4 || objSize > 128) { pos += 2; continue; }
        std::vector<uint16_t> vt(vtSize / 2);
        for (int i = 0; i < vtSize / 2; i++) memcpy(&vt[i], data + pos + i * 2, 2);
        result.push_back(vt);
        pos += vtSize;
    }
    return result;
}

inline std::vector<uint16_t> extractMessageVTable(const uint8_t* data, int size) {
    if (size < 16) return {};
    int off = 0;
    if (size >= 16 && data[7] == 0x0F && data[6] == 0xDB) off = 8;
    if (off + 8 > size) return {};
    uint32_t rootOff; memcpy(&rootOff, data + off, 4);
    int fakeRootPos = off + (int)rootOff;
    if (fakeRootPos + 8 > size) return {};
    uint32_t msgRelOff; memcpy(&msgRelOff, data + fakeRootPos + 4, 4);
    int msgPos = fakeRootPos + 4 + (int)msgRelOff;
    if (msgPos + 4 > size) return {};
    int32_t vtOff; memcpy(&vtOff, data + msgPos, 4);
    int vtPos = msgPos - vtOff;
    if (vtPos < 0 || vtPos + 2 > size) return {};
    uint16_t vtSize; memcpy(&vtSize, data + vtPos, 2);
    if (vtSize < 6 || vtSize > 128 || vtSize % 2 != 0 || vtPos + vtSize > size) return {};
    std::vector<uint16_t> vt(vtSize / 2);
    for (int i = 0; i < vtSize / 2; i++) memcpy(&vt[i], data + vtPos + i * 2, 2);
    return vt;
}

template <class T>
constexpr uint32_t getFileId() {
    if constexpr (requires { T::file_identifier; }) return T::file_identifier;
    else return 0;
}

static std::string toLower(const char* name) {
    std::string s;
    for (const char* p = name; *p; p++) s += tolower(*p);
    return s;
}

// Check if all non-zero, non-optional, non-nested fields are DynamicSize.
static bool allFieldsDynamicSize(const std::vector<FieldDesc>& fields) {
    bool hasDynamic = false;
    for (auto& fd : fields) {
        if (fd.size == 0) continue; // Arena — skip
        if (fd.kind == FieldKind::Optional) continue; // Skip optionals
        if (fd.kind == FieldKind::DynamicSize) { hasDynamic = true; continue; }
        return false; // Any non-DynamicSize field → not a String strategy type
    }
    return hasDynamic; // At least one DynamicSize field exists
}
