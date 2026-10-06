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

func (*VectorIndexClusterTooLargeError) JavaRecordCoreException() {}

func (e *NegativeTaskCountError) Error() string {
	return fmt.Sprintf("vector index deferred-task count is negative: %d for %v", e.Count, e.Prefix)
}

func (*NegativeTaskCountError) JavaRecordCoreException() {}

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

// The vector counters Java 4.14 adds to FDBStoreTimer.Counts: references
// GuardiANN reads (OnRead.onVectorRead), deferred tasks it enqueues and executes
// (OnWrite), and indexes disabled because a task count went negative
// (VectorIndexMaintainer.disableIndexOnNegativeTaskCount).
var (
	CountVectorVectorReads                      = Event{"vector_vector_reads", "vector references read", KindCount}
	CountVectorTaskEnqueued                     = Event{"vector_task_enqueued", "vector maintenance tasks enqueued", KindCount}
	CountVectorTaskExecuted                     = Event{"vector_task_executed", "vector maintenance tasks executed", KindCount}
	CountVectorIndexDisabledOnNegativeTaskCount = Event{"vector_index_disabled_on_negative_task_count", "vector indexes disabled on negative task count", KindCount}
)

// guardiannRegister composes TaskCountRegister and MaintenanceControlRegister,
// and counts task events on the store's timer as Java's OnWrite does.
type guardiannRegister struct {
	timer    *StoreTimer
	tx       fdb.WritableTransaction
	counts   vectorTaskCounts
	prefix   tuple.Tuple
	control  *IndexDeferredMaintenanceControl // set when the caller merges later
	index    *Index
	signaled bool
}

func (r *guardiannRegister) onTaskEnqueued() {
	r.timer.Increment(CountVectorTaskEnqueued)
	r.counts.adjust(r.tx, r.prefix, littleEndianOne)
	if r.control != nil && !r.signaled {
		r.signaled = true
		_ = r.control.SetMergeRequiredIndexes(r.index)
	}
}

func (r *guardiannRegister) onTaskExecuted() {
	r.timer.Increment(CountVectorTaskExecuted)
	r.counts.adjust(r.tx, r.prefix, littleEndianMinusOne)
}

// timer is the store context's timer, or nil.
func (m *vectorIndexMaintainer) timer() *StoreTimer {
	if s, ok := m.store.(*FDBRecordStore); ok {
		return s.context.Timer()
	}
	return nil
}

func (m *vectorIndexMaintainer) mergeControl() *IndexDeferredMaintenanceControl {
	if s, ok := m.store.(*FDBRecordStore); ok {
		return s.GetIndexDeferredMaintenanceControl()
	}
	return nil
}

func (m *vectorIndexMaintainer) guardiannFor(prefix tuple.Tuple, listener guardiannListener) *guardiann {
	g := newGuardiann(m.getSubspaceForPrefix(prefix), m.guardiannConfig, m.store.Env(), listener)
	g.poison = m.poisonTask
	g.timer = m.timer()
	return g
}

// poisonTask registers the vectorTask commit check: the first task error of the
// transaction is its commit error.
func (m *vectorIndexMaintainer) poisonTask(err error) {
	s, ok := m.store.(*FDBRecordStore)
	if !ok || s.context == nil {
		return
	}
	name := fmt.Sprintf("vectorTask/%x/%s", s.subspace.Bytes(), m.index.Name)
	s.context.getOrCreateCommitCheck(name, func(string) CommitCheckFunc { return func() error { return err } })
}

// VectorCapabilityError is a vector index operation Go refuses where Java would
// accept a write no query can use (RFC-257 WS-D section 1, "capability
// errors"). It is Go-only and terminal: no retry owner retries it.
type VectorCapabilityError struct {
	IndexName string
	Engine    string
	Operation string
	Option    string
	Value     int
	Reason    string
}

func (e *VectorCapabilityError) Error() string {
	return fmt.Sprintf("vector index %q (%s): %s refused: %s = %d %s",
		e.IndexName, e.Engine, e.Operation, e.Option, e.Value, e.Reason)
}

// insertAdmission refuses a GuardiANN insert into an index whose
// insertMaxCandidateClusters is below 1: Java writes the vector's identity and
// no reference, so no search can ever return it, and the knob is immutable
// (declared in DIVERGENCES.md, "GuardiANN refuses inserts no search can find").
func (m *vectorIndexMaintainer) insertAdmission() error {
	if m.engine != VectorEngineGuardiann || m.guardiannConfig.insertMaxCandidateClusters >= 1 {
		return nil
	}
	return &VectorCapabilityError{
		IndexName: m.index.Name, Engine: "GUARDIANN", Operation: "insert",
		Option: IndexOptionGuardiannInsertMaxCandidateClusters, Value: m.guardiannConfig.insertMaxCandidateClusters,
		Reason: "leaves no cluster to hold the vector, so no search could return it",
	}
}

