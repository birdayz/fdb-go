package recordlayer

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync/atomic"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
)

// The rebalancer (RFC-094 §6): the autonomous consumer of the trigger tasks
// the write path files. In-process on writers by default — applications (and
// tests) call RebalanceSPFreshIndex on their own cadence; a dedicated runner
// is just another caller. Claims serialize transactionally through the task
// rows' lease machinery, so any number of concurrent rebalancers is safe:
// a live foreign lease skips the task, an expired one is reclaimed (crash
// recovery), and every lifecycle step is idempotent under commit_unknown
// retries (see the per-lifecycle files).
//
// The §5 "inline split at the 4×Lmax hard ceiling" maps here too: the write
// path files the trigger unconditionally past 2×Lmax, and a writer that
// wants the RFC's synchronous behavior calls RebalanceSPFreshIndex after its
// save commits (the record context has no after-commit hook infrastructure
// yet; when it grows one, wiring it is a one-liner around this entry point).

// spfreshOwnerSeq disambiguates rebalancer invocations within a process.
var spfreshOwnerSeq atomic.Int64

// spfreshProcessNonce makes lease owners unique ACROSS processes. Every
// process counts spfreshOwnerSeq from zero, so without a process-unique
// component two live workers on different machines both mint
// "rebalance-<index>-1" and the same-owner reclaim in spfreshTaskClaim
// voids mutual exclusion. Lease expiry does NOT cover this: it protects
// against DEAD owners, not live name collisions (codex P1).
var spfreshProcessNonce = newSPFreshProcessNonce()

