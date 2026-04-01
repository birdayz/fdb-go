// fdb-schema-extract — Extracts vtables, closures, and field traits from real FDB types.
// Outputs a single Go source file with constants for all types.
//
// Build: compiled inside FDB's Docker environment against fdbclient + fdbrpc + flow.
// Output: vtables_generated.go

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
#include <sstream>
#include <sys/stat.h>
#include <unistd.h>
#include <sys/wait.h>

// Defined in name_capture.cpp.
extern std::map<std::string, std::string>& nameRegistry();
extern void captureAllNames();

// ============================================================
// Type metadata extraction
// ============================================================

struct FieldInfo {
    std::string name;
    const char* trait;
    uint32_t size;
    uint32_t align;
    bool indirection;
    const char* goType;      // Go type string (e.g. "int64", "bool", "[]byte")
    const char* readerMethod; // e.g. "ReadInt64", "ReadBool", "ReadBytes"
    const char* writerMethod; // e.g. "WriteInt64", "WriteBool", "WriteBytes"
    const char* cppTypeName; // C++ type name for nested structs (e.g. "TenantInfo")
};

template <class T>
const char* classifyTrait() {
    using namespace detail;
    if constexpr (is_scalar<T>) return "scalar";
    else if constexpr (is_dynamic_size<T>) return "dynamic_size";
    else if constexpr (is_vector_like<T>) return "vector_like";
    else if constexpr (is_union_like<T>) return "union_like";
    else if constexpr (is_struct_like<T>) return "struct_like";
    else return "serialize_member";
}

// Map a C++ type to its Go representation for code generation.
// Returns {goType, readerMethod, writerMethod}.
template <class T>
struct GoTypeMapping {
    static constexpr const char* goType = "[]byte";
    static constexpr const char* reader = "ReadBytes";
    static constexpr const char* writer = "WriteBytes";
};

// Scalar specializations — the compiler resolves the exact C++ type.
template<> struct GoTypeMapping<bool> {
    static constexpr const char* goType = "bool";
    static constexpr const char* reader = "ReadBool";
    static constexpr const char* writer = "WriteBool";
};
template<> struct GoTypeMapping<int8_t> {
    static constexpr const char* goType = "int8";
    static constexpr const char* reader = "ReadInt8";
    static constexpr const char* writer = "WriteInt8";
};
template<> struct GoTypeMapping<uint8_t> {
    static constexpr const char* goType = "uint8";
    static constexpr const char* reader = "ReadUint8";
    static constexpr const char* writer = "WriteUint8";
};
template<> struct GoTypeMapping<int16_t> {
    static constexpr const char* goType = "int16";
    static constexpr const char* reader = "ReadInt16";
    static constexpr const char* writer = "WriteInt16";
};
template<> struct GoTypeMapping<uint16_t> {
    static constexpr const char* goType = "uint16";
    static constexpr const char* reader = "ReadUint16";
    static constexpr const char* writer = "WriteUint16";
};
template<> struct GoTypeMapping<int32_t> {
    static constexpr const char* goType = "int32";
    static constexpr const char* reader = "ReadInt32";
    static constexpr const char* writer = "WriteInt32";
};
// int and int32_t are the same type on most platforms — skip duplicate.
template<> struct GoTypeMapping<uint32_t> {
    static constexpr const char* goType = "uint32";
    static constexpr const char* reader = "ReadUint32";
    static constexpr const char* writer = "WriteUint32";
};
template<> struct GoTypeMapping<int64_t> {
    static constexpr const char* goType = "int64";
    static constexpr const char* reader = "ReadInt64";
    static constexpr const char* writer = "WriteInt64";
};
template<> struct GoTypeMapping<uint64_t> {
    static constexpr const char* goType = "uint64";
    static constexpr const char* reader = "ReadUint64";
    static constexpr const char* writer = "WriteUint64";
};
template<> struct GoTypeMapping<double> {
    static constexpr const char* goType = "float64";
    static constexpr const char* reader = "ReadFloat64";
    static constexpr const char* writer = "WriteFloat64";
};
template<> struct GoTypeMapping<UID> {
    static constexpr const char* goType = "[16]byte";
    static constexpr const char* reader = "ReadUID";
    static constexpr const char* writer = "WriteUID";
};

