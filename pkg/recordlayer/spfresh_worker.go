package recordlayer

import (
	"context"
	"time"
)

// Reference maintenance worker (RFC-156 §3.2). SPFresh maintenance is
// caller-driven — SweepSPFreshIndexes / RefineSPFreshIndexes are library entry
// points, and nothing ships that drives them on a cadence. This is that driver:
// a small, embeddable loop that sweeps the rebalance lifecycle on one cadence
// and runs RFC-104 refinement on a slower one, over a caller-supplied tenant
// list, with the existing StoreTimer metrics wired through. It closes the "how
// do I actually run this" gap (094-status Tier-2 #4) without inventing a new
// runtime: a deployment embeds RunSPFreshMaintenance, or runs the thin
// cmd/spfresh-maintainer wrapper around it.

const (
	// spfreshDefaultSweepInterval drives the rebalance lifecycle (split / merge
	// / coarse-split / NPA / GC). Seconds-scale: maintenance should track write
	// volume closely so postings stay within the Lmax envelope.
	spfreshDefaultSweepInterval = 10 * time.Second
	// spfreshDefaultRefineInterval drives RFC-104 assignment refinement.
	// Minutes-scale and deliberately slower than the sweep: refinement is
	// recall-recovery, not correctness, and a converged tenant re-scans its
	// cursor for zero moves each pass (RefineSPFreshIndexes' Converged count
	// lets a caller back off quiescent tenants).
	spfreshDefaultRefineInterval = 5 * time.Minute
)

// SPFreshMaintenanceOptions configures the reference maintenance loop.
type SPFreshMaintenanceOptions struct {
	// Tenants is the set of (store, index) pairs to maintain. The caller owns
	// discovery — store layout is application keyspace, not enumerable from
	// inside the record layer. Sharding the list across workers is safe:
	// SweepSPFreshIndexes is concurrent-safe by construction (unique lease
	// owners, task-level exclusion).
	Tenants []SPFreshTenant

	// SweepInterval is the rebalance cadence. <= 0 uses the default (10s).
	SweepInterval time.Duration
	// RefineInterval is the refinement cadence. <= 0 uses the default (5m).
	// Ignored when DisableRefine is set.
	RefineInterval time.Duration
	// DisableRefine turns off the RFC-104 refinement loop entirely (sweep
	// only) — e.g. for a read-mostly fleet where ingest drift never builds up.
	DisableRefine bool

	// Sweep / Refine carry the per-pass budgets and the StoreTimer that
	// accumulates instrumentation (per-kind action counts, lease skips,
	// refine moves/converged — see spfresh_metrics.go). Their zero values are
	// the documented defaults; set Timer to scrape metrics.
	Sweep  SPFreshSweepOptions
	Refine SPFreshRefineOptions

	// OnSweep / OnRefine, when set, are called after each pass with its result
	// and joined per-tenant error — the hook for logging / exporting metrics /
	// alerting on backlog. A pass error is never fatal to the loop (per-tenant
	// failures are already isolated inside Sweep/Refine); the loop runs until
	// ctx is canceled.
	OnSweep  func(SPFreshSweepResult, error)
	OnRefine func(SPFreshRefineResult, error)
}

// RunSPFreshMaintenance runs the maintenance loop until ctx is canceled, then
// returns nil. It does an immediate sweep on entry (so a freshly-started worker
// makes progress without waiting a full interval), then sweeps on SweepInterval
// and refines on RefineInterval. Per-pass results/errors flow to OnSweep /
// OnRefine; nothing here fails the loop. With no tenants it is a quiet no-op
// loop (each pass returns an empty result), so it is safe to start before the
// tenant list is populated.
func RunSPFreshMaintenance(ctx context.Context, db *FDBDatabase, opts SPFreshMaintenanceOptions) error {
	sweepEvery := opts.SweepInterval
	if sweepEvery <= 0 {
		sweepEvery = spfreshDefaultSweepInterval
	}
	refineEvery := opts.RefineInterval
	if refineEvery <= 0 {
		refineEvery = spfreshDefaultRefineInterval
	}

	runSweep := func() {
		res, err := SweepSPFreshIndexes(ctx, db, opts.Tenants, opts.Sweep)
		if opts.OnSweep != nil {
			opts.OnSweep(res, err)
		}
	}
	runRefine := func() {
		res, err := RefineSPFreshIndexes(ctx, db, opts.Tenants, opts.Refine)
		if opts.OnRefine != nil {
			opts.OnRefine(res, err)
		}
	}

	sweepTicker := time.NewTicker(sweepEvery)
	defer sweepTicker.Stop()

	var refineC <-chan time.Time
	if !opts.DisableRefine {
		refineTicker := time.NewTicker(refineEvery)
		defer refineTicker.Stop()
		refineC = refineTicker.C
	}

	// Immediate first sweep — but honor an already-canceled ctx.
	if ctx.Err() != nil {
		return nil
	}
	runSweep()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-sweepTicker.C:
			runSweep()
		case <-refineC:
			runRefine()
		}
	}
}
