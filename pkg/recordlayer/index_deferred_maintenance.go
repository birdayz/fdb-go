package recordlayer

import (
	"sync"

	"github.com/google/uuid"
)

// DeferredMaintenanceStep identifies the operation whose budget needs reducing
// after a failed merge attempt, matching IndexDeferredMaintenanceControl.LastStep.
type DeferredMaintenanceStep int

const (
	DeferredMaintenanceNone DeferredMaintenanceStep = iota
	DeferredMaintenanceRepartition
	DeferredMaintenanceMerge
)

// IndexDeferredMaintenanceControl is a transaction-local exchange between index
// maintainers and the online merger. Its zero values match Java's initializer
// (auto-merge is false, despite the contradictory setter javadoc).
// Callbacks run outside the control's lock.
type IndexDeferredMaintenanceControl struct {
	mu                       sync.Mutex
	required                 map[string]*Index
	mergesTried, totalMerges int64
	autoMergeDuringCommit    bool
	explicitMergePath        bool
	mergesLimit              int64
	mergesFound              int64
	timeQuotaMillis          int64
	sizeQuotaBytes           int64
	repartitionDocumentCount int
	repartitionCapped        bool
	lastStep                 DeferredMaintenanceStep
	mergeSessionID           *uuid.UUID
	preCommitCallback        func(*FDBRecordStore) error
}

func (c *IndexDeferredMaintenanceControl) ShouldAutoMergeDuringCommit() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.autoMergeDuringCommit
}

func (c *IndexDeferredMaintenanceControl) SetAutoMergeDuringCommit(value bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.autoMergeDuringCommit = value
}

func (c *IndexDeferredMaintenanceControl) IsExplicitMergePath() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.explicitMergePath
}

func (c *IndexDeferredMaintenanceControl) SetExplicitMergePath(value bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.explicitMergePath = value
}

func (c *IndexDeferredMaintenanceControl) GetMergesLimit() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mergesLimit
}

func (c *IndexDeferredMaintenanceControl) SetMergesLimit(value int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mergesLimit = value
}

func (c *IndexDeferredMaintenanceControl) GetMergesFound() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mergesFound
}

func (c *IndexDeferredMaintenanceControl) SetMergesFound(value int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mergesFound = value
}

func (c *IndexDeferredMaintenanceControl) GetTimeQuotaMillis() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.timeQuotaMillis
}

func (c *IndexDeferredMaintenanceControl) SetTimeQuotaMillis(value int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.timeQuotaMillis = value
}

func (c *IndexDeferredMaintenanceControl) GetSizeQuotaBytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sizeQuotaBytes
}

func (c *IndexDeferredMaintenanceControl) SetSizeQuotaBytes(value int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sizeQuotaBytes = value
}

func (c *IndexDeferredMaintenanceControl) GetRepartitionDocumentCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.repartitionDocumentCount
}

func (c *IndexDeferredMaintenanceControl) SetRepartitionDocumentCount(value int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.repartitionDocumentCount = value
}

func (c *IndexDeferredMaintenanceControl) RepartitionCapped() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.repartitionCapped
}

func (c *IndexDeferredMaintenanceControl) SetRepartitionCapped(value bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.repartitionCapped = value
}

func (c *IndexDeferredMaintenanceControl) GetLastStep() DeferredMaintenanceStep {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastStep
}

func (c *IndexDeferredMaintenanceControl) SetLastStep(value DeferredMaintenanceStep) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastStep = value
}

func (c *IndexDeferredMaintenanceControl) GetMergeSessionID() *uuid.UUID {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mergeSessionID == nil {
		return nil
	}
	id := *c.mergeSessionID
	return &id
}

func (c *IndexDeferredMaintenanceControl) SetMergeSessionID(id *uuid.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mergeSessionID = nil
	if id != nil {
		copyID := *id
		c.mergeSessionID = &copyID
	}
}

func (c *IndexDeferredMaintenanceControl) GetPreCommitCallback() func(*FDBRecordStore) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.preCommitCallback
}

func (c *IndexDeferredMaintenanceControl) SetPreCommitCallback(value func(*FDBRecordStore) error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.preCommitCallback = value
}

func (c *IndexDeferredMaintenanceControl) GetMergeRequiredIndexes() []*Index {
	c.mu.Lock()
	defer c.mu.Unlock()
	var indexes []*Index
	for _, index := range c.required {
		indexes = append(indexes, index)
	}
	return indexes
}

func (c *IndexDeferredMaintenanceControl) SetMergeRequiredIndexes(index *Index) error {
	if index == nil {
		return &RecordCoreError{Message: "merge request requires an index"}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.required == nil {
		c.required = make(map[string]*Index)
	}
	c.required[index.Name] = index
	return nil
}

func (c *IndexDeferredMaintenanceControl) GetMergesTried() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mergesTried
}

func (c *IndexDeferredMaintenanceControl) SetMergesTried(count int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mergesTried = count
	c.totalMerges += count
}

func (c *IndexDeferredMaintenanceControl) GetTotalMerges() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.totalMerges
}

func (c *IndexDeferredMaintenanceControl) MergeHadFailed() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mergesTried > 0 && c.totalMerges >= c.mergesTried {
		c.totalMerges -= c.mergesTried
	}
}

// GetIndexDeferredMaintenanceControl returns the control owned by this handle,
// matching Java's per-store transaction lifetime.
func (store *FDBRecordStore) GetIndexDeferredMaintenanceControl() *IndexDeferredMaintenanceControl {
	return &store.deferredMaintenance
}

// RegisterPreCommit registers the callback on the actual write transaction.
// A backend that creates child transactions must call this for each child too;
// installing it on an outer coordinator alone does not protect child commits.
func (c *IndexDeferredMaintenanceControl) RegisterPreCommit(store *FDBRecordStore, index *Index) {
	callback := c.GetPreCommitCallback()
	if callback != nil {
		store.context.getOrCreateCommitCheck(pendingWriteCommitCheckPrefix(store.subspace)+"merge:"+index.Name, func(string) CommitCheckFunc { return func() error { return callback(store) } })
	}
}

// MergeIndex has no deferred work for synchronous standard maintainers. HNSW
// inherits this behavior; SPFresh retains its separate rebalance lifecycle.
func (m *standardIndexMaintainer) MergeIndex() error       { return nil }
func (m *atomicMutationIndexMaintainer) MergeIndex() error { return nil }
func (m *bitmapValueIndexMaintainer) MergeIndex() error    { return nil }
func (m *maxEverVersionIndexMaintainer) MergeIndex() error { return nil }
func (m *textIndexMaintainer) MergeIndex() error           { return nil }
func (m *versionIndexMaintainer) MergeIndex() error        { return nil }
func (m *slidingWindowIndexMaintainer) MergeIndex() error  { return m.delegate.MergeIndex() }
