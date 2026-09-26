package recordlayer

import (
	"context"
	"fmt"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

// InvalidHeartbeatInfo is Java's IndexingHeartbeat.INVALID_HEARTBEAT_INFO: a heartbeat
// whose value does not parse is reported with this info and zero create/heartbeat
// times, so a caller sees it instead of losing it.
const InvalidHeartbeatInfo = "<< Invalid Heartbeat >>"

func invalidHeartbeat() *gen.IndexBuildHeartbeat {
	return &gen.IndexBuildHeartbeat{
		Info:                      proto.String(InvalidHeartbeatInfo),
		CreateTimeMilliseconds:    proto.Int64(0),
		HeartbeatTimeMilliseconds: proto.Int64(0),
	}
}

// GetIndexingHeartbeats reads an index's session heartbeats keyed by indexer id.
// maxCount > 0 reads at most that many (Java's safety valve); 0 or less reads all.
// A key that is not a UUID heartbeat key is an IndexingHeartbeatKeyError, as in
// admission: Java's getUUID on such a key throws too.
// Matches Java's IndexingHeartbeat.getIndexingHeartbeats (IndexingHeartbeat.java:135-163).
func GetIndexingHeartbeats(tx fdb.ReadTransaction, storeSubspace subspace.Subspace, index *Index, maxCount int) (map[uuid.UUID]*gen.IndexBuildHeartbeat, error) {
	entries, err := collectIndexingHeartbeats(tx, storeSubspace, index, maxCount)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]*gen.IndexBuildHeartbeat, len(entries))
	for id, e := range entries {
		out[id] = e.hb
	}
	return out, nil
}

// heartbeatEntry is one indexer id's heartbeat as Java's getIndexingHeartbeats leaves
// it: the value of the LAST key that names the id, in key order, because the Java
// method puts every key into a HashMap<UUID, ...> and a later put replaces an
// earlier one (IndexingHeartbeat.java:135-163). Several keys name one id when a key
// carries elements after the UUID ((U) and (U, x)): getUUID(0) reads only the first.
// valid is false for a value that does not parse, which Java reports as the
// invalid-heartbeat placeholder and Go carries in hb the same way.
type heartbeatEntry struct {
	hb    *gen.IndexBuildHeartbeat
	valid bool
}

func collectIndexingHeartbeats(tx fdb.ReadTransaction, storeSubspace subspace.Subspace, index *Index, maxCount int) (map[uuid.UUID]heartbeatEntry, error) {
	hbSub := heartbeatSubspace(storeSubspace, index)
	opts := fdb.RangeOptions{}
	if maxCount > 0 {
		opts.Limit = maxCount
	}
	kvs, err := tx.GetRange(hbSub, opts).GetSliceWithError()
	if err != nil {
		return nil, fmt.Errorf("scan heartbeats: %w", err)
	}
	out := make(map[uuid.UUID]heartbeatEntry, len(kvs))
	for _, kv := range kvs {
		id, err := heartbeatIndexerID(kv.Key, hbSub, index)
		if err != nil {
			return nil, err
		}
		hb := &gen.IndexBuildHeartbeat{}
		if err := UnmarshalVTAsJava(hb, kv.Value); err != nil {
			out[id] = heartbeatEntry{hb: invalidHeartbeat()}
			continue
		}
		out[id] = heartbeatEntry{hb: hb, valid: true}
	}
	return out, nil
}

// ClearIndexingHeartbeats clears the heartbeats of an index whose last beat is at
// least minAgeMs old at nowMs, and every heartbeat whose value does not parse. It
// examines at most maxIteration heartbeats when maxIteration > 0 and returns how
// many it cleared. Like Java it never parses the KEY, so it also clears legacy
// string-keyed heartbeats once their values are old or invalid.
// Matches Java's IndexingHeartbeat.clearIndexingHeartbeats (IndexingHeartbeat.java:165-195).
func ClearIndexingHeartbeats(tx fdb.WritableTransaction, storeSubspace subspace.Subspace, index *Index, minAgeMs int64, maxIteration int, nowMs int64) (int, error) {
	hbSub := heartbeatSubspace(storeSubspace, index)
	opts := fdb.RangeOptions{}
	if maxIteration > 0 {
		opts.Limit = maxIteration
	}
	kvs, err := tx.GetRange(hbSub, opts).GetSliceWithError()
	if err != nil {
		return 0, fmt.Errorf("scan heartbeats: %w", err)
	}
	cleared := 0
	for _, kv := range kvs {
		var hb gen.IndexBuildHeartbeat
		remove := true // an unparsable heartbeat is always cleared
		if err := UnmarshalVTAsJava(&hb, kv.Value); err == nil {
			remove = nowMs >= hb.GetHeartbeatTimeMilliseconds()+minAgeMs
		}
		if remove {
			tx.Clear(kv.Key)
			cleared++
		}
	}
	return cleared, nil
}