// Get a human-readable C++ type name for code generation comments and
// nested struct dispatch. Returns "" for scalar/dynamic types (not needed).
template <class T>
const char* getCppTypeName() {
    // Only needed for serialize_member types (nested structs).
    // Add entries as needed for types we compose in MarshalFDB.
    return "";
}
template<> const char* getCppTypeName<SpanContext>() { return "SpanContext"; }
template<> const char* getCppTypeName<TenantInfo>() { return "TenantInfo"; }
template<> const char* getCppTypeName<CommitTransactionRef>() { return "CommitTransactionRef"; }
template<> const char* getCppTypeName<KeySelectorRef>() { return "KeySelectorRef"; }
template<> const char* getCppTypeName<ReadOptions>() { return "ReadOptions"; }
// ReplyPromise<T> — all instantiations share the same vtable. Add each used instantiation.
template<> const char* getCppTypeName<ReplyPromise<GetValueReply>>() { return "ReplyPromise"; }
template<> const char* getCppTypeName<ReplyPromise<GetKeyValuesReply>>() { return "ReplyPromise"; }
template<> const char* getCppTypeName<ReplyPromise<GetKeyReply>>() { return "ReplyPromise"; }
template<> const char* getCppTypeName<ReplyPromise<GetReadVersionReply>>() { return "ReplyPromise"; }
template<> const char* getCppTypeName<ReplyPromise<GetKeyServerLocationsReply>>() { return "ReplyPromise"; }
template<> const char* getCppTypeName<ReplyPromise<CommitID>>() { return "ReplyPromise"; }
template<> const char* getCppTypeName<ReplyPromise<Void>>() { return "ReplyPromise"; }

struct TypeVisitor {
    static constexpr bool isDeserializing = false;
    static constexpr bool isSerializing = false;
    static constexpr bool is_fb_visitor = true;

    std::vector<FieldInfo> fields;
    std::vector<uint16_t> vtable;

    TypeVisitor& context() { return *this; }
    ProtocolVersion protocolVersion() const { return currentProtocolVersion(); }

    template <class... Members>
    void operator()(Members&... members) {
        const auto* vt = detail::get_vtable<std::decay_t<Members>...>();
        vtable.assign(vt->begin(), vt->end());
        (pushField<std::decay_t<Members>>(), ...);
    }
private:
    template <class T> void pushField() {
        using namespace detail;
        auto goType = GoTypeMapping<T>::goType;
        auto reader = GoTypeMapping<T>::reader;
        auto writer = GoTypeMapping<T>::writer;
        // If the default GoTypeMapping applies ([]byte) but the field is actually
        // a scalar, fall back to size-based mapping. Catches enum types, typedef'd
        // scalars, and other types without explicit GoTypeMapping specialization.
        if (is_scalar<T> && std::string(goType) == "[]byte") {
            switch (fb_size<T>) {
                case 1: goType = "uint8"; reader = "ReadUint8"; writer = "WriteUint8"; break;
                case 2: goType = "uint16"; reader = "ReadUint16"; writer = "WriteUint16"; break;
                case 4: goType = "uint32"; reader = "ReadUint32"; writer = "WriteUint32"; break;
                case 8: goType = "uint64"; reader = "ReadUint64"; writer = "WriteUint64"; break;
            }
        }
        fields.push_back(FieldInfo{"", classifyTrait<T>(),
            (uint32_t)fb_size<T>, (uint32_t)fb_align<T>, use_indirection<T>,
            goType, reader, writer,
            getCppTypeName<T>()});
    }
};

template <class T>
constexpr uint32_t getFileId() {
    if constexpr (requires { T::file_identifier; }) return T::file_identifier;
    else return 0;
}

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

// Read the vtable at a given object position.
std::vector<uint16_t> readVTableAt(const uint8_t* data, int size, int objPos) {
    if (objPos + 4 > size) return {};
    int32_t vtOff; memcpy(&vtOff, data + objPos, 4);
    int vtPos = objPos - vtOff;
    if (vtPos < 0 || vtPos + 2 > size) return {};
    uint16_t vtSize; memcpy(&vtSize, data + vtPos, 2);
    if (vtSize < 6 || vtSize > 128 || vtSize % 2 != 0 || vtPos + vtSize > size) return {};
    std::vector<uint16_t> vt(vtSize / 2);
    for (int i = 0; i < vtSize / 2; i++) memcpy(&vt[i], data + vtPos + i * 2, 2);
    return vt;
}

