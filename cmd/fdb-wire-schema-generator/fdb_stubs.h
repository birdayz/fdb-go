// fdb_stubs.h — Lightweight stub types matching FDB's serialization traits.
//
// These stubs have the same _SizeOf (fb_size/fb_align) as the real FDB types,
// so get_vtable<>() and the SchemaExtractor produce correct vtable layouts.
// NO runtime behavior — just enough for compile-time type classification.
//
// Trait classification summary:
//   scalar_traits:          int8-64, uint8-64, bool, double, enums
//   dynamic_size_traits:    StringRef, Key, Value, TagSet, VersionVector, KeyRangeRef, IdempotencyIdRef
//   vector_like_traits:     std::vector<T>, VectorRef<T>, boost::container::flat_map<K,V>
//   union_like_traits:      Optional<T> → 2 vtable slots (uint8 + uint32)
//   expect_serialize_member: SpanContext, ReplyPromise<T>, ReadOptions, KeySelectorRef,
//                           MutationRef, CommitTransactionRef, UID, NetworkAddress, Endpoint,
//                           StorageServerInterface, CommitProxyInterface, GrvProxyInterface,
//                           Error, ClientTagThrottleLimits, ClientTrCommitCostEstimation,
//                           ClientVersionRef, VersionHistory, Hostname

#pragma once
#include "flow/flat_buffers.h"
#include <cstdint>
#include <deque>
#include <string>
#include <unordered_map>
#include <vector>

// ============================================================
// Forward declarations
// ============================================================
template <class T> class Optional;
template <class T> struct ReplyPromise;
template <class T> struct CachedSerialization;

// ============================================================
// Arena — zero size, not serialized
// ============================================================
struct Arena {};
// Arena uses fb_must_appear_last and has zero size.
template <>
struct fb_must_appear_last_t<Arena> : std::true_type {};

// _SizeOf<Arena> = {0, 0} — handled by default since there's no trait match
// and the fields_helper returns pack<> for zero-size.
// We need to ensure _SizeOf picks up size=0. The default fb_size computation
// for types without traits falls back to sizeof(RelativeOffset)=4 if
// expect_serialize_member is true. Arena has none of the traits, so we
// give it scalar_traits with size=0.
template <>
struct scalar_traits<Arena> : std::true_type {
    constexpr static size_t size = 0;
    template <class Context>
    static void save(uint8_t*, const Arena&, Context&) {}
    template <class Context>
    static void load(const uint8_t*, Arena&, Context&) {}
};

// ============================================================
// StringRef / Key / Value — dynamic_size_traits
// ============================================================
struct StringRef {
    const uint8_t* data_ = nullptr;
    int length_ = 0;
    StringRef() = default;
    StringRef(const char* s, int len) : data_(reinterpret_cast<const uint8_t*>(s)), length_(len) {}
    StringRef(const char* s) : data_(reinterpret_cast<const uint8_t*>(s)), length_(s ? (int)strlen(s) : 0) {}
};

template <>
struct dynamic_size_traits<StringRef> : std::true_type {
    template <class Context>
    static size_t size(const StringRef& s, Context&) { return s.length_; }
    template <class Context>
    static void save(uint8_t* out, const StringRef& s, Context&) {
        if (s.length_ > 0 && s.data_) memcpy(out, s.data_, s.length_);
    }
    template <class Context>
    static void load(const uint8_t* in, size_t len, StringRef& s, Context&) {
        s.data_ = in; s.length_ = (int)len;
    }
};

using KeyRef = StringRef;
using ValueRef = StringRef;
using Key = StringRef;
using Value = StringRef;
using Version = int64_t;

// ============================================================
// KeyRangeRef — dynamic_size_traits
// ============================================================
struct KeyRangeRef {
    KeyRef begin, end;
};

