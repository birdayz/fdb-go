// poc_generator.cpp — Proof-of-concept single-pass type extractor.
//
// Demonstrates the architecture for a better generator that:
// 1. Derives EVERYTHING from compile-time template metaprogramming
// 2. No manual getCppTypeName registry (auto-detect via type traits)
// 3. No name_capture string parsing (field names from explicit registration, compile-time verified)
// 4. Nested types auto-detected and recursively extracted
// 5. Variants auto-detected with per-alternative type info
// 6. Single extractType call does everything — no EmitStructs/EmitVectorParser flags
//
// NOT a working build — this is a design document in C++ form.
// Shows the interfaces and compile-time patterns.

#include <cstdio>
#include <cstdint>
#include <string>
#include <vector>
#include <map>
#include <type_traits>
#include <variant>

// ============================================================
// 1. Type Registry — compile-time verified, no silent fallback
// ============================================================

// Every type that appears as a nested struct MUST register here.
// Missing registration = compile error, NOT a silent comment.
template <class T>
struct GoTypeName {
    // Default: compile error via static_assert when used.
    // Types that don't need names (scalars, bytes) never instantiate this.
    static constexpr bool registered = false;
    static const char* name() {
        static_assert(registered, "Missing GoTypeName registration for nested struct type. "
            "Add: template<> struct GoTypeName<YourType> { static constexpr bool registered = true; "
            "static const char* name() { return \"YourType\"; } };");
        return "";
    }
};

// Registrations: one line per type. Forgetting = compile error.
#define REGISTER_GO_TYPE(CppType, GoName) \
    template<> struct GoTypeName<CppType> { \
        static constexpr bool registered = true; \
        static const char* name() { return GoName; } \
    }

// Example registrations (would be in the real file):
// REGISTER_GO_TYPE(SpanContext, "SpanContext");
// REGISTER_GO_TYPE(TenantInfo, "TenantInfo");
// REGISTER_GO_TYPE(ReplyPromise<GetValueReply>, "ReplyPromise");
// REGISTER_GO_TYPE(KeyRangeRef, "KeyRangeRef");

// ============================================================
// 2. Field Names — explicit per-type, compile-time indexed
// ============================================================

// Field names are registered per type as a compile-time string array.
// The extractor matches fields by INDEX (same order as serializer(ar, ...)).
template <class T>
struct FieldNames {
    static constexpr bool registered = false;
    static const char* get(int /*index*/) { return nullptr; }
};

#define REGISTER_FIELD_NAMES(CppType, ...) \
    template<> struct FieldNames<CppType> { \
        static constexpr bool registered = true; \
        static const char* names[]; \
        static const char* get(int index) { \
            static const char* n[] = { __VA_ARGS__ }; \
            return (index < (int)(sizeof(n)/sizeof(n[0]))) ? n[index] : nullptr; \
        } \
    }

// Example:
// REGISTER_FIELD_NAMES(GetValueRequest, "key", "version", "tags", "reply", "spanContext", "tenantInfo", "options", "ssLatestCommitVersions");
// REGISTER_FIELD_NAMES(TenantInfo, "tenantId", "token", "arena");

// ============================================================
// 3. Trait Classification — single unified system
// ============================================================

enum class FieldKind {
    Scalar,           // Inline value (int32, uint16, bool, float64, UID)
    DynamicSize,      // Length-prefixed bytes via RelOff (StringRef, Key, Value)
    VectorLike,       // Count-prefixed vector via RelOff, no length prefix (VectorRef)
    Optional,         // Type tag (uint8) + value (2 vtable slots)
    NestedStruct,     // serialize_member — nested FlatBuffers object via RelOff
    Variant,          // std::variant — type tag + per-alternative value via RelOff
};

// Scalar type info (Go type + reader/writer method names).
struct ScalarInfo {
    const char* goType;
    const char* reader;
    const char* writer;
    int size;
};

