// Command spfresh-maintainer is the reference SPFresh maintenance worker
// (RFC-156 §3.2). SPFresh maintenance is caller-driven; this is the thin,
// runnable wrapper around recordlayer.RunSPFreshMaintenance that a deployment
// can run standalone or copy as a starting point.
//
// It opens an FDB database, builds the tenant list (the ONE part every
// deployment must supply — store layout is application keyspace, see
// discoverTenants below), wires a StoreTimer for metrics, and runs the
// sweep+refine loop until SIGINT/SIGTERM.
//
//	spfresh-maintainer -cluster-file /etc/foundationdb/fdb.cluster \
//	  -sweep-interval 10s -refine-interval 5m -metrics-interval 30s
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
)

func main() {
	clusterFile := flag.String("cluster-file", "", "FDB cluster file path (empty = default location)")
	sweepInterval := flag.Duration("sweep-interval", 10*time.Second, "rebalance/sweep cadence")
	refineInterval := flag.Duration("refine-interval", 5*time.Minute, "RFC-104 refinement cadence (slower than sweep)")
	disableRefine := flag.Bool("disable-refine", false, "run sweep only, no refinement")
	metricsInterval := flag.Duration("metrics-interval", 30*time.Second, "how often to log accumulated maintenance metrics")
	maxActions := flag.Int("max-actions-per-tenant", 64, "per-tenant lifecycle action budget per sweep pass")
	refineBudget := flag.Int("refine-budget-per-tenant", 1000, "per-tenant vectors re-evaluated per refine pass")
	flag.Parse()

	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(*clusterFile)
	if err != nil {
		log.Fatalf("spfresh-maintainer: open database: %v", err)
	}
	db := recordlayer.NewFDBDatabase(rawDB)

	tenants := discoverTenants(db)
	if len(tenants) == 0 {
		log.Println("spfresh-maintainer: WARNING — no tenants configured; the loop will idle. " +
			"Wire discoverTenants() to your store layout (see the comment in main.go).")
	} else {
		log.Printf("spfresh-maintainer: maintaining %d tenant index(es)", len(tenants))
	}

	// One shared timer accumulates instrumentation across both loops; a ticker
	// logs it so an operator (or a metrics sidecar scraping stdout) sees
	// backlog/lag without extra wiring.
	timer := recordlayer.NewStoreTimer()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go logMetricsLoop(ctx, timer, *metricsInterval)

	opts := recordlayer.SPFreshMaintenanceOptions{
		Tenants:        tenants,
		SweepInterval:  *sweepInterval,
		RefineInterval: *refineInterval,
		DisableRefine:  *disableRefine,
		Sweep:          recordlayer.SPFreshSweepOptions{MaxActionsPerTenant: *maxActions, Timer: timer},
		Refine:         recordlayer.SPFreshRefineOptions{BudgetPerTenant: *refineBudget, Timer: timer},
		OnSweep: func(res recordlayer.SPFreshSweepResult, err error) {
			if err != nil {
				log.Printf("spfresh-maintainer: sweep: worked=%d actions=%d undrained=%d err=%v",
					res.Worked, res.Actions, res.Undrained, err)
			}
		},
		OnRefine: func(res recordlayer.SPFreshRefineResult, err error) {
			if err != nil {
				log.Printf("spfresh-maintainer: refine: refined=%d moves=%d converged=%d err=%v",
					res.Refined, res.Moves, res.Converged, err)
			}
		},
	}

	log.Printf("spfresh-maintainer: started (sweep=%s refine=%s disableRefine=%v)",
		*sweepInterval, *refineInterval, *disableRefine)
	if err := recordlayer.RunSPFreshMaintenance(ctx, db, opts); err != nil {
		log.Fatalf("spfresh-maintainer: %v", err)
	}
	logMetrics(timer) // final snapshot
	log.Println("spfresh-maintainer: stopped")
}

// discoverTenants returns the (store, index) pairs to maintain. THIS IS THE ONE
// FUNCTION A DEPLOYMENT MUST SUPPLY: a store's subspace, metadata, and the set
// of SPFresh indexes within it are application keyspace, not enumerable from
// inside the record layer. Replace the body with your enumeration — typically a
// scan of your tenant directory plus, per tenant, a storeBuilder closure:
//
//	return []recordlayer.SPFreshTenant{{
//	    StoreBuilder: func(rtx *recordlayer.FDBRecordContext) (*recordlayer.FDBRecordStore, error) {
//	        return recordlayer.NewStoreBuilder().SetContext(rtx).
//	            SetMetaDataProvider(myMetadata).SetSubspace(tenantSubspace).CreateOrOpen()
//	    },
//	    IndexName: "docs_emb",
//	}}
//
// Sharding the returned list across N worker processes is safe — sweeps are
// concurrent-safe by construction (unique lease owners, task-level exclusion).
func discoverTenants(_ *recordlayer.FDBDatabase) []recordlayer.SPFreshTenant {
	return nil
}

// logMetricsLoop periodically logs the accumulated maintenance metrics until
// ctx is canceled.
func logMetricsLoop(ctx context.Context, timer *recordlayer.StoreTimer, every time.Duration) {
	if every <= 0 {
		every = 30 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			logMetrics(timer)
		}
	}
}

func logMetrics(timer *recordlayer.StoreTimer) {
	log.Printf("spfresh-maintainer metrics: splits=%d merges=%d csplits=%d npas=%d zombieCleans=%d leaseSkips=%d taskErrors=%d refineMoves=%d refineConverged=%d",
		timer.GetCount(recordlayer.CountSPFreshSplits),
		timer.GetCount(recordlayer.CountSPFreshMerges),
		timer.GetCount(recordlayer.CountSPFreshCSplits),
		timer.GetCount(recordlayer.CountSPFreshNPAs),
		timer.GetCount(recordlayer.CountSPFreshZombieCleans),
		timer.GetCount(recordlayer.CountSPFreshLeaseSkips),
		timer.GetCount(recordlayer.CountSPFreshTaskErrors),
		timer.GetCount(recordlayer.CountSPFreshRefineMoves),
		timer.GetCount(recordlayer.CountSPFreshRefineConverged),
	)
}