// Extract the MESSAGE type's vtable from serialized bytes.
// Wire layout: [rootOff][protocolVersion][vtables...][FakeRoot][Message][nested...]
// FakeRoot has vtable {6,8,4} with one RelOff field at offset 4 pointing to Message.
// We follow: root -> FakeRoot -> Message -> Message's vtable.
std::vector<uint16_t> extractMessageVTable(const uint8_t* data, int size) {
    if (size < 16) return {};
    int off = 0;
    if (size >= 16 && data[7] == 0x0F && data[6] == 0xDB) off = 8;
    if (off + 8 > size) return {};
    // Follow root offset to FakeRoot object.
    uint32_t rootOff; memcpy(&rootOff, data + off, 4);
    int fakeRootPos = off + (int)rootOff;
    if (fakeRootPos + 8 > size) return {};
    // FakeRoot field at offset 4: RelOff pointing to the actual message object.
    uint32_t msgRelOff; memcpy(&msgRelOff, data + fakeRootPos + 4, 4);
    int msgPos = fakeRootPos + 4 + (int)msgRelOff;
    // Read vtable from the message object.
    return readVTableAt(data, size, msgPos);
}

std::vector<std::string> splitNames(const std::string& csv) {
    std::vector<std::string> result;
    std::istringstream ss(csv);
    std::string token;
    while (std::getline(ss, token, ',')) {
        auto s = token.find_first_not_of(" \t"), e = token.find_last_not_of(" \t");
        if (s != std::string::npos) result.push_back(token.substr(s, e - s + 1));
    }
    return result;
}

// ============================================================
// Go code generation — accumulates into a single file
// ============================================================

struct GoEmitter {
    FILE* f;

    void header() {
        fprintf(f, "// Code generated by fdb-schema-extract from FDB 7.3.75. DO NOT EDIT.\n\n");
        fprintf(f, "package types\n\n");
        fprintf(f, "import \"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire\"\n\n");
    }

    void emitVTable(const char* name, const std::vector<uint16_t>& vt) {
        fprintf(f, "var %sVTable = wire.VTable{", name);
        for (size_t i = 0; i < vt.size(); i++) {
            if (i) fprintf(f, ", ");
            fprintf(f, "%u", vt[i]);
        }
        fprintf(f, "}\n");
    }

    void emitFileID(const char* name, uint32_t id) {
        if (id > 0) fprintf(f, "const %sFileID uint32 = %u\n", name, id);
    }

    void emitClosure(const char* name, const std::vector<std::vector<uint16_t>>& closure) {
        if (closure.empty()) return;
        fprintf(f, "var %sVTableClosure = []wire.VTable{\n", name);
        for (auto& vt : closure) {
            fprintf(f, "\t{");
            for (size_t i = 0; i < vt.size(); i++) {
                if (i) fprintf(f, ", ");
                fprintf(f, "%u", vt[i]);
            }
            fprintf(f, "},\n");
        }
        fprintf(f, "}\n");
    }

    void emitFieldComment(const char* name, const std::vector<FieldInfo>& fields) {
        fprintf(f, "// %s fields:\n", name);
        int readerSlot = 0;
        for (size_t i = 0; i < fields.size(); i++) {
            auto& fi = fields[i];
            fprintf(f, "//   slot %d: %s — %s, size=%u, align=%u%s\n",
                    readerSlot, fi.name.c_str(), fi.trait, fi.size, fi.align,
                    fi.indirection ? ", indirection" : "");
            readerSlot += (strcmp(fi.trait, "union_like") == 0) ? 2 : 1;
        }
    }

    // Sanitize a C++ field name into a valid Go identifier.
    // "LoadBalancedReply::penalty" → "Penalty"
    // "const_cast<KeyRef&>(begin)" → "Begin"
    // "m_Flags" → "MFlags"
    static std::string sanitizeGoName(const std::string& name) {
        std::string s = name;
        // Strip const_cast<...>() wrapper.
        auto ccPos = s.find("const_cast");
        if (ccPos != std::string::npos) {
            auto paren = s.find('(', ccPos);
            auto endParen = s.rfind(')');
            if (paren != std::string::npos && endParen != std::string::npos)
                s = s.substr(paren + 1, endParen - paren - 1);
        }
        // Take only the part after last "::" or ".".
        // Handles "Base::field", "v.tenantId", "p.first".
        auto colonPos = s.rfind("::");
        if (colonPos != std::string::npos) s = s.substr(colonPos + 2);
        auto dotPos = s.rfind('.');
        if (dotPos != std::string::npos) s = s.substr(dotPos + 1);
        // Strip leading underscores / m_ prefix.
        if (s.size() > 2 && s[0] == 'm' && s[1] == '_') s = s.substr(2);
        // Remove any non-alphanumeric chars.
        std::string clean;
        for (char c : s) { if (isalnum(c) || c == '_') clean += c; }
        // Capitalize first letter.
        if (!clean.empty()) clean[0] = toupper(clean[0]);
        return clean;
    }