// KeyRangeRef: serialized as [begin_len(4)][begin_data][end_len(4)][end_data]
template <>
struct dynamic_size_traits<KeyRangeRef> : std::true_type {
    template <class Context>
    static size_t size(const KeyRangeRef& kr, Context&) {
        return 4 + kr.begin.length_ + 4 + kr.end.length_;
    }
    template <class Context>
    static void save(uint8_t* out, const KeyRangeRef& kr, Context&) {
        uint32_t blen = kr.begin.length_;
        memcpy(out, &blen, 4); out += 4;
        if (blen > 0 && kr.begin.data_) memcpy(out, kr.begin.data_, blen);
        out += blen;
        uint32_t elen = kr.end.length_;
        memcpy(out, &elen, 4); out += 4;
        if (elen > 0 && kr.end.data_) memcpy(out, kr.end.data_, elen);
    }
    template <class Context>
    static void load(const uint8_t* in, size_t, KeyRangeRef& kr, Context&) {
        uint32_t blen; memcpy(&blen, in, 4); in += 4;
        kr.begin = StringRef(reinterpret_cast<const char*>(in), blen); in += blen;
        uint32_t elen; memcpy(&elen, in, 4); in += 4;
        kr.end = StringRef(reinterpret_cast<const char*>(in), elen);
    }
};

// ============================================================
// VectorRef<T> — vector_like_traits
// ============================================================
template <class T, int Strategy = 0>
struct VectorRef {
    std::vector<T> data;
};

template <class T, int S>
struct vector_like_traits<VectorRef<T, S>> : std::true_type {
    using value_type = T;
    using iterator = typename std::vector<T>::const_iterator;
    using insert_iterator = std::back_insert_iterator<std::vector<T>>;
    template <class Context>
    static size_t num_entries(const VectorRef<T, S>& v, Context&) { return v.data.size(); }
    template <class Context>
    static insert_iterator insert(VectorRef<T, S>& v, size_t, Context&) { return std::back_inserter(v.data); }
    template <class Context>
    static iterator begin(const VectorRef<T, S>& v, Context&) { return v.data.begin(); }
};

// Standalone<T> = T for serialization.
template <class T>
using Standalone = T;

// KeyValueRef
struct KeyValueRef {
    KeyRef key;
    ValueRef value;
    template <class Ar>
    void serialize(Ar& ar) { serializer(ar, key, value); }
};

// ============================================================
// Optional<T> — union_like_traits (2 vtable slots: uint8 + uint32)
// ============================================================
template <class T>
class Optional {
    bool present_ = false;
    T value_{};
public:
    Optional() = default;
    Optional(const T& v) : present_(true), value_(v) {}
    bool present() const { return present_; }
    const T& get() const { return value_; }
};

template <class T>
struct union_like_traits<Optional<T>> : std::true_type {
    using Member = Optional<T>;
    using alternatives = pack<T>;
    template <class Context>
    static uint8_t index(const Member&, Context&) { return 0; }
    template <class Context>
    static bool empty(const Member& v, Context&) { return !v.present(); }
    template <int i, class Context>
    static const T& get(const Member& v, Context&) { return v.get(); }
    template <int i, class Alt, class Context>
    static void assign(Member& m, const Alt& a, Context&) { m = Optional<T>(a); }
    template <class Context>
    static void done(Member&, Context&) {}
};

// ============================================================
// UID — has serialize(), expect_serialize_member
// In FDB, UID serializes as two uint64_t via serialize_unversioned.
// For FlatBuffers: expect_serialize_member → RelativeOffset (size=4, align=4).
// ============================================================
struct UID {
    constexpr static FileIdentifier file_identifier = 15597147;
    uint64_t part[2] = {};
    template <class Ar>
    void serialize(Ar& ar) { serializer(ar, part[0], part[1]); }
};

// ============================================================
// Error — has serialize()
// ============================================================
struct Error {
    int error_code = 0;
    template <class Ar>
    void serialize(Ar& ar) { serializer(ar, error_code); }
};

