package recordlayer

import (
	"fmt"

	"fdb.dev/gen"
	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

const (
	// indexBuildHeartbeatSubKey matches Java's IndexingSubspaces.INDEX_BUILD_HEARTBEAT_PREFIX.
	indexBuildHeartbeatSubKey = int64(7)
)

// IndexingHeartbeat manages liveness heartbeats for concurrent index building.
// Each indexer process writes a heartbeat at [9, indexSubspaceKey, 7, uuid] so
// that other processes can detect stale/crashed builders.
//
// In non-mutual (exclusive) mode, an active heartbeat from another process
// prevents this indexer from starting. In mutual mode, heartbeats are written
// but not checked (concurrent building is allowed).
//
// Matches Java's IndexingHeartbeat.
type IndexingHeartbeat struct {
	indexerID     uuid.UUID
	info          string // method description (e.g. "MUTUAL_BY_RECORDS")
	createTimeMs  int64  // epoch ms when this indexer was created
	leaseLengthMs int64  // heartbeat lease duration in ms
	allowMutual   bool   // true for mutual/concurrent mode

	// env is the RFC-199 Tier-0 environment (Clock + Randomness) inherited from the
	// indexer's database. Nil means production; read it through the nil-safe *dst.Env
	// accessors. The indexer UUID, the createTime, and every persisted per-heartbeat
	// timestamp draw from here so a simulation run is byte-reproducible.
	env *dst.Env
}

// NewIndexingHeartbeat creates a heartbeat manager for an indexer.
// leaseLengthMs is how long a heartbeat remains valid before being considered stale.
// env is the DST environment (from the indexer's database, oi.db.Env()); a nil env means
// production — the UUID and timestamps then come from crypto/rand and the wall clock,
// byte-identical to the pre-seam behavior.
func NewIndexingHeartbeat(info string, leaseLengthMs int64, allowMutual bool, env *dst.Env) *IndexingHeartbeat {
	return newIndexingHeartbeatWithID(newIndexerID(env), info, leaseLengthMs, allowMutual, env)
}

// newIndexingHeartbeatWithID is Java's IndexingHeartbeat constructor, which takes
// the indexer's ID (IndexingHeartbeat.java:66): an OnlineIndexer passes its one
// identity rather than minting one per heartbeat.
func newIndexingHeartbeatWithID(indexerID uuid.UUID, info string, leaseLengthMs int64, allowMutual bool, env *dst.Env) *IndexingHeartbeat {
	return &IndexingHeartbeat{
		indexerID:     indexerID,
		info:          info,
		createTimeMs:  env.Now().UnixMilli(),
		leaseLengthMs: leaseLengthMs,
		allowMutual:   allowMutual,
		env:           env,
	}
}

// newIndexerID mints the indexer's unique ID, drawing its 16 bytes from the DST randomness
// seam so a simulation run is reproducible. A nil env reads crypto/rand exactly as uuid.New()
// does — uuid.NewRandomFromReader over crypto/rand, with the same v4 version/variant bits —
// so production output is byte-identical. The err path (crypto/rand cannot fail on supported
// platforms) falls back to uuid.New() so the ID is never the nil UUID.
func newIndexerID(env *dst.Env) uuid.UUID {
	if id, err := uuid.NewRandomFromReader(env); err == nil {
		return id
	}
	return uuid.New()
}

// heartbeatSubspace returns the subspace for all heartbeats of an index.
// Layout: [9, indexSubspaceKey, 7]
func heartbeatSubspace(storeSubspace subspace.Subspace, index *Index) subspace.Subspace {
	return storeSubspace.Sub(IndexBuildSpaceKey, index.SubspaceTupleKey(), indexBuildHeartbeatSubKey)
}

// heartbeatKey returns the FDB key for this indexer's heartbeat.
// Layout: [9, indexSubspaceKey, 7, UUID]
func (h *IndexingHeartbeat) heartbeatKey(storeSubspace subspace.Subspace, index *Index) fdb.Key {
	return heartbeatSubspace(storeSubspace, index).Sub(tuple.UUID(h.indexerID)).Bytes()
}

// CheckAndUpdate checks for conflicting heartbeats and updates this indexer's heartbeat.
//
// In non-mutual mode: scans all heartbeats for this index. If any active (non-stale)
// heartbeat from a different indexer exists, returns a SynchronizedSessionLockedError.
// Stale heartbeats (older than leaseLengthMs) are ignored — the process is presumed dead.
//
// In mutual mode: validates key compatibility but permits active peers.
//
// Used for admission and exclusive renewal. Already-admitted mutual sessions
// renew only their own key, matching Java's mutual checkAndUpdateHeartbeat path.
func (h *IndexingHeartbeat) CheckAndUpdate(tx fdb.WritableTransaction, storeSubspace subspace.Subspace, index *Index) error {
	if err := h.checkAdmission(tx, storeSubspace, index, h.allowMutual); err != nil {
		return err
	}
	h.update(tx, storeSubspace, index)
	return nil
}

// heartbeatBlocksSession is Java's admission predicate over another session's heartbeat
// age: it is a live session when age > -1 day and age < lease
// (IndexingHeartbeat.checkSingleHeartbeat, IndexingHeartbeat.java:106-107). A heartbeat
// dated more than a day ahead is bad data, "long enough to tolerate reasonable clock skews
// between nodes". Admission applies it to every heartbeat key;
// CheckAnyOngoingOnlineIndexBuilds applies it to each indexer id's surviving heartbeat
// after Java's collapse of (U) and (U, x) keys, so the two agree on one key per id and
// can differ when an id has two keys (see CheckAnyOngoingOnlineIndexBuilds).
func heartbeatBlocksSession(ageMs, leaseMs int64) bool {
	return ageMs > -86_400_000 && ageMs < leaseMs
}

// checkAdmission reads without renewing so destructive preparation cannot hide
// incompatible keys or live ownership. A fresh rebuild must exclude peers even
// when its subsequent range-building transactions will allow mutual workers.
func (h *IndexingHeartbeat) checkAdmission(tx fdb.WritableTransaction, storeSubspace subspace.Subspace, index *Index, allowMutual bool) error {
	// Legacy Go builders use string keys and ignore UUID keys. Fail closed even
	// in mutual mode until operators fence old workers and clear legacy keys.
	// A dual reader cannot make those old writers respect Java UUID heartbeats.
	// Non-mutual sessions additionally reject active UUID peers.
	hbSub := heartbeatSubspace(storeSubspace, index)
	rr := tx.GetRange(hbSub, fdb.RangeOptions{})
	kvs, err := rr.GetSliceWithError()
	if err != nil {
		return fmt.Errorf("scan heartbeats: %w", err)
	}

	now := h.env.Now().UnixMilli()
	for _, kv := range kvs {
		otherID, err := heartbeatIndexerID(kv.Key, hbSub, index)
		if err != nil {
			return err
		}
		if allowMutual || otherID == h.indexerID {
			continue // our own heartbeat
		}

		// Parse the heartbeat proto.
		var hb gen.IndexBuildHeartbeat
		if err := UnmarshalVTAsJava(&hb, kv.Value); err != nil {
			continue // corrupt heartbeat, ignore
		}

		age := now - hb.GetHeartbeatTimeMilliseconds()

		if heartbeatBlocksSession(age, h.leaseLengthMs) {
			// Active heartbeat from another process — cannot proceed.
			return &SynchronizedSessionLockedError{
				IndexerID:         h.indexerID,
				ExistingIndexerID: otherID.String(),
				ExistingInfo:      hb.GetInfo(),
				HeartbeatAgeMs:    age,
				LeaseLengthMs:     h.leaseLengthMs,
			}
		}
		// Stale heartbeat — process is presumed dead, ignore.
	}

	return nil
}

// update writes this indexer's heartbeat with the current timestamp.
// Matches Java's IndexingHeartbeat.updateHeartbeat().
func (h *IndexingHeartbeat) update(tx fdb.WritableTransaction, storeSubspace subspace.Subspace, index *Index) {
	hb := &gen.IndexBuildHeartbeat{
		Info:                      proto.String(h.info),
		CreateTimeMilliseconds:    proto.Int64(h.createTimeMs),
		HeartbeatTimeMilliseconds: proto.Int64(h.env.Now().UnixMilli()),
	}
	data, err := hb.MarshalVT()
	if err != nil {
		return // best-effort
	}
	tx.Set(h.heartbeatKey(storeSubspace, index), data)
}

// Cleanup removes this indexer's heartbeat. Called when the build completes or
// is explicitly cancelled.
// Matches Java's IndexingHeartbeat.removeHeartbeat().
func (h *IndexingHeartbeat) Cleanup(tx fdb.WritableTransaction, storeSubspace subspace.Subspace, index *Index) {
	tx.Clear(h.heartbeatKey(storeSubspace, index))
}

// CleanupAll removes ALL heartbeats for an index. Used during index rebuild or
// full clear operations.
func CleanupAllHeartbeats(tx fdb.WritableTransaction, storeSubspace subspace.Subspace, index *Index) {
	hbSub := heartbeatSubspace(storeSubspace, index)
	begin, end := hbSub.FDBRangeKeys()
	tx.ClearRange(fdb.KeyRange{Begin: begin, End: end})
}

// SynchronizedSessionLockedError is returned when a non-mutual indexer detects
// an active heartbeat from another indexer process.
// Matches Java's SynchronizedSessionLockedException, with its log keys
// (indexing/IndexingHeartbeat.java:111-115): INDEXER_ID (IndexerID, the refused
// indexer's own identity), EXISTING_INDEXER_ID, AGE_MILLISECONDS and
// TIME_LIMIT_MILLIS. ExistingInfo, the holder's heartbeat info, is a Go
// addition: Java's exception carries no info key.
type SynchronizedSessionLockedError struct {
	IndexerID         uuid.UUID
	ExistingIndexerID string
	ExistingInfo      string
	HeartbeatAgeMs    int64
	LeaseLengthMs     int64
}

func (e *SynchronizedSessionLockedError) Error() string {
	return fmt.Sprintf(
		"index build session locked by another indexer (id=%s, info=%s, age=%dms, lease=%dms)",
		e.ExistingIndexerID, e.ExistingInfo, e.HeartbeatAgeMs, e.LeaseLengthMs,
	)
}

// IndexingHeartbeatKeyError rejects legacy or malformed heartbeat keys before
// admitting UUID-only builders. Old Go builders must be stopped and fenced before
// administrative cleanup; automatically expiring these keys cannot establish that.
type IndexingHeartbeatKeyError struct {
	IndexName string
	Key       []byte
}

func (e *IndexingHeartbeatKeyError) Error() string {
	return fmt.Sprintf("incompatible indexing heartbeat key %x for index %q: fence legacy builders and clean heartbeat keys before starting UUID-only builders", e.Key, e.IndexName)
}

// heartbeatIndexerID is Java's heartbeatKeyToIndexerId,
// indexHeartbeatSubspace(store, index).unpack(key).getUUID(0)
// (IndexingHeartbeat.java:201-203): the key's FIRST element must be a UUID and any
// trailing elements are ignored, so a (UUID, x) key names that UUID's heartbeat in
// both engines. A key whose first element is not a UUID, or that has no element, is
// a throw in Java (ClassCastException / IndexOutOfBoundsException) and an
// IndexingHeartbeatKeyError here.
func heartbeatIndexerID(key []byte, ss subspace.Subspace, index *Index) (uuid.UUID, error) {
	parts, err := fastSubspaceUnpack(key, len(ss.Bytes()))
	if err == nil && len(parts) >= 1 {
		if id, ok := parts[0].(tuple.UUID); ok {
			return uuid.UUID(id), nil
		}
	}
	return uuid.Nil, &IndexingHeartbeatKeyError{IndexName: index.Name, Key: append([]byte(nil), key...)}
}

// ReadHeartbeats reads all heartbeats for an index. Useful for diagnostics.
func ReadHeartbeats(tx fdb.ReadTransaction, storeSubspace subspace.Subspace, index *Index) ([]*gen.IndexBuildHeartbeat, []string, error) {
	hbSub := heartbeatSubspace(storeSubspace, index)
	rr := tx.GetRange(hbSub, fdb.RangeOptions{})
	kvs, err := rr.GetSliceWithError()
	if err != nil {
		return nil, nil, err
	}

	var heartbeats []*gen.IndexBuildHeartbeat
	var indexerIDs []string
	for _, kv := range kvs {
		id, err := heartbeatIndexerID(kv.Key, hbSub, index)
		if err != nil {
			return nil, nil, err
		}
		var hb gen.IndexBuildHeartbeat
		if err := UnmarshalVTAsJava(&hb, kv.Value); err != nil {
			hb = gen.IndexBuildHeartbeat{
				Info:                      proto.String(InvalidHeartbeatInfo),
				CreateTimeMilliseconds:    proto.Int64(0),
				HeartbeatTimeMilliseconds: proto.Int64(0),
			}
		}
		indexerIDs = append(indexerIDs, id.String())
		heartbeats = append(heartbeats, &hb)
	}
	return heartbeats, indexerIDs, nil
}
