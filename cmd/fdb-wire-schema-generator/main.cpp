// extractor.cpp — Extracts wire format metadata from FDB protocol messages.
//
// Uses stub types (fdb_stubs.h) that match FDB's serialization traits so that
// get_vtable<>() produces correct vtable layouts. Outputs wire_schema.json.
//
// Build: bazelisk build //pkg/fdbgo/wire/testdata:extractor
// Run:   bazelisk run //pkg/fdbgo/wire/testdata:extractor > wire_schema.json

#include "fdb_stubs.h"
#include <cstdio>
#include <cstring>
#include <string>
#include <typeinfo>
#include <vector>

// ============================================================
// Protocol message definitions (from FDB headers, serialize() only)
// ============================================================

// --- Storage Server messages ---

struct GetValueReply : LoadBalancedReply {
    constexpr static FileIdentifier file_identifier = 1378929;
    Optional<Value> value;
    bool cached = false;
    template <class Ar> void serialize(Ar& ar) {
        serializer(ar, penalty, error, value, cached);
    }
};

struct GetValueRequest : TimedRequest {
    constexpr static FileIdentifier file_identifier = 8454530;
    SpanContext spanContext;
    Key key;
    Version version = 0;
    Optional<TagSet> tags;
    ReplyPromise<GetValueReply> reply;
    Optional<ReadOptions> options;
    VersionVector ssLatestCommitVersions;
    template <class Ar> void serialize(Ar& ar) {
        serializer(ar, key, version, tags, reply, spanContext, options, ssLatestCommitVersions);
    }
};

struct GetKeyReply : LoadBalancedReply {
    constexpr static FileIdentifier file_identifier = 11226513;
    KeySelector sel;
    bool cached = false;
    template <class Ar> void serialize(Ar& ar) {
        serializer(ar, penalty, error, sel, cached);
    }
};

struct GetKeyRequest : TimedRequest {
    constexpr static FileIdentifier file_identifier = 10457870;
    SpanContext spanContext;
    Arena arena;
    KeySelectorRef sel;
    Version version = 0;
    Optional<TagSet> tags;
    ReplyPromise<GetKeyReply> reply;
    Optional<ReadOptions> options;
    VersionVector ssLatestCommitVersions;
    template <class Ar> void serialize(Ar& ar) {
        serializer(ar, sel, version, tags, reply, spanContext, options, ssLatestCommitVersions, arena);
    }
};

struct GetKeyValuesReply : LoadBalancedReply {
    constexpr static FileIdentifier file_identifier = 1783066;
    Arena arena;
    VectorRef<KeyValueRef, 0> data;
    Version version = 0;
    bool more = false;
    bool cached = false;
    template <class Ar> void serialize(Ar& ar) {
        serializer(ar, penalty, error, data, version, more, cached, arena);
    }
};

struct GetKeyValuesRequest : TimedRequest {
    constexpr static FileIdentifier file_identifier = 6795746;
    SpanContext spanContext;
    Arena arena;
    KeySelectorRef begin;
    KeySelectorRef end;
    Version version = 0;
    int limit = 0;
    int limitBytes = 0;
    Optional<TagSet> tags;
    Optional<ReadOptions> options;
    ReplyPromise<GetKeyValuesReply> reply;
    VersionVector ssLatestCommitVersions;
    Optional<TaskPriority> taskID;
    template <class Ar> void serialize(Ar& ar) {
        serializer(ar, begin, end, version, limit, limitBytes, tags, reply, spanContext,
                   options, ssLatestCommitVersions, taskID, arena);
    }
};

struct WatchValueReply {
    constexpr static FileIdentifier file_identifier = 3;
    Version version = 0;
    bool cached = false;
    template <class Ar> void serialize(Ar& ar) { serializer(ar, version, cached); }
};

