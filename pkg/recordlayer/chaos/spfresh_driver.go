package chaos

import (
	"context"

	"fdb.dev/pkg/recordlayer"
)

// SPFresh maintenance drivers for the chaos scenario. The write path
// (SaveRecord/DeleteRecord) already runs through the fault-injecting
// transactor; these drive the *maintenance lifecycle* (the rebalancer's
// seal/split/merge/coarse-split/NPA/GC steps and RFC-104 refinement) the same
// way, so an injected commit_unknown / conflict lands mid-lifecycle — the
// surface RFC-104's isolation tests never exercised.

// RebalanceSPFresh drains the index's maintenance queue THROUGH the
// fault-injecting transactor. Returns the lifecycle actions taken and any error
// (an undrained queue or a poisoned task surfaces here). Maintenance changes
// layout/recall, not record membership, so it does not touch the model.
func (s *Scenario) RebalanceSPFresh(indexName string) (int, error) {
	return recordlayer.RebalanceSPFreshIndex(context.Background(), s.chaosDB, s.openStore, indexName)
}

// RefineSPFresh runs one budgeted RFC-104 refinement pass through the
// fault-injecting transactor. Returns (moved, cycleConverged, error).
func (s *Scenario) RefineSPFresh(indexName string, budget int) (int, bool, error) {
	return recordlayer.RefineSPFreshIndex(context.Background(), s.chaosDB, s.openStore, indexName, budget)
}

// SweepSPFresh runs one bounded multi-tenant sweep pass through the
// fault-injecting transactor, accumulating per-kind lifecycle counts into the
// timer. Generous per-pass budgets so a sweep drains most pending work in one
// call; tests read timer.GetCount(CountSPFreshSplits/Merges/...) to PROVE the
// lifecycle fired under faults (not a fake checkbox).
func (s *Scenario) SweepSPFresh(indexName string, timer *recordlayer.StoreTimer) (recordlayer.SPFreshSweepResult, error) {
	return recordlayer.SweepSPFreshIndexes(context.Background(), s.chaosDB,
		[]recordlayer.SPFreshTenant{{StoreBuilder: s.openStore, IndexName: indexName}},
		recordlayer.SPFreshSweepOptions{Timer: timer, MaxRoundsPerTenant: 16, MaxActionsPerTenant: 256})
}

// DrainSPFresh drains the maintenance queue to quiescence through the CLEAN
// transactor (no faults), failing the test on error. Call before Verify: the
// structural-integrity invariant is strict (every membership target ACTIVE),
// which holds only once the lifecycle has settled. Draining clean also proves
// the post-fault state is *recoverable* — whatever a fault left mid-flight, a
// clean pass completes it.
func (s *Scenario) DrainSPFresh(indexName string) {
	s.t.Helper()
	if _, err := recordlayer.RebalanceSPFreshIndex(context.Background(), s.cleanDB, s.openStore, indexName); err != nil {
		s.t.Fatalf("chaos: DrainSPFresh %q (seed=%d): %v", indexName, s.seed, err)
	}
}

// ChaosDB exposes the fault-injecting database for tests that drive concurrent
// raw operations (where the single-threaded model would race). Pair with
// VerifySnapshot, which rebuilds the model from store state.
func (s *Scenario) ChaosDB() *recordlayer.FDBDatabase { return s.chaosDB }

// CleanDB exposes the no-fault database for draining and verification.
func (s *Scenario) CleanDB() *recordlayer.FDBDatabase { return s.cleanDB }

// OpenStore opens the scenario's store within a transaction — a storeBuilder
// usable with the exported SPFresh maintenance/search entry points.
func (s *Scenario) OpenStore(rtx *recordlayer.FDBRecordContext) (*recordlayer.FDBRecordStore, error) {
	return s.openStore(rtx)
}
