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
        fields.push_back(FieldInfo{"", classifyTrait<T>(),
            (uint32_t)fb_size<T>, (uint32_t)fb_align<T>, use_indirection<T>});
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
        for (size_t i = 0; i < fields.size(); i++) {
            auto& fi = fields[i];
            fprintf(f, "//   slot %zu: %s — %s, size=%u, align=%u%s\n",
                    i, fi.name.c_str(), fi.trait, fi.size, fi.align,
                    fi.indirection ? ", indirection" : "");
        }
    }

    void separator() { fprintf(f, "\n"); }
};

// Extract metadata for one type and write to the Go emitter.
// Runs in a forked child (crash-safe).
template <class T>
void extractType(GoEmitter& out, const char* name) {
    // Extract in a child process to survive constructor crashes.
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

        // Names.
        auto it = nameRegistry().find(name);
        auto names = (it != nameRegistry().end()) ? splitNames(it->second) : std::vector<std::string>{};
        for (size_t i = 0; i < visitor.fields.size(); i++)
            visitor.fields[i].name = (i < names.size()) ? names[i] : "field_" + std::to_string(i);

        // Closure + authoritative root vtable from ObjectWriter.
        std::vector<std::vector<uint16_t>> closure;
        std::vector<uint16_t> rootVTable;
        if constexpr (requires { T::file_identifier; }) {
            ObjectWriter wr(IncludeVersion(currentProtocolVersion()));
            wr.serialize(T::file_identifier, msg);
            auto bytes = wr.toStringRef();
            closure = extractVTableClosure(bytes.begin(), bytes.size());
            rootVTable = extractMessageVTable(bytes.begin(), bytes.size());
        }
        // For types with file_identifier, prefer ObjectWriter's root vtable
        // (it includes conditionally-serialized fields like tenantInfo).
        // Fall back to TypeVisitor vtable for nested types without file_identifier.
        auto& emitVT = rootVTable.empty() ? visitor.vtable : rootVTable;

        // Write Go to pipe.
        FILE* pf = fdopen(pipefd[1], "w");
        GoEmitter e{pf};
        e.emitFieldComment(name, visitor.fields);
        e.emitVTable(name, emitVT);
        e.emitFileID(name, getFileId<T>());
        e.emitClosure(name, closure);
        e.separator();
        fclose(pf);
        _exit(0);
    }

    close(pipefd[1]);
    // Read child output and append to main file.
    char buf[4096];
    ssize_t n;
    while ((n = read(pipefd[0], buf, sizeof(buf))) > 0)
        fwrite(buf, 1, n, out.f);
    close(pipefd[0]);

    int status;
    waitpid(pid, &status, 0);
    if (WIFEXITED(status) && WEXITSTATUS(status) == 0)
        fprintf(stderr, "OK %s\n", name);
    else
        fprintf(stderr, "SKIP %s\n", name);
}

// ============================================================
// Main
// ============================================================

int main(int argc, char** argv) {
    if (argc < 2) { fprintf(stderr, "Usage: %s <output-file>\n", argv[0]); return 1; }

    TLSConfig tlsConfig;
    g_network = newNet2(tlsConfig, false, false);
    FlowTransport::createInstance(false, 1, WLTOKEN_FIRST_AVAILABLE, nullptr);

    captureAllNames();

    FILE* f = fopen(argv[1], "w");
    if (!f) { perror(argv[1]); return 1; }
    GoEmitter out{f};
    out.header();

    // --- Client request/reply messages ---
    extractType<GetValueRequest>(out, "GetValueRequest");
    extractType<GetValueReply>(out, "GetValueReply");
    extractType<GetKeyValuesRequest>(out, "GetKeyValuesRequest");
    extractType<GetKeyValuesReply>(out, "GetKeyValuesReply");
    extractType<GetKeyRequest>(out, "GetKeyRequest");
    extractType<GetKeyReply>(out, "GetKeyReply");
    extractType<GetReadVersionRequest>(out, "GetReadVersionRequest");
    extractType<GetReadVersionReply>(out, "GetReadVersionReply");
    extractType<GetKeyServerLocationsRequest>(out, "GetKeyServerLocationsRequest");
    extractType<GetKeyServerLocationsReply>(out, "GetKeyServerLocationsReply");
    extractType<CommitTransactionRequest>(out, "CommitTransactionRequest");
    extractType<CommitID>(out, "CommitID");
    extractType<OpenDatabaseCoordRequest>(out, "OpenDatabaseCoordRequest");

    // --- Nested types ---
    extractType<SpanContext>(out, "SpanContext");
    extractType<KeySelectorRef>(out, "KeySelectorRef");
    extractType<MutationRef>(out, "MutationRef");
    extractType<KeyRangeRef>(out, "KeyRangeRef");
    extractType<CommitTransactionRef>(out, "CommitTransactionRef");
    extractType<ReadOptions>(out, "ReadOptions");
    extractType<Error>(out, "Error");

    fclose(f);
    fprintf(stderr, "Done.\n");
    return 0;
}
