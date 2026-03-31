// name_capture.h — Captures serializer field names via macro override.
//
// HOW IT WORKS:
// FDB structs have: void serialize(Ar& ar) { serializer(ar, field1, field2, ...); }
// The serializer() function for fb_visitors (ObjectSerializerTraits.h:87) calls ar(items...).
// We can't get variable names from templates. But the PREPROCESSOR can stringify them.
//
// We #define serializer to stringify __VA_ARGS__ BEFORE including FDB headers.
// This breaks normal FDB serialization, so this header is used in a SEPARATE
// compilation unit that ONLY captures names — no actual serialization.
//
// The captured names are stored in a global registry keyed by type name.

#pragma once

#include <string>
#include <map>
#include <vector>
#include <sstream>

namespace name_capture {

// Global registry: type name → comma-separated field names.
inline std::map<std::string, std::string>& registry() {
    static std::map<std::string, std::string> r;
    return r;
}

// Archive that captures stringified field names.
struct NameArchive {
    std::string captured_names;

    // Called by our redefined serializer macro.
    void capture(const char* names) {
        captured_names = names;
    }

    // Required by FDB's serialize() methods for conditional branches.
    ProtocolVersion protocolVersion() const;
    static constexpr bool isDeserializing = false;
    static constexpr bool isSerializing = false;
};

// Split "field1, field2, field3" → ["field1", "field2", "field3"]
inline std::vector<std::string> split(const std::string& csv) {
    std::vector<std::string> result;
    std::istringstream ss(csv);
    std::string token;
    while (std::getline(ss, token, ',')) {
        size_t start = token.find_first_not_of(" \t\n\r");
        size_t end = token.find_last_not_of(" \t\n\r");
        if (start != std::string::npos)
            result.push_back(token.substr(start, end - start + 1));
    }
    return result;
}

// Retrieve names for a type. Returns empty vector if not captured.
inline std::vector<std::string> getNamesFor(const std::string& typeName) {
    auto it = registry().find(typeName);
    if (it != registry().end())
        return split(it->second);
    return {};
}

} // namespace name_capture

// CAPTURE_NAMES: call in the name-capture compilation unit for each type.
// Usage: CAPTURE_NAMES(GetValueRequest)
// The struct's serialize() calls serializer(ar, field1, field2, ...).
// Our redefined serializer captures the stringified args.
#define CAPTURE_NAMES(Type) do { \
    name_capture::NameArchive _ar; \
    Type _msg{}; \
    _msg.serialize(_ar); \
    name_capture::registry()[#Type] = _ar.captured_names; \
} while(0)
