// schema_extractor.h — C++ template machinery to extract wire format metadata
// from FDB types at compile/link time. Uses the REAL FDB flat_buffers.h
// type system — no stubs, no guessing.
//
// For each type T with serialize(), extracts:
//   - VTable (from get_vtable<Fields...>())
//   - Per-field: trait classification, fb_size, fb_align, use_indirection
//   - VTable closure (from get_vtableset_impl)
//
// Field NAMES are not available from C++ (they're source-code identifiers).
// Those come from the Go parser which extracts serializer() argument names.

#pragma once
#include "flow/flat_buffers.h"
#include <cstdio>
#include <cstdint>
#include <vector>
#include <string>

namespace schema {

// FieldInfo holds extracted metadata for one serialized field.
struct FieldInfo {
    const char* trait;  // "scalar", "dynamic_size", "vector_like", "union_like", "serialize_member"
    uint32_t size;      // fb_size<T> in bytes
    uint32_t align;     // fb_align<T>
    bool uses_indirection; // use_indirection<T> — false = inlined, true = RelativeOffset
};

// Classify a single type's trait.
template <class T>
const char* classifyTrait() {
    if constexpr (detail::is_scalar<T>) return "scalar";
    else if constexpr (detail::is_dynamic_size<T>) return "dynamic_size";
    else if constexpr (detail::is_vector_like<T>) return "vector_like";
    else if constexpr (detail::is_union_like<T>) return "union_like";
    else if constexpr (detail::is_struct_like<T>) return "struct_like";
    else return "serialize_member";
}

// Extract FieldInfo for one type.
template <class T>
FieldInfo extractField() {
    return FieldInfo{
        classifyTrait<T>(),
        (uint32_t)detail::fb_size<T>,
        (uint32_t)detail::fb_align<T>,
        detail::use_indirection<T>,
    };
}

// SchemaVisitor — called by serializer() to collect field metadata.
// Usage: T msg{}; msg.serialize(visitor); → visitor.fields has all field info.
struct SchemaVisitor {
    static constexpr bool isDeserializing = false;
    static constexpr bool isSerializing = false;
    static constexpr bool is_fb_visitor = true;

    std::vector<FieldInfo> fields;
    std::vector<uint16_t> vtable;

    SchemaVisitor& context() { return *this; }
    ProtocolVersion protocolVersion() const { return currentProtocolVersion(); }

    template <class... Members>
    void operator()(const Members&... members) {
        // Extract vtable.
        const auto* vt = detail::get_vtable<Members...>();
        vtable.assign(vt->begin(), vt->end());

        // Extract per-field info.
        (fields.push_back(extractField<Members>()), ...);
    }
};

// Write field info as JSON.
inline void writeFieldsJSON(FILE* f, const std::vector<FieldInfo>& fields) {
    fprintf(f, "  \"field_traits\": [\n");
    for (size_t i = 0; i < fields.size(); i++) {
        auto& fi = fields[i];
        fprintf(f, "    {\"trait\": \"%s\", \"size\": %u, \"align\": %u, \"indirection\": %s}",
                fi.trait, fi.size, fi.align, fi.uses_indirection ? "true" : "false");
        if (i + 1 < fields.size()) fprintf(f, ",");
        fprintf(f, "\n");
    }
    fprintf(f, "  ]");
}

// Write vtable as JSON.
inline void writeVTableJSON(FILE* f, const std::vector<uint16_t>& vt) {
    fprintf(f, "  \"vtable\": [");
    for (size_t i = 0; i < vt.size(); i++) {
        if (i > 0) fprintf(f, ", ");
        fprintf(f, "%u", vt[i]);
    }
    fprintf(f, "]");
}

// Extract and write schema for one message type.
template <class T>
bool extractSchema(const char* outDir, const char* name) {
    T msg{};
    SchemaVisitor visitor;

    if constexpr (detail::serializable_traits<T>::value) {
        detail::serializable_traits<T>::serialize(visitor, msg);
    } else {
        msg.serialize(visitor);
    }

    char path[4096];
    snprintf(path, sizeof(path), "%s/%s.schema.json", outDir, name);
    FILE* f = fopen(path, "w");
    if (!f) return false;

    fprintf(f, "{\n");
    fprintf(f, "  \"name\": \"%s\",\n", name);
    fprintf(f, "  \"file_identifier\": %u,\n", FileIdentifierFor<T>::value);
    writeVTableJSON(f, visitor.vtable);
    fprintf(f, ",\n");
    writeFieldsJSON(f, visitor.fields);
    fprintf(f, "\n}\n");
    fclose(f);
    return true;
}

// Fork-safe wrapper — handles types whose constructors crash.
template <class T>
void emitSchema(const char* outDir, const char* name) {
    pid_t pid = fork();
    if (pid == 0) {
        if (extractSchema<T>(outDir, name)) _exit(0);
        // Zero-init fallback.
        alignas(T) char storage[sizeof(T)] = {};
        T& msg = *reinterpret_cast<T*>(storage);
        SchemaVisitor visitor;
        if constexpr (detail::serializable_traits<T>::value) {
            detail::serializable_traits<T>::serialize(visitor, msg);
        } else {
            msg.serialize(visitor);
        }
        char path[4096];
        snprintf(path, sizeof(path), "%s/%s.schema.json", outDir, name);
        FILE* f = fopen(path, "w");
        if (f) {
            fprintf(f, "{\n");
            fprintf(f, "  \"name\": \"%s\",\n", name);
            fprintf(f, "  \"file_identifier\": %u,\n", FileIdentifierFor<T>::value);
            writeVTableJSON(f, visitor.vtable);
            fprintf(f, ",\n");
            writeFieldsJSON(f, visitor.fields);
            fprintf(f, "\n}\n");
            fclose(f);
        }
        _exit(0);
    }
    int status = 0;
    waitpid(pid, &status, 0);
}

} // namespace schema