// refuseCapability poisons the context with err (the keyed
// vectorCapability/<store>/<index> commit check) and returns it: the refusal
// may follow buffered record writes, which must not commit without their
// index entry.
func (m *vectorIndexMaintainer) refuseCapability(err error) error {
	if s, ok := m.store.(*FDBRecordStore); ok && s.context != nil {
		name := fmt.Sprintf("vectorCapability/%x/%s", s.subspace.Bytes(), m.index.Name)
		s.context.getOrCreateCommitCheck(name, func(string) CommitCheckFunc { return func() error { return err } })
	}
	return err
}

// applyGuardiannEntry is VectorIndexMaintainer.updateIndexEntry over the
// GuardiANN engine.
func (m *vectorIndexMaintainer) applyGuardiannEntry(prefix, pk tuple.Tuple, vector gVector, remove bool) error {
	if !remove {
		if err := m.insertAdmission(); err != nil {
			return m.refuseCapability(err)
		}
	}
	control := m.mergeControl()
	maintainInTransaction := control == nil || control.ShouldAutoMergeDuringCommit()
	reg := &guardiannRegister{timer: m.timer(), tx: m.tx, counts: m.taskCounts, prefix: prefix, index: m.index}
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
		return &VectorIndexClusterTooLargeError{Message: "vector index cluster reached its hard cap; a background merge must drain the deferred split " +
			"backlog before more vectors can be inserted", Cause: err}
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

func (m *vectorIndexMaintainer) searchGuardiann(readTx fdb.ReadTransaction, prefix tuple.Tuple, query []float64, k int, opts VectorIndexScanOptions) ([]guardiannResult, error) {
	if len(query) != m.guardiannConfig.numDimensions {
		return nil, fmt.Errorf("VECTOR index %q expects %d dimensions, but query vector has %d",
			m.index.Name, m.guardiannConfig.numDimensions, len(query))
	}
	cfg, err := opts.guardiannSearchConfig()
	if err != nil {
		return nil, err
	}
	return m.guardiannFor(prefix, nil).search(readTx, k, cfg, gVector{data: query, typ: 2})
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
	reg := &guardiannRegister{timer: m.timer(), tx: m.tx, counts: m.taskCounts, prefix: owned.prefix, index: m.index}
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
// disableIndexOnNegativeTaskCount commit check does
// (VectorIndexMaintainer.java:605-616, keyed "disableVectorIndexOnNegativeTaskCount:"
// per index, so repeated trips disable once). At commit it runs after the
// checks registered before it, the merger's heartbeat refresh among them,
// which still see the index in its state; the disable then commits, and the
// session's next refresh finds it disabled. Scoped to the store, as the
// pending-queue overflow disable is, so DeleteStore cancels it.
func (m *vectorIndexMaintainer) disableOnNegativeTaskCount() error {
	s, ok := m.store.(*FDBRecordStore)
	if !ok {
		return nil
	}
	name := pendingWriteCommitCheckPrefix(s.subspace) + "disableVectorIndexOnNegativeTaskCount:" + m.index.Name
	indexName := m.index.Name
	s.context.getOrCreateCommitCheck(name, func(string) CommitCheckFunc {
		return func() error {
			changed, err := s.MarkIndexDisabled(indexName)
			if err == nil && changed {
				s.context.Timer().Increment(CountVectorIndexDisabledOnNegativeTaskCount)
			}
			return err
		}
	})
	return nil
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
	owner, ts, err := mergeLockFromValue(v)
	if err != nil {
		return nil, err
	}
	now := l.now().UnixMilli()
	window := vectorMergeLeaseWindow.Milliseconds()
	if ts <= now-window || ts >= now+window {
		return nil, nil
	}
	u := uuid.UUID(owner)
	return &u, nil
}

// mergeLockFromValue reads VectorIndexMergeLock's (owner, millis) value.
func mergeLockFromValue(v []byte) (tuple.UUID, int64, error) {
	t, err := tuple.Unpack(v)
	if err != nil {
		return tuple.UUID{}, 0, err
	}
	owner, err := guardiannElem[tuple.UUID](t, 0, "merge lock")
	if err != nil {
		return tuple.UUID{}, 0, err
	}
	ts, err := guardiannElem[int64](t, 1, "merge lock")
	return owner, ts, err
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
	owner, _, err := mergeLockFromValue(v)
	if err != nil {
		return err
	}
	if uuid.UUID(owner) == l.owner {
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
