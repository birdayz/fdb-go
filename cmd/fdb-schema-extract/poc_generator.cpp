// poc_generator.cpp — Working proof-of-concept single-pass type extractor.
// Compiles against real FDB headers. Outputs Go files to a directory.
// Run alongside the old generator to compare outputs.
//
// Build: add to cmake alongside schema_extract:
//   add_executable(poc_gen poc_generator.cpp schema_extract_names.cpp)
//   target_link_libraries(poc_gen PRIVATE fdbclient fdbrpc flow)

#include "fdbclient/StorageServerInterface.h"
#include "fdbclient/CommitProxyInterface.h"
#include "fdbclient/GrvProxyInterface.h"
#include "fdbclient/CoordinationInterface.h"
#include "fdbclient/ClusterInterface.h"
#include "fdbclient/FDBTypes.h"
#include "fdbclient/GlobalConfig.h"
#include "fdbrpc/FlowTransport.h"
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
REGISTER_GO_TYPE(CommitTransactionRef, "CommitTransactionRef");
REGISTER_GO_TYPE(ReadOptions, "ReadOptions");
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

// ============================================================
// 3. Field Classification — compile-time from FDB traits
// ============================================================

enum class FieldKind { Scalar, DynamicSize, VectorLike, Optional, NestedStruct, Variant };

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

// Classify a field type into FieldKind.
template <class T>
FieldKind classifyField() {
    using namespace detail;
    if constexpr (is_scalar<T>) return FieldKind::Scalar;
    else if constexpr (is_dynamic_size<T>) return FieldKind::DynamicSize;
    else if constexpr (is_standalone<T>::value) {
        using Inner = typename T::RefType;
        if constexpr (is_vector_like<Inner>) return FieldKind::VectorLike;
        else if constexpr (is_dynamic_size<Inner>) return FieldKind::DynamicSize;
        else return FieldKind::NestedStruct;
    }
    else if constexpr (is_vector_like<T>) return FieldKind::VectorLike;
    else if constexpr (is_union_like<T>) return FieldKind::Optional; // TODO: distinguish variant
    else return FieldKind::NestedStruct; // serialize_member / struct_like
}

// ============================================================
// 4. FieldDesc + FieldCollector
// ============================================================

struct FieldDesc {
    const char* name;
    FieldKind kind;
    ScalarInfo scalar;
    const char* nestedGoType;
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
            // If not registered, nestedGoType is "" — we'll detect and warn.
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
// 5. GoEmitter
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

struct GoEmitter {
    FILE* f;

