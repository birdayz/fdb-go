package recordlayer

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// The record-layer side of a GUARDIANN vector index: GuardiannVectorIndexEngine,
// VectorIndexTaskCounts, the task registers and VectorIndexMergeLock.

// Secondary-subspace keys (VectorIndexSecondarySubspaceKeys).
const (
	vectorSecondaryTaskCounts     = 0x00
	vectorSecondaryMergeLock      = 0x01
	vectorSecondaryMergeLockGuard = 0x02
)

// VectorIndexClusterTooLargeError is Java's VectorIndexClusterTooLargeException:
// a deferred-maintenance insert found its cluster at the hard cap.
type VectorIndexClusterTooLargeError struct {
	Message string
	Cause   error
}

func (e *VectorIndexClusterTooLargeError) Error() string { return e.Message }
func (e *VectorIndexClusterTooLargeError) Unwrap() error { return e.Cause }

// NegativeTaskCountError is VectorIndexTaskCounts.NegativeTaskCountException.
type NegativeTaskCountError struct {
	Prefix tuple.Tuple
	Count  int64
}

func (e *NegativeTaskCountError) Error() string {
	return fmt.Sprintf("vector index deferred-task count is negative: %d for %v", e.Count, e.Prefix)
}

// vectorTaskCounts is VectorIndexTaskCounts: per-partition counts of queued
// GuardiANN tasks, maintained with atomic adds.
type vectorTaskCounts struct{ ss subspace.Subspace }

var (
	littleEndianOne      = []byte{1, 0, 0, 0, 0, 0, 0, 0}
	littleEndianMinusOne = []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	zeroCount            = make([]byte, 8)
)

func newVectorTaskCounts(secondary subspace.Subspace) vectorTaskCounts {
	return vectorTaskCounts{ss: secondary.Sub(int64(vectorSecondaryTaskCounts))}
}

func (c vectorTaskCounts) adjust(tx fdb.WritableTransaction, prefix tuple.Tuple, delta []byte) {
	key := fdb.Key(c.ss.Pack(prefix))
	tx.Add(key, delta)
	tx.CompareAndClear(key, zeroCount)
}

func (c vectorTaskCounts) clearPrefix(tx fdb.WritableTransaction, prefix tuple.Tuple) error {
	r, err := fdb.PrefixRange(c.ss.Pack(prefix))
	if err != nil {
		return err
	}
	tx.ClearRange(r)
	return nil
}

type prefixTaskCount struct {
	prefix tuple.Tuple
	count  int64
}

func (c vectorTaskCounts) outstanding(tx fdb.ReadTransaction, limit int) ([]prefixTaskCount, error) {
	r, err := fdb.PrefixRange(c.ss.Bytes())
	if err != nil {
		return nil, err
	}
	kvs, err := tx.GetRange(r, fdb.RangeOptions{}).GetSliceWithError()
	if err != nil {
		return nil, err
	}
	var out []prefixTaskCount
	for _, kv := range kvs {
		if len(out) >= limit {
			break
		}
		p, err := c.ss.Unpack(kv.Key)
		if err != nil {
			return nil, err
		}
		n := decodeTaskCount(kv.Value)
		if n < 0 {
			return nil, &NegativeTaskCountError{Prefix: p, Count: n}
		}
		if n > 0 {
			out = append(out, prefixTaskCount{prefix: p, count: n})
		}
	}
	return out, nil
}

func (c vectorTaskCounts) countFor(tx fdb.ReadTransaction, prefix tuple.Tuple) (int64, error) {
	v, err := tx.Get(fdb.Key(c.ss.Pack(prefix))).Get()
	if err != nil {
		return 0, err
	}
	n := decodeTaskCount(v)
	if n < 0 {
		return 0, &NegativeTaskCountError{Prefix: prefix, Count: n}
	}
	return n, nil
}

func (c vectorTaskCounts) hasOutstanding(tx fdb.ReadTransaction) (bool, error) {
	r, err := fdb.PrefixRange(c.ss.Bytes())
	if err != nil {
		return false, err
	}
	kvs, err := tx.GetRange(r, fdb.RangeOptions{Limit: 1}).GetSliceWithError()
	return len(kvs) > 0, err
}

func decodeTaskCount(v []byte) int64 {
	if len(v) < 8 {
		var b [8]byte
		copy(b[:], v)
		return int64(binary.LittleEndian.Uint64(b[:]))
	}
	return int64(binary.LittleEndian.Uint64(v))
}

// guardiannRegister composes TaskCountRegister and MaintenanceControlRegister.
type guardiannRegister struct {
	tx       fdb.WritableTransaction
	counts   vectorTaskCounts
	prefix   tuple.Tuple
	control  *IndexDeferredMaintenanceControl // set when the caller merges later
	index    *Index
	signaled bool
}

