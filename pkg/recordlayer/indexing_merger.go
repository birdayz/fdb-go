package recordlayer

import (
	"context"
	"errors"

	"fdb.dev/pkg/fdbgo/fdb"
)

// indexingMerger retains adaptive budgets across batches for one target, as in
// Java IndexingMerger. Each invocation gets its own outer failure allowance.
type indexingMerger struct {
	index                                              *Index
	mergesLimit                                        int64
	successes                                          int
	timeQuotaMillis                                    int64
	repartitionDocumentCount, repartitionSecondChances int
}

func (m *indexingMerger) merge(ctx context.Context, oi *OnlineIndexer) error {
	failures := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var control *IndexDeferredMaintenanceControl
		_, err := oi.db.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			control = nil
			store, err := oi.openStore(rc)
			if err != nil {
				return nil, err
			}
			control = store.GetIndexDeferredMaintenanceControl()
			control.SetMergesLimit(m.mergesLimit)
			control.SetTimeQuotaMillis(m.timeQuotaMillis)
			control.SetRepartitionDocumentCount(m.repartitionDocumentCount)
			if oi.sessionHeartbeat != nil {
				control.SetMergeSessionID(&oi.sessionHeartbeat.indexerID)
				control.SetPreCommitCallback(func(store *FDBRecordStore) error {
					return oi.refreshFollowupHeartbeats(store, oi.sessionHeartbeat)
				})
			}
			control.RegisterPreCommit(store, m.index)
			maintainer, err := store.getIndexMaintainer(m.index)
			if err != nil {
				return nil, err
			}
			return nil, maintainer.MergeIndex()
		})
		if cause := ctx.Err(); cause != nil {
			return cause
		}
		if err == nil {
			if !m.handleSuccess(control) {
				return nil
			}
			continue
		}
		failures++
		if control == nil || failures > 1000 {
			return err
		}
		if err := m.handleFailure(control, err); err != nil {
			return err
		}
	}
}

func (m *indexingMerger) handleSuccess(control *IndexDeferredMaintenanceControl) bool {
	if m.mergesLimit > 0 && m.successes > 2 {
		m.successes = 0
		m.mergesLimit = m.mergesLimit * 5 / 4
	}
	m.successes++
	m.timeQuotaMillis = 0
	if m.repartitionDocumentCount > 0 {
		m.repartitionDocumentCount = 0
	}
	if m.repartitionDocumentCount == -1 && m.repartitionSecondChances == 0 {
		m.repartitionSecondChances++
		m.repartitionDocumentCount = 0
		return true
	}
	if control.RepartitionCapped() {
		control.SetRepartitionCapped(false)
		m.repartitionDocumentCount = 0
		return true
	}
	m.repartitionSecondChances = 0
	return control.GetMergesFound() > control.GetMergesTried()
}

func (m *indexingMerger) handleFailure(control *IndexDeferredMaintenanceControl, err error) error {
	control.MergeHadFailed()
	var fdbErr fdb.Error
	if errors.As(err, &fdbErr) {
		switch fdbErr.Code {
		case 1051, 1213, 1515: // batch_transaction_throttled, tag_throttled, no_cluster_file_found
			return err
		}
	} else {
		var timeout interface{ Timeout() bool }
		if !errors.Is(err, context.DeadlineExceeded) && !(errors.As(err, &timeout) && timeout.Timeout()) {
			return err
		}
	}
	switch control.GetLastStep() {
	case DeferredMaintenanceRepartition:
		m.repartitionDocumentCount = control.GetRepartitionDocumentCount()
		if m.repartitionDocumentCount == -1 {
			return err
		}
		m.repartitionDocumentCount /= 2
		if m.repartitionDocumentCount == 0 {
			m.repartitionDocumentCount = -1
		}
	case DeferredMaintenanceMerge:
		if control.GetMergesTried() >= 2 {
			m.mergesLimit = control.GetMergesTried() / 2
		} else {
			m.timeQuotaMillis = control.GetTimeQuotaMillis()
			if m.timeQuotaMillis <= 2 {
				return err
			}
			m.timeQuotaMillis /= 2
		}
	default:
		return err
	}
	return nil
}

// mergeRequestedIndexes runs after the committed batch's queue drains, even
// when that batch exhausted its input. Failed transactions publish no requests.
func (oi *OnlineIndexer) mergeRequestedIndexes(ctx context.Context) error {
	for _, index := range oi.mergeRequiredIndexes {
		if err := oi.mergeIndex(ctx, index); err != nil {
			return err
		}
	}
	oi.mergeRequiredIndexes = nil
	return nil
}

func (oi *OnlineIndexer) mergeIndex(ctx context.Context, index *Index) error {
	if oi.admittedHeartbeat != nil && oi.retiredBuildTargets[index.Name] {
		return nil
	}
	if oi.mergers == nil {
		oi.mergers = make(map[string]*indexingMerger)
	}
	merger := oi.mergers[index.Name]
	if merger == nil {
		merger = &indexingMerger{index: index}
		if oi.policy != nil {
			merger.mergesLimit = oi.policy.InitialMergesCountLimit
		}
		oi.mergers[index.Name] = merger
	}
	return merger.merge(ctx, oi)
}

// MergeIndexes explicitly runs deferred maintenance for all configured targets.
// Synchronous maintainers report no work; this does not substitute for a backend
// adapter capable of performing real deferred work.
func (oi *OnlineIndexer) MergeIndexes(ctx context.Context) error {
	if oi.sessionHeartbeat == nil {
		oi.retiredBuildTargets = nil
		oi.sessionHeartbeat = oi.newHeartbeat("explicit index merge", false, oi.db.Env())
		defer func() { oi.cleanupPendingQueueHeartbeat(oi.sessionHeartbeat); oi.sessionHeartbeat = nil }()
	}
	for _, index := range oi.targetIndexes {
		if err := oi.mergeIndex(ctx, index); err != nil {
			return err
		}
	}
	return nil
}

// addMergeRequests combines requests from successful build and drain commits.
// A failed attempt's control is never published into this session state.
func (oi *OnlineIndexer) addMergeRequests(indexes []*Index) {
	for _, index := range indexes {
		found := false
		for _, existing := range oi.mergeRequiredIndexes {
			if existing.Name == index.Name {
				found = true
				break
			}
		}
		if !found {
			oi.mergeRequiredIndexes = append(oi.mergeRequiredIndexes, index)
		}
	}
}