    // Emit Go constants mapping field names to Reader slot indices.
    void emitSlotConstants(const char* typeName, const std::vector<FieldInfo>& fields) {
        if (fields.empty()) return;
        fprintf(f, "const (\n");
        int readerSlot = 0;
        for (size_t i = 0; i < fields.size(); i++) {
            auto& fi = fields[i];
            std::string goName = sanitizeGoName(fi.name);
            fprintf(f, "\t%sSlot%s = %d\n", typeName, goName.c_str(), readerSlot);
            readerSlot += (strcmp(fi.trait, "union_like") == 0) ? 2 : 1;
        }
        fprintf(f, ")\n");
    }

    void separator() { fprintf(f, "\n"); }

    // Emit a Go struct definition with fields annotated by slot and reader method.
    void emitStruct(const char* typeName, const std::vector<FieldInfo>& fields) {
        fprintf(f, "type %s struct {\n", typeName);
        int readerSlot = 0;
        for (auto& fi : fields) {
            std::string goName = sanitizeGoName(fi.name);
            if (strcmp(fi.trait, "union_like") == 0) {
                // Optional: emit Has+Value fields.
                fprintf(f, "\tHas%s bool   // slot %d, Optional, presence flag\n", goName.c_str(), readerSlot);
                fprintf(f, "\t%s    []byte // slot %d, Optional, ReadBytes\n", goName.c_str(), readerSlot + 1);
                readerSlot += 2;
            } else if (strcmp(fi.trait, "serialize_member") == 0 || strcmp(fi.trait, "struct_like") == 0) {
                if (fi.cppTypeName[0] != '\0') {
                    // Known nested struct — emit as a real field.
                    fprintf(f, "\t%s %s // slot %d, nested\n",
                            goName.c_str(), fi.cppTypeName, readerSlot);
                } else {
                    // Unknown nested struct — comment only.
                    fprintf(f, "\t// %s: nested struct at slot %d — use ReadNestedReader(%sSlot%s)\n",
                            goName.c_str(), readerSlot, typeName, goName.c_str());
                }
                readerSlot++;
            } else {
                fprintf(f, "\t%s %s // slot %d, %s\n", goName.c_str(), fi.goType, readerSlot, fi.readerMethod);
                readerSlot++;
            }
        }
        fprintf(f, "}\n\n");
    }

    // Emit UnmarshalFDB with all reads inlined in one function.
    void emitUnmarshalFDB(const char* typeName, const std::vector<FieldInfo>& fields) {
        fprintf(f, "func (m *%s) UnmarshalFDB(data []byte) error {\n", typeName);
        fprintf(f, "\tr, err := wire.NewReader(data)\n");
        fprintf(f, "\tif err != nil {\n\t\treturn err\n\t}\n");

        int readerSlot = 0;
        for (auto& fi : fields) {
            std::string goName = sanitizeGoName(fi.name);
            std::string slotConst = std::string(typeName) + "Slot" + goName;

            if (strcmp(fi.trait, "union_like") == 0) {
                // Optional: check tag, read value.
                fprintf(f, "\tif r.FieldPresent(%s) && r.ReadUint8(%s) > 0 {\n",
                        slotConst.c_str(), slotConst.c_str());
                fprintf(f, "\t\tm.%s = r.ReadBytes(%s + 1)\n", goName.c_str(), slotConst.c_str());
                fprintf(f, "\t\tm.Has%s = true\n", goName.c_str());
                fprintf(f, "\t}\n");
                readerSlot += 2;
            } else if (strcmp(fi.trait, "serialize_member") == 0 || strcmp(fi.trait, "struct_like") == 0) {
                // Nested struct: emit comment, skip.
                fprintf(f, "\t// %s (slot %d): nested struct — use r.ReadNestedReader(%s)\n",
                        goName.c_str(), readerSlot, slotConst.c_str());
                readerSlot++;
            } else {
                // Scalar, dynamic_size, vector_like — direct read.
                fprintf(f, "\tif r.FieldPresent(%s) {\n", slotConst.c_str());
                fprintf(f, "\t\tm.%s = r.%s(%s)\n", goName.c_str(), fi.readerMethod, slotConst.c_str());
                fprintf(f, "\t}\n");
                readerSlot++;
            }
        }
        fprintf(f, "\treturn nil\n}\n\n");
    }

