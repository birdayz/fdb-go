// schema_extractor.h — Single C++ binary schema extraction from real FDB types.
//
// Extracts vtable, per-field trait/size/align from C++ type system.
// Field names come from name_capture.cpp (separate compilation unit
// that redefines serializer to stringify __VA_ARGS__).
//
// Include this AFTER all FDB headers (it uses the real flat_buffers.h).

#pragma once
#include "flow/flat_buffers.h"
#include "name_capture.h"
#include <cstdio>
#include <cstdint>
#include <vector>
#include <string>
#include <unistd.h>
#include <sys/wait.h>

// Defined in name_capture.cpp.
void captureAllNames();

namespace schema {

struct FieldInfo {
    std::string name;
    std::string trait;
    uint32_t size;
    uint32_t align;
    bool indirection;
};

template <class T>
const char* classifyTrait() {
    if constexpr (detail::is_scalar<T>) return "scalar";
    else if constexpr (detail::is_dynamic_size<T>) return "dynamic_size";
    else if constexpr (detail::is_vector_like<T>) return "vector_like";
    else if constexpr (detail::is_union_like<T>) return "union_like";
    else if constexpr (detail::is_struct_like<T>) return "struct_like";
    else return "serialize_member";
}

// TypeVisitor — fb_visitor that collects vtable + field metadata.
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
    template <class T>
    void pushField() {
        fields.push_back(FieldInfo{
            "",
            classifyTrait<T>(),
            (uint32_t)detail::fb_size<T>,
            (uint32_t)detail::fb_align<T>,
            detail::use_indirection<T>,
        });
    }
};

inline void writeJSON(FILE* f, const char* name, uint32_t fileId,
                      const std::vector<uint16_t>& vt,
                      const std::vector<FieldInfo>& fields) {
    fprintf(f, "{\n  \"name\": \"%s\",\n  \"file_identifier\": %u,\n  \"vtable\": [", name, fileId);
    for (size_t i = 0; i < vt.size(); i++) {
        if (i) fprintf(f, ", ");
        fprintf(f, "%u", vt[i]);
    }
    fprintf(f, "],\n  \"fields\": [\n");
    for (size_t i = 0; i < fields.size(); i++) {
        auto& fi = fields[i];
        fprintf(f, "    {\"name\": \"%s\", \"trait\": \"%s\", \"size\": %u, \"align\": %u, \"indirection\": %s}",
                fi.name.c_str(), fi.trait.c_str(), fi.size, fi.align,
                fi.indirection ? "true" : "false");
        if (i + 1 < fields.size()) fprintf(f, ",");
        fprintf(f, "\n");
    }
    fprintf(f, "  ]\n}\n");
}

template <class T>
void emitSchema(const char* outDir, const char* typeName) {
    pid_t pid = fork();
    if (pid == 0) {
        // Phase 1: extract types.
        TypeVisitor visitor;
        T msg{};
        if constexpr (detail::serializable_traits<T>::value) {
            detail::serializable_traits<T>::serialize(visitor, msg);
        } else {
            msg.serialize(visitor);
        }

        // Phase 2: merge names from the registry.
        auto names = name_capture::getNamesFor(typeName);
        for (size_t i = 0; i < visitor.fields.size(); i++) {
            visitor.fields[i].name = (i < names.size()) ? names[i] : "field_" + std::to_string(i);
        }

        char path[4096];
        snprintf(path, sizeof(path), "%s/%s.schema.json", outDir, typeName);
        FILE* f = fopen(path, "w");
        if (f) {
            writeJSON(f, typeName, FileIdentifierFor<T>::value, visitor.vtable, visitor.fields);
            fclose(f);
        }
        _exit(0);
    }
    int status = 0;
    waitpid(pid, &status, 0);
}

} // namespace schema
