package embedded

// The statement-level half of scratch retry idempotence: the paging loop runs
// its whole body — BeginPage included — inside the transaction retry, so an
// ambiguous commit re-executes a page from the UNCHANGED continuation. What the
// dead attempt parked must not still be held, and must not still be CHARGED.
//
// An internal test for the same reason the rest of the wiring pins are: the
// executor-level pin (TestDistinctRetriedAttemptsDoNotStackCharges) drives
// BeginPage itself, so it passes whether or not fetchPage calls it per ATTEMPT.
// Only a statement retried by the real transactor can see that.

import (
	"context"
	"database/sql/driver"
	"fmt"
	"io"
	"testing"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/chaos"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/catalog"
	"fdb.dev/pkg/relational/core/ddl"
	"fdb.dev/pkg/relational/core/keyspace"
	"fdb.dev/pkg/simfdb"
)

// newChaosSimConnection is newSimConnection with the fault-injecting transactor
// in the middle, and the returned FaultConfig is live: mutating its Rates after
// setup arms the fault for the statements that follow, leaving the DDL and the
// seed inserts alone.
func newChaosSimConnection(t *testing.T, seed uint64) (*EmbeddedConnection, *chaos.FaultConfig) {
	t.Helper()
	env := dst.NewSim(seed)
	env.Buggify = dst.DisabledBuggifier()
	sim := simfdb.New(env)
	faults := &chaos.FaultConfig{Rates: map[chaos.FaultType]float64{}}
	transactor := chaos.NewChaosTransactor(sim, faults, seed)
	// The concrete-db slot is empty for the same reason
	// NewFDBDatabaseWithBackend leaves it empty for SimFDB: the simulator is a
	// BackendDatabase, not the pure-Go fdb.Database. Everything this test
	// touches runs through the transactor's Run/RunRead path.
	fdbDB := recordlayer.NewFDBDatabaseWithTransactor(transactor, fdb.Database{}).SetEnv(env)
	fdbDB.SetStoreStateCache(recordlayer.NewMetaDataVersionStampStoreStateCache())

	ks := keyspace.New(subspace.Sub())
	cat, err := catalog.NewRecordLayerStoreCatalog(ks.CatalogSubspace())
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	factory := ddl.NewRecordLayerMetadataOperationsFactoryWithKeyspace(cat, ks)

	c := New("/simdb", fdbDB, cat, factory, ks)
	ctx := context.Background()
	for _, stmt := range []string{
		"CREATE DATABASE /simdb",
		"CREATE SCHEMA TEMPLATE tmpl CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))",
		"CREATE SCHEMA /simdb/s WITH TEMPLATE tmpl",
	} {
		if _, err := c.ExecContext(ctx, stmt, nil); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	c.SetDefaultSchema("s")
	return c, faults
}

// TestPagedDistinctUnderAmbiguousCommitsHoldsSteady pins that a paged SELECT
// DISTINCT whose every page comes back with an ambiguous commit — so every page
// executes TWICE from the same continuation — holds no more scratch state than
// the same statement with no faults at all.
//
// FaultCommitUnknown is FDB error 1021: the commit landed, the client cannot
// know it, so it re-runs the transaction body. The paging loop's body is where
// BeginPage lives, and the dead attempt's parked entry keeps both its slot and
// its charge unless the retry discards it — one attempt-sized delta per attempt,
// reclaimed by nothing until the page finally commits. Under a real conflict
// storm the statement fails its memory budget for state it does not hold.
//
// THE MEASUREMENT MUST BE THE HIGH-WATER MARK, not anything sampled between
// pages. A page that retries once and then commits has its leftovers collected
// by its own sweep, so residency and charge read IDENTICALLY either way from
// outside — the first draft of this test asserted on both and stayed green with
// the defect fully present. What a conflict storm multiplies is what the page
// holds WHILE it is being retried, which only PeakDistinctSets can see.
//
// Measured before the fix on this shape: peak 3 live scratch states with every
// page retried, against 2 for the same statement unfaulted.
func TestPagedDistinctUnderAmbiguousCommitsHoldsSteady(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const rows, distinct = 40, 8

	// drain runs the same paged DISTINCT statement, optionally with every page's
	// commit reported ambiguous, and reports the values it returned, the peak
	// scratch residency the statement ever reached, and what it still holds when
	// the drain ends.
	drain := func(faulted bool) (values map[int64]int, peak int, finalMem int64) {
		t.Helper()
		c, faults := newChaosSimConnection(t, 991)
		for i := 0; i < rows; i++ {
			if _, err := c.ExecContext(ctx,
				fmt.Sprintf("INSERT INTO t (id, v) VALUES (%d, %d)", i, i%distinct), nil); err != nil {
				t.Fatalf("insert %d: %v", i, err)
			}
		}
		// Small pages, so the statement takes many of them and every page both
		// adopts and parks.
		c.SetOptions(api.NewOptionsBuilder().Set(api.OptExecutionScannedRowsLimit, 2).Build())
		if faulted {
			faults.Rates[chaos.FaultCommitUnknown] = 1.0
		}

		result, err := c.QueryContext(ctx, "SELECT DISTINCT v FROM t", nil)
		if err != nil {
			t.Fatalf("query (faulted=%v): %v", faulted, err)
		}
		pr, ok := result.(*paginatingRows)
		if !ok {
			t.Fatalf("query returned %T, want *paginatingRows", result)
		}
		defer pr.Close() //nolint:errcheck

		values = map[int64]int{}
		dest := make([]driver.Value, 1)
		for {
			if err := pr.Next(dest); err != nil {
				if err == io.EOF {
					break
				}
				t.Fatalf("iterate (faulted=%v): %v", faulted, err)
			}
			values[dest[0].(int64)]++
		}
		if minted := pr.scratch.MintedDistinctSets(); minted < 2 {
			t.Fatalf("statement parked %d seen-sets; the shape must actually page", minted)
		}
		return values, pr.scratch.PeakDistinctSets(), pr.execState.MemUsed()
	}

	cleanValues, cleanPeak, cleanMem := drain(false)
	faultedValues, faultedPeak, faultedMem := drain(true)

	// Correctness first: an ambiguous commit must not cost or duplicate a row.
	for _, got := range []map[int64]int{cleanValues, faultedValues} {
		if len(got) != distinct {
			t.Fatalf("paged DISTINCT returned %d values (%v), want %d", len(got), got, distinct)
		}
		for v, n := range got {
			if n != 1 {
				t.Fatalf("value %d returned %d times", v, n)
			}
		}
	}
	if faultedPeak > cleanPeak {
		t.Fatalf(
			"a statement whose every page was retried after an ambiguous commit peaks at %d "+
				"live scratch states against %d for the same statement with no faults: the "+
				"dead attempt's parked entry stays live — and CHARGED — into its retry, so a "+
				"conflict storm accumulates one attempt-sized delta per attempt and the "+
				"statement fails its memory budget before any attempt commits. The paging "+
				"loop re-enters BeginPage per ATTEMPT, which is where the discard belongs",
			faultedPeak, cleanPeak,
		)
	}
	// And the retries left nothing behind: a fully drained statement holds no
	// scratch bytes whichever way it got there.
	if cleanMem != 0 || faultedMem != 0 {
		t.Fatalf("drained statement still holds %d bytes clean / %d faulted, want 0",
			cleanMem, faultedMem)
	}
}