// ============================================================
// SpanContext — has serialize()
// ============================================================
enum class TraceFlags : uint8_t { unsampled = 0, sampled = 1 };
struct SpanContext {
    UID traceID;
    uint64_t spanID = 0;
    TraceFlags m_Flags = TraceFlags::unsampled;
    template <class Ar>
    void serialize(Ar& ar) { serializer(ar, traceID, spanID, m_Flags); }
};

// ============================================================
// Tag — has serialize() (via serialize_unversioned in FDB, but
// for FlatBuffers it's expect_serialize_member → RelativeOffset)
// ============================================================
struct Tag {
    int8_t locality = 0;
    uint16_t id = 0;
    template <class Ar>
    void serialize(Ar& ar) { serializer(ar, locality, id); }
};

// ============================================================
// TagSet — dynamic_size_traits (custom binary encoding)
// ============================================================
// TagSet: stores raw bytes for serialization.
struct TagSet {
    std::vector<uint8_t> raw;
};
template <>
struct dynamic_size_traits<TagSet> : std::true_type {
    template <class Context>
    static size_t size(const TagSet& ts, Context&) { return ts.raw.size(); }
    template <class Context>
    static void save(uint8_t* out, const TagSet& ts, Context&) {
        if (!ts.raw.empty()) memcpy(out, ts.raw.data(), ts.raw.size());
    }
    template <class Context>
    static void load(const uint8_t* in, size_t len, TagSet& ts, Context&) {
        ts.raw.assign(in, in + len);
    }
};

using TransactionTag = StringRef;

// Hash for StringRef so it can be used as unordered_map key.
struct StringRefHash {
    size_t operator()(const StringRef&) const { return 0; }
};
struct StringRefEqual {
    bool operator()(const StringRef&, const StringRef&) const { return true; }
};

template <class V>
using TransactionTagMap = std::unordered_map<TransactionTag, V, StringRefHash, StringRefEqual>;

// ============================================================
// VersionVector — dynamic_size_traits
// ============================================================
struct VersionVector {
    constexpr static FileIdentifier file_identifier = 5253554;
    std::vector<uint8_t> raw;
};
template <>
struct dynamic_size_traits<VersionVector> : std::true_type {
    template <class Context>
    static size_t size(const VersionVector& vv, Context&) { return vv.raw.size(); }
    template <class Context>
    static void save(uint8_t* out, const VersionVector& vv, Context&) {
        if (!vv.raw.empty()) memcpy(out, vv.raw.data(), vv.raw.size());
    }
    template <class Context>
    static void load(const uint8_t* in, size_t len, VersionVector& vv, Context&) {
        vv.raw.assign(in, in + len);
    }
};

// ============================================================
// IdempotencyIdRef — dynamic_size_traits
// ============================================================
struct IdempotencyIdRef {
    constexpr static FileIdentifier file_identifier = 3858470;
    std::vector<uint8_t> raw;
};
template <>
struct dynamic_size_traits<IdempotencyIdRef> : std::true_type {
    template <class Context>
    static size_t size(const IdempotencyIdRef& id, Context&) { return id.raw.size(); }
    template <class Context>
    static void save(uint8_t* out, const IdempotencyIdRef& id, Context&) {
        if (!id.raw.empty()) memcpy(out, id.raw.data(), id.raw.size());
    }
    template <class Context>
    static void load(const uint8_t* in, size_t len, IdempotencyIdRef& id, Context&) {
        id.raw.assign(in, in + len);
    }
};

// HealthMetrics: dynamic_size_traits
struct HealthMetrics {
    std::vector<uint8_t> raw;
};
template <>
struct dynamic_size_traits<HealthMetrics> : std::true_type {
    template <class Context>
    static size_t size(const HealthMetrics& hm, Context&) { return hm.raw.size(); }
    template <class Context>
    static void save(uint8_t* out, const HealthMetrics& hm, Context&) {
        if (!hm.raw.empty()) memcpy(out, hm.raw.data(), hm.raw.size());
    }
    template <class Context>
    static void load(const uint8_t* in, size_t len, HealthMetrics& hm, Context&) {
        hm.raw.assign(in, in + len);
    }
};