    void emitFull(const char* typeName, const std::vector<FieldDesc>& fields,
                  const std::vector<uint16_t>& vt, uint32_t fileId,
                  const std::vector<std::vector<uint16_t>>& closure) {
        // Header.
        fprintf(f, "// Code generated by poc_generator from FDB 7.3.75. DO NOT EDIT.\n\n");
        fprintf(f, "package types\n\n");

        bool needsBinary = allFieldsDynamicSize(fields);
        if (needsBinary) {
            fprintf(f, "import (\n");
            fprintf(f, "\t\"encoding/binary\"\n\n");
            fprintf(f, "\t\"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire\"\n");
            fprintf(f, ")\n\n");
        } else {
            fprintf(f, "import \"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire\"\n\n");
        }

        // Slot constants.
        fprintf(f, "const (\n");
        for (auto& fd : fields) {
            auto gn = fieldGoName(fd);
            fprintf(f, "\t%sSlot%s = %d\n", typeName, gn.c_str(), fd.vtableSlot);
        }
        fprintf(f, ")\n\n");

        // VTable.
        fprintf(f, "var %sVTable = wire.VTable{", typeName);
        for (size_t i = 0; i < vt.size(); i++) {
            if (i) fprintf(f, ", ");
            fprintf(f, "%u", vt[i]);
        }
        fprintf(f, "}\n");

        // FileID.
        if (fileId > 0) fprintf(f, "const %sFileID uint32 = %u\n", typeName, fileId);

        // Closure.
        if (!closure.empty()) {
            fprintf(f, "var %sVTableClosure = []wire.VTable{\n", typeName);
            for (auto& cvt : closure) {
                fprintf(f, "\t{");
                for (size_t i = 0; i < cvt.size(); i++) {
                    if (i) fprintf(f, ", ");
                    fprintf(f, "%u", cvt[i]);
                }
                fprintf(f, "},\n");
            }
            fprintf(f, "}\n");
        }

        // Template.
        if (fileId > 0 && !closure.empty()) {
            int maxAlign = 4;
            for (auto& fd : fields)
                if (fd.kind == FieldKind::Scalar && fd.scalar.goType &&
                    (strcmp(fd.scalar.goType, "int64") == 0 || strcmp(fd.scalar.goType, "uint64") == 0 ||
                     strcmp(fd.scalar.goType, "float64") == 0 || strcmp(fd.scalar.goType, "[16]byte") == 0))
                    maxAlign = 8;
            fprintf(f, "var %sTemplate = wire.NewMessageTemplate(\n", typeName);
            fprintf(f, "\t%sFileID, %sVTable, %d, %sVTableClosure,\n)\n",
                    typeName, typeName, maxAlign, typeName);
        }
        fprintf(f, "\n");

        // Struct.
        fprintf(f, "type %s struct {\n", typeName);
        for (auto& fd : fields) {
            auto gn = fieldGoName(fd);
            switch (fd.kind) {
            case FieldKind::Scalar:
                fprintf(f, "\t%s %s // slot %d\n", gn.c_str(), fd.scalar.goType, fd.vtableSlot);
                break;
            case FieldKind::DynamicSize:
            case FieldKind::VectorLike:
                fprintf(f, "\t%s []byte // slot %d\n", gn.c_str(), fd.vtableSlot);
                break;
            case FieldKind::NestedStruct:
                if (fd.nestedGoType[0])
                    fprintf(f, "\t%s %s // slot %d, nested\n", gn.c_str(), fd.nestedGoType, fd.vtableSlot);
                else
                    fprintf(f, "\t// %s: unregistered nested struct at slot %d\n", gn.c_str(), fd.vtableSlot);
                break;
            case FieldKind::Optional:
                fprintf(f, "\tHas%s bool   // slot %d, optional tag\n", gn.c_str(), fd.vtableSlot);
                fprintf(f, "\t%s    []byte // slot %d, optional value\n", gn.c_str(), fd.vtableSlot + 1);
                break;
            case FieldKind::Variant:
                fprintf(f, "\t// variant at slot %d (TODO)\n", fd.vtableSlot);
                break;
            }
        }
        fprintf(f, "}\n\n");

        // UnmarshalFromReader.
        fprintf(f, "func (m *%s) UnmarshalFromReader(r *wire.Reader) {\n", typeName);
        emitReads(typeName, fields, "r");
        fprintf(f, "}\n\n");

        // UnmarshalFDB.
        fprintf(f, "func (m *%s) UnmarshalFDB(data []byte) error {\n", typeName);
        fprintf(f, "\tr, err := wire.NewReader(data)\n");
        fprintf(f, "\tif err != nil { return err }\n");
        emitReads(typeName, fields, "r");
        fprintf(f, "\treturn nil\n}\n\n");

        // MarshalInto — KEY IMPROVEMENT: writes ALL fields including nested!
        fprintf(f, "func (m *%s) MarshalInto(obj *wire.ObjectWriter) {\n", typeName);
        bool hasWrites = false;
        for (auto& fd : fields) {
            if (fd.kind == FieldKind::Scalar || fd.kind == FieldKind::DynamicSize ||
                fd.kind == FieldKind::VectorLike || fd.kind == FieldKind::NestedStruct)
                if (fd.size > 0) { hasWrites = true; break; }
        }
        if (hasWrites) fprintf(f, "\tvt := %sVTable\n", typeName);
        for (auto& fd : fields) {
            if (fd.size == 0) continue; // Arena
            auto gn = fieldGoName(fd);
            auto slot = std::string(typeName) + "Slot" + gn;
            switch (fd.kind) {
            case FieldKind::Scalar:
                fprintf(f, "\tobj.%s(int(vt[%s+2]), m.%s)\n", fd.scalar.writer, slot.c_str(), gn.c_str());
                break;
            case FieldKind::DynamicSize:
                fprintf(f, "\tif m.%s != nil {\n\t\tobj.WriteBytes(int(vt[%s+2]), m.%s)\n\t}\n",
                        gn.c_str(), slot.c_str(), gn.c_str());
                break;
            case FieldKind::VectorLike:
                fprintf(f, "\tif m.%s != nil {\n\t\tobj.WriteRawOOL(int(vt[%s+2]), m.%s)\n\t}\n",
                        gn.c_str(), slot.c_str(), gn.c_str());
                break;
            case FieldKind::NestedStruct:
                if (fd.nestedGoType[0])
                    fprintf(f, "\tobj.WriteStruct(int(vt[%s+2]), %sVTable, 8, m.%s.MarshalInto)\n",
                            slot.c_str(), fd.nestedGoType, gn.c_str());
                break;
            default: break;
            }
        }
        fprintf(f, "}\n\n");

        // MarshalFDB — delegates to MarshalInto.
        if (fileId > 0 && !closure.empty()) {
            fprintf(f, "func (m *%s) MarshalFDB() []byte {\n", typeName);
            fprintf(f, "\tw := wire.NewWriter(nil)\n");
            fprintf(f, "\treturn w.WriteMessagePacked(%sTemplate, m.MarshalInto)\n", typeName);
            fprintf(f, "}\n\n");
        }

        // WriteNested helper.
        fprintf(f, "func Write%s(obj *wire.ObjectWriter, parentOffset int", typeName);
        for (auto& fd : fields) {
            if (fd.size == 0 || fd.kind == FieldKind::Optional || fd.kind == FieldKind::NestedStruct) continue;
            auto gn = fieldGoName(fd);
            auto pn = safeParam(gn);
            fprintf(f, ", %s %s", pn.c_str(),
                    fd.kind == FieldKind::Scalar ? fd.scalar.goType : "[]byte");
        }
        fprintf(f, ") {\n");
        fprintf(f, "\tm := %s{", typeName);
        bool first = true;
        for (auto& fd : fields) {
            if (fd.size == 0 || fd.kind == FieldKind::Optional || fd.kind == FieldKind::NestedStruct) continue;
            auto gn = fieldGoName(fd);
            auto pn = safeParam(gn);
            if (!first) fprintf(f, ", ");
            fprintf(f, "%s: %s", gn.c_str(), pn.c_str());
            first = false;
        }
        fprintf(f, "}\n");
        int maxAlign = 4;
        for (auto& fd : fields)
            if (fd.kind == FieldKind::Scalar && fd.size >= 8) maxAlign = 8;
        fprintf(f, "\tobj.WriteStruct(parentOffset, %sVTable, %d, m.MarshalInto)\n", typeName, maxAlign);
        fprintf(f, "}\n\n");

        // --- Vector parsers: derived from the type's field structure ---

        // Every type gets a FlatBuffers vector parser.
        // Elements are nested objects with vtables, read via ReadVectorElementReader.
        emitFBVectorParser(typeName);

        // Types where ALL non-zero fields are DynamicSize also get a
        // VecSerStrategy::String parser. Elements are inline: [len][data] per field.
        if (allFieldsDynamicSize(fields)) {
            emitStringVectorParser(typeName, fields);
        }
    }

private:
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

