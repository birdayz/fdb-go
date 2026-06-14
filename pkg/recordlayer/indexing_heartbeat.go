package recordlayer

import (
	"fmt"
	"time"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
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
}

// NewIndexingHeartbeat creates a heartbeat manager for an indexer.
// leaseLengthMs is how long a heartbeat remains valid before being considered stale.
func NewIndexingHeartbeat(info string, leaseLengthMs int64, allowMutual bool) *IndexingHeartbeat {
	return &IndexingHeartbeat{
		indexerID:     uuid.New(),
		info:          info,
		createTimeMs:  time.Now().UnixMilli(),
		leaseLengthMs: leaseLengthMs,
		allowMutual:   allowMutual,
	}
}

// heartbeatSubspace returns the subspace for all heartbeats of an index.
// Layout: [9, indexSubspaceKey, 7]
func heartbeatSubspace(storeSubspace subspace.Subspace, index *Index) subspace.Subspace {
	return storeSubspace.Sub(IndexBuildSpaceKey, index.SubspaceTupleKey(), indexBuildHeartbeatSubKey)
}

// heartbeatKey returns the FDB key for this indexer's heartbeat.
// Layout: [9, indexSubspaceKey, 7, uuid_string]
func (h *IndexingHeartbeat) heartbeatKey(storeSubspace subspace.Subspace, index *Index) fdb.Key {
	return heartbeatSubspace(storeSubspace, index).Sub(h.indexerID.String()).Bytes()
}

// CheckAndUpdate checks for conflicting heartbeats and updates this indexer's heartbeat.
//
// In non-mutual mode: scans all heartbeats for this index. If any active (non-stale)
// heartbeat from a different indexer exists, returns a SynchronizedSessionLockedError.
// Stale heartbeats (older than leaseLengthMs) are ignored — the process is presumed dead.
//
// In mutual mode: skips the check entirely and just updates.
//
// Called once per buildRange transaction to maintain liveness.
// Matches Java's IndexingHeartbeat.checkAndUpdateHeartbeat().
func (h *IndexingHeartbeat) CheckAndUpdate(tx fdb.WritableTransaction, storeSubspace subspace.Subspace, index *Index) error {
	if h.allowMutual {
		h.update(tx, storeSubspace, index)
		return nil
	}

	// Non-mutual: scan all heartbeats and reject if active peer exists.
	hbSub := heartbeatSubspace(storeSubspace, index)
	rr := tx.GetRange(hbSub, fdb.RangeOptions{})
	kvs, err := rr.GetSliceWithError()
	if err != nil {
		return fmt.Errorf("scan heartbeats: %w", err)
	}

	now := time.Now().UnixMilli()
	for _, kv := range kvs {
		// Extract the UUID from the key.
		t, err := fastSubspaceUnpack(kv.Key, len(hbSub.Bytes()))
		if err != nil || len(t) == 0 {
			continue
		}
		otherID, ok := t[0].(string)
		if !ok {
			continue
		}
		if otherID == h.indexerID.String() {
			continue // our own heartbeat
		}

		// Parse the heartbeat proto.
		var hb gen.IndexBuildHeartbeat
		if err := hb.UnmarshalVT(kv.Value); err != nil {
			continue // corrupt heartbeat, ignore
		}

		age := now - hb.GetHeartbeatTimeMilliseconds()

		// Clock skew protection: reject heartbeats >1 day in the future.
		// Matches Java's age > TimeUnit.DAYS.toMillis(-1).
		if age < -86_400_000 {
			continue
		}

		if age < h.leaseLengthMs {
			// Active heartbeat from another process — cannot proceed.
			return &SynchronizedSessionLockedError{
				ExistingIndexerID: otherID,
				ExistingInfo:      hb.GetInfo(),
				HeartbeatAgeMs:    age,
				LeaseLengthMs:     h.leaseLengthMs,
			}
		}
		// Stale heartbeat — process is presumed dead, ignore.
	}

	h.update(tx, storeSubspace, index)
	return nil
}

// update writes this indexer's heartbeat with the current timestamp.
// Matches Java's IndexingHeartbeat.updateHeartbeat().
func (h *IndexingHeartbeat) update(tx fdb.WritableTransaction, storeSubspace subspace.Subspace, index *Index) {
	hb := &gen.IndexBuildHeartbeat{
		Info:                      proto.String(h.info),
		CreateTimeMilliseconds:    proto.Int64(h.createTimeMs),
		HeartbeatTimeMilliseconds: proto.Int64(time.Now().UnixMilli()),
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
// Matches Java's SynchronizedSessionLockedException.
type SynchronizedSessionLockedError struct {
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
		t, err := fastSubspaceUnpack(kv.Key, len(hbSub.Bytes()))
		if err != nil || len(t) == 0 {
			continue
		}
		id, ok := t[0].(string)
		if !ok {
			continue
		}
		var hb gen.IndexBuildHeartbeat
		if err := hb.UnmarshalVT(kv.Value); err != nil {
			continue
		}
		indexerIDs = append(indexerIDs, id)
		heartbeats = append(heartbeats, &hb)
	}
	return heartbeats, indexerIDs, nil
}