struct WatchValueRequest {
    constexpr static FileIdentifier file_identifier = 14747733;
    SpanContext spanContext;
    Key key;
    Optional<Value> value;
    Version version = 0;
    Optional<TagSet> tags;
    Optional<UID> debugID;
    ReplyPromise<WatchValueReply> reply;
    template <class Ar> void serialize(Ar& ar) {
        serializer(ar, key, value, version, tags, debugID, reply, spanContext);
    }
};

// --- Commit Proxy messages ---

struct CommitID {
    constexpr static FileIdentifier file_identifier = 14254927;
    Version version = 0;
    uint16_t txnBatchId = 0;
    Optional<Value> metadataVersion;
    Optional<VectorRef<int>> conflictingKRIndices;
    template <class Ar> void serialize(Ar& ar) {
        serializer(ar, version, txnBatchId, metadataVersion, conflictingKRIndices);
    }
};

struct CommitTransactionRequest : TimedRequest {
    constexpr static FileIdentifier file_identifier = 93948;
    Arena arena;
    SpanContext spanContext;
    CommitTransactionRef transaction;
    ReplyPromise<CommitID> reply;
    uint32_t flags = 0;
    Optional<UID> debugID;
    Optional<ClientTrCommitCostEstimation> commitCostEstimation;
    Optional<TagSet> tagSet;
    IdempotencyIdRef idempotencyId;
    template <class Ar> void serialize(Ar& ar) {
        serializer(ar, transaction, reply, flags, debugID, commitCostEstimation, tagSet,
                   spanContext, idempotencyId, arena);
    }
};

// Forward decl (full definition below, under Interfaces section)
struct StorageServerInterface;

struct GetKeyServerLocationsReply {
    constexpr static FileIdentifier file_identifier = 10636023;
    Arena arena;
    std::vector<std::pair<KeyRangeRef, std::vector<StorageServerInterface>>> results;
    std::vector<std::pair<UID, StorageServerInterface>> resultsTssMapping;
    std::vector<std::pair<UID, Tag>> resultsTagMapping;
    template <class Ar> void serialize(Ar& ar) {
        serializer(ar, results, resultsTssMapping, resultsTagMapping, arena);
    }
};

// Forward decl for reply type
struct GetKeyServerLocationsRequest {
    constexpr static FileIdentifier file_identifier = 9144680;
    Arena arena;
    SpanContext spanContext;
    KeyRef begin;
    Optional<KeyRef> end;
    int limit = 0;
    bool reverse = false;
    ReplyPromise<GetKeyServerLocationsReply> reply;
    Version legacyVersion = 0;
    template <class Ar> void serialize(Ar& ar) {
        serializer(ar, begin, end, limit, reverse, reply, spanContext, legacyVersion, arena);
    }
};

// --- GRV Proxy messages ---

struct GetReadVersionReply : BasicLoadBalancedReply {
    constexpr static FileIdentifier file_identifier = 15709388;
    Version version = 0;
    bool locked = false;
    Optional<Value> metadataVersion;
    int64_t midShardSize = 0;
    bool rkDefaultThrottled = false;
    bool rkBatchThrottled = false;
    TransactionTagMap<ClientTagThrottleLimits> tagThrottleInfo;
    double proxyTagThrottledDuration = 0.0;
    VersionVector ssVersionVectorDelta;
    UID proxyId;
    template <class Ar> void serialize(Ar& ar) {
        serializer(ar, processBusyTime, version, locked, metadataVersion, tagThrottleInfo,
                   midShardSize, rkDefaultThrottled, rkBatchThrottled,
                   ssVersionVectorDelta, proxyId, proxyTagThrottledDuration);
    }
};

struct GetReadVersionRequest : TimedRequest {
    constexpr static FileIdentifier file_identifier = 838566;
    SpanContext spanContext;
    uint32_t transactionCount = 0;
    uint32_t flags = 0;
    TransactionTagMap<uint32_t> tags;
    Optional<UID> debugID;
    ReplyPromise<GetReadVersionReply> reply;
    Version maxVersion = 0;
    template <class Ar> void serialize(Ar& ar) {
        serializer(ar, transactionCount, flags, tags, debugID, reply, spanContext, maxVersion);
    }
};

