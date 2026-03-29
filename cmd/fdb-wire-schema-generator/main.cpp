// main.cpp — Generates ground-truth serialized bytes using FDB's actual
// flat_buffers.h templates. Outputs one JSON file per test case.
//
// Usage: gen_test_vectors <output-dir>
//
// Each output file: {"name": "...", "file_identifier": N, "hex": "..."}

#include "fdb_stubs.h"
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <string>
#include <sys/stat.h>
#include <vector>

// ============================================================
// Test message types — simple structs exercising each wire type category.
// ============================================================

struct MsgSingleInt32 {
    constexpr static FileIdentifier file_identifier = 100001;
    int32_t x;
    template <class Ar> void serialize(Ar& ar) { serializer(ar, x); }
};

struct MsgMultiScalar {
    constexpr static FileIdentifier file_identifier = 100002;
    uint8_t a;
    uint8_t b;
    int32_t c;
    int64_t d;
    int32_t e;
    template <class Ar> void serialize(Ar& ar) { serializer(ar, a, b, c, d, e); }
};

struct MsgWithString {
    constexpr static FileIdentifier file_identifier = 100003;
    int64_t version;
    std::string name;
    template <class Ar> void serialize(Ar& ar) { serializer(ar, version, name); }
};

struct MsgBoolDouble {
    constexpr static FileIdentifier file_identifier = 100004;
    bool flag;
    double value;
    template <class Ar> void serialize(Ar& ar) { serializer(ar, flag, value); }
};

struct MsgVectorInt32 {
    constexpr static FileIdentifier file_identifier = 100005;
    int32_t id;
    std::vector<int32_t> values;
    template <class Ar> void serialize(Ar& ar) { serializer(ar, id, values); }
};

struct MsgEmptyVector {
    constexpr static FileIdentifier file_identifier = 100006;
    std::vector<int32_t> values;
    template <class Ar> void serialize(Ar& ar) { serializer(ar, values); }
};

struct MsgOptionalPresent {
    constexpr static FileIdentifier file_identifier = 100007;
    int32_t id;
    Optional<int32_t> value;
    template <class Ar> void serialize(Ar& ar) { serializer(ar, id, value); }
};

struct MsgOptionalAbsent {
    constexpr static FileIdentifier file_identifier = 100008;
    int32_t id;
    Optional<int32_t> value;
    template <class Ar> void serialize(Ar& ar) { serializer(ar, id, value); }
};

struct MsgOptionalString {
    constexpr static FileIdentifier file_identifier = 100009;
    Optional<std::string> name;
    template <class Ar> void serialize(Ar& ar) { serializer(ar, name); }
};

struct MsgVectorString {
    constexpr static FileIdentifier file_identifier = 100010;
    std::vector<std::string> names;
    template <class Ar> void serialize(Ar& ar) { serializer(ar, names); }
};

struct Inner {
    constexpr static FileIdentifier file_identifier = 100099;
    int32_t x;
    int32_t y;
    template <class Ar> void serialize(Ar& ar) { serializer(ar, x, y); }
};

struct MsgNested {
    constexpr static FileIdentifier file_identifier = 100011;
    int64_t id;
    Inner pos;
    template <class Ar> void serialize(Ar& ar) { serializer(ar, id, pos); }
};

struct InnerWithString {
    constexpr static FileIdentifier file_identifier = 100098;
    int32_t code;
    std::string label;
    template <class Ar> void serialize(Ar& ar) { serializer(ar, code, label); }
};

struct MsgNestedString {
    constexpr static FileIdentifier file_identifier = 100012;
    InnerWithString info;
    int32_t version;
    template <class Ar> void serialize(Ar& ar) { serializer(ar, info, version); }
};

// ============================================================
// Serialization helpers
// ============================================================

struct TestArena {
    std::vector<std::pair<uint8_t*, size_t>> allocated;
    ~TestArena() { for (auto& b : allocated) delete[] b.first; }
    uint8_t* operator()(size_t sz) {
        auto res = new uint8_t[sz];
        allocated.emplace_back(res, sz);
        return res;
    }
    size_t get_size(const uint8_t* ptr) const {
        for (auto& p : allocated) if (p.first == ptr) return p.second;
        return 0;
    }
};

struct TestContext {
    TestArena& arena_;
    TestArena& arena() { return arena_; }
    uint8_t* allocate(size_t size) { return arena_(size); }
    TestContext& context() { return *this; }
};

template <class T>
void emit(const char* outDir, const char* name, const T& msg) {
    TestArena arena;
    TestContext ctx{arena};

    auto* out = save_members(ctx, FileIdentifierFor<T>::value, const_cast<T&>(msg));
    size_t len = arena.get_size(out);

    char path[4096];
    snprintf(path, sizeof(path), "%s/%s.json", outDir, name);
    FILE* f = fopen(path, "w");
    if (!f) { perror(path); exit(1); }

    fprintf(f, "{\n");
    fprintf(f, "  \"name\": \"%s\",\n", name);
    fprintf(f, "  \"file_identifier\": %u,\n", FileIdentifierFor<T>::value);
    fprintf(f, "  \"size\": %zu,\n", len);
    fprintf(f, "  \"hex\": \"");
    for (size_t i = 0; i < len; i++) fprintf(f, "%02x", out[i]);
    fprintf(f, "\"\n");
    fprintf(f, "}\n");
    fclose(f);
}

int main(int argc, char** argv) {
    if (argc < 2) {
        fprintf(stderr, "Usage: %s <output-dir>\n", argv[0]);
        return 1;
    }
    const char* outDir = argv[1];
    mkdir(outDir, 0755);

    emit(outDir, "MsgSingleInt32", MsgSingleInt32{42});
    emit(outDir, "MsgMultiScalar", MsgMultiScalar{0xAA, 0xBB, 100, 200, 300});
    emit(outDir, "MsgWithString", MsgWithString{0x1234567890ABCDEFLL, "hello, fdb!"});
    emit(outDir, "MsgBoolDouble", MsgBoolDouble{true, 3.14159});
    emit(outDir, "MsgVectorInt32", MsgVectorInt32{99, {10, 20, 30}});
    emit(outDir, "MsgEmptyVector", MsgEmptyVector{{}});
    emit(outDir, "MsgOptionalPresent", MsgOptionalPresent{42, Optional<int32_t>(123)});
    emit(outDir, "MsgOptionalAbsent", MsgOptionalAbsent{42, Optional<int32_t>()});
    emit(outDir, "MsgOptionalString", MsgOptionalString{Optional<std::string>("test")});
    emit(outDir, "MsgVectorString", MsgVectorString{{"abc", "def", "ghi"}});
    emit(outDir, "MsgNested", MsgNested{1000, {42, 99}});
    emit(outDir, "MsgNestedString", MsgNestedString{{200, "hello"}, 7});

    fprintf(stderr, "Wrote 12 test vectors to %s\n", outDir);
    return 0;
}
