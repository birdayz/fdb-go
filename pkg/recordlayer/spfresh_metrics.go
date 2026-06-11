package recordlayer

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
	EventSPFreshSearch = Event{"spfresh_search", "SPFresh Search"}
	// Probed/pruned: the Eq.(3) pruning decomposition per search — probed
	// lists cost range reads, pruned ones were skipped.
	CountSPFreshPostingsProbed  = Event{"spfresh_postings_probed", "SPFresh Postings Probed"}
	CountSPFreshPostingsPruned  = Event{"spfresh_postings_pruned", "SPFresh Postings Pruned"}
	CountSPFreshEntriesScanned  = Event{"spfresh_entries_scanned", "SPFresh Entries Scanned"}
	CountSPFreshRerankReads     = Event{"spfresh_rerank_reads", "SPFresh Rerank Reads"}
	CountSPFreshStarvationWiden = Event{"spfresh_starvation_widenings", "SPFresh Starvation Widenings"}
	CountSPFreshForwardFollows  = Event{"spfresh_forward_follows", "SPFresh Forward Follows"}

	// Write path.
	EventSPFreshInsert           = Event{"spfresh_insert", "SPFresh Insert"}
	CountSPFreshInsertFenceReads = Event{"spfresh_insert_fence_reads", "SPFresh Insert Fence Reads"}
	CountSPFreshInsertReplicas   = Event{"spfresh_insert_replicas", "SPFresh Insert Replicas"}
	CountSPFreshStaleRouteRetry  = Event{"spfresh_stale_route_retries", "SPFresh Stale Route Retries"}

	// Maintenance (rebalancer / sweeper).
	CountSPFreshSplits       = Event{"spfresh_splits", "SPFresh Splits"}
	CountSPFreshMerges       = Event{"spfresh_merges", "SPFresh Merges"}
	CountSPFreshCSplits      = Event{"spfresh_csplits", "SPFresh Coarse Splits"}
	CountSPFreshNPAs         = Event{"spfresh_npas", "SPFresh NPA Reassignments"}
	CountSPFreshZombieCleans = Event{"spfresh_zombie_cleans", "SPFresh Zombie Cleanups"}
	CountSPFreshCSplitDefers = Event{"spfresh_csplit_defers", "SPFresh Coarse Split Deferrals"}
	CountSPFreshLeaseSkips   = Event{"spfresh_lease_skips", "SPFresh Lease Skips"}
)
