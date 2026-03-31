// fdb-schema-extract — Pure C++ tool that extracts wire format schemas from FDB types.
//
// Compiles against real FDB headers. No stubs. No Go. No regex parsing.
// The C++ compiler resolves all types, traits, vtables, and sizes.
//
// Output: one JSON file per type with:
//   - name, file_identifier
//   - vtable (from get_vtable<Fields...>())
//   - per-field: name, trait, size, alignment, indirection
//   - vtable_closure (all vtables reachable from the type graph)
//
// Field names are captured by redefining `serializer` in a separate TU
// (name_capture.cpp) which stringifies __VA_ARGS__ at the preprocessor level.
//
// Build: compiled inside FDB's Docker build environment, linked against
// fdbclient + fdbrpc + flow + fdbserver_lib.

// --- Normal FDB includes (real types, real traits) ---

#include "fdbclient/StorageServerInterface.h"
#include "fdbclient/CommitProxyInterface.h"
#include "fdbclient/GrvProxyInterface.h"
#include "fdbclient/CoordinationInterface.h"
#include "fdbclient/ClusterInterface.h"
#include "fdbclient/FDBTypes.h"
#include "fdbclient/GlobalConfig.h"
#include "fdbrpc/FlowTransport.h"
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

// Defined in name_capture.cpp (separate TU with redefined serializer).
extern std::map<std::string, std::string>& nameRegistry();
extern void captureAllNames();

// ============================================================
// Schema extraction — uses real FDB flat_buffers type system
// ============================================================

// Check for file_identifier via the constexpr static member directly.
template <class T>
constexpr uint32_t getFileId() {
    if constexpr (requires { T::file_identifier; }) {
        return T::file_identifier;
    } else {
        return 0;
    }
}

struct FieldInfo {
    std::string name;
    const char* trait;
    uint32_t size;
    uint32_t align;
    bool indirection;
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

// fb_visitor that collects vtable + per-field type metadata.
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
        using namespace detail;
        fields.push_back(FieldInfo{"", classifyTrait<T>(),
                                   (uint32_t)fb_size<T>, (uint32_t)fb_align<T>,
                                   use_indirection<T>});
    }
};

// Extract vtable closure from serialized bytes (same as C++ ObjectWriter produces).
std::vector<std::vector<uint16_t>> extractVTableClosure(const uint8_t* data, int size) {
    std::vector<std::vector<uint16_t>> result;
    if (size < 16) return result;
    int off = 0;
    if (size >= 16 && data[7] == 0x0F && data[6] == 0xDB) off = 8;
    if (off + 8 > size) return result;

    uint32_t rootOff;
    memcpy(&rootOff, data + off, 4);
    int vtEnd = off + (int)rootOff;
    int pos = off + 8;

    while (pos < vtEnd && pos + 2 <= size) {
        uint16_t vtSize;
        memcpy(&vtSize, data + pos, 2);
        if (vtSize == 0) { pos += 2; continue; }
        if (vtSize < 6 || vtSize > 64 || vtSize % 2 != 0 || pos + vtSize > size) break;
        uint16_t objSize;
        memcpy(&objSize, data + pos + 2, 2);
        if (objSize < 4 || objSize > 128) { pos += 2; continue; }

        std::vector<uint16_t> vt(vtSize / 2);
        for (int i = 0; i < vtSize / 2; i++)
            memcpy(&vt[i], data + pos + i * 2, 2);
        result.push_back(vt);
        pos += vtSize;
    }
    return result;
}

// Split comma-separated names.
std::vector<std::string> splitNames(const std::string& csv) {
    std::vector<std::string> result;
    std::istringstream ss(csv);
    std::string token;
    while (std::getline(ss, token, ',')) {
        auto start = token.find_first_not_of(" \t");
        auto end = token.find_last_not_of(" \t");
        if (start != std::string::npos)
            result.push_back(token.substr(start, end - start + 1));
    }
    return result;
}

// Write one schema JSON file.
void writeJSON(FILE* f, const char* name, uint32_t fileId,
               const std::vector<uint16_t>& vt,
               const std::vector<FieldInfo>& fields,
               const std::vector<std::vector<uint16_t>>& closure) {
    fprintf(f, "{\n  \"name\": \"%s\",\n  \"file_identifier\": %u,\n", name, fileId);

    fprintf(f, "  \"vtable\": [");
    for (size_t i = 0; i < vt.size(); i++) { if (i) fprintf(f, ", "); fprintf(f, "%u", vt[i]); }
    fprintf(f, "],\n");

    fprintf(f, "  \"vtable_closure\": [\n");
    for (size_t i = 0; i < closure.size(); i++) {
        fprintf(f, "    [");
        for (size_t j = 0; j < closure[i].size(); j++) {
            if (j) fprintf(f, ", ");
            fprintf(f, "%u", closure[i][j]);
        }
        fprintf(f, "]%s\n", i + 1 < closure.size() ? "," : "");
    }
    fprintf(f, "  ],\n");

    fprintf(f, "  \"fields\": [\n");
    for (size_t i = 0; i < fields.size(); i++) {
        auto& fi = fields[i];
        fprintf(f, "    {\"name\": \"%s\", \"trait\": \"%s\", \"size\": %u, \"align\": %u, \"indirection\": %s}%s\n",
                fi.name.c_str(), fi.trait, fi.size, fi.align,
                fi.indirection ? "true" : "false",
                i + 1 < fields.size() ? "," : "");
    }
    fprintf(f, "  ]\n}\n");
}

