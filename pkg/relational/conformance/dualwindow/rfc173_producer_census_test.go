package dualwindow_test

// RFC-173 producer census gate (Outcome-B slice B0). The name-model
// ("AnchoredJoin") result-value producers P4 (buildJoinResultValue) and P5
// (buildUnnestResultValue) are what the atomic cap must drive to zero; P1/P2/P3
// are derived and retire for free once P4/P5 are gone. This gate is the
// "can-we-reach-zero" instrument: it observes every P4/P5 firing across a corpus
// and pins the residual name-model surface so the remaining slices can be
// sequenced against a live inventory and a regression to name-model is loud.
//
// It ALSO CORRECTED the Outcome-B design consult empirically. The consult claimed
// every residual P4/P5 firing is ENCLOSED (t.inInnerCluster == true) — no
// un-enclosed producers. Building this census FALSIFIED that: a multi-way (>=3)
// inner join under a WHERE EXISTS declined its whole join subtree to name-model,
// with the TOP join UN-ENCLOSED (the plain 3-way without EXISTS ordinalized; the
// existential was the trigger). That class — "existential-over-multi-way-join"
// (U-1) — has since been ORDINALIZED by B1 (translateExistsOverGatheredCluster:
// the join translates as its own gathered ordinal cluster, the existential wraps
// it, and every leg reference rebases onto the box's positional output), so the
// probe below pins ZERO firings for it and is a regression flip-sentinel.
//
// Two assertions, both honest:
//   - SeedRunCorpus (the executable §5 differential corpus, 1641 entries) produces
//     ZERO name-model firings — it is already fully ordinal. A flip to >0 is a
//     regression (a corpus shape fell to name-model) OR a deliberate corpus growth
//     into a residual shape; either way, investigate.
//   - A small discriminating probe set: 2-way inner, plain 3-way inner,
//     correlated-scalar-in-projection, AND the 3-way-under-WHERE-EXISTS (post-B1)
//     all ordinalize (0 firings). A flip to >0 on any is a regression to the name
//     model. B1 correctness (the SARG surviving + the falsification control) is
//     pinned by the e2e TestFDB_RFC173S4_B1_NwayExists.
//
// Arrays cannot be INSERTed via SQL in this dialect (they go through the
// programmatic store builder), so the SQL-only probe corpus covers P4 (join)
// enclosure only; P5 (unnest) enclosure is covered by the box e2e suite where the
// consult's instrumentation ran. Runs ORDINAL-only (never flips SetNameModelOracle):
// under the name oracle every shape is name-model, so the census only has meaning
// in ordinal mode.

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"fdb.dev/pkg/relational/conformance/plandiff"
	"fdb.dev/pkg/relational/core/query"
)

// censusSignature renders a set of firings as a sorted, stable "count producer|
// enclosed|shape" multiset string — the comparable form for baseline pins.
func censusSignature(recs []query.ProducerCensusRecord) string {
	hist := map[string]int{}
	for _, r := range recs {
		hist["P="+r.Producer+" enclosed="+strconv.FormatBool(r.Enclosed)+" "+r.Shape]++
	}
	keys := make([]string, 0, len(hist))
	for k := range hist {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(strconv.Itoa(hist[k]))
		b.WriteString("x ")
		b.WriteString(k)
		b.WriteByte('\n')
	}
	return b.String()
}