// ============================================================
// ReplyPromise<T> — serializes endpoint UID token.
// expect_serialize_member → RelativeOffset (size=4, align=4).
// ============================================================
template <class T>
struct ReplyPromise {
    UID token;
    template <class Ar>
    void serialize(Ar& ar) { serializer(ar, token); }
};

// CachedSerialization<T> — wraps T, delegates serialize.
template <class T>
struct CachedSerialization {
    T value;
    template <class Ar>
    void serialize(Ar& ar) { serializer(ar, value); }
};

// ============================================================
// ReadOptions — has serialize()
// ============================================================
enum class ReadType : uint8_t { NORMAL = 3 };
struct ReadOptions {
    ReadType type = ReadType::NORMAL;
    bool cacheResult = false;
    Optional<UID> debugID;
    Optional<Version> consistencyCheckStartVersion;
    bool lockAware = false;
    template <class Ar>
    void serialize(Ar& ar) {
        serializer(ar, type, cacheResult, debugID, consistencyCheckStartVersion, lockAware);
    }
};

// ============================================================
// KeySelectorRef — has serialize()
// ============================================================
struct KeySelectorRef {
    KeyRef key;
    bool orEqual = false;
    int offset = 0;
    template <class Ar>
    void serialize(Ar& ar) { serializer(ar, key, orEqual, offset); }
};
using KeySelector = KeySelectorRef;

// ============================================================
// MutationRef — has serialize()
// ============================================================
struct MutationRef {
    uint8_t type = 0;
    StringRef param1;
    StringRef param2;
    template <class Ar>
    void serialize(Ar& ar) { serializer(ar, type, param1, param2); }
};

// ============================================================
// CommitTransactionRef — has serialize()
// ============================================================
struct CommitTransactionRef {
    VectorRef<KeyRangeRef> read_conflict_ranges;
    VectorRef<KeyRangeRef> write_conflict_ranges;
    VectorRef<MutationRef> mutations;
    Version read_snapshot = 0;
    bool report_conflicting_keys = false;
    bool lock_aware = false;
    Optional<SpanContext> spanContext;
    Optional<VectorRef<int64_t>> tenantIds;
    template <class Ar>
    void serialize(Ar& ar) {
        serializer(ar, read_conflict_ranges, write_conflict_ranges, mutations,
                   read_snapshot, report_conflicting_keys, lock_aware, spanContext, tenantIds);
    }
};

// ============================================================
// ClientTrCommitCostEstimation — has serialize()
// ============================================================
struct ClientTrCommitCostEstimation {
    int opsCount = 0;
    uint64_t writeCosts = 0;
    std::deque<std::pair<int, uint64_t>> clearIdxCosts;
    uint32_t expensiveCostEstCount = 0;
    template <class Ar>
    void serialize(Ar& ar) {
        serializer(ar, opsCount, writeCosts, clearIdxCosts, expensiveCostEstCount);
    }
};

// ============================================================
// ClientTagThrottleLimits — has serialize()
// ============================================================
struct ClientTagThrottleLimits {
    double tpsRate = 0;
    double duration = 0;
    template <class Ar>
    void serialize(Ar& ar) { serializer(ar, tpsRate, duration); }
};

// ============================================================
// Network types — stubs for interface serialization
// ============================================================
struct IPAddress {
    uint32_t addr = 0; // simplified, real has union
};
template <>
struct scalar_traits<IPAddress> : std::true_type {
    constexpr static size_t size = 4;
    template <class Context> static void save(uint8_t*, const IPAddress&, Context&) {}
    template <class Context> static void load(const uint8_t*, IPAddress&, Context&) {}
};

struct NetworkAddress {
    IPAddress ip;
    uint16_t port = 0;
    uint16_t flags = 0;
    bool fromHostname = false;
    template <class Ar>
    void serialize(Ar& ar) { serializer(ar, ip, port, flags, fromHostname); }
};