func newSPFreshProcessNonce() string {
	var b [8]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		// crypto/rand read cannot fail on supported platforms; if it ever
		// does, pid+walltime still beats a constant.
		return fmt.Sprintf("%d.%d", os.Getpid(), time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// spfreshRebalanceOwner mints a lease owner UNIQUE to one rebalancer
// invocation, within and across processes. Never share an owner string
// across invocations: the claim keeps same-owner reclaim (in-executor
// retries), so shared names give zero mutual exclusion between concurrent
// executors.
func spfreshRebalanceOwner(indexName string) string {
	return fmt.Sprintf("rebalance-%s-%s-%d", indexName, spfreshProcessNonce, spfreshOwnerSeq.Add(1))
}

// spfreshTaskRef is one scanned task: kind, id, and (for fine lifecycles)
// the cell resolved at scan time — staleness is fine, every lifecycle
// re-verifies authoritatively and treats absent-at-cell as a zombie.
type spfreshTaskRef struct {
	kind   int64
	id     int64
	cellID int64
}

// spfreshTaskOutcome classifies one lifecycle-handler invocation for budget
// and metrics attribution (Torvalds 094.4 r4: lumping cleanup writes into
// the per-kind action counters made the operator metrics lie):
//
//	Skipped  — wrote nothing: task gone or foreign live lease. Budget-free.
//	Cleaned  — committed a cleanup write (zombie/cooldown/no-target clear).
//	Deferred — csplit's pause-window defer-count bump (a write, not a split).
//	Acted    — the lifecycle action itself.
//
// Cleaned/Deferred/Acted all consume action budget: they are committed
// writes that make progress.
type spfreshTaskOutcome uint8

const (
	spfreshOutcomeSkipped spfreshTaskOutcome = iota
	spfreshOutcomeCleaned
	spfreshOutcomeDeferred
	spfreshOutcomeActed
)

// spfreshRebalanceOnce scans the task queue once and runs actionable tasks,
// in lifecycle order (splits before NPA before merges — splits enqueue NPAs;
// merges of split children respect the cooldown anyway), up to `limit` tasks
// (≤ 0 = unlimited; the sweeper bounds it so a whale tenant's queue cannot
// monopolize a fleet pass — codex MT P2). Returns the number of tasks acted
// on; tasks under live foreign leases are skipped.
func spfreshRebalanceOnce(ctx context.Context, db *FDBDatabase, s *spfreshStorage, config SPFreshConfig, owner string, seed int64, limit int, timer *StoreTimer) (int, error) {
	// Scan (snapshot — the queue is advisory; claims are the authority).
	var refs []spfreshTaskRef
	var legacyCellfin []int64
	err := spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
		refs = refs[:0]
		legacyCellfin = legacyCellfin[:0]
		tx := rtx.Transaction()
		r, rerr := fdb.PrefixRange(s.tasks.Bytes())
		if rerr != nil {
			return rerr
		}
		kvs, gerr := tx.Snapshot().GetRange(r, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
		if gerr != nil {
			return gerr
		}
		for _, kv := range kvs {
			t, uerr := s.tasks.Unpack(kv.Key)
			if uerr != nil || len(t) != 2 {
				return fmt.Errorf("spfresh rebalance: malformed task key")
			}
			kind, kok := t[0].(int64)
			id, iok := t[1].(int64)
			if !kok || !iok {
				return fmt.Errorf("spfresh rebalance: malformed task key elements")
			}
			if kind == spfreshTaskCellfin {
				// Build machinery, not a rebalancer concern. Rows in the
				// READABLE generation are LEAKED bookkeeping from builds
				// that flipped before the flip learned to clear them — an
				// in-flight build's rows live under its own unpublished
				// generation, never here. Self-heal: clear on sight, or the
				// pending-work probe reports this tenant busy forever
				// (codex MT P2).
				legacyCellfin = append(legacyCellfin, id)
				continue
			}
			ref := spfreshTaskRef{kind: kind, id: id}
			if kind == spfreshTaskSplit || kind == spfreshTaskMerge {
				cellID, ferr := spfreshFindCentroidCell(tx, s, id)
				if ferr != nil {
					if errors.Is(ferr, errSPFreshNotFound) {
						// Centroid gone entirely: the lifecycle's zombie rule
						// will delete the task; give it cell 0 to land there.
						ref.cellID = 0
					} else {
						return ferr
					}
				} else {
					ref.cellID = cellID
				}
			}
			refs = append(refs, ref)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if len(legacyCellfin) > 0 {
		if err := spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
			for _, id := range legacyCellfin {
				rtx.Transaction().Clear(s.taskKey(spfreshTaskCellfin, id))
			}
			return nil
		}); err != nil {
			return 0, err
		}
	}

	// Lifecycle execution order (NOT the kind constants): splits first, then
	// the NPAs they enqueue, then merges, then coarse splits. Within a kind,
	// id order for determinism.
	order := map[int64]int{spfreshTaskSplit: 0, spfreshTaskNPA: 1, spfreshTaskMerge: 2, spfreshTaskCSplit: 3}
	sort.Slice(refs, func(i, j int) bool {
		if order[refs[i].kind] != order[refs[j].kind] {
			return order[refs[i].kind] < order[refs[j].kind]
		}
		return refs[i].id < refs[j].id
	})

	// One routing cache per round for the NPA arm (lazily loaded): the
	// per-task full reload was O(fines) × one NPA per split — the dominant
	// maintenance CPU at fine granularity. Round staleness is the same
	// tolerated staleness as the plan phase's snapshot reads.
	var npaRouting *spfreshRoutingCache
	// worked counts COMMITTED WRITES — lifecycle actions and zombie-cleanup
	// clears (both make progress and cost real transactions). Foreign-lease
	// and task-gone skips write nothing and consume no budget: a tenant whose
	// queue head is live-leased by another executor must not have its whole
	// action budget burned on skips while actionable tasks behind it starve
	// (codex 094.4).
	worked := 0
	// A failed task is SKIPPED, counted, and surfaced at the end — never
	// allowed to halt the pass: the scan order is deterministic (kind+id),
	// so returning on the first handler error would park a poisoned task at
	// the queue head and silently starve everything behind it on every pass,
	// with an operator signal identical to "sweeper down" (Torvalds
	// final-gauntlet S5). The errors still fail the pass loudly; the work
	// behind the poison still happens.
	var taskErrs []error
	fail := func(err error) {
		timer.Increment(CountSPFreshTaskErrors)
		taskErrs = append(taskErrs, err)
	}
	for _, ref := range refs {
		if limit > 0 && worked >= limit {
			break // per-pass budget spent: the caller schedules the rest
		}
		switch ref.kind {
		case spfreshTaskSplit:
			out, serr := spfreshSealFine(ctx, db, s, owner, ref.cellID, ref.id)
			if serr != nil {
				fail(fmt.Errorf("seal fine %d (cell %d): %w", ref.id, ref.cellID, serr))
				continue
			}
			if !out.proceed {
				if out.cleaned {
					timer.Increment(CountSPFreshZombieCleans)
					worked++
				} else {
					timer.Increment(CountSPFreshLeaseSkips)
				}
				continue // zombie cleaned or foreign lease
			}
			if spfreshSealSplitGapHook != nil {
				spfreshSealSplitGapHook(ref.cellID, ref.id)
			}
			if serr := spfreshSplitFine(ctx, db, s, config, owner, ref.cellID, ref.id, seed+ref.id); serr != nil {
				if errors.Is(serr, errSPFreshLeaseHeld) {
					// Short leases let another executor take the SPLIT task over in
					// the window between SEAL (which claimed + proceeded) and SPLIT's
					// own re-claim. The new owner completes the split; the lease loss
					// is benign here because spfreshSplitFine claims BEFORE any
					// mutation, so nothing is half-written or orphaned. Skip, don't
					// fail — mirrors the SEAL phase above and the merge/npa/csplit
					// lease-skip handling.
					//
					// Unlike those sibling sites we absorb ONLY errSPFreshLeaseHeld,
					// not errSPFreshNotFound: spfreshSplitFine re-claims only after
					// proving the centroid SEALED, and a centroid already split away
					// is caught earlier by its FORWARD no-op — so a missing task row
					// at this point is a genuine SEALED-without-task anomaly that must
					// fail loudly, not be skipped.
					timer.Increment(CountSPFreshLeaseSkips)
					continue
				}
				fail(fmt.Errorf("split fine %d (cell %d): %w", ref.id, ref.cellID, serr))
				continue
			}
			timer.Increment(CountSPFreshSplits)
			worked++
		case spfreshTaskNPA:
			if npaRouting == nil {
				npaRouting = newSPFreshRoutingCache(0)
				if lerr := spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
					return npaRouting.fullReload(rtx.Transaction(), s, s.generation)
				}); lerr != nil {
					// Not a task failure: the shared cache is broken — every
					// NPA would fail identically. Abort the pass.
					return worked, errors.Join(append(taskErrs, fmt.Errorf("NPA routing load: %w", lerr))...)
				}
			}
			out, nerr := spfreshNPARun(ctx, db, s, config, owner, ref.id, npaRouting)
			if nerr != nil {
				fail(fmt.Errorf("NPA %d: %w", ref.id, nerr))
				continue
			}
			worked += spfreshMeterOutcome(timer, out, CountSPFreshNPAs)
		case spfreshTaskMerge:
			out, merr := spfreshMergeFine(ctx, db, s, config, owner, ref.cellID, ref.id)
			if merr != nil {
				fail(fmt.Errorf("merge fine %d (cell %d): %w", ref.id, ref.cellID, merr))
				continue
			}
			worked += spfreshMeterOutcome(timer, out, CountSPFreshMerges)
		case spfreshTaskCSplit:
			out, cerr := spfreshCoarseSplit(ctx, db, s, config, owner, ref.id, seed+ref.id)
			if cerr != nil {
				fail(fmt.Errorf("coarse split cell %d: %w", ref.id, cerr))
				continue
			}
			worked += spfreshMeterOutcome(timer, out, CountSPFreshCSplits)
		}
	}
	return worked, errors.Join(taskErrs...)
}

