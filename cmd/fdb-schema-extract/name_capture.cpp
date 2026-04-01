// name_capture.cpp — Extracts serializer field names from FDB source text.
//
// Since we can't redefine `serializer` without breaking templates,
// we extract field names by reading the source files at runtime and
// finding serializer(ar, field1, field2, ...) calls via string matching.
// This is the same approach as the Go parser but in C++.
//
// The C++ compiler handles all TYPE information (vtables, sizes, traits).
// This file handles only NAME extraction — pure string processing.

#include <string>
#include <map>
#include <vector>
#include <fstream>
#include <sstream>
#include <algorithm>
#include <filesystem>
#include <regex>
#include <cstdio>

static std::map<std::string, std::string> g_names;
std::map<std::string, std::string>& nameRegistry() { return g_names; }

// Find "serializer(ar, field1, field2, ...)" in a struct's serialize() method.
// Returns the comma-separated field list (without "ar").
static std::string extractSerializerArgs(const std::string& content, const std::string& structName) {
    // Find "struct <name> " or "struct <name>:" (declaration, not forward ref).
    size_t pos = std::string::npos;
    for (auto prefix : {"struct ", "class "}) {
        std::string pat = std::string(prefix) + structName;
        size_t p = 0;
        while ((p = content.find(pat, p)) != std::string::npos) {
            // Check that the next char after the name is space, colon, or brace.
            size_t afterName = p + pat.size();
            if (afterName < content.size()) {
                char c = content[afterName];
                if (c == ' ' || c == ':' || c == '{' || c == '\n' || c == '\r') {
                    pos = p;
                    break;
                }
            }
            p += pat.size();
        }
        if (pos != std::string::npos) break;
    }
    if (pos == std::string::npos) return "";

    // Find the last "serializer(ar," within this struct (handles conditional branches).
    // Scan forward from the struct declaration, tracking brace depth.
    int braceDepth = 0;
    bool foundBrace = false;
    std::string lastArgs;

    for (size_t i = pos; i < content.size(); i++) {
        if (content[i] == '{') { braceDepth++; foundBrace = true; }
        if (content[i] == '}') { braceDepth--; if (foundBrace && braceDepth <= 0) break; }

        // Look for "serializer(ar," or "serializer( ar,"
        if (content.substr(i, 11) == "serializer(" || content.substr(i, 12) == "serializer (") {
            // Find the opening paren.
            auto paren = content.find('(', i);
            if (paren == std::string::npos) continue;

            // Find matching closing paren.
            int depth = 1;
            size_t end = paren + 1;
            for (; end < content.size() && depth > 0; end++) {
                if (content[end] == '(') depth++;
                if (content[end] == ')') depth--;
            }
            if (depth != 0) continue;

            // Extract args between parens.
            std::string inner = content.substr(paren + 1, end - paren - 2);

            // Strip "ar," prefix — first arg is always the archive.
            auto comma = inner.find(',');
            if (comma != std::string::npos) {
                lastArgs = inner.substr(comma + 1);
            }
        }
    }

    // Clean up: remove newlines, collapse whitespace.
    for (auto& c : lastArgs) { if (c == '\n' || c == '\r' || c == '\t') c = ' '; }
    // Collapse multiple spaces.
    std::string clean;
    bool lastSpace = false;
    for (char c : lastArgs) {
        if (c == ' ') { if (!lastSpace) clean += c; lastSpace = true; }
        else { clean += c; lastSpace = false; }
    }
    return clean;
}

void captureAllNames() {
    // Read the FDB source files that contain our types.
    // These paths are relative to the FDB source root (/fdb/ in Docker).
    struct { const char* file; std::vector<std::string> types; } sources[] = {
        {"fdbclient/include/fdbclient/StorageServerInterface.h",
         {"GetValueRequest", "GetValueReply", "GetKeyValuesRequest", "GetKeyValuesReply",
          "GetKeyRequest", "GetKeyReply", "StorageServerInterface"}},
        {"fdbclient/include/fdbclient/CommitProxyInterface.h",
         {"GetReadVersionRequest", "GetReadVersionReply",
          "GetKeyServerLocationsRequest", "GetKeyServerLocationsReply",
          "CommitTransactionRequest", "CommitID", "CommitTransactionRef",
          "CommitProxyInterface", "ClientDBInfo"}},
        {"fdbclient/include/fdbclient/CoordinationInterface.h",
         {"OpenDatabaseCoordRequest"}},
        {"fdbclient/include/fdbclient/FDBTypes.h",
         {"KeySelectorRef", "KeyRangeRef", "ReadOptions", "MutationRef", "KeyValueRef"}},
        {"fdbclient/include/fdbclient/Tracing.h",
         {"SpanContext"}},
        {"fdbclient/include/fdbclient/GrvProxyInterface.h",
         {"GrvProxyInterface", "GetReadVersionRequest", "GetReadVersionReply"}},
        {"fdbrpc/include/fdbrpc/TenantInfo.h",
         {"TenantInfo"}},
        {"flow/include/flow/NetworkAddress.h",
         {"NetworkAddress", "IPAddress"}},
    };

    // Read and parse.
    for (auto& src : sources) {
        std::string path = std::string("/fdb/") + src.file;
        std::ifstream f(path);
        if (!f.is_open()) {
            fprintf(stderr, "name_capture: cannot open %s\n", path.c_str());
            continue;
        }
        std::string content((std::istreambuf_iterator<char>(f)),
                            std::istreambuf_iterator<char>());
        for (auto& typeName : src.types) {
            auto args = extractSerializerArgs(content, typeName);
            if (!args.empty()) {
                g_names[typeName] = args;
            }
        }
    }
    // Types with trivial, non-standard, or external serialize — manually specified names.
    g_names["Error"] = "error_code";
    g_names["ReplyPromise"] = "token";
    g_names["MutationRef"] = "mutType, param1, param2";
    // TenantInfo: serialize is in serializable_traits<TenantInfo>, not inside struct body.
    g_names["TenantInfo"] = "tenantId, token, arena";
    // std::pair<KeyRangeRef, vector<SS>>: serialize(ar, first, second)
    g_names["LocationPair"] = "keyRange, servers";

    fprintf(stderr, "name_capture: captured %zu type names\n", g_names.size());
}