struct NetworkAddressList {
    NetworkAddress address;
    Optional<NetworkAddress> secondaryAddress;
    template <class Ar>
    void serialize(Ar& ar) { serializer(ar, address, secondaryAddress); }
};

struct Endpoint {
    NetworkAddressList addresses;
    UID token;
    template <class Ar>
    void serialize(Ar& ar) { serializer(ar, addresses, token); }
};

// RequestStream<T> serializes as Endpoint.
template <class T, bool P = false>
struct RequestStream {
    Endpoint endpoint;
    template <class Ar>
    void serialize(Ar& ar) { serializer(ar, endpoint); }
};

template <class T, bool P = false>
using PublicRequestStream = RequestStream<T, true>;

// Hostname
struct Hostname {
    std::string host, service;
    bool isTLS = false;
    template <class Ar>
    void serialize(Ar& ar) { serializer(ar, host, service, isTLS); }
};

// LocalityData
struct LocalityData {
    Optional<StringRef> dcId, processId, zoneId, machineId;
    template <class Ar>
    void serialize(Ar& ar) { serializer(ar, dcId, processId, zoneId, machineId); }
};

// ClientVersionRef
struct ClientVersionRef {
    StringRef clientVersion, sourceVersion, protocolVersion;
    template <class Ar>
    void serialize(Ar& ar) { serializer(ar, clientVersion, sourceVersion, protocolVersion); }
};

// VersionHistory
struct VersionHistory {
    constexpr static FileIdentifier file_identifier = 5863456;
    VectorRef<MutationRef> mutations;
    Version version = 0;
    template <class Ar>
    void serialize(Ar& ar) { serializer(ar, mutations, version); }
};

// TaskPriority (enum)
enum class TaskPriority : int64_t { DefaultEndpoint = 0 };
enum class ClusterType : uint8_t { STANDALONE = 0 };

// ============================================================
// Additional types referenced by protocol messages
// ============================================================
struct Void {
    constexpr static FileIdentifier file_identifier = 2010442;
};
template <>
struct scalar_traits<Void> : std::true_type {
    constexpr static size_t size = 0;
    template <class Context> static void save(uint8_t*, const Void&, Context&) {}
    template <class Context> static void load(const uint8_t*, Void&, Context&) {}
};

struct FieldContainer {
    std::vector<std::pair<StringRef, StringRef>> fields;
};
template <>
struct vector_like_traits<FieldContainer> : std::true_type {
    using value_type = std::pair<StringRef, StringRef>;
    using iterator = typename std::vector<value_type>::const_iterator;
    using insert_iterator = std::back_insert_iterator<std::vector<value_type>>;
    template <class Context>
    static size_t num_entries(const FieldContainer& v, Context&) { return v.fields.size(); }
    template <class Context>
    static insert_iterator insert(FieldContainer& v, size_t, Context&) { return std::back_inserter(v.fields); }
    template <class Context>
    static iterator begin(const FieldContainer& v, Context&) { return v.fields.begin(); }
};

struct WipedString {
    constexpr static FileIdentifier file_identifier = 6228563;
    std::string data;
    template <class Ar> void serialize(Ar& ar) { serializer(ar, data); }
};

// Additional enum/type stubs referenced by protocol messages.
enum class EncryptionAtRestMode : int { DISABLED = 0 };
enum class RecoveryState : int { UNINITIALIZED = 0 };
enum class TLogVersion : int { DEFAULT = 0 };
enum class TLogSpillType : int { DEFAULT = 0 };
enum class KeyValueStoreType_StoreType : int { SSD_BTREE_V2 = 0 };
enum class BackupFormat : uint32_t { BLOB = 0 };
enum class BulkDumpPhase : uint8_t { Submitted = 0 };
enum class BulkLoadPhase : uint8_t { Submitted = 0 };
enum class BulkLoadType : uint8_t { SST = 0 };
enum class BulkLoadTransportMethod : uint8_t { CP = 0 };
enum class BulkLoadJobPhase : uint8_t { Submitted = 0 };
enum class ResolverMoveType : uint8_t { INVALID = 0 };
enum class ProfilerRequest_Type : int { NONE = 0 };
enum class ProfilerRequest_Action : int { DISABLE = 0 };