// spfreshMeterOutcome attributes one handler outcome to the right counter
// (Torvalds 094.4 r4: per-kind action counters must count ACTIONS — cleanup
// clears go to ZombieCleans, csplit defer-bumps to CSplitDefers, skips to
// LeaseSkips) and returns the budget charge: every committed write consumes
// budget, skips are free.
func spfreshMeterOutcome(timer *StoreTimer, out spfreshTaskOutcome, acted Event) int {
	switch out {
	case spfreshOutcomeActed:
		timer.Increment(acted)
		return 1
	case spfreshOutcomeCleaned:
		timer.Increment(CountSPFreshZombieCleans)
		return 1
	case spfreshOutcomeDeferred:
		timer.Increment(CountSPFreshCSplitDefers)
		return 1
	default:
		timer.Increment(CountSPFreshLeaseSkips)
		return 0
	}
}

// RebalanceSPFreshIndex drains the index's task queue to quiescence: scan,
// act, repeat until a round acts on nothing (splits enqueue NPA follow-ups,
// so multiple rounds are normal). Bounded against pathological re-triggering.
// Returns the total number of lifecycle actions taken.
func RebalanceSPFreshIndex(ctx context.Context, db *FDBDatabase, storeBuilder func(*FDBRecordContext) (*FDBRecordStore, error), indexName string) (int, error) {
	const maxRounds = 32
	total, drained, err := rebalanceSPFreshIndexRounds(ctx, db, storeBuilder, indexName, maxRounds, 0, nil)
	if err != nil {
		return total, err
	}
	if !drained {
		return total, fmt.Errorf("spfresh rebalance: task queue did not quiesce in %d rounds (%d actions) — re-trigger loop?", maxRounds, total)
	}
	return total, nil
}