func (r *guardiannRegister) onTaskEnqueued() {
	r.counts.adjust(r.tx, r.prefix, littleEndianOne)
	if r.control != nil && !r.signaled {
		r.signaled = true
		_ = r.control.SetMergeRequiredIndexes(r.index)
	}
}

func (r *guardiannRegister) onTaskExecuted() { r.counts.adjust(r.tx, r.prefix, littleEndianMinusOne) }

func (m *vectorIndexMaintainer) mergeControl() *IndexDeferredMaintenanceControl {
	if s, ok := m.store.(*FDBRecordStore); ok {
		return s.GetIndexDeferredMaintenanceControl()
	}
	return nil
}

func (m *vectorIndexMaintainer) guardiannFor(prefix tuple.Tuple, listener guardiannListener) *guardiann {
	return newGuardiann(m.getSubspaceForPrefix(prefix), m.guardiannConfig, m.store.Env(), listener)
}

// applyGuardiannEntry is VectorIndexMaintainer.updateIndexEntry over the
// GuardiANN engine.
func (m *vectorIndexMaintainer) applyGuardiannEntry(prefix, pk tuple.Tuple, vector gVector, remove bool) error {
	control := m.mergeControl()
	maintainInTransaction := control == nil || control.ShouldAutoMergeDuringCommit()
	reg := &guardiannRegister{tx: m.tx, counts: m.taskCounts, prefix: prefix, index: m.index}
	if !maintainInTransaction {
		reg.control = control
	}
	lockKey := string(m.getSubspaceForPrefix(prefix).Bytes())
	m.store.AcquireWriteLock(lockKey)
	g := m.guardiannFor(prefix, reg)
	var err error
	if remove {
		err = g.delete(m.tx, pk, vector, maintainInTransaction)
	} else {
		err = g.insert(m.tx, pk, vector, nil, maintainInTransaction)
	}
	m.store.ReleaseWriteLock(lockKey)
	var capacity *guardiannClusterCapacityError
	if errors.As(err, &capacity) {
		return &VectorIndexClusterTooLargeError{Message: capacity.Error(), Cause: err}
	}
	if err != nil || reg.control == nil || reg.signaled {
		return err
	}
	has, err := m.taskCounts.hasOutstanding(m.tx.Snapshot())
	if err != nil || !has {
		return err
	}
	return control.SetMergeRequiredIndexes(m.index)
}

func (m *vectorIndexMaintainer) searchGuardiann(readTx fdb.ReadTransaction, prefix tuple.Tuple, query []float64, k int) ([]guardiannResult, error) {
	if len(query) != m.guardiannConfig.numDimensions {
		return nil, fmt.Errorf("VECTOR index %q expects %d dimensions, but query vector has %d",
			m.index.Name, m.guardiannConfig.numDimensions, len(query))
	}
	return m.guardiannFor(prefix, nil).search(readTx, k, defaultGuardiannSearchConfig(), gVector{data: query, typ: 2})
}

// vectorMergeLeaseWindow is VectorIndexMergeLock.DEFAULT_LEASE_WINDOW_MILLIS.
const vectorMergeLeaseWindow = 60 * time.Second

// vectorMergeMaxPrefixesExamined bounds the prefix scan of one merge step
// (VectorIndexMaintainer.MAX_PREFIXES_EXAMINED).
const vectorMergeMaxPrefixesExamined = 16

// vectorMergeDefaultTimeQuota is VectorIndexMaintainer.DEFAULT_MERGE_TIME_QUOTA_MILLIS.
const vectorMergeDefaultTimeQuota = 4 * time.Second

// MergeIndex is VectorIndexMaintainer.mergeIndex: drain one owned partition's
// deferred tasks under the merge lock, or claim a free partition.
func (m *vectorIndexMaintainer) MergeIndex() error {
	if m.engine != VectorEngineGuardiann {
		return nil
	}
	control := m.mergeControl()
	if control == nil {
		return nil
	}
	control.SetLastStep(DeferredMaintenanceMerge)
	budget := 1
	if l := control.GetMergesLimit(); l > 0 {
		budget = int(min(l, 1<<31-1))
	}
	owner := control.GetMergeSessionID()
	if owner == nil {
		return &RecordCoreError{Message: "vector index merge requires a merge session id on the " +
			"IndexDeferredMaintenanceControl; drive the merge through the OnlineIndexer / IndexingMerger"}
	}
	lock := vectorMergeLock{secondary: m.secondarySubspace, owner: *owner, now: m.store.Env().Now}
	prefixes, err := m.taskCounts.outstanding(m.tx.Snapshot(), vectorMergeMaxPrefixesExamined)
	var negative *NegativeTaskCountError
	if errors.As(err, &negative) {
		reportMergeProgress(control, 0, false)
		return m.disableOnNegativeTaskCount()
	}
	if err != nil {
		return err
	}
	var free []tuple.Tuple
	for _, p := range prefixes {
		cur, err := lock.currentOwner(m.tx.Snapshot(), p.prefix)
		if err != nil {
			return err
		}
		if cur != nil && *cur == *owner {
			return m.drainOwnedPrefix(lock, p, budget, control)
		}
		if cur == nil {
			free = append(free, p.prefix)
		}
	}
	if len(free) > 0 {
		var b [8]byte
		if _, err := m.store.Env().Read(b[:]); err != nil {
			return err
		}
		lock.acquire(m.tx, free[binary.BigEndian.Uint64(b[:])%uint64(len(free))])
		reportMergeProgress(control, 0, true)
		return nil
	}
	reportMergeProgress(control, 0, false)
	return nil
}