// --- Cluster discovery ---

struct StorageServerInterface {
    constexpr static FileIdentifier file_identifier = 15302073;
    LocalityData locality;
    UID uniqueID;
    Optional<UID> tssPairID;
    RequestStream<GetValueRequest> getValue;
    bool acceptingRequests = true;
    template <class Ar> void serialize(Ar& ar) {
        serializer(ar, uniqueID, locality, getValue, tssPairID, acceptingRequests);
    }
};

struct CommitProxyInterface {
    constexpr static FileIdentifier file_identifier = 8954922;
    Optional<Key> processId;
    bool provisional = false;
    PublicRequestStream<CommitTransactionRequest> commit;
    template <class Ar> void serialize(Ar& ar) {
        serializer(ar, processId, provisional, commit);
    }
};

struct GrvProxyInterface {
    constexpr static FileIdentifier file_identifier = 8743216;
    Optional<Key> processId;
    bool provisional = false;
    PublicRequestStream<GetReadVersionRequest> getConsistentReadVersion;
    template <class Ar> void serialize(Ar& ar) {
        serializer(ar, processId, provisional, getConsistentReadVersion);
    }
};

struct ClientDBInfo {
    constexpr static FileIdentifier file_identifier = 5355080;
    UID id;
    std::vector<GrvProxyInterface> grvProxies;
    std::vector<CommitProxyInterface> commitProxies;
    Optional<Value> forward;
    std::vector<VersionHistory> history;
    UID clusterId;
    ClusterType clusterType = ClusterType::STANDALONE;
    template <class Ar> void serialize(Ar& ar) {
        serializer(ar, grvProxies, commitProxies, id, forward, history, clusterId, clusterType);
    }
};

struct OpenDatabaseCoordRequest {
    constexpr static FileIdentifier file_identifier = 214728;
    Key traceLogGroup;
    VectorRef<StringRef> issues;
    VectorRef<ClientVersionRef> supportedVersions;
    UID knownClientInfoID;
    Key clusterKey;
    std::vector<Hostname> hostnames;
    std::vector<NetworkAddress> coordinators;
    ReplyPromise<CachedSerialization<ClientDBInfo>> reply;
    bool internal = true;
    template <class Ar> void serialize(Ar& ar) {
        serializer(ar, issues, supportedVersions, traceLogGroup, knownClientInfoID,
                   clusterKey, coordinators, reply, hostnames, internal);
    }
};

// ============================================================
// Schema extractor visitor
// ============================================================

struct FieldInfo {
    std::string type;
    int vtableSlot;
    unsigned wireSize;
    unsigned wireAlignment;
    bool isInline;
};

struct MessageInfo {
    std::string name;
    uint32_t fileIdentifier;
    std::string replyType;
    std::vector<unsigned> vtable;
    std::vector<FieldInfo> fields;
};

// Type classification helpers
template <class T> constexpr bool is_scalar_type() {
    return scalar_traits<T>::value;
}
template <class T> constexpr bool is_dynamic_size_type() {
    return dynamic_size_traits<T>::value;
}
template <class T> constexpr bool is_vector_type() {
    return vector_like_traits<T>::value;
}
template <class T> constexpr bool is_union_type() {
    return union_like_traits<T>::value;
}