    // Emit MarshalInto — writes all non-optional, non-struct fields into an ObjectWriter.
    // Used by WriteStruct callers and MarshalStructBlob.
    void emitMarshalInto(const char* typeName, const std::vector<FieldInfo>& fields) {
        // Check if there are any writable fields.
        bool hasWritable = false;
        for (auto& fi : fields) {
            if (strcmp(fi.trait, "union_like") != 0 &&
                strcmp(fi.trait, "serialize_member") != 0 &&
                strcmp(fi.trait, "struct_like") != 0 &&
                fi.size > 0) {
                hasWritable = true;
                break;
            }
        }
        fprintf(f, "func (m *%s) MarshalInto(obj *wire.ObjectWriter) {\n", typeName);
        if (hasWritable) fprintf(f, "\tvt := %sVTable\n", typeName);

        int readerSlot = 0;
        for (auto& fi : fields) {
            if (strcmp(fi.trait, "union_like") == 0) {
                readerSlot += 2;
                continue;
            }
            if (isSkippedField(fi)) {
                readerSlot++;
                continue;
            }

            std::string goName = sanitizeGoName(fi.name);
            std::string slotConst = std::string(typeName) + "Slot" + goName;
            fprintf(f, "\tobj.%s(int(vt[%s+2]), m.%s)\n",
                    fi.writerMethod, slotConst.c_str(), goName.c_str());
            readerSlot++;
        }
        fprintf(f, "}\n\n");
    }

    // Make a safe Go parameter name (lowercase first letter, avoid keywords).
    static std::string safeParamName(const std::string& goName) {
        std::string p = goName;
        if (!p.empty()) p[0] = tolower(p[0]);
        // Avoid Go reserved words.
        if (p == "type" || p == "range" || p == "error" || p == "func" ||
            p == "map" || p == "chan" || p == "var" || p == "select" ||
            p == "default" || p == "interface" || p == "struct" || p == "package")
            p += "_";
        return p;
    }

    static bool isSkippedField(const FieldInfo& fi) {
        if (strcmp(fi.trait, "union_like") == 0) return true;
        if (strcmp(fi.trait, "serialize_member") == 0 || strcmp(fi.trait, "struct_like") == 0) return true;
        if (fi.size == 0) return true; // zero-size (Arena)
        return false;
    }

    // Emit MarshalStructBlob — for types used as vector elements.
    // Produces: func MarshalXxx(field1, field2, ...) []byte
    void emitMarshalStructBlob(const char* typeName, const std::vector<FieldInfo>& fields) {
        // Compute max field alignment.
        int maxAlign = 4;
        for (auto& fi : fields)
            if ((int)fi.align > maxAlign) maxAlign = fi.align;

        // Build parameter list from struct fields.
        fprintf(f, "func Marshal%s(", typeName);
        bool first = true;
        for (auto& fi : fields) {
            if (isSkippedField(fi)) continue;
            std::string goName = sanitizeGoName(fi.name);
            // Lowercase first letter for parameter name.
            std::string paramName = safeParamName(goName);

            if (!first) fprintf(f, ", ");
            fprintf(f, "%s %s", paramName.c_str(), fi.goType);
            first = false;
        }
        fprintf(f, ") []byte {\n");
        fprintf(f, "\tm := %s{", typeName);
        first = true;
        for (auto& fi : fields) {
            if (isSkippedField(fi)) continue;
            std::string goName = sanitizeGoName(fi.name);
            std::string paramName = safeParamName(goName);

            if (!first) fprintf(f, ", ");
            fprintf(f, "%s: %s", goName.c_str(), paramName.c_str());
            first = false;
        }
        fprintf(f, "}\n");
        fprintf(f, "\treturn wire.MarshalStructBlob(%sVTable, m.MarshalInto)\n", typeName);
        fprintf(f, "}\n\n");
    }

