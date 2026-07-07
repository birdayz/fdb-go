// Package ordinalspike_test is the RFC-173 S4 B1 CERTIFICATE — the corpus-level
// off-vs-on differential that gates the ATOMIC demolition cap. The join name-model
// retirement is irreducibly atomic: the two PURELY-CIRCULAR enclosure declines
// (rfc173_cluster_gate.go — the outer-box-enclosed and inner-cluster-enclosed
// `t.inInnerCluster` guards) are faithful symptoms of a name-model parent — a child
// gates iff its parent gates, so they can only lift TOGETHER, when the name model is
// deleted. This spike models the EXACT cap-gate state: it flips ONLY those two
// enclosure declines and KEEPS the `ordinalEligible` (`:152`) decline, which is a
// GENUINE name-model decline for unnest/aggregate legs (it self-corrects for join legs
// via recursion once the enclosure declines lift, but a genuine leg must keep declining
// until it ordinalizes in W5). Flipping `:152` too — B1's earlier over-aggressive
// version — would gate genuine unnest legs and yield wrong rows for shapes the corpus
// need not cover. So the certificate proves the state the cap actually ships.
//
// A green run PROVES that deleting the AnchoredJoin flag + the anchored producers
// (buildJoinResultValue/buildUnnestResultValue/NewReEnumerationAnchoredRecord) + the
// §5 oracle preserves every row — i.e. the atomic cap is safe to fire. A mismatch is
// a shape the ordinal machinery does not yet handle: the cap is NOT yet safe there,
// and that shape is the next pre-cap ordinalization slice.
//
// Mechanism mirrors the §5 dual-window differential: each corpus entry runs twice
// through the SAME Go engine under a process-global toggle flipped at a phase barrier —
//   - OFF: normal translation (circular declines fall to the name model);
//   - ON:  query.SetForceOrdinalSpike suppresses the three circular declines so the
//     whole gate ordinalizes every circular parent+child.
//
// Unlike the dual-window oracle (an EMISSION switch), this is a GATE switch — it
// changes which joins ordinalize, exactly what the cap changes permanently.
package ordinalspike_test

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/relational/conformance/plandiff"
	"fdb.dev/pkg/relational/core/query"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
)

var clusterFilePath string

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := foundationdbtc.Run(ctx, "")
	if err != nil {
		os.Exit(m.Run()) // No Docker — the differential skips.
	}
	defer container.Terminate(context.Background()) //nolint:errcheck

	clusterContent, err := container.ClusterFile(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ClusterFile: %v\n", err)
		os.Exit(1)
	}
	tmp, err := os.CreateTemp("", "fdb-ordinalspike-*.cluster")
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

// spikeCarveOuts enumerates corpus entries where the whole-gate ordinal flip is
// EXPECTED to differ from name-mode today (a shape whose ordinalization is a booked
// later slice). Every entry MUST cite the reason. An empty map is the goal: it means
// the whole-gate flip preserves every corpus row.
//
// NECESSARY, NOT SUFFICIENT: a green run here does NOT license the cap. The SeedRunCorpus
// is SQL-buildable-schema only — it has NO array columns, so NO lateral-unnest shapes. A
// full-suite flip (forceOrdinalSpike default=true) over the real FDB suite PANICS on
// multi-source unnest over a join (`FROM A, B, A.arr AS X`): the enclosure flip gates the
// inner join while its unnest parent stays name-model, and the group-by re-enumeration
// (rule_partition_select.go:469, NewReEnumerationAnchoredRecord) hits an ordinal child
// under a name-model parent → "RFC-077 7.6: anchored re-enumeration must resolve an
// anchored parent's legs". So the enclosure flip is COUPLED to W5 unnest ordinalization:
// W5 (retire the :1528 unnest producer) must land FIRST. The FULL FDB suite under the
// flip — not this corpus — is the cap's real exit gate. See scratchpad/demolition-readiness.md.
var spikeCarveOuts = map[string]string{}

type outcome struct {
	rows plandiff.RowSet
	err  string
}

var ephemeralNames = regexp.MustCompile(`(PLAN_DIFF_T?_?|S_)[0-9a-fA-F]{32}`)

func normalizeErr(err error) string {
	if err == nil {
		return ""
	}
	return ephemeralNames.ReplaceAllString(err.Error(), "<ephemeral>")
}

// runPhase executes every corpus entry once in the CURRENT spike mode with a bounded
// worker pool. The pool drains before returning so the caller can flip the
// process-global at the phase barrier.
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

// TestFDB_OrdinalSpikeDifferential_RFC173 runs the full executable corpus with the
// circular declines OFF (normal) and ON (whole-gate ordinalized), requiring
// row-for-row (and error-for-error) agreement on every non-carved-out entry. A
// mismatch means the atomic cap would change that shape's result — the cap is not yet
// safe there. NOT t.Parallel(): flips the process-global gate toggle between phases.
func TestFDB_OrdinalSpikeDifferential_RFC173(t *testing.T) {
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}

	entries := plandiff.SeedRunCorpus()

	// Phase 1 — spike OFF (normal translation).
	off := runPhase(t, entries)

	// Phase barrier: no translation in flight; flip the whole gate ordinal.
	query.SetForceOrdinalSpike(true)
	on := runPhase(t, entries)
	query.SetForceOrdinalSpike(false)

	mismatches := 0
	for i, q := range entries {
		if reason, ok := spikeCarveOuts[q.Name]; ok {
			t.Logf("carve-out %q: %s", q.Name, reason)
			continue
		}
		o, n := off[i], on[i]
		if o.err != n.err {
			mismatches++
			t.Errorf("%s: ERROR divergence under whole-gate ordinal flip\n  off: %q\n  on:  %q\n  query: %s",
				q.Name, o.err, n.err, q.Query)
			continue
		}
		if o.err != "" {
			continue // both errored identically — agreement.
		}
		if !reflect.DeepEqual(o.rows, n.rows) {
			mismatches++
			t.Errorf("%s: ROW divergence under whole-gate ordinal flip\n  off: %+v\n  on:  %+v\n  query: %s",
				q.Name, o.rows, n.rows, q.Query)
		}
	}
	t.Logf("ordinal-spike differential: %d corpus entries, %d carve-outs, %d mismatches",
		len(entries), len(spikeCarveOuts), mismatches)
}
