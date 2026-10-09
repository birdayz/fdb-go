package vectorindex

import "fdb.dev/pkg/recordlayer"

// SPFresh instrumentation events, recorded into the context's StoreTimer —
// the same FDBStoreTimer idiom every other index uses (the TEXT index's
// InstrumentedBunchedMap is the precedent). StoreTimer methods are
// nil-receiver-safe, so an uninstrumented context costs one nil check per
// site and SPFresh internals thread the timer unconditionally.
//
// Timed events record nanoseconds; counts record occurrences or sized
// quantities. These are the operator-facing signals the index needs in
// production: query cost decomposition (probed/pruned/scanned/reranked),
// write-path health (fence reads, replicas, stale-route retries), and
// maintenance progress (per-kind lifecycle actions, zombie cleanups, lease
// skips). Scrape via StoreTimer.Snapshot().
var (
	// Query path.
	EventSPFreshSearch = recordlayer.Event{Name: "spfresh_search", Title: "SPFresh Search", Kind: recordlayer.KindTimed}
	// Probed/pruned: the Eq.(3) pruning decomposition per search — probed
	// lists cost range reads, pruned ones were skipped.
	CountSPFreshPostingsProbed  = recordlayer.Event{Name: "spfresh_postings_probed", Title: "SPFresh Postings Probed", Kind: recordlayer.KindCount}
	CountSPFreshPostingsPruned  = recordlayer.Event{Name: "spfresh_postings_pruned", Title: "SPFresh Postings Pruned", Kind: recordlayer.KindCount}
	CountSPFreshEntriesScanned  = recordlayer.Event{Name: "spfresh_entries_scanned", Title: "SPFresh Entries Scanned", Kind: recordlayer.KindCount}
	CountSPFreshRerankReads     = recordlayer.Event{Name: "spfresh_rerank_reads", Title: "SPFresh Rerank Reads", Kind: recordlayer.KindCount}
	CountSPFreshStarvationWiden = recordlayer.Event{Name: "spfresh_starvation_widenings", Title: "SPFresh Starvation Widenings", Kind: recordlayer.KindCount}
	CountSPFreshForwardFollows  = recordlayer.Event{Name: "spfresh_forward_follows", Title: "SPFresh Forward Follows", Kind: recordlayer.KindCount}
	// Phase 2 reached: the VBASE relaxed-monotonicity termination (M_q^s > R_q)
	// latched during a one-shot search's traversal — the recently-traversed cells
	// no longer beat the running k-th best (RFC-156 Phase A). PURE TELEMETRY on
	// the one-shot wrapper path: nothing consults f.phase2 as a stop signal (it
	// does not truncate the exact horizon). The resumable Phase B/C streaming
	// cursor does NOT consult phase2 — it terminates on the relaxed-monotonicity
	// EMISSION BARRIER plus the budget/exhaustion caps (spfresh_stream.go), and
	// it never calls refreshPhase2, so this counter NEVER increments on a streaming
	// query (the SHARED searchInit probe still feeds observe(), but the streaming
	// WIDEN bursts gate it off — scoreCells, f.reranked==nil — since those queues
	// are dead weight once nothing will read phase2).
	CountSPFreshPhase2Reached = recordlayer.Event{Name: "spfresh_phase2_reached", Title: "SPFresh Phase 2 Reached", Kind: recordlayer.KindCount}
	// Capped posting reads: a search's posting fetch returned exactly the
	// 4×Lmax+1 cap — the posting is PAST the split-dispatch envelope and its
	// tail is invisible to queries. Nonzero means a split trigger was lost
	// (the read path re-files it; see CountSPFreshReadPathSplitFiles).
	CountSPFreshCappedPostingReads = recordlayer.Event{Name: "spfresh_capped_posting_reads", Title: "SPFresh Capped Posting Reads", Kind: recordlayer.KindCount}
	// Split tasks re-filed from the read path after a capped read found an
	// over-envelope posting with no pending split.
	CountSPFreshReadPathSplitFiles = recordlayer.Event{Name: "spfresh_readpath_split_files", Title: "SPFresh Read-Path Split Files", Kind: recordlayer.KindCount}
	// Stream widen batches: each demand-driven widening step of the RFC-156
	// Phase C ordered-stream cursor (a batch of ε-pruned/re-routed cells admitted
	// in d2 order because the consumer above drained the finalized prefix and
	// pulled for more). Batched, never one-cell-serial.
	CountSPFreshStreamWiden = recordlayer.Event{Name: "spfresh_stream_widenings", Title: "SPFresh Stream Widenings", Kind: recordlayer.KindCount}
	// Filtered truncation: the RFC-156 Phase C ordered-stream cursor hit its
	// budget cap (max cells probed / max candidates) BEFORE the consumer was
	// satisfied, and returned NoNextReason.ScanLimitReached + a positional
	// continuation rather than a silent < k. This is telemetry IN ADDITION to the
	// reason (RFC-156 §C) — the reason is the contract, this counts
	// how often the budget bound a filtered KNN.
	CountSPFreshFilteredTruncated = recordlayer.Event{Name: "spfresh_filtered_truncated", Title: "SPFresh Filtered Truncations", Kind: recordlayer.KindCount}

	// Write path.
	EventSPFreshInsert           = recordlayer.Event{Name: "spfresh_insert", Title: "SPFresh Insert", Kind: recordlayer.KindTimed}
	CountSPFreshInsertFenceReads = recordlayer.Event{Name: "spfresh_insert_fence_reads", Title: "SPFresh Insert Fence Reads", Kind: recordlayer.KindCount}
	CountSPFreshInsertReplicas   = recordlayer.Event{Name: "spfresh_insert_replicas", Title: "SPFresh Insert Replicas", Kind: recordlayer.KindCount}
	CountSPFreshStaleRouteRetry  = recordlayer.Event{Name: "spfresh_stale_route_retries", Title: "SPFresh Stale Route Retries", Kind: recordlayer.KindCount}

	// Maintenance (rebalancer / sweeper).
	CountSPFreshSplits       = recordlayer.Event{Name: "spfresh_splits", Title: "SPFresh Splits", Kind: recordlayer.KindCount}
	CountSPFreshMerges       = recordlayer.Event{Name: "spfresh_merges", Title: "SPFresh Merges", Kind: recordlayer.KindCount}
	CountSPFreshCSplits      = recordlayer.Event{Name: "spfresh_csplits", Title: "SPFresh Coarse Splits", Kind: recordlayer.KindCount}
	CountSPFreshNPAs         = recordlayer.Event{Name: "spfresh_npas", Title: "SPFresh NPA Reassignments", Kind: recordlayer.KindCount}
	CountSPFreshZombieCleans = recordlayer.Event{Name: "spfresh_zombie_cleans", Title: "SPFresh Zombie Cleanups", Kind: recordlayer.KindCount}
	CountSPFreshCSplitDefers = recordlayer.Event{Name: "spfresh_csplit_defers", Title: "SPFresh Coarse Split Deferrals", Kind: recordlayer.KindCount}
	CountSPFreshLeaseSkips   = recordlayer.Event{Name: "spfresh_lease_skips", Title: "SPFresh Lease Skips", Kind: recordlayer.KindCount}
	// Assignment refinement (RFC-104) fleet pass: vectors re-routed against the
	// converged topology, and tenants whose cursor wrapped a full cycle moving
	// nothing. Moves trending to zero while Converged rises = the fleet has
	// recovered ingest recall-drift and is quiescing; sustained nonzero Moves =
	// ongoing drift (e.g. a steady fast-ingest tenant) the refinement loop is
	// absorbing.
	CountSPFreshRefineMoves     = recordlayer.Event{Name: "spfresh_refine_moves", Title: "SPFresh Refinement Moves", Kind: recordlayer.KindCount}
	CountSPFreshRefineConverged = recordlayer.Event{Name: "spfresh_refine_converged", Title: "SPFresh Refinement Converged Tenants", Kind: recordlayer.KindCount}
	// Task handler errors in a rebalance pass: the pass SKIPS the failed
	// task and continues (a poisoned task at the deterministic queue head
	// must not starve everything behind it), then surfaces the joined
	// errors. Nonzero here with a stable queue depth = a poisoned task; the
	// runbook's "task queue growing" playbook keys off it.
	CountSPFreshTaskErrors = recordlayer.Event{Name: "spfresh_task_errors", Title: "SPFresh Task Errors", Kind: recordlayer.KindCount}
)