    // Emit WriteNested — helper for writing this type as a nested struct in a parent.
    // Produces: func WriteXxx(obj *ObjectWriter, parentOffset int, field1, field2, ...)
    void emitWriteNested(const char* typeName, const std::vector<FieldInfo>& fields) {
        int maxAlign = 4;
        for (auto& fi : fields)
            if ((int)fi.align > maxAlign) maxAlign = fi.align;

        fprintf(f, "func Write%s(obj *wire.ObjectWriter, parentOffset int", typeName);
        for (auto& fi : fields) {
            if (isSkippedField(fi)) continue;
            std::string goName = sanitizeGoName(fi.name);
            std::string paramName = safeParamName(goName);

            fprintf(f, ", %s %s", paramName.c_str(), fi.goType);
        }
        fprintf(f, ") {\n");
        fprintf(f, "\tm := %s{", typeName);
        bool first = true;
        for (auto& fi : fields) {
            if (isSkippedField(fi)) continue;
            std::string goName = sanitizeGoName(fi.name);
            std::string paramName = safeParamName(goName);

            if (!first) fprintf(f, ", ");
            fprintf(f, "%s: %s", goName.c_str(), paramName.c_str());
            first = false;
        }
        fprintf(f, "}\n");
        fprintf(f, "\tobj.WriteStruct(parentOffset, %sVTable, %d, m.MarshalInto)\n",
                typeName, maxAlign);
        fprintf(f, "}\n\n");
    }

    // Emit MarshalFDB using the template (emitted separately by extractType).
    // Only emitted for types with a file_identifier.
    void emitMarshalFDB(const char* typeName, const std::vector<FieldInfo>& fields,
                        uint32_t fileId, bool hasClosure) {
        if (fileId == 0) return;

        // MarshalFDB references {TypeName}Template (always emitted by extractType).
        fprintf(f, "func (m *%s) MarshalFDB() []byte {\n", typeName);
        fprintf(f, "\tw := wire.NewWriter(nil)\n");
        if (hasClosure) {
            fprintf(f, "\treturn w.WriteMessagePacked(%sTemplate, func(obj *wire.ObjectWriter) {\n",
                    typeName);
        } else {
            int maxAlign = 4;
            for (auto& fi : fields)
                if ((int)fi.align > maxAlign) maxAlign = fi.align;
            fprintf(f, "\treturn w.WriteMessage(%sFileID, %sVTable, %d, func(obj *wire.ObjectWriter) {\n",
                    typeName, typeName, maxAlign);
        }

        int readerSlot = 0;
        for (auto& fi : fields) {
            if (strcmp(fi.trait, "union_like") == 0) {
                readerSlot += 2;
                continue;
            }
            if (fi.size == 0) { readerSlot++; continue; } // zero-size (Arena)

            std::string goName = sanitizeGoName(fi.name);
            std::string slotConst = std::string(typeName) + "Slot" + goName;

            if ((strcmp(fi.trait, "serialize_member") == 0 || strcmp(fi.trait, "struct_like") == 0)
                && fi.cppTypeName[0] != '\0') {
                // Known nested struct — use MarshalInto via WriteStruct.
                // Compute max alignment for the nested type from its vtable.
                fprintf(f, "\t\tobj.WriteStruct(int(%sVTable[%s+2]), %sVTable, 8, m.%s.MarshalInto)\n",
                        typeName, slotConst.c_str(), fi.cppTypeName, goName.c_str());
            } else if (strcmp(fi.trait, "serialize_member") != 0 && strcmp(fi.trait, "struct_like") != 0) {
                // Scalar/bytes field — direct write.
                fprintf(f, "\t\tobj.%s(int(%sVTable[%s+2]), m.%s)\n",
                        fi.writerMethod, typeName, slotConst.c_str(), goName.c_str());
            }
            // Unknown nested struct (cppTypeName empty) — skip, needs manual handling.
            readerSlot++;
        }
        fprintf(f, "\t})\n}\n");
    }
};

// Convert TypeName to lowercase filename: "GetValueReply" → "getvaluereply"
std::string toLowerFilename(const char* name) {
    std::string s;
    for (const char* p = name; *p; p++) s += tolower(*p);
    return s;
}

