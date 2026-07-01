// Package dualwindow_test is the RFC-173 §5 dual-window corpus DIFFERENTIAL
// (Round 5): while both row representations are emitted (P2 → Slice 4), assert
// `ordinal result == name result` row-for-row across the executable corpus at
// every slice, with explicit carve-outs for the enumerated known-different
// shapes. The §5 item-2 execution pins certify the KNOWN model differences;
// this differential catches the UNKNOWN ones — exactly the coverage class that
// would have caught Slice 1's Step 2b buried-reference divergence before the
// spike tripped over it. (The anti-dark-diff argument at the top of §5 applies
// to plan-shape gates, not to row agreement on shapes where the two models are
// supposed to agree.)
//
// Mechanism: each corpus entry runs twice through the SAME Go engine —
//   - ORDINAL mode: normal execution (Slice 1: ordinal resolution authoritative
//     on the non-join frontier);
//   - NAME mode: executor.DisablePositionalEmission suppresses the PositionalRow
//     at the row-birth sites, so no frontier gate fires and the engine runs the
//     pre-Slice-1 name model end-to-end (an EMISSION switch used as a test
//     oracle — not a resolution fallback; Graefe's no-fallback rule governs
//     resolution).
//
// Each RunWithSetup call builds its own ephemeral schema, so the two modes are
// identically seeded and independent (DML entries included). The toggle is a
// process-global, so the phases are strictly sequential: all ORDINAL runs
// complete before the toggle flips — this package owns its test binary and the
// single test function is the only query traffic.
//
// This differential lives for the dual-representation window and is DELETED
// with the name map in Slice 4.
package dualwindow_test

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/relational/conformance/plandiff"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
)

// setDisablePositionalEmission flips the executor's §5 differential oracle
// switch. Callers must guarantee no query is in flight (phase barrier).
func setDisablePositionalEmission(v bool) { executor.DisablePositionalEmission = v }

// clusterFilePath is set by TestMain when an FDB testcontainer is available.
// Empty means "skip" (no Docker).
var clusterFilePath string

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := foundationdbtc.Run(ctx, "")
	if err != nil {
		// No Docker — the differential skips.
		os.Exit(m.Run())
	}
	defer container.Terminate(context.Background()) //nolint:errcheck

	clusterContent, err := container.ClusterFile(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ClusterFile: %v\n", err)
		os.Exit(1)
	}
	tmp, err := os.CreateTemp("", "fdb-dualwindow-*.cluster")
	if err != nil {
		fmt.Fprintf(os.Stderr, "CreateTemp: %v\n", err)
		os.Exit(1)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(clusterContent); err != nil {
		fmt.Fprintf(os.Stderr, "WriteString: %v\n", err)
		os.Exit(1)
	}
	tmp.Close()
	clusterFilePath = tmp.Name()

	os.Exit(m.Run())
}

// carveOuts enumerates corpus entries where the two models are SUPPOSED to
// differ (RFC-173 §5 item 1), keyed by entry name with the §-level reason.
// Empty on the Slice 1 frontier: the buried-reference precursor resolves
// derived/CTE refs to output names under BOTH models (the Datum carries both
// keys), the `SELECT *` duplicate-name collision fix is not live until Slice 4,
// and CTE column-rename returns identical rows under both models today. Every
// future carve-out MUST cite the RFC section that declares the difference
// intentional.
var carveOuts = map[string]string{}

// outcome is one mode's result for one corpus entry: the rows, or the error.
type outcome struct {
	rows plandiff.RowSet
	err  string
}

// ephemeralNames strips the per-call ephemeral identifiers (template/db/schema
// carry a fresh UUID per RunWithSetup) from error text so the two modes'
// errors compare on substance, not on the throwaway names.
var ephemeralNames = regexp.MustCompile(`(PLAN_DIFF_T?_?|S_)[0-9a-fA-F]{32}`)

func normalizeErr(err error) string {
	if err == nil {
		return ""
	}
	return ephemeralNames.ReplaceAllString(err.Error(), "<ephemeral>")
}

// runPhase executes every corpus entry once in the CURRENT toggle mode, with a
// bounded worker pool. The pool drains completely before returning, so the
// caller can safely flip the process-global toggle at the phase barrier.
func runPhase(t *testing.T, entries []plandiff.RunQuery) []outcome {
	t.Helper()
	runner := plandiff.NewGoSQLSetupRunner(clusterFilePath)
	out := make([]outcome, len(entries))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, q := range entries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res := runner.RunWithSetup(context.Background(), q.SchemaTemplate, q.SetupSqls, q.Query)
			out[i] = outcome{rows: res.Rows, err: normalizeErr(res.Err)}
		}()
	}
	wg.Wait()
	return out
}

// TestFDB_DualWindowDifferential_RFC173 runs the full executable corpus under
// both resolution models and requires row-for-row (and error-for-error)
// agreement on every entry not carved out. A mismatch here is an UNKNOWN
// model divergence — the §5 differential's entire purpose — and must be
// root-caused, never carved out without an RFC citation.
func TestFDB_DualWindowDifferential_RFC173(t *testing.T) {
	// NOT t.Parallel(): flips the process-global emission toggle between phases.
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}

	entries := plandiff.SeedRunCorpus()

	// Phase 1 — ORDINAL (normal Slice 1 execution).
	ordinal := runPhase(t, entries)

	// Phase barrier: no queries in flight; flip to the NAME model.
	setDisablePositionalEmission(true)
	name := runPhase(t, entries)
	setDisablePositionalEmission(false)

	mismatches := 0
	for i, q := range entries {
		if reason, ok := carveOuts[q.Name]; ok {
			t.Logf("carve-out %q: %s", q.Name, reason)
			continue
		}
		o, n := ordinal[i], name[i]
		if o.err != n.err {
			mismatches++
			t.Errorf("%s: ERROR divergence between models\n  ordinal: %q\n  name:    %q\n  query: %s",
				q.Name, o.err, n.err, q.Query)
			continue
		}
		if o.err != "" {
			continue // both errored identically — agreement.
		}
		if !reflect.DeepEqual(o.rows, n.rows) {
			mismatches++
			t.Errorf("%s: ROW divergence between models\n  ordinal: %+v\n  name:    %+v\n  query: %s",
				q.Name, o.rows, n.rows, q.Query)
		}
	}
	t.Logf("dual-window differential: %d corpus entries, %d carve-outs, %d mismatches",
		len(entries), len(carveOuts), mismatches)
}
