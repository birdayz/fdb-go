// name_capture.cpp — Separate compilation unit that captures serializer field names.
//
// This file REDEFINES the serializer() function to stringify field names
// via __VA_ARGS__. It CANNOT coexist with normal FDB serialization in the
// same translation unit. That's why it's a separate .cpp file.
//
// The captured names are stored in a global registry (name_capture.h)
// and read by the schema extractor at runtime.

// CRITICAL: Override serializer BEFORE any FDB includes.
// The original serializer(ar, field1, field2, ...) is a template function
// in flow/serialize.h and a fb_visitor overload in ObjectSerializerTraits.h.
// We replace it with a macro that stringifies the field names.
#define serializer(ar, ...) ar.capture(#__VA_ARGS__)

// Now include FDB headers. All serializer() calls in struct serialize()
// methods will expand to ar.capture("field1, field2, ...").
// This breaks actual serialization but that's fine — we only capture names.

// Minimal includes needed for struct definitions.
// We need the struct declarations but not the full runtime.
#include "fdbclient/StorageServerInterface.h"
#include "fdbclient/CommitProxyInterface.h"
#include "fdbclient/GrvProxyInterface.h"
#include "fdbclient/CoordinationInterface.h"
#include "fdbclient/FDBTypes.h"
#include "fdbclient/Tenant.h"
#include "fdbclient/GlobalConfig.h"
#include "fdbrpc/FlowTransport.h"

#undef serializer  // Restore for any subsequent includes.

#include "name_capture.h"

// protocolVersion() stub — needed because some serialize() methods
// check ar.protocolVersion() in conditional branches.
ProtocolVersion name_capture::NameArchive::protocolVersion() const {
    return currentProtocolVersion();
}

// captureAllNames: called from main() to populate the name registry.
// Add CAPTURE_NAMES(Type) for every message type we care about.
void captureAllNames() {
    // Client messages.
    CAPTURE_NAMES(GetValueRequest);
    CAPTURE_NAMES(GetKeyValuesRequest);
    CAPTURE_NAMES(GetReadVersionRequest);
    CAPTURE_NAMES(GetKeyServerLocationsRequest);
    CAPTURE_NAMES(CommitTransactionRequest);

    // Replies.
    CAPTURE_NAMES(GetValueReply);
    CAPTURE_NAMES(GetKeyValuesReply);
    CAPTURE_NAMES(GetReadVersionReply);
    CAPTURE_NAMES(CommitID);

    // Key nested types (serialize_member).
    CAPTURE_NAMES(SpanContext);
    CAPTURE_NAMES(KeySelectorRef);
    CAPTURE_NAMES(MutationRef);
    CAPTURE_NAMES(KeyRangeRef);
    CAPTURE_NAMES(CommitTransactionRef);
    CAPTURE_NAMES(ReadOptions);
    CAPTURE_NAMES(Error);
    CAPTURE_NAMES(Tag);
}