// Extract metadata for one type and write to per-type _generated.go file.
// For custom types (EmitStructs=false), also writes _custom.go stub if it doesn't exist.
template <class T, bool SkipObjectWriter = false, bool EmitStructs = true>
void extractType(const char* outDir, const char* name) {
    int pipefd[2];
    pipe(pipefd);

    pid_t pid = fork();
    if (pid == 0) {
        close(pipefd[0]);

        TypeVisitor visitor;
        T msg{};
        if constexpr (serializable_traits<T>::value)
            serializable_traits<T>::serialize(visitor, msg);
        else
            msg.serialize(visitor);

        auto it = nameRegistry().find(name);
        auto names = (it != nameRegistry().end()) ? splitNames(it->second) : std::vector<std::string>{};
        for (size_t i = 0; i < visitor.fields.size(); i++)
            visitor.fields[i].name = (i < names.size()) ? names[i] : "field_" + std::to_string(i);

        std::vector<std::vector<uint16_t>> closure;
        std::vector<uint16_t> rootVTable;
        if constexpr (!SkipObjectWriter && requires { T::file_identifier; }) {
            ObjectWriter wr(IncludeVersion(currentProtocolVersion()));
            wr.serialize(T::file_identifier, msg);
            auto bytes = wr.toStringRef();
            closure = extractVTableClosure(bytes.begin(), bytes.size());
            rootVTable = extractMessageVTable(bytes.begin(), bytes.size());
        }
        auto& emitVT = rootVTable.empty() ? visitor.vtable : rootVTable;

        FILE* pf = fdopen(pipefd[1], "w");
        GoEmitter e{pf};

        // All types get: header, field comments, slot constants, vtable, fileID, closure.
        e.header();
        e.emitFieldComment(name, visitor.fields);
        e.emitSlotConstants(name, visitor.fields);
        e.emitVTable(name, emitVT);
        e.emitFileID(name, getFileId<T>());
        e.emitClosure(name, closure);

        // All types get struct + UnmarshalFDB + MarshalInto + helpers.
        e.emitStruct(name, visitor.fields);
        e.emitUnmarshalFDB(name, visitor.fields);
        e.emitMarshalInto(name, visitor.fields);
        // WriteNested for ALL types — any type can be used as a nested struct.
        e.emitWriteNested(name, visitor.fields);
        // MarshalStructBlob for simple types and nested types (no file_identifier).
        // Skip for custom types with file_identifier — they have hand-written
        // MarshalXxx in _custom.go that would conflict.
        if constexpr (EmitStructs) {
            e.emitMarshalStructBlob(name, visitor.fields);
        } else if (getFileId<T>() == 0) {
            e.emitMarshalStructBlob(name, visitor.fields);
        }

        if constexpr (EmitStructs) {
            // Simple type: also emit MarshalFDB in _generated.go.
            e.emitMarshalFDB(name, visitor.fields, getFileId<T>(), !closure.empty());
        }
        // Template is always emitted (custom types use it from _custom.go).
        if (getFileId<T>() != 0 && !closure.empty()) {
            int maxAlign = 4;
            for (auto& fi : visitor.fields)
                if ((int)fi.align > maxAlign) maxAlign = fi.align;
            fprintf(pf, "var %sTemplate = wire.NewMessageTemplate(\n", name);
            fprintf(pf, "\t%sFileID, %sVTable, %d, %sVTableClosure,\n)\n\n",
                    name, name, maxAlign, name);
        }

        e.separator();
        fclose(pf);
        _exit(0);
    }

    close(pipefd[1]);

    // Read child output → write to {name}_generated.go.
    std::string lowerName = toLowerFilename(name);
    std::string genPath = std::string(outDir) + "/" + lowerName + "_generated.go";
    FILE* genFile = fopen(genPath.c_str(), "w");
    if (!genFile) { perror(genPath.c_str()); goto wait; }
    {
        char buf[4096];
        ssize_t n;
        while ((n = read(pipefd[0], buf, sizeof(buf))) > 0)
            fwrite(buf, 1, n, genFile);
        fclose(genFile);
    }

wait:
    close(pipefd[0]);
    int status;
    waitpid(pid, &status, 0);
    if (!(WIFEXITED(status) && WEXITSTATUS(status) == 0)) {
        fprintf(stderr, "SKIP %s\n", name);
        unlink(genPath.c_str());
        return;
    }
    fprintf(stderr, "OK %s → %s_generated.go\n", name, lowerName.c_str());

    // For custom types: emit _custom.go stub if it doesn't exist.
    if constexpr (!EmitStructs) {
        std::string customPath = std::string(outDir) + "/" + lowerName + "_custom.go";
        struct stat st;
        if (stat(customPath.c_str(), &st) == 0) {
            fprintf(stderr, "   %s_custom.go exists, skipping stub\n", lowerName.c_str());
            return;
        }
        FILE* cf = fopen(customPath.c_str(), "w");
        if (!cf) { perror(customPath.c_str()); return; }
        fprintf(cf, "package types\n\n");
        fprintf(cf, "// %s has custom MarshalFDB logic.\n", name);
        fprintf(cf, "// UnmarshalFDB and MarshalInto are generated in %s_generated.go.\n", lowerName.c_str());
        fprintf(cf, "// Fill in MarshalFDB below (use the generated Template and MarshalInto).\n\n");
        // Only emit MarshalFDB stub for types with file_identifier (top-level messages).
        if (getFileId<T>() != 0) {
            fprintf(cf, "func (m *%s) MarshalFDB() []byte {\n", name);
            fprintf(cf, "\tpanic(\"%s.MarshalFDB not implemented\")\n", name);
            fprintf(cf, "}\n");
        }
        fclose(cf);
        fprintf(stderr, "   %s_custom.go stub created\n", lowerName.c_str());
    }
}

