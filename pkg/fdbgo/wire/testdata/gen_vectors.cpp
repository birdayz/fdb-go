// gen_vectors.cpp — Generates wire format test vectors using FDB's actual
// flat_buffers serialization code.
//
// Build: bazelisk build //pkg/fdbgo/wire/testdata:gen_vectors
// Run:   bazelisk run //pkg/fdbgo/wire/testdata:gen_vectors

#include <cstdio>
#include <cstdint>
#include <cstring>
#include <string>
#include <vector>

// Minimal Optional<T> matching FDB's union_like_traits<Optional<T>> from Arena.h.
// We don't include Arena.h (too many deps), so we replicate the essential parts.
template <class T>
class Optional {
    bool present_;
    T value_;
public:
    Optional() : present_(false), value_() {}
    Optional(const T& v) : present_(true), value_(v) {}
    bool present() const { return present_; }
    const T& get() const { return value_; }
};

// FDB headers.
#include "flow/flat_buffers.h"

// Register Optional<T> as union_like (matching FDB's Arena.h)
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

// Simple arena (same as flat_buffers.cpp unit tests).
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

// ---- Test message types ----

// 1. Single int32 field.
struct MsgSingleInt32 {
    constexpr static FileIdentifier file_identifier = 100001;
    int32_t x;
    template <class Ar> void serialize(Ar& ar) { serializer(ar, x); }
};

// 2. Multiple scalar fields.
struct MsgMultiScalar {
    constexpr static FileIdentifier file_identifier = 100002;
    uint8_t a;
    uint8_t b;
    int32_t c;
    int64_t d;
    int32_t e;
    template <class Ar> void serialize(Ar& ar) { serializer(ar, a, b, c, d, e); }
};

// 3. Message with a string (dynamic_size) field.
struct MsgWithString {
    constexpr static FileIdentifier file_identifier = 100003;
    int64_t version;
    std::string name;
    template <class Ar> void serialize(Ar& ar) { serializer(ar, version, name); }
};

// 4. Message with bool and double.
struct MsgBoolDouble {
    constexpr static FileIdentifier file_identifier = 100004;
    bool flag;
    double value;
    template <class Ar> void serialize(Ar& ar) { serializer(ar, flag, value); }
};

// 5. Message with a vector of int32.
struct MsgVectorInt32 {
    constexpr static FileIdentifier file_identifier = 100005;
    int32_t id;
    std::vector<int32_t> values;
    template <class Ar> void serialize(Ar& ar) { serializer(ar, id, values); }
};

// 6. Message with empty vector.
struct MsgEmptyVector {
    constexpr static FileIdentifier file_identifier = 100006;
    std::vector<int32_t> values;
    template <class Ar> void serialize(Ar& ar) { serializer(ar, values); }
};

// 7. Message with Optional<int32> (present).
struct MsgOptionalPresent {
    constexpr static FileIdentifier file_identifier = 100007;
    int32_t id;
    Optional<int32_t> value;
    template <class Ar> void serialize(Ar& ar) { serializer(ar, id, value); }
};

// 8. Message with Optional<int32> (absent).
struct MsgOptionalAbsent {
    constexpr static FileIdentifier file_identifier = 100008;
    int32_t id;
    Optional<int32_t> value;
    template <class Ar> void serialize(Ar& ar) { serializer(ar, id, value); }
};

// 9. Message with Optional<string> (present).
struct MsgOptionalString {
    constexpr static FileIdentifier file_identifier = 100009;
    Optional<std::string> name;
    template <class Ar> void serialize(Ar& ar) { serializer(ar, name); }
};

// 10. Message with vector of strings.
struct MsgVectorString {
    constexpr static FileIdentifier file_identifier = 100010;
    std::vector<std::string> names;
    template <class Ar> void serialize(Ar& ar) { serializer(ar, names); }
};

// 11. Nested struct — inner struct as a field.
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

// 12. Nested struct with string — inner has out-of-line data.
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

// ---- Helpers ----

template <class T>
void emit_vector(const char* name, const T& msg) {
    TestArena arena;
    TestContext ctx{arena};

    auto* out = save_members(ctx, FileIdentifierFor<T>::value, const_cast<T&>(msg));
    size_t len = arena.get_size(out);

    printf("  {\n");
    printf("    \"name\": \"%s\",\n", name);
    printf("    \"file_identifier\": %u,\n", FileIdentifierFor<T>::value);
    printf("    \"size\": %zu,\n", len);
    printf("    \"hex\": \"");
    for (size_t i = 0; i < len; i++) printf("%02x", out[i]);
    printf("\"\n");
    printf("  }");
}

int main() {
    printf("[\n");

    { MsgSingleInt32 msg{42}; emit_vector("MsgSingleInt32", msg); }
    printf(",\n");
    { MsgMultiScalar msg{0xAA, 0xBB, 100, 200, 300}; emit_vector("MsgMultiScalar", msg); }
    printf(",\n");
    { MsgWithString msg{0x1234567890ABCDEFLL, "hello, fdb!"}; emit_vector("MsgWithString", msg); }
    printf(",\n");
    { MsgBoolDouble msg{true, 3.14159}; emit_vector("MsgBoolDouble", msg); }
    printf(",\n");

    // Vector tests
    { MsgVectorInt32 msg{99, {10, 20, 30}}; emit_vector("MsgVectorInt32", msg); }
    printf(",\n");
    { MsgEmptyVector msg{{}}; emit_vector("MsgEmptyVector", msg); }
    printf(",\n");

    // Optional tests
    { MsgOptionalPresent msg{42, Optional<int32_t>(123)}; emit_vector("MsgOptionalPresent", msg); }
    printf(",\n");
    { MsgOptionalAbsent msg{42, Optional<int32_t>()}; emit_vector("MsgOptionalAbsent", msg); }
    printf(",\n");
    { MsgOptionalString msg{Optional<std::string>("test")}; emit_vector("MsgOptionalString", msg); }
    printf(",\n");

    // Vector of strings (dynamic size elements → RelativeOffset per element)
    { MsgVectorString msg{{"abc", "def", "ghi"}}; emit_vector("MsgVectorString", msg); }
    printf(",\n");

    // Nested struct tests
    { MsgNested msg{1000, {42, 99}}; emit_vector("MsgNested", msg); }
    printf(",\n");
    { MsgNestedString msg{{200, "hello"}, 7}; emit_vector("MsgNestedString", msg); }
    printf("\n");

    printf("]\n");
    return 0;
}