func (m *vectorIndexMaintainer) drainOwnedPrefix(lock vectorMergeLock, owned prefixTaskCount, budget int, control *IndexDeferredMaintenanceControl) error {
	lock.acquire(m.tx, owned.prefix)
	quota := time.Duration(control.GetTimeQuotaMillis()) * time.Millisecond
	if quota <= 0 {
		quota = vectorMergeDefaultTimeQuota
		control.SetTimeQuotaMillis(quota.Milliseconds())
	}
	reg := &guardiannRegister{tx: m.tx, counts: m.taskCounts, prefix: owned.prefix, index: m.index}
	g := m.guardiannFor(owned.prefix, reg)
	executed, err := g.executeDeferredTasks(m.tx, int(min(owned.count, int64(budget))), m.store.Env().Now().Add(quota))
	if err != nil {
		return err
	}
	remaining, err := m.taskCounts.countFor(m.tx.Snapshot(), owned.prefix)
	if err != nil {
		return err
	}
	if remaining <= 0 {
		if err := lock.release(m.tx, owned.prefix); err != nil {
			return err
		}
	}
	reportMergeProgress(control, executed, true)
	return nil
}

func reportMergeProgress(control *IndexDeferredMaintenanceControl, executed int, moreWork bool) {
	control.SetMergesTried(int64(executed))
	found := int64(executed)
	if moreWork {
		found++
	}
	control.SetMergesFound(found)
}

// disableOnNegativeTaskCount marks the index disabled at commit, as Java's
// disableIndexOnNegativeTaskCount commit check does.
func (m *vectorIndexMaintainer) disableOnNegativeTaskCount() error {
	s, ok := m.store.(*FDBRecordStore)
	if !ok {
		return nil
	}
	_, err := s.MarkIndexDisabled(m.index.Name)
	return err
}

// vectorMergeLock is VectorIndexMergeLock: a leased owner per partition.
type vectorMergeLock struct {
	secondary subspace.Subspace
	owner     uuid.UUID
	now       func() time.Time
}

func (l vectorMergeLock) key(prefix tuple.Tuple) fdb.Key {
	return fdb.Key(l.secondary.Sub(int64(vectorSecondaryMergeLock)).Pack(prefix))
}

func (l vectorMergeLock) guardKey() fdb.Key {
	return fdb.Key(l.secondary.Pack(tuple.Tuple{int64(vectorSecondaryMergeLockGuard)}))
}

func (l vectorMergeLock) currentOwner(tx fdb.ReadTransaction, prefix tuple.Tuple) (*uuid.UUID, error) {
	v, err := tx.Get(l.key(prefix)).Get()
	if err != nil || v == nil {
		return nil, err
	}
	t, err := tuple.Unpack(v)
	if err != nil {
		return nil, err
	}
	ts := t[1].(int64)
	now := l.now().UnixMilli()
	window := vectorMergeLeaseWindow.Milliseconds()
	if ts <= now-window || ts >= now+window {
		return nil, nil
	}
	u := uuid.UUID(t[0].(tuple.UUID))
	return &u, nil
}

func (l vectorMergeLock) acquire(tx fdb.WritableTransaction, prefix tuple.Tuple) {
	tx.Set(l.key(prefix), tuple.Tuple{tuple.UUID(l.owner), l.now().UnixMilli()}.Pack())
	_ = tx.AddReadConflictKey(l.guardKey())
}

func (l vectorMergeLock) release(tx fdb.WritableTransaction, prefix tuple.Tuple) error {
	v, err := tx.Get(l.key(prefix)).Get()
	if err != nil || v == nil {
		return err
	}
	t, err := tuple.Unpack(v)
	if err != nil {
		return err
	}
	if uuid.UUID(t[0].(tuple.UUID)) == l.owner {
		tx.Clear(l.key(prefix))
	}
	return nil
}

// addDeleteWhereConflicts is VectorIndexMergeLock.addDeleteWhereConflicts.
func addVectorDeleteWhereConflicts(tx fdb.WritableTransaction, secondary subspace.Subspace, prefix tuple.Tuple) error {
	l := vectorMergeLock{secondary: secondary}
	r, err := fdb.PrefixRange(l.key(prefix))
	if err != nil {
		return err
	}
	tx.ClearRange(r)
	return tx.AddWriteConflictKey(l.guardKey())
}