// rebalanceSPFreshIndexRounds is the budgeted core: up to maxRounds
// scan-and-act passes and (when maxActions > 0) at most maxActions lifecycle
// actions TOTAL, reporting whether the queue drained. The multi-tenant
// sweeper uses small budgets for fairness — the action budget caps work even
// when a single scan finds a wide queue (a whale tenant with thousands of
// independent task rows must not monopolize a fleet pass through one round).
// An undrained queue is NOT an error there — the next sweep pass continues.
func rebalanceSPFreshIndexRounds(ctx context.Context, db *FDBDatabase, storeBuilder func(*FDBRecordContext) (*FDBRecordStore, error), indexName string, maxRounds, maxActions int, timer *StoreTimer) (int, bool, error) {
	var indexSubspace subspace.Subspace
	var config SPFreshConfig
	if err := spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
		store, serr := storeBuilder(rtx)
		if serr != nil {
			return serr
		}
		index := store.GetMetaData().GetIndex(indexName)
		if index == nil {
			return fmt.Errorf("spfresh rebalance: index %q not found", indexName)
		}
		if index.Type != IndexTypeVectorSPFresh {
			return fmt.Errorf("spfresh rebalance: index %q has type %q", indexName, index.Type)
		}
		config = parseSPFreshConfig(index)
		indexSubspace = store.indexSubspace(index)
		return nil
	}); err != nil {
		return 0, false, err
	}

	// The readable generation is the one being maintained. No generation =
	// the index was never bootstrapped or built (§6b insert-first): nothing
	// to rebalance — idle, don't error (a production rebalancer loop starts
	// alongside the writers, often before the first insert).
	var gen int64
	bootstrapped := true
	if err := spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
		g, gerr := spfreshReadGenerationSnapshot(rtx.Transaction(), newSPFreshStorage(indexSubspace, 0))
		if gerr != nil {
			if errors.Is(gerr, errSPFreshNotFound) {
				bootstrapped = false
				return nil
			}
			return fmt.Errorf("spfresh rebalance: read generation: %w", gerr)
		}
		gen = g
		return nil
	}); err != nil {
		return 0, false, err
	}
	if !bootstrapped {
		return 0, true, nil // nothing to drain
	}
	s := newSPFreshStorage(indexSubspace, gen)

	// UNIQUE owner per invocation: the lease check is `row.owner != owner`,
	// so two executors sharing an owner string reclaim each other's live
	// leases freely — zero mutual exclusion. Two concurrent
	// RebalanceSPFreshIndex calls for the same index (e.g. a rebalancer loop
	// overlapping a final drain) then interleave MULTI-TRANSACTION lifecycles
	// on the same tasks: one executor's coarse split races another's chunked
	// split mid-drain, writing children into a cell the first just cleared —
	// the 300k fill orphaned ~3/4 of its entries exactly this way.
	owner := spfreshRebalanceOwner(indexName)
	total := 0
	drained := false
	for round := 0; round < maxRounds; round++ {
		limit := 0
		if maxActions > 0 {
			limit = maxActions - total
			if limit <= 0 {
				break // action budget spent: not drained
			}
		}
		worked, err := spfreshRebalanceOnce(ctx, db, s, config, owner, int64(round)*7919, limit, timer)
		// A pass can return COMMITTED WORK alongside a joined task error
		// (poisoned tasks are skipped, the rest of the queue still drains —
		// codex delta P2): account the work and run the eager cache refresh
		// below before surfacing the error, or splits committed behind the
		// poison are reported as zero and the process-local cache routes on
		// the pre-pass topology until an amortized refresh.
		total += worked
		if err != nil {
			refreshErr := spfreshRefreshAfterRebalance(ctx, db, s, total)
			return total, false, errors.Join(err, refreshErr)
		}
		if worked == 0 {
			drained = true
			break
		}
	}
	// GC: retired topology past the cooldown horizon (the same window that
	// guards split↔merge oscillation guards stale readers' grace period).
	if _, err := spfreshGCSweep(ctx, db, s, config, int64(config.CooldownSec)*1000); err != nil {
		return total, drained, errors.Join(err, spfreshRefreshAfterRebalance(ctx, db, s, total))
	}
	return total, drained, spfreshRefreshAfterRebalance(ctx, db, s, total)
}

// spfreshRefreshAfterRebalance eagerly reloads the process-local routing
// cache after a rebalance that committed work — the §4 "maintainer timer"
// action; other processes converge via the amortized query-path refresh. It
// runs on the error exits too: committed lifecycle work changed the topology
// regardless of how the pass ended.
func spfreshRefreshAfterRebalance(ctx context.Context, db *FDBDatabase, s *spfreshStorage, total int) error {
	if total == 0 {
		return nil
	}
	return spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
		return spfreshCacheFor(s.index, s.generation).fullReload(rtx.Transaction(), s, s.generation)
	})
}