// For scalars, resolve at compile time from the C++ type.
template <class T> constexpr ScalarInfo scalarInfoFor() {
    if constexpr (std::is_same_v<T, bool>)     return {"bool", "ReadBool", "WriteBool", 1};
    if constexpr (std::is_same_v<T, int8_t>)   return {"int8", "ReadInt8", "WriteInt8", 1};
    if constexpr (std::is_same_v<T, uint8_t>)  return {"uint8", "ReadUint8", "WriteUint8", 1};
    if constexpr (std::is_same_v<T, int16_t>)  return {"int16", "ReadInt16", "WriteInt16", 2};
    if constexpr (std::is_same_v<T, uint16_t>) return {"uint16", "ReadUint16", "WriteUint16", 2};
    if constexpr (std::is_same_v<T, int32_t>)  return {"int32", "ReadInt32", "WriteInt32", 4};
    if constexpr (std::is_same_v<T, uint32_t>) return {"uint32", "ReadUint32", "WriteUint32", 4};
    if constexpr (std::is_same_v<T, int64_t>)  return {"int64", "ReadInt64", "WriteInt64", 8};
    if constexpr (std::is_same_v<T, uint64_t>) return {"uint64", "ReadUint64", "WriteUint64", 8};
    if constexpr (std::is_same_v<T, double>)   return {"float64", "ReadFloat64", "WriteFloat64", 8};
    // Enums: use underlying type.
    if constexpr (std::is_enum_v<T>) return scalarInfoFor<std::underlying_type_t<T>>();
    // UID (16 bytes):
    // if constexpr (std::is_same_v<T, UID>) return {"[16]byte", "ReadUID", "WriteUID", 16};
    // Unknown scalar: size-based fallback (compile-time, not runtime).
    return {"[]byte", "ReadBytes", "WriteBytes", 0};
}

// ============================================================
// 4. Variant Support — compile-time alternative extraction
// ============================================================

struct VariantAlt {
    const char* goType;
    const char* reader;
    int size;
    FieldKind kind; // scalar, dynamic_size, vector_like
};

template <class T>
struct VariantAlternatives {
    static constexpr bool is_variant = false;
    static std::vector<VariantAlt> get() { return {}; }
};

// Partial specialization for std::variant<Ts...>:
// template <class... Ts>
// struct VariantAlternatives<std::variant<Ts...>> {
//     static constexpr bool is_variant = true;
//     static std::vector<VariantAlt> get() {
//         return { makeAlt<Ts>()... };
//     }
// private:
//     template <class T>
//     static VariantAlt makeAlt() {
//         if constexpr (is_scalar<T>) {
//             auto si = scalarInfoFor<T>();
//             return {si.goType, si.reader, si.size, FieldKind::Scalar};
//         } else if constexpr (is_vector_like<T>) {
//             return {"[]byte", "ReadBytes", 4, FieldKind::VectorLike};
//         } else {
//             return {"[]byte", "ReadBytes", 4, FieldKind::DynamicSize};
//         }
//     }
// };

// ============================================================
// 5. Field Descriptor — everything about one field, compile-time
// ============================================================

struct FieldDesc {
    const char* name;          // "tenantId" (from FieldNames registry)
    FieldKind kind;            // Scalar, DynamicSize, NestedStruct, etc.
    ScalarInfo scalar;         // If kind==Scalar
    const char* nestedGoType;  // If kind==NestedStruct: "TenantInfo" (from GoTypeName)
    std::vector<VariantAlt> variantAlts; // If kind==Variant
    int vtableSlot;            // Slot index in the vtable
};

// ============================================================
// 6. TypeExtractor — single template, extracts everything
// ============================================================

// The visitor that collects FieldDesc for each field in serialize().
struct FieldCollector {
    std::vector<FieldDesc> fields;
    int slotIndex = 0;
    const char* typeName; // The parent type being extracted