// CheckAnyOngoingOnlineIndexBuilds reports whether any indexer's heartbeat marks a live
// session at nowMs, mutual or exclusive: a heartbeat that PARSES and that exclusive
// session admission would refuse to start against, heartbeatBlocksSession(age,
// leaseMs) (mutual admission refuses no live peer, IndexingHeartbeat.java:90-93). It
// judges each indexer id's SURVIVING heartbeat (below), where admission judges every
// key, so over one key per id "ongoing" is "a new exclusive session would be refused",
// and over two keys for one id the two can differ.
//
// Every heartbeat KEY is parsed first, as Java's getIndexingHeartbeats does before
// any time is compared (getUUID(0) sits outside its try, IndexingHeartbeat.java:148):
// a key whose first tuple element is not a UUID is an IndexingHeartbeatKeyError
// wherever it sits in the range (a string key sorts before every UUID key, a
// versionstamp key after it), whatever the other heartbeats say, and a (UUID, x) key
// is that UUID's heartbeat, as in Java. Admission fails closed on the
// same keys (heartbeatIndexerID), so a legacy string-keyed heartbeat cannot read "not
// ongoing" here while every session is refused.
//
// DIVERGENCE (DIVERGENCES.md, "checkAnyOngoingOnlineIndexBuilds"): Java documents the
// same contract ("a heartbeat that is less than DEFAULT_LEASE_LENGTH_MILLIS old",
// OnlineIndexer.java:442) but computes heartbeatTime < now + lease over
// getIndexingHeartbeats (OnlineIndexer.java:459-462), where an unparseable heartbeat
// carries time 0. Over UUID-keyed heartbeats the engines therefore disagree on exactly
// three populations, each pinned against the JVM: a stale heartbeat (a crashed
// session: Java ongoing forever, Go not); an invalid-only heartbeat (Java ongoing, Go
// not: admission ignores it, IndexingHeartbeat.java:118-123); and a heartbeat dated
// more than the lease but less than a day ahead (Java not ongoing, Go ongoing:
// admission refuses a session against it, IndexingHeartbeat.java:107). A heartbeat
// more than a day ahead is bad data to both, and a non-UUID key is an error in both.
//
// The rule is applied to the SAME population Java's check reads: each indexer id's
// surviving heartbeat after getIndexingHeartbeats' collapse (collectIndexingHeartbeats),
// not every key. When a (U) key and a (U, x) key both exist, the (U, x) value is the
// one both engines judge, so the three populations above are still the only
// differences between the engines: a live (U) beside a (U, x) a day ahead is not
// ongoing in either, and a live (U) beside an UNPARSEABLE (U, x) is the invalid-only
// population (Java ongoing, Go not), each pinned against the JVM. In both cases
// admission, which reads every key, refuses a new exclusive session over the live (U):
// that is where "ongoing" and "would be refused" part, in Java as in Go.
func CheckAnyOngoingOnlineIndexBuilds(tx fdb.ReadTransaction, storeSubspace subspace.Subspace, index *Index, leaseMs int64, nowMs int64) (bool, error) {
	entries, err := collectIndexingHeartbeats(tx, storeSubspace, index, 0)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if !e.valid {
			continue
		}
		if heartbeatBlocksSession(nowMs-e.hb.GetHeartbeatTimeMilliseconds(), leaseMs) {
			return true, nil
		}
	}
	return false, nil
}

// CheckAnyOngoingOnlineIndexBuildsForStore is Java's static
// OnlineIndexer.checkAnyOngoingOnlineIndexBuildsAsync(recordStore, index): the check at
// Java's default lease, in the store's transaction and clock.
func CheckAnyOngoingOnlineIndexBuildsForStore(store *FDBRecordStore, index *Index) (bool, error) {
	return CheckAnyOngoingOnlineIndexBuilds(store.context.Transaction(), store.subspace, index,
		defaultLeaseLengthMs, store.context.Env().Now().UnixMilli())
}

// GetIndexingHeartbeats reads the primary target index's session heartbeats in one
// transaction. Matches Java's OnlineIndexer.getIndexingHeartbeats(maxCount)
// (IndexingBase.java:1235-1238).
func (oi *OnlineIndexer) GetIndexingHeartbeats(ctx context.Context, maxCount int) (map[uuid.UUID]*gen.IndexBuildHeartbeat, error) {
	result, err := oi.db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		store, err := oi.openStore(rtx)
		if err != nil {
			return nil, err
		}
		return GetIndexingHeartbeats(rtx.Transaction(), store.subspace, oi.primaryIndex(), maxCount)
	})
	if err != nil {
		return nil, err
	}
	return result.(map[uuid.UUID]*gen.IndexBuildHeartbeat), nil
}

// ClearIndexingHeartbeats clears the primary target index's heartbeats older than
// minAgeMs (and invalid ones), examining at most maxIteration when positive, and
// returns how many it cleared. Matches Java's OnlineIndexer.clearIndexingHeartbeats
// (IndexingBase.java:1240-1243).
func (oi *OnlineIndexer) ClearIndexingHeartbeats(ctx context.Context, minAgeMs int64, maxIteration int) (int, error) {
	result, err := oi.db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		store, err := oi.openStore(rtx)
		if err != nil {
			return nil, err
		}
		return ClearIndexingHeartbeats(rtx.Transaction(), store.subspace, oi.primaryIndex(), minAgeMs, maxIteration,
			rtx.Env().Now().UnixMilli())
	})
	if err != nil {
		return 0, err
	}
	return result.(int), nil
}

// CheckAnyOngoingOnlineIndexBuilds reports whether the primary target index has a
// session heartbeat younger than this indexer's lease. Matches Java's
// OnlineIndexer.checkAnyOngoingOnlineIndexBuilds() (OnlineIndexer.java:426-438), with
// the documented-contract divergence described on the package-level function.
func (oi *OnlineIndexer) CheckAnyOngoingOnlineIndexBuilds(ctx context.Context) (bool, error) {
	result, err := oi.db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		store, err := oi.openStore(rtx)
		if err != nil {
			return nil, err
		}
		return CheckAnyOngoingOnlineIndexBuilds(rtx.Transaction(), store.subspace, oi.primaryIndex(),
			resolvedLeaseLengthMs(oi.leaseLengthMs), rtx.Env().Now().UnixMilli())
	})
	if err != nil {
		return false, err
	}
	return result.(bool), nil
}