    // FlatBuffers vector parser: reads vector of nested structs via ReadVectorElementReader.
    void emitFBVectorParser(const char* typeName) {
        fprintf(f, "// Parse%sVectorFromReader reads a FlatBuffers vector of %s.\n", typeName, typeName);
        fprintf(f, "func Parse%sVectorFromReader(r *wire.Reader, slot int) []%s {\n", typeName, typeName);
        fprintf(f, "\tcount, err := r.ReadVectorCount(slot)\n");
        fprintf(f, "\tif err != nil || count == 0 { return nil }\n");
        fprintf(f, "\tresult := make([]%s, 0, count)\n", typeName);
        fprintf(f, "\tfor i := 0; i < count; i++ {\n");
        fprintf(f, "\t\telemR, err := r.ReadVectorElementReader(slot, i)\n");
        fprintf(f, "\t\tif err != nil { continue }\n");
        fprintf(f, "\t\tvar elem %s\n", typeName);
        fprintf(f, "\t\telem.UnmarshalFromReader(elemR)\n");
        fprintf(f, "\t\tresult = append(result, elem)\n");
        fprintf(f, "\t}\n");
        fprintf(f, "\treturn result\n");
        fprintf(f, "}\n\n");
    }

    // VecSerStrategy::String vector parser: elements inline as [len(4)][data] per DynamicSize field.
    // Derived from the type's field structure — not hardcoded.
    void emitStringVectorParser(const char* typeName, const std::vector<FieldDesc>& fields) {
        fprintf(f, "// Parse%sStringVector decodes a VectorRef<%s, VecSerStrategy::String>.\n", typeName, typeName);
        fprintf(f, "// Each element's DynamicSize fields are inline: [len(4)][data] per field.\n");
        fprintf(f, "func Parse%sStringVector(data []byte) []%s {\n", typeName, typeName);
        fprintf(f, "\tif len(data) < 4 { return nil }\n");
        fprintf(f, "\tcount := binary.LittleEndian.Uint32(data[0:4])\n");
        fprintf(f, "\tif count == 0 { return nil }\n");
        fprintf(f, "\tpos := 4\n");
        fprintf(f, "\tresult := make([]%s, 0, count)\n", typeName);
        fprintf(f, "\tfor i := uint32(0); i < count; i++ {\n");
        fprintf(f, "\t\tvar elem %s\n", typeName);
        for (auto& fd : fields) {
            if (fd.size == 0 || fd.kind != FieldKind::DynamicSize) continue;
            auto gn = fieldGoName(fd);
            fprintf(f, "\t\tif pos+4 > len(data) { break }\n");
            fprintf(f, "\t\t{\n");
            fprintf(f, "\t\t\tn := int(binary.LittleEndian.Uint32(data[pos:]))\n");
            fprintf(f, "\t\t\tpos += 4\n");
            fprintf(f, "\t\t\tif n < 0 || pos+n > len(data) { break }\n"); // bounds + sign check
            fprintf(f, "\t\t\telem.%s = make([]byte, n)\n", gn.c_str());
            fprintf(f, "\t\t\tcopy(elem.%s, data[pos:pos+n])\n", gn.c_str());
            fprintf(f, "\t\t\tpos += n\n");
            fprintf(f, "\t\t}\n");
        }
        fprintf(f, "\t\tresult = append(result, elem)\n");
        fprintf(f, "\t}\n");
        fprintf(f, "\treturn result\n");
        fprintf(f, "}\n\n");
    }