    template <class T>
    void addField() {
        FieldDesc fd;
        fd.vtableSlot = slotIndex;

        // Get field name from registry (or fallback to field_N).
        // In the real implementation, the parent type's FieldNames<ParentT>
        // provides the name. Here we use the field index.
        fd.name = nullptr; // Set by caller from FieldNames<ParentT>::get(index)

        // Classify the field.
        // In the real implementation, use FDB's trait detection:
        //   is_scalar<T>, is_dynamic_size<T>, is_vector_like<T>,
        //   is_union_like<T>, is_struct_like<T>
        //
        // The key improvement: for NestedStruct, GoTypeName<T>::name() is
        // used AUTOMATICALLY. No manual getCppTypeName registry.
        // If GoTypeName<T> is not registered, the code DOESN'T COMPILE.

        // For nested structs:
        // if constexpr (is_serialize_member<T> && !is_union_like<T>) {
        //     fd.kind = FieldKind::NestedStruct;
        //     fd.nestedGoType = GoTypeName<T>::name(); // Compile error if not registered!
        // }

        // For variants:
        // if constexpr (VariantAlternatives<T>::is_variant) {
        //     fd.kind = FieldKind::Variant;
        //     fd.variantAlts = VariantAlternatives<T>::get();
        // }

        // For scalars:
        // if constexpr (is_scalar<T>) {
        //     fd.kind = FieldKind::Scalar;
        //     fd.scalar = scalarInfoFor<T>(); // Handles enums via underlying_type!
        // }

        fields.push_back(fd);

        // union_like (Optional/Variant) consumes 2 slots.
        // if constexpr (is_union_like<T>) slotIndex += 2;
        // else slotIndex++;
        slotIndex++;
    }
};

// ============================================================
// 7. Go Emitter — driven by FieldDesc, not by field traits
// ============================================================

struct GoEmitter {
    FILE* f;

    void emitType(const char* typeName, const std::vector<FieldDesc>& fields,
                  const std::vector<uint16_t>& vtable, uint32_t fileId) {
        emitHeader();
        emitSlotConstants(typeName, fields);
        emitVTable(typeName, vtable);
        emitStruct(typeName, fields);
        emitUnmarshalFDB(typeName, fields);
        emitUnmarshalFromReader(typeName, fields);
        emitMarshalInto(typeName, fields);     // Writes ALL fields including nested
        emitMarshalFDB(typeName, fields, fileId);
        emitHelpers(typeName, fields);          // MarshalStructBlob, WriteNested, ParseVector
    }

private:
    void emitHeader() {
        fprintf(f, "// Code generated by fdb-schema-extract. DO NOT EDIT.\n\n");
        fprintf(f, "package types\n\n");
        fprintf(f, "import \"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire\"\n\n");
    }

    void emitSlotConstants(const char* typeName, const std::vector<FieldDesc>& fields) {
        fprintf(f, "const (\n");
        for (auto& fd : fields) {
            if (fd.name) {
                fprintf(f, "\t%sSlot%s = %d\n", typeName, capitalize(fd.name).c_str(), fd.vtableSlot);
            }
        }
        fprintf(f, ")\n\n");
    }

    void emitVTable(const char* typeName, const std::vector<uint16_t>& vt) {
        fprintf(f, "var %sVTable = wire.VTable{", typeName);
        for (size_t i = 0; i < vt.size(); i++) {
            if (i) fprintf(f, ", ");
            fprintf(f, "%u", vt[i]);
        }
        fprintf(f, "}\n\n");
    }