template <class T>
std::string classifyType() {
    if constexpr (is_scalar_type<T>()) {
        if constexpr (std::is_same_v<T, bool>) return "bool";
        if constexpr (std::is_same_v<T, int8_t>) return "int8";
        if constexpr (std::is_same_v<T, uint8_t>) return "uint8";
        if constexpr (std::is_same_v<T, int16_t>) return "int16";
        if constexpr (std::is_same_v<T, uint16_t>) return "uint16";
        if constexpr (std::is_same_v<T, int32_t> || std::is_same_v<T, int>) return "int32";
        if constexpr (std::is_same_v<T, uint32_t>) return "uint32";
        if constexpr (std::is_same_v<T, int64_t>) return "int64";
        if constexpr (std::is_same_v<T, uint64_t>) return "uint64";
        if constexpr (std::is_same_v<T, double>) return "double";
        if constexpr (std::is_enum_v<T>) return "enum";
        if constexpr (std::is_same_v<T, Arena>) return "arena";
        if constexpr (std::is_same_v<T, IPAddress>) return "uint32"; // simplified
        return "scalar_unknown";
    }
    if constexpr (is_dynamic_size_type<T>()) return "bytes";
    if constexpr (is_vector_type<T>()) return "vector";
    if constexpr (is_union_type<T>()) return "optional";
    // expect_serialize_member
    return "struct";
}

struct SchemaExtractor {
    static constexpr bool is_fb_visitor = true;
    static constexpr bool isDeserializing = false;
    static constexpr bool isSerializing = false;

    MessageInfo* msg;
    int slot = 0;

    template <class... Members>
    void operator()(const Members&... members) {
        // Capture vtable.
        const auto* vt = detail::get_vtable<Members...>();
        for (size_t i = 0; i < vt->size(); i++) {
            msg->vtable.push_back((*vt)[i]);
        }
        // Capture field info.
        detail::for_each([&](const auto& member) {
            using M = std::decay_t<decltype(member)>;
            if constexpr (union_like_traits<M>::value) {
                // Union/Optional: 2 vtable slots (type tag + value offset)
                FieldInfo fi;
                fi.type = classifyType<M>();
                fi.vtableSlot = slot;
                fi.wireSize = 0; // union is 2 entries, handled by fields_helper
                fi.wireAlignment = 0;
                fi.isInline = false;
                msg->fields.push_back(fi);
                slot += 2; // skip both slots
            } else if constexpr (detail::_SizeOf<M>::size == 0) {
                // Zero-size (Arena) — no vtable slot consumed
                FieldInfo fi;
                fi.type = "arena";
                fi.vtableSlot = -1;
                fi.wireSize = 0;
                fi.wireAlignment = 0;
                fi.isInline = false;
                msg->fields.push_back(fi);
            } else {
                FieldInfo fi;
                fi.type = classifyType<M>();
                fi.vtableSlot = slot;
                fi.wireSize = detail::_SizeOf<M>::size;
                fi.wireAlignment = detail::_SizeOf<M>::align;
                fi.isInline = is_scalar_type<M>();
                msg->fields.push_back(fi);
                slot++;
            }
        }, members...);
    }
};

template <class T>
MessageInfo extractMessage(const char* name, const char* replyType = "", uint32_t fileId = 0) {
    MessageInfo mi;
    mi.name = name;
    if constexpr (HasFileIdentifier<T>::value) {
        mi.fileIdentifier = FileIdentifierFor<T>::value;
    } else {
        mi.fileIdentifier = fileId;
    }
    mi.replyType = replyType;

    SchemaExtractor ex;
    ex.msg = &mi;
    T instance{};
    instance.serialize(ex);
    return mi;
}

// ============================================================
// JSON output
// ============================================================

void printField(const FieldInfo& f, bool last) {
    printf("      {\"type\": \"%s\", \"vtable_slot\": %d, \"wire_size\": %u, \"wire_alignment\": %u, \"inline\": %s}%s\n",
           f.type.c_str(), f.vtableSlot, f.wireSize, f.wireAlignment,
           f.isInline ? "true" : "false", last ? "" : ",");
}