    void emitReads(const char* typeName, const std::vector<FieldDesc>& fields, const char* rv) {
        for (auto& fd : fields) {
            auto gn = fieldGoName(fd);
            auto slot = std::string(typeName) + "Slot" + gn;
            switch (fd.kind) {
            case FieldKind::Scalar:
                fprintf(f, "\tif %s.FieldPresent(%s) {\n", rv, slot.c_str());
                fprintf(f, "\t\tm.%s = %s.%s(%s)\n", gn.c_str(), rv, fd.scalar.reader, slot.c_str());
                fprintf(f, "\t}\n");
                break;
            case FieldKind::DynamicSize:
            case FieldKind::VectorLike:
                fprintf(f, "\tif %s.FieldPresent(%s) {\n", rv, slot.c_str());
                fprintf(f, "\t\tm.%s = %s.ReadBytes(%s)\n", gn.c_str(), rv, slot.c_str());
                fprintf(f, "\t}\n");
                break;
            case FieldKind::NestedStruct:
                if (fd.nestedGoType[0]) {
                    fprintf(f, "\tif nr, err := %s.ReadNestedReader(%s); err == nil {\n", rv, slot.c_str());
                    fprintf(f, "\t\tm.%s.UnmarshalFromReader(nr)\n", gn.c_str());
                    fprintf(f, "\t}\n");
                }
                break;
            case FieldKind::Optional:
                fprintf(f, "\tif %s.FieldPresent(%s) && %s.ReadUint8(%s) > 0 {\n",
                        rv, slot.c_str(), rv, slot.c_str());
                fprintf(f, "\t\tm.%s = %s.ReadBytes(%s + 1)\n", gn.c_str(), rv, slot.c_str());
                fprintf(f, "\t\tm.Has%s = true\n", gn.c_str());
                fprintf(f, "\t}\n");
                break;
            default: break;
            }
        }
    }
};

// ============================================================
// 6. VTable extraction helpers (reused from main.cpp)
// ============================================================

std::vector<std::vector<uint16_t>> extractVTableClosure(const uint8_t* data, int size) {
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

std::vector<uint16_t> extractMessageVTable(const uint8_t* data, int size) {
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

// ============================================================
// 7. extractType — single call, no flags
// ============================================================

template <class T, bool SkipObjectWriter = false>
void extractType(const char* outDir, const char* name) {
    int pipefd[2];
    pipe(pipefd);
    pid_t pid = fork();
    if (pid == 0) {
        close(pipefd[0]);

        // Collect fields.
        FieldCollector<T> collector;
        T msg{};
        if constexpr (serializable_traits<T>::value)
            serializable_traits<T>::serialize(collector, msg);
        else
            msg.serialize(collector);

        // Get authoritative vtable from ObjectWriter (when possible).
        std::vector<std::vector<uint16_t>> closure;
        std::vector<uint16_t> rootVTable;
        if constexpr (!SkipObjectWriter && requires { T::file_identifier; }) {
            ObjectWriter wr(IncludeVersion(currentProtocolVersion()));
            wr.serialize(T::file_identifier, msg);
            auto bytes = wr.toStringRef();
            closure = extractVTableClosure(bytes.begin(), bytes.size());
            rootVTable = extractMessageVTable(bytes.begin(), bytes.size());
        }
        auto& vt = rootVTable.empty() ? collector.vtable : rootVTable;

        // Emit.
        FILE* pf = fdopen(pipefd[1], "w");
        GoEmitter emitter{pf};
        emitter.emitFull(name, collector.fields, vt, getFileId<T>(), closure);
        fclose(pf);
        _exit(0);
    }

    close(pipefd[1]);
    std::string lowerName = toLower(name);
    std::string path = std::string(outDir) + "/" + lowerName + "_poc.go";
    FILE* out = fopen(path.c_str(), "w");
    if (!out) { perror(path.c_str()); goto wait; }
    {
        char buf[4096];
        ssize_t n;
        while ((n = read(pipefd[0], buf, sizeof(buf))) > 0)
            fwrite(buf, 1, n, out);
        fclose(out);
    }

wait:
    close(pipefd[0]);
    int status;
    waitpid(pid, &status, 0);
    if (WIFEXITED(status) && WEXITSTATUS(status) == 0)
        fprintf(stderr, "POC OK %s → %s_poc.go\n", name, lowerName.c_str());
    else
        fprintf(stderr, "POC SKIP %s\n", name);
}

// ============================================================
// 8. Main
// ============================================================

int main(int argc, char** argv) {
    if (argc < 2) { fprintf(stderr, "Usage: %s <output-dir>\n", argv[0]); return 1; }
    const char* outDir = argv[1];

    TLSConfig tlsConfig;
    g_network = newNet2(tlsConfig, false, false);
    FlowTransport::createInstance(false, 1, WLTOKEN_FIRST_AVAILABLE, nullptr);

    // Extract ALL types — no flags, no EmitStructs/EmitVectorParser.
    extractType<GetValueReply>(outDir, "GetValueReply");
    extractType<GetKeyValuesReply>(outDir, "GetKeyValuesReply");
    extractType<GetKeyReply>(outDir, "GetKeyReply");
    extractType<GetReadVersionReply>(outDir, "GetReadVersionReply");
    extractType<GetKeyServerLocationsReply>(outDir, "GetKeyServerLocationsReply");
    extractType<CommitID>(outDir, "CommitID");

    extractType<GetValueRequest>(outDir, "GetValueRequest");
    extractType<GetKeyValuesRequest>(outDir, "GetKeyValuesRequest");
    extractType<GetKeyRequest>(outDir, "GetKeyRequest");
    extractType<GetReadVersionRequest>(outDir, "GetReadVersionRequest");
    extractType<GetKeyServerLocationsRequest>(outDir, "GetKeyServerLocationsRequest");
    extractType<CommitTransactionRequest>(outDir, "CommitTransactionRequest");
    extractType<OpenDatabaseCoordRequest>(outDir, "OpenDatabaseCoordRequest");

    extractType<SpanContext>(outDir, "SpanContext");
    extractType<KeySelectorRef>(outDir, "KeySelectorRef");
    extractType<MutationRef>(outDir, "MutationRef");
    extractType<KeyRangeRef>(outDir, "KeyRangeRef");
    extractType<CommitTransactionRef>(outDir, "CommitTransactionRef");
    extractType<ReadOptions>(outDir, "ReadOptions");
    extractType<Error>(outDir, "Error");
    extractType<KeyValueRef>(outDir, "KeyValueRef");

    extractType<ClientDBInfo>(outDir, "ClientDBInfo");
    extractType<GrvProxyInterface, true>(outDir, "GrvProxyInterface");
    extractType<CommitProxyInterface, true>(outDir, "CommitProxyInterface");
    extractType<StorageServerInterface, true>(outDir, "StorageServerInterface");
    extractType<NetworkAddress>(outDir, "NetworkAddress");
    extractType<IPAddress>(outDir, "IPAddress");
    extractType<TenantInfo>(outDir, "TenantInfo");
    extractType<ReplyPromise<GetValueReply>>(outDir, "ReplyPromise");
    extractType<Endpoint>(outDir, "Endpoint");
    extractType<NetworkAddressList>(outDir, "NetworkAddressList");

    using LocationPair = std::pair<KeyRangeRef, std::vector<StorageServerInterface>>;
    extractType<LocationPair>(outDir, "LocationPair");

    fprintf(stderr, "POC Done.\n");
    return 0;
}