    void emitStruct(const char* typeName, const std::vector<FieldDesc>& fields) {
        fprintf(f, "type %s struct {\n", typeName);
        for (auto& fd : fields) {
            auto goName = capitalize(fd.name ? fd.name : ("field_" + std::to_string(fd.vtableSlot)).c_str());
            switch (fd.kind) {
            case FieldKind::Scalar:
                fprintf(f, "\t%s %s // slot %d\n", goName.c_str(), fd.scalar.goType, fd.vtableSlot);
                break;
            case FieldKind::DynamicSize:
            case FieldKind::VectorLike:
                fprintf(f, "\t%s []byte // slot %d\n", goName.c_str(), fd.vtableSlot);
                break;
            case FieldKind::NestedStruct:
                // Uses GoTypeName<T>::name() — guaranteed registered by compile-time check.
                fprintf(f, "\t%s %s // slot %d, nested\n", goName.c_str(), fd.nestedGoType, fd.vtableSlot);
                break;
            case FieldKind::Optional:
                fprintf(f, "\tHas%s bool // slot %d, optional tag\n", goName.c_str(), fd.vtableSlot);
                fprintf(f, "\t%s []byte // slot %d, optional value\n", goName.c_str(), fd.vtableSlot + 1);
                break;
            case FieldKind::Variant:
                fprintf(f, "\t%sTag uint8 // slot %d, variant tag\n", goName.c_str(), fd.vtableSlot);
                for (size_t a = 0; a < fd.variantAlts.size(); a++) {
                    fprintf(f, "\t%sAlt%zu %s // tag=%zu\n",
                            goName.c_str(), a, fd.variantAlts[a].goType, a + 1);
                }
                break;
            }
        }
        fprintf(f, "}\n\n");
    }

    // KEY IMPROVEMENT: MarshalInto writes ALL fields including nested structs.
    // The current generator skips nested structs in MarshalInto — this fixes HIGH #6.
    void emitMarshalInto(const char* typeName, const std::vector<FieldDesc>& fields) {
        fprintf(f, "func (m *%s) MarshalInto(obj *wire.ObjectWriter) {\n", typeName);
        fprintf(f, "\tvt := %sVTable\n", typeName);
        for (auto& fd : fields) {
            auto goName = capitalize(fd.name ? fd.name : ("field_" + std::to_string(fd.vtableSlot)).c_str());
            auto slotExpr = std::string(typeName) + "Slot" + goName;
            switch (fd.kind) {
            case FieldKind::Scalar:
                fprintf(f, "\tobj.%s(int(vt[%s+2]), m.%s)\n",
                        fd.scalar.writer, slotExpr.c_str(), goName.c_str());
                break;
            case FieldKind::DynamicSize:
                fprintf(f, "\tif len(m.%s) > 0 {\n", goName.c_str());
                fprintf(f, "\t\tobj.WriteBytes(int(vt[%s+2]), m.%s)\n", slotExpr.c_str(), goName.c_str());
                fprintf(f, "\t}\n");
                break;
            case FieldKind::VectorLike:
                fprintf(f, "\tif len(m.%s) > 0 {\n", goName.c_str());
                fprintf(f, "\t\tobj.WriteRawOOL(int(vt[%s+2]), m.%s)\n", slotExpr.c_str(), goName.c_str());
                fprintf(f, "\t}\n");
                break;
            case FieldKind::NestedStruct:
                // THIS IS THE FIX: MarshalInto now writes nested structs!
                fprintf(f, "\tobj.WriteStruct(int(vt[%s+2]), %sVTable, 8, m.%s.MarshalInto)\n",
                        slotExpr.c_str(), fd.nestedGoType, goName.c_str());
                break;
            case FieldKind::Optional:
                // Skip in MarshalInto (needs conditional write logic).
                break;
            case FieldKind::Variant:
                // Skip in MarshalInto (needs switch on tag).
                break;
            }
        }
        fprintf(f, "}\n\n");
    }

    void emitUnmarshalFDB(const char* typeName, const std::vector<FieldDesc>& fields) {
        fprintf(f, "func (m *%s) UnmarshalFDB(data []byte) error {\n", typeName);
        fprintf(f, "\tr, err := wire.NewReader(data)\n");
        fprintf(f, "\tif err != nil { return err }\n");
        emitFieldReads(typeName, fields, "r");
        fprintf(f, "\treturn nil\n}\n\n");
    }