// ============================================================
// Main
// ============================================================

int main(int argc, char** argv) {
    if (argc < 2) { fprintf(stderr, "Usage: %s <output-dir>\n", argv[0]); return 1; }
    const char* outDir = argv[1];

    TLSConfig tlsConfig;
    g_network = newNet2(tlsConfig, false, false);
    FlowTransport::createInstance(false, 1, WLTOKEN_FIRST_AVAILABLE, nullptr);

    captureAllNames();

    // --- Reply types: GENERATED struct + template + UnmarshalFDB + MarshalFDB ---
    extractType<GetValueReply>(outDir, "GetValueReply");
    extractType<GetKeyValuesReply>(outDir, "GetKeyValuesReply");
    extractType<GetKeyReply>(outDir, "GetKeyReply");
    extractType<GetReadVersionReply>(outDir, "GetReadVersionReply");
    extractType<GetKeyServerLocationsReply>(outDir, "GetKeyServerLocationsReply");
    extractType<CommitID>(outDir, "CommitID");

    // --- Request types: GENERATED struct + template + constants, CUSTOM MarshalFDB ---
    extractType<GetValueRequest, false, false>(outDir, "GetValueRequest");
    extractType<GetKeyValuesRequest, false, false>(outDir, "GetKeyValuesRequest");
    extractType<GetKeyRequest, false, false>(outDir, "GetKeyRequest");
    extractType<GetReadVersionRequest>(outDir, "GetReadVersionRequest");
    extractType<GetKeyServerLocationsRequest, false, false>(outDir, "GetKeyServerLocationsRequest");
    extractType<CommitTransactionRequest, false, false>(outDir, "CommitTransactionRequest");
    extractType<OpenDatabaseCoordRequest, false, false>(outDir, "OpenDatabaseCoordRequest");

    // --- Nested types: GENERATED struct + constants, CUSTOM marshal/unmarshal ---
    extractType<SpanContext, false, false>(outDir, "SpanContext");
    extractType<KeySelectorRef, false, false>(outDir, "KeySelectorRef");
    extractType<MutationRef, false, false>(outDir, "MutationRef");
    extractType<KeyRangeRef, false, false>(outDir, "KeyRangeRef");
    extractType<CommitTransactionRef, false, false>(outDir, "CommitTransactionRef");
    extractType<ReadOptions, false, false>(outDir, "ReadOptions");
    extractType<Error, false, false>(outDir, "Error");

    // --- Interface/nested types: GENERATED struct + constants, CUSTOM methods ---
    extractType<ClientDBInfo, false, false>(outDir, "ClientDBInfo");
    extractType<GrvProxyInterface, true, false>(outDir, "GrvProxyInterface");
    extractType<CommitProxyInterface, true, false>(outDir, "CommitProxyInterface");
    extractType<StorageServerInterface, true, false>(outDir, "StorageServerInterface");
    extractType<NetworkAddress, false, false>(outDir, "NetworkAddress");
    extractType<IPAddress, false, false>(outDir, "IPAddress");
    extractType<TenantInfo, false, false>(outDir, "TenantInfo");
    extractType<ReplyPromise<GetValueReply>, false, false>(outDir, "ReplyPromise");

    fprintf(stderr, "Done.\n");
    return 0;
}
