package transport

// Well-known endpoint token IDs.
// These are deterministic UIDs that FDB coordinators listen on.
// From fdbrpc/include/fdbrpc/WellKnownEndpoints.h
const (
	// WLTokenFirstAvailable is the first available well-known token ID.
	WLTokenFirstAvailable = 3

	// WLTokenClientLeaderRegGetLeader is the getLeader endpoint.
	WLTokenClientLeaderRegGetLeader = 3

	// WLTokenClientLeaderRegOpenDatabase is the openDatabase endpoint
	// used for coordinator bootstrap (OpenDatabaseCoordRequest → ClientDBInfo).
	WLTokenClientLeaderRegOpenDatabase = 4

	// WLTokenProtocolInfo is the protocol info endpoint (stable value).
	WLTokenProtocolInfo = 10
)

// WellKnownToken returns the UID for a well-known endpoint token.
// In C++: Endpoint::wellKnownToken(id) = UID(-1, id).
// The first part is all-ones (0xFFFFFFFFFFFFFFFF), second part is the token ID.
func WellKnownToken(id int) UID {
	return UID{
		First:  0xFFFFFFFFFFFFFFFF,
		Second: uint64(id),
	}
}