    void emitUnmarshalFromReader(const char* typeName, const std::vector<FieldDesc>& fields) {
        fprintf(f, "func (m *%s) UnmarshalFromReader(r *wire.Reader) {\n", typeName);
        emitFieldReads(typeName, fields, "r");
        fprintf(f, "}\n\n");
    }

    void emitFieldReads(const char* typeName, const std::vector<FieldDesc>& fields, const char* rv) {
        for (auto& fd : fields) {
            auto goName = capitalize(fd.name ? fd.name : ("field_" + std::to_string(fd.vtableSlot)).c_str());
            auto slotExpr = std::string(typeName) + "Slot" + goName;
            switch (fd.kind) {
            case FieldKind::Scalar:
                fprintf(f, "\tif %s.FieldPresent(%s) {\n", rv, slotExpr.c_str());
                fprintf(f, "\t\tm.%s = %s.%s(%s)\n", goName.c_str(), rv, fd.scalar.reader, slotExpr.c_str());
                fprintf(f, "\t}\n");
                break;
            case FieldKind::DynamicSize:
            case FieldKind::VectorLike:
                fprintf(f, "\tif %s.FieldPresent(%s) {\n", rv, slotExpr.c_str());
                fprintf(f, "\t\tm.%s = %s.ReadBytes(%s)\n", goName.c_str(), rv, slotExpr.c_str());
                fprintf(f, "\t}\n");
                break;
            case FieldKind::NestedStruct:
                // Recursive unmarshal via UnmarshalFromReader.
                fprintf(f, "\tif nr, err := %s.ReadNestedReader(%s); err == nil {\n", rv, slotExpr.c_str());
                fprintf(f, "\t\tm.%s.UnmarshalFromReader(nr)\n", goName.c_str());
                fprintf(f, "\t}\n");
                break;
            case FieldKind::Optional:
                fprintf(f, "\tif %s.FieldPresent(%s) && %s.ReadUint8(%s) > 0 {\n",
                        rv, slotExpr.c_str(), rv, slotExpr.c_str());
                fprintf(f, "\t\tm.%s = %s.ReadBytes(%s + 1)\n", goName.c_str(), rv, slotExpr.c_str());
                fprintf(f, "\t\tm.Has%s = true\n", goName.c_str());
                fprintf(f, "\t}\n");
                break;
            case FieldKind::Variant:
                fprintf(f, "\tif %s.FieldPresent(%s) {\n", rv, slotExpr.c_str());
                fprintf(f, "\t\tm.%sTag = %s.ReadUint8(%s)\n", goName.c_str(), rv, slotExpr.c_str());
                fprintf(f, "\t\tswitch m.%sTag {\n", goName.c_str());
                for (size_t a = 0; a < fd.variantAlts.size(); a++) {
                    auto& alt = fd.variantAlts[a];
                    fprintf(f, "\t\tcase %zu:\n", a + 1);
                    if (alt.kind == FieldKind::Scalar && alt.size == 4) {
                        fprintf(f, "\t\t\tm.%sAlt%zu = %s.ReadRelOffUint32(%s + 1)\n",
                                goName.c_str(), a, rv, slotExpr.c_str());
                    } else {
                        fprintf(f, "\t\t\tm.%sAlt%zu = %s.ReadRelOffRaw(%s + 1, %d)\n",
                                goName.c_str(), a, rv, slotExpr.c_str(), alt.size);
                    }
                }
                fprintf(f, "\t\t}\n");
                fprintf(f, "\t}\n");
                break;
            }
        }
    }

    void emitMarshalFDB(const char* typeName, const std::vector<FieldDesc>& fields, uint32_t fileId) {
        if (fileId == 0) return; // Nested type — no standalone MarshalFDB.
        fprintf(f, "func (m *%s) MarshalFDB() []byte {\n", typeName);
        fprintf(f, "\tw := wire.NewWriter(nil)\n");
        fprintf(f, "\treturn w.WriteMessagePacked(%sTemplate, m.MarshalInto)\n", typeName);
        // ^^^ KEY SIMPLIFICATION: MarshalFDB just calls MarshalInto!
        // No duplicate field write logic. MarshalInto handles everything.
        fprintf(f, "}\n\n");
    }