// Extract and write schema for one type. Fork-safe.
template <class T>
void emitSchema(const char* outDir, const char* typeName) {
    pid_t pid = fork();
    if (pid == 0) {
        // Type metadata.
        TypeVisitor visitor;
        T msg{};
        if constexpr (serializable_traits<T>::value) {
            serializable_traits<T>::serialize(visitor, msg);
        } else {
            msg.serialize(visitor);
        }

        // Field names from name_capture.cpp.
        auto it = nameRegistry().find(typeName);
        auto names = (it != nameRegistry().end()) ? splitNames(it->second) : std::vector<std::string>{};
        for (size_t i = 0; i < visitor.fields.size(); i++)
            visitor.fields[i].name = (i < names.size()) ? names[i] : "field_" + std::to_string(i);

        // Vtable closure from serialized bytes.
        std::vector<std::vector<uint16_t>> closure;
        if constexpr (requires { T::file_identifier; }) {
            ObjectWriter wr(IncludeVersion(currentProtocolVersion()));
            wr.serialize(T::file_identifier, msg);
            auto bytes = wr.toStringRef();
            closure = extractVTableClosure(bytes.begin(), bytes.size());
        }

        char path[4096];
        snprintf(path, sizeof(path), "%s/%s.json", outDir, typeName);
        FILE* f = fopen(path, "w");
        if (f) {
            writeJSON(f, typeName, getFileId<T>(), visitor.vtable, visitor.fields, closure);
            fclose(f);
            fprintf(stderr, "OK %s\n", typeName);
        }
        _exit(0);
    }
    int status = 0;
    waitpid(pid, &status, 0);
    if (!WIFEXITED(status) || WEXITSTATUS(status) != 0) {
        fprintf(stderr, "SKIP %s\n", typeName);
    }
}

// ============================================================
// Main — list every type we need. Add a line, get a schema.
// ============================================================

int main(int argc, char** argv) {
    if (argc < 2) {
        fprintf(stderr, "Usage: %s <output-dir>\n", argv[0]);
        return 1;
    }
    const char* outDir = argv[1];
    mkdir(outDir, 0755);

    // Initialize FDB runtime.
    TLSConfig tlsConfig;
    g_network = newNet2(tlsConfig, false, false);
    FlowTransport::createInstance(false, 1, WLTOKEN_FIRST_AVAILABLE, nullptr);

    // Capture field names (from name_capture.cpp).
    captureAllNames();

    // --- Client request/reply messages ---
    emitSchema<GetValueRequest>(outDir, "GetValueRequest");
    emitSchema<GetValueReply>(outDir, "GetValueReply");
    emitSchema<GetKeyValuesRequest>(outDir, "GetKeyValuesRequest");
    emitSchema<GetKeyValuesReply>(outDir, "GetKeyValuesReply");
    emitSchema<GetKeyRequest>(outDir, "GetKeyRequest");
    emitSchema<GetKeyReply>(outDir, "GetKeyReply");
    emitSchema<GetReadVersionRequest>(outDir, "GetReadVersionRequest");
    emitSchema<GetReadVersionReply>(outDir, "GetReadVersionReply");
    emitSchema<GetKeyServerLocationsRequest>(outDir, "GetKeyServerLocationsRequest");
    emitSchema<GetKeyServerLocationsReply>(outDir, "GetKeyServerLocationsReply");
    emitSchema<CommitTransactionRequest>(outDir, "CommitTransactionRequest");
    emitSchema<CommitID>(outDir, "CommitID");

    // --- Coordinator messages ---
    emitSchema<OpenDatabaseCoordRequest>(outDir, "OpenDatabaseCoordRequest");

    // --- Nested types (serialize_member, used inside messages) ---
    emitSchema<SpanContext>(outDir, "SpanContext");
    emitSchema<KeySelectorRef>(outDir, "KeySelectorRef");
    emitSchema<MutationRef>(outDir, "MutationRef");
    emitSchema<KeyRangeRef>(outDir, "KeyRangeRef");
    emitSchema<CommitTransactionRef>(outDir, "CommitTransactionRef");
    emitSchema<ReadOptions>(outDir, "ReadOptions");
    emitSchema<Error>(outDir, "Error");
    // UID: scalar_traits, size=16. Tag: struct_like_traits. Both inlined, no serialize().

    fprintf(stderr, "Done.\n");
    return 0;
}