void printMessage(const MessageInfo& mi, bool last) {
    printf("  {\n");
    printf("    \"name\": \"%s\",\n", mi.name.c_str());
    printf("    \"file_identifier\": %u,\n", mi.fileIdentifier);
    if (!mi.replyType.empty())
        printf("    \"reply_type\": \"%s\",\n", mi.replyType.c_str());
    printf("    \"vtable\": [");
    for (size_t i = 0; i < mi.vtable.size(); i++) {
        printf("%s%u", i ? ", " : "", mi.vtable[i]);
    }
    printf("],\n");
    printf("    \"fields\": [\n");
    for (size_t i = 0; i < mi.fields.size(); i++) {
        printField(mi.fields[i], i == mi.fields.size() - 1);
    }
    printf("    ]\n");
    printf("  }%s\n", last ? "" : ",");
}

int main() {
    std::vector<MessageInfo> messages;

    // Storage Server
    messages.push_back(extractMessage<GetValueRequest>("GetValueRequest", "GetValueReply"));
    messages.push_back(extractMessage<GetValueReply>("GetValueReply"));
    messages.push_back(extractMessage<GetKeyRequest>("GetKeyRequest", "GetKeyReply"));
    messages.push_back(extractMessage<GetKeyReply>("GetKeyReply"));
    messages.push_back(extractMessage<GetKeyValuesRequest>("GetKeyValuesRequest", "GetKeyValuesReply"));
    messages.push_back(extractMessage<GetKeyValuesReply>("GetKeyValuesReply"));
    messages.push_back(extractMessage<WatchValueRequest>("WatchValueRequest", "WatchValueReply"));
    messages.push_back(extractMessage<WatchValueReply>("WatchValueReply"));

    // Commit Proxy
    messages.push_back(extractMessage<CommitTransactionRequest>("CommitTransactionRequest", "CommitID"));
    messages.push_back(extractMessage<CommitID>("CommitID"));
    messages.push_back(extractMessage<GetKeyServerLocationsRequest>("GetKeyServerLocationsRequest", "GetKeyServerLocationsReply"));
    messages.push_back(extractMessage<GetKeyServerLocationsReply>("GetKeyServerLocationsReply"));

    // GRV Proxy
    messages.push_back(extractMessage<GetReadVersionRequest>("GetReadVersionRequest", "GetReadVersionReply"));
    messages.push_back(extractMessage<GetReadVersionReply>("GetReadVersionReply"));

    // Cluster discovery
    messages.push_back(extractMessage<ClientDBInfo>("ClientDBInfo"));
    messages.push_back(extractMessage<OpenDatabaseCoordRequest>("OpenDatabaseCoordRequest"));

    // Interfaces
    messages.push_back(extractMessage<StorageServerInterface>("StorageServerInterface"));
    messages.push_back(extractMessage<CommitProxyInterface>("CommitProxyInterface"));
    messages.push_back(extractMessage<GrvProxyInterface>("GrvProxyInterface"));

    // Supporting types (serialized as nested structs)
    messages.push_back(extractMessage<SpanContext>("SpanContext"));
    messages.push_back(extractMessage<UID>("UID"));
    messages.push_back(extractMessage<ReadOptions>("ReadOptions"));
    messages.push_back(extractMessage<KeySelectorRef>("KeySelectorRef"));
    messages.push_back(extractMessage<MutationRef>("MutationRef"));
    messages.push_back(extractMessage<CommitTransactionRef>("CommitTransactionRef"));
    messages.push_back(extractMessage<ReplyPromise<GetValueReply>>("ReplyPromise"));
    messages.push_back(extractMessage<Endpoint>("Endpoint"));
    messages.push_back(extractMessage<NetworkAddress>("NetworkAddress"));
    messages.push_back(extractMessage<NetworkAddressList>("NetworkAddressList"));
    messages.push_back(extractMessage<KeyValueRef>("KeyValueRef"));

    printf("{\n");
    printf("\"fdb_version\": \"7.3.x\",\n");
    printf("\"messages\": [\n");
    for (size_t i = 0; i < messages.size(); i++) {
        printMessage(messages[i], i == messages.size() - 1);
    }
    printf("]\n");
    printf("}\n");

    return 0;
}