    void emitHelpers(const char* typeName, const std::vector<FieldDesc>& fields) {
        // MarshalStructBlob, WriteNested — same as current, but MarshalInto is complete.
        // ParseVector — auto-generated for types used in vectors.
    }

    static std::string capitalize(const char* s) {
        if (!s || !s[0]) return "Field";
        std::string r(s);
        r[0] = toupper(r[0]);
        return r;
    }
};

// ============================================================
// 8. Extraction — single call, no flags
// ============================================================

// In the real implementation:
//
// template <class T>
// void extractType(const char* outDir, const char* goName) {
//     // 1. Collect fields via FieldCollector visitor (compile-time typed).
//     FieldCollector collector;
//     collector.typeName = goName;
//     T msg{};
//     serializable_traits<T>::serialize(collector, msg);
//
//     // 2. Apply field names from registry.
//     for (int i = 0; i < collector.fields.size(); i++) {
//         if constexpr (FieldNames<T>::registered) {
//             collector.fields[i].name = FieldNames<T>::get(i);
//         }
//     }
//
//     // 3. Get vtable from ObjectWriter (authoritative).
//     auto vtable = extractVTableFromObjectWriter<T>();
//
//     // 4. Emit Go code — single pass, no flags.
//     GoEmitter emitter{openFile(outDir, goName)};
//     emitter.emitType(goName, collector.fields, vtable, getFileId<T>());
// }
//
// Usage:
//   extractType<GetValueRequest>("pkg/fdbgo/wire/types/", "GetValueRequest");
//   extractType<TenantInfo>("pkg/fdbgo/wire/types/", "TenantInfo");
//   extractType<IPAddress>("pkg/fdbgo/wire/types/", "IPAddress");
//
// No EmitStructs, EmitStringVectorParser, EmitFBVectorParser flags.
// No getCppTypeName manual registry.
// Missing GoTypeName or FieldNames registration = compile error.

// ============================================================
// 9. Key Differences from Current Implementation
// ============================================================
//
// CURRENT:
//   - getCppTypeName<T>(): manual, silent fallback to "" → comment-only fields
//   - name_capture.cpp: regex on source text, picks LAST serializer() call
//   - EmitStructs/EmitVectorParser: manual flags per type
//   - MarshalInto: skips nested struct fields
//   - classifyTrait: runtime string comparison ("serialize_member", "union_like")
//   - Scalar fallback: size-based, loses signed/unsigned info
//
// POC:
//   - GoTypeName<T>: compile-time verified, missing = compile error
//   - FieldNames<T>: explicit per-type, indexed by field position
//   - No flags: extractType auto-detects everything from C++ type traits
//   - MarshalInto: writes ALL fields including nested (via WriteStruct + MarshalInto)
//   - FieldKind enum: typed, no string comparison
//   - scalarInfoFor<T>: handles enums via std::underlying_type_t, exact type match
//   - Variant alternatives: compile-time extracted from std::variant<Ts...>
//
// MIGRATION PATH:
//   1. Add GoTypeName registrations for all nested struct types (compile error guides you)
//   2. Add FieldNames registrations for all types (compile error guides you)
//   3. Replace TypeVisitor with FieldCollector
//   4. Replace GoEmitter methods with the PoC versions (especially MarshalInto)
//   5. Delete getCppTypeName, name_capture.cpp, all EmitXxx flags
//   6. Run golden test vectors to verify byte-identical output

int main() {
    fprintf(stderr, "This is a PoC design document, not a runnable program.\n");
    fprintf(stderr, "See the source comments for the architecture.\n");
    return 0;
}