func TestFDB_RFC173_ProducerCensus(t *testing.T) {
	// NOT t.Parallel(): installs a process-global census observer, and shares the
	// binary with the dual-window differential (Go serializes non-parallel tests,
	// so the two never overlap on the global).
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}

	var mu sync.Mutex
	var records []query.ProducerCensusRecord
	query.SetProducerCensusObserver(func(rec query.ProducerCensusRecord) {
		mu.Lock()
		records = append(records, rec)
		mu.Unlock()
	})
	defer query.SetProducerCensusObserver(nil)

	runner := plandiff.NewGoSQLSetupRunner(clusterFilePath)

	// --- Phase 1: the executable differential corpus must be fully ordinal. ---
	entries := plandiff.SeedRunCorpus()
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, q := range entries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			// Translation (which fires the census) happens inside RunWithSetup's
			// plan phase; the row result is irrelevant to the census.
			runner.RunWithSetup(context.Background(), q.SchemaTemplate, q.SetupSqls, q.Query)
		}()
	}
	wg.Wait()

	mu.Lock()
	corpusRecords := append([]query.ProducerCensusRecord(nil), records...)
	mu.Unlock()
	if len(corpusRecords) != 0 {
		t.Errorf("RFC-173 producer census: the SeedRunCorpus (%d entries) produced %d name-model firing(s); it must be fully ORDINAL (zero producers). A flip to >0 is a regression to the name model on a differential-corpus shape, or a deliberate corpus growth into a residual shape — investigate, do not carve out.\n%s",
			len(entries), len(corpusRecords), censusSignature(corpusRecords))
	}

	// --- Phase 2: discriminating probes pin the residual name-model surface. ---
	probeSchema := "CREATE TABLE p (id BIGINT, v BIGINT, PRIMARY KEY (id))" +
		" CREATE TABLE q (qid BIGINT, qv BIGINT, PRIMARY KEY (qid))" +
		" CREATE TABLE r (rid BIGINT, rv BIGINT, PRIMARY KEY (rid))" +
		" CREATE TABLE e (eid BIGINT, eref BIGINT, PRIMARY KEY (eid))"
	probeSetup := []string{
		"INSERT INTO p VALUES (1, 10), (2, 20)",
		"INSERT INTO q VALUES (1, 100), (2, 200)",
		"INSERT INTO r VALUES (1, 1000)",
		"INSERT INTO e VALUES (301, 1)",
	}
	probe := func(sql string) []query.ProducerCensusRecord {
		mu.Lock()
		before := len(records)
		mu.Unlock()
		res := runner.RunWithSetup(context.Background(), probeSchema, probeSetup, sql)
		if res.Err != nil {
			t.Fatalf("probe query errored (must plan+run): %v\n  %s", res.Err, sql)
		}
		mu.Lock()
		fired := append([]query.ProducerCensusRecord(nil), records[before:]...)
		mu.Unlock()
		return fired
	}

	// These three ordinalize — a fresh (un-enclosed) 2-way/3-way inner join and a
	// correlated scalar subquery in a projection all resolve positionally.
	for _, sql := range []string{
		"SELECT p.v FROM p JOIN q ON q.qid = p.id",
		"SELECT p.v FROM p JOIN q ON q.qid = p.id JOIN r ON r.rid = p.id",
		"SELECT p.v, (SELECT COUNT(*) FROM e WHERE e.eref = p.id) FROM p JOIN q ON q.qid = p.id",
	} {
		if fired := probe(sql); len(fired) != 0 {
			t.Errorf("expected 0 name-model firings (shape ordinalizes), got %d:\n%s  %s", len(fired), censusSignature(fired), sql)
		}
	}

	// B1 (U-1) ORDINALIZED the existential-over-multi-join: an arity>=3 inner join
	// under a WHERE EXISTS translates as its own gathered ordinal cluster wrapped
	// by the existential (translateExistsOverGatheredCluster), so it fires ZERO
	// name-model producers. (Before B1 it fired 1 enclosed Scan|Scan + 1
	// un-enclosed Join|Scan — the reframe-correcting un-enclosed residual U-1,
	// found and then retired by this census.) Flip-sentinel: if B1 regresses, this
	// fires again. Correctness — the index SARG surviving AND the falsification
	// control (EXISTS correlated to the 3rd/4th leg) — is pinned by the e2e
	// TestFDB_RFC173S4_B1_NwayExists + TestFDB_RFC173_CorrelatedIndexExistsStaysIndexed.
	existMultiJoin := probe("SELECT p.v FROM p JOIN q ON q.qid = p.id JOIN r ON r.rid = p.id WHERE EXISTS (SELECT 1 FROM e WHERE e.eid = p.id)")
	if len(existMultiJoin) != 0 {
		t.Errorf("existential-over-3-way-join now ordinalizes (B1/U-1) — expected 0 name-model firings, got %d:\n%s (B1 regressed, or a new residual appeared on this shape)", len(existMultiJoin), censusSignature(existMultiJoin))
	}
}
