// name_capture.cpp — Captures serializer field names via preprocessor stringification.
//
// SEPARATE compilation unit. Redefines `serializer` BEFORE including FDB headers.
// This breaks normal serialization but captures field names as strings.
//
// The redefined serializer(ar, field1, field2, ...) expands to:
//   ar.capture("field1, field2, ...")
// where the preprocessor stringifies __VA_ARGS__.

#include <string>
#include <map>

// Global name registry.
static std::map<std::string, std::string> g_names;
std::map<std::string, std::string>& nameRegistry() { return g_names; }

// Lightweight archive that just captures the stringified field names.
struct NameArchive {
    std::string names;
    void capture(const char* s) { names = s; }

    // Stubs required by some serialize() conditional branches.
    static constexpr bool isDeserializing = false;
    static constexpr bool isSerializing = false;
    struct FakeProtocolVersion {
        bool hasMutationChecksum() const { return false; }
        bool hasAccumulativeChecksumIndex() const { return false; }
    };
    FakeProtocolVersion protocolVersion() const { return {}; }
};

// Override serializer BEFORE including FDB headers.
// The original is a template function in flow/serialize.h and
// ObjectSerializerTraits.h. Our macro intercepts at the preprocessor level.
#define serializer(ar, ...) (ar).capture(#__VA_ARGS__)

// Include FDB headers — serialize() methods now capture names instead of serializing.
#include "fdbclient/StorageServerInterface.h"
#include "fdbclient/CommitProxyInterface.h"
#include "fdbclient/GrvProxyInterface.h"
#include "fdbclient/CoordinationInterface.h"
#include "fdbclient/FDBTypes.h"

#undef serializer

// Capture names for one type.
#define CAPTURE(Type) do { \
    NameArchive ar; \
    Type msg{}; \
    msg.serialize(ar); \
    g_names[#Type] = ar.names; \
} while(0)

void captureAllNames() {
    // Client messages.
    CAPTURE(GetValueRequest);
    CAPTURE(GetValueReply);
    CAPTURE(GetKeyValuesRequest);
    CAPTURE(GetKeyValuesReply);
    CAPTURE(GetKeyRequest);
    CAPTURE(GetKeyReply);
    CAPTURE(GetReadVersionRequest);
    CAPTURE(GetReadVersionReply);
    CAPTURE(GetKeyServerLocationsRequest);
    CAPTURE(GetKeyServerLocationsReply);
    CAPTURE(CommitTransactionRequest);
    CAPTURE(CommitID);

    // Coordinator.
    CAPTURE(OpenDatabaseCoordRequest);

    // Nested types.
    CAPTURE(SpanContext);
    CAPTURE(KeySelectorRef);
    CAPTURE(MutationRef);
    CAPTURE(KeyRangeRef);
    CAPTURE(CommitTransactionRef);
    CAPTURE(ReadOptions);
    CAPTURE(Error);
    CAPTURE(Tag);
    // UID uses serialize_unversioned, not serialize — skip name capture.
}