struct LifetimeToken {
    uint64_t ccID = 0;
    int64_t num = 0;
    template <class Ar> void serialize(Ar& ar) { serializer(ar, ccID, num); }
};

struct DatabaseConfiguration {
    // Complex type — stub as a struct with serialize.
    int dummy = 0;
    template <class Ar> void serialize(Ar& ar) { serializer(ar, dummy); }
};

struct LogSystemConfig {
    int dummy = 0;
    template <class Ar> void serialize(Ar& ar) { serializer(ar, dummy); }
};

struct CheckpointMetaData {
    int dummy = 0;
    template <class Ar> void serialize(Ar& ar) { serializer(ar, dummy); }
};

struct RestoreFileFR {
    int dummy = 0;
    template <class Ar> void serialize(Ar& ar) { serializer(ar, dummy); }
};

struct GranuleHistory {
    int dummy = 0;
    template <class Ar> void serialize(Ar& ar) { serializer(ar, dummy); }
};

struct IReplicationPolicy {
    int dummy = 0;
    template <class Ar> void serialize(Ar& ar) { serializer(ar, dummy); }
};

template <class T>
struct Reference {
    T* ptr = nullptr;
    template <class Ar> void serialize(Ar& ar) { /* no-op for stub */ }
};

// ============================================================
// TimedRequest — base class, ZERO serialized fields
// ============================================================
struct TimedRequest {};

// ============================================================
// LoadBalancedReply — base class with serialized fields
// ============================================================
struct LoadBalancedReply {
    double penalty = 0;
    Optional<Error> error;
};

struct BasicLoadBalancedReply {
    int processBusyTime = 0;
};

// ============================================================
// ErrorOr<T> — union_like_traits (alternatives = [Error, T])
// Used for all ReplyPromise response wrapping.
// ============================================================
template <class T>
class ErrorOr {
    bool present_ = false;
    Error error_;
    T value_{};
public:
    // ComposedIdentifier: file_identifier = (2 << 24) | inner file_id
    constexpr static FileIdentifier file_identifier = (2 << 24) | FileIdentifierFor<T>::value;
    ErrorOr() = default;
    explicit ErrorOr(const T& v) : present_(true), value_(v) {}
    explicit ErrorOr(const Error& e) : present_(false), error_(e) {}
    bool present() const { return present_; }
    const Error& getError() const { return error_; }
    const T& get() const { return value_; }
};

template <class T>
struct union_like_traits<ErrorOr<T>> : std::true_type {
    using Member = ErrorOr<T>;
    using alternatives = pack<Error, T>;
    template <class Context>
    static uint8_t index(const Member& m, Context&) { return m.present() ? 1 : 0; }
    template <class Context>
    static bool empty(const Member& m, Context&) { return false; }
    template <int i, class Context>
    static const auto& get(const Member& m, Context&) {
        if constexpr (i == 0) return m.getError();
        else return m.get();
    }
    template <int i, class Alt, class Context>
    static void assign(Member& m, const Alt& a, Context&) {
        if constexpr (i == 0) m = ErrorOr<T>(a);
        else m = ErrorOr<T>(a);
    }
    template <class Context>
    static void done(Member&, Context&) {}
};

// ============================================================
// EnsureTable<T> — wraps T to ensure it gets a FlatBuffers table
// ============================================================
template <class T>
struct EnsureTable {
    constexpr static FileIdentifier file_identifier = FileIdentifierFor<T>::value;
    T t{};
    EnsureTable() = default;
    explicit EnsureTable(const T& v) : t(v) {}
    template <class Ar>
    void serialize(Ar& ar) {
        if constexpr (is_fb_function<Ar>) {
            if constexpr (_SizeOf<T>::size > 0) {
                serializer(ar, t);
            }
        } else {
            serializer(ar, t);
        }
    }
};
