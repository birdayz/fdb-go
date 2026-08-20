//go:build stress

package stress_test

// RFC-236 at scale: collected statistics change the join order — does the
// order they choose actually RUN faster, on a table big enough for the wrong
// choice to cost real time?
//
// The correctness test in pkg/relational/sqldriver proves the DECISION follows
// the data. It cannot prove the decision is worth making: at 400 rows every
// plan finishes instantly. This one measures.
//
// THE MEASUREMENT IS OFF-vs-ON WITHIN ONE ARRANGEMENT, over a MIRRORED PAIR.
// Without statistics the planner still has to pick a side, and it picks by a
// tie-break over plan structure (RFC-235 §18) — so on any ONE arrangement it is
// right or wrong by luck, and a single-arrangement timing measures the luck.
// Over a mirrored PAIR the luck cancels: a fixed tie-break drives from the same
// table in both, so it is right in one and wrong in the other, whatever it
// picked. The pair then splits into a WIN and a CONTROL by its PLANS.
//
// The ratio is never taken ACROSS the pair. The two arrangements return
// different row counts, so their absolute times answer different questions; the
// assertion block below says more about why, because an earlier revision of
// this file did take a max across them and reported a control regression that
// was two result sets being differenced.
//
//	statistics OFF: max over the pair is the SLOW plan — it cannot be right twice
//	statistics ON:  max over the pair is the fast plan — right in both
//
// The pair's MINIMUM is the control. If ON were simply faster at everything —
// a warmer cache, less work per row, a measurement artifact — the minimum would
// move too. It must not: on the arrangement where the tie-break got lucky, OFF
// and ON plan the SAME thing and must time the same.
//
// Sized so the wrong order is expensive rather than merely different: the big
// table drives 200k point lookups where the right order does 50 range scans.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/embedded"
)

const (
	statsJoinSmallRows = 50
	statsJoinBigRows   = 1_000_000
	// statsJoinRepeats is how many times each (arrangement, setting) pair is
	// timed. The reported figure is the MINIMUM, not the mean: this measures a
	// plan's cost, and the minimum is the sample least polluted by whatever
	// else the machine was doing. A mean over a noisy box measures the box.
	statsJoinRepeats = 3
)

// statsJoinArrangement is one loaded database plus the two connections that
// differ only in the planner_statistics flag.
type statsJoinArrangement struct {
	name    string
	aRows   int
	bRows   int
	off     *sql.DB
	on      *sql.DB
	planOff string
	planOn  string
}

func TestFDB_Stress_StatisticsJoinOrder(t *testing.T) {
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	// Both arrangements are built and measured before anything is asserted, so
	// a failure report can show the whole 2x2 rather than the first cell that
	// tripped. A partial table is what makes a timing failure unreadable.
	arrangements := []*statsJoinArrangement{
		{name: "a_small", aRows: statsJoinSmallRows, bRows: statsJoinBigRows},
		{name: "a_big", aRows: statsJoinBigRows, bRows: statsJoinSmallRows},
	}

	const joinSQL = "SELECT a.v, b.id FROM a, b WHERE b.a_id = a.id"

	type cell struct {
		best time.Duration
		rows int
	}
	results := map[string]map[bool]cell{}

	for _, arr := range arrangements {
		buildStatsJoinArrangement(t, ctx, arr)
		results[arr.name] = map[bool]cell{}

		for _, useStats := range []bool{false, true} {
			db := arr.off
			if useStats {
				db = arr.on
			}
			best := time.Duration(0)
			rows := 0
			for rep := 0; rep < statsJoinRepeats; rep++ {
				d, n := timeStatsJoin(t, ctx, db, joinSQL)
				// A join whose row count moves between repeats is not a timing
				// signal, it is a correctness bug — and averaging it away would
				// hide the bug behind the number this test exists to produce.
				if rep > 0 && n != rows {
					t.Fatalf("%s stats=%v: row count changed between repeats: %d then %d",
						arr.name, useStats, rows, n)
				}
				rows = n
				if best == 0 || d < best {
					best = d
				}
			}
			results[arr.name][useStats] = cell{best: best, rows: rows}
			t.Logf("%-8s stats=%-5v  %8s  rows=%d", arr.name, useStats, best.Round(time.Millisecond), rows)
		}

		// Both settings must return the SAME rows. Statistics change the join
		// ORDER, never the result set; if they differ, every timing below is
		// comparing two different questions.
		if got, want := results[arr.name][true].rows, results[arr.name][false].rows; got != want {
			t.Fatalf("%s: statistics changed the RESULT, not just the plan: %d rows on, %d rows off",
				arr.name, got, want)
		}
		if results[arr.name][false].rows == 0 {
			t.Fatalf("%s: the join returned no rows — a timing over an empty result measures nothing",
				arr.name)
		}
	}

	for _, arr := range arrangements {
		t.Logf("%-8s plan OFF: %s", arr.name, arr.planOff)
		t.Logf("%-8s plan ON : %s", arr.name, arr.planOn)
	}

	// THE COMPARISON IS OFF-vs-ON WITHIN ONE ARRANGEMENT, never across the
	// pair. The two arrangements answer different questions — one join returns
	// |b| rows and the mirrored one returns |a| — so their absolute times are
	// not comparable, and a max taken across them silently compares the slow
	// half of one query against the fast half of the other. That was the first
	// version of this test: it reported a 12.7x "control regression" that was
	// really two different result sets being differenced.
	//
	// Within an arrangement, both settings run the same SQL over the same rows
	// and return the same count, so the ratio isolates the plan.
	//
	// The arrangements then split into two classes BY THEIR PLANS, not by their
	// timings — which class a measurement lands in must not be decided by the
	// measurement:
	//
	//	plan changed   -> the tie-break chose wrong and statistics fixed it: WIN
	//	plan unchanged -> the tie-break already chose right: CONTROL, must not move
	//
	// Both classes must be non-empty. An all-CONTROL run means the shape stopped
	// being sensitive and the win is unmeasured rather than absent; an all-WIN
	// run means there is no control and the win could be anything.
	type outcome struct {
		name    string
		ratio   float64
		off, on time.Duration
		changed bool
	}
	var wins, controls []outcome
	for _, arr := range arrangements {
		off := results[arr.name][false].best
		on := results[arr.name][true].best
		o := outcome{
			name:    arr.name,
			ratio:   float64(off) / float64(on),
			off:     off,
			on:      on,
			changed: arr.planOff != arr.planOn,
		}
		class := "unchanged (control)"
		if o.changed {
			class = "CHANGED (win)"
		}
		t.Logf("%-8s off=%s on=%s  %.2fx  plan %s",
			o.name, o.off.Round(time.Millisecond), o.on.Round(time.Millisecond), o.ratio, class)
		if o.changed {
			wins = append(wins, o)
		} else {
			controls = append(controls, o)
		}
	}

	if len(wins) == 0 {
		t.Fatalf("statistics changed no plan in either arrangement — every timing above\n" +
			"would be two measurements of the same plan, so this run measures nothing.\n" +
			"Check the gates (freshness, completeness) and that planner_statistics\n" +
			"reached the connection.")
	}
	if len(controls) == 0 {
		t.Fatalf("statistics changed the plan in BOTH arrangements, so there is no control.\n" +
			"The mirrored pair is built so a fixed tie-break drives from the same table in\n" +
			"both — right in one, wrong in the other. If both changed, the OFF plans differ\n" +
			"across the pair, which means something other than collected statistics is\n" +
			"already reading the data.")
	}

	// THE CLAIM. Where statistics changed the plan, the new plan is
	// substantially faster. 2x is a floor set well below what the shape
	// predicts, so ordinary machine noise cannot decide the verdict; the logged
	// ratio is the number worth reading.
	for _, o := range wins {
		if o.ratio < 2.0 {
			t.Errorf("%s: statistics changed the plan but did not make it faster: off=%s on=%s (%.2fx, want >=2x)\n"+
				"  plan OFF: %s\n  plan ON : %s\n"+
				"  A plan change that costs time is worse than no statistics at all.",
				o.name, o.off, o.on, o.ratio,
				planFor(arrangements, o.name, false), planFor(arrangements, o.name, true))
		}
	}

	// THE CONTROL. Where the plan did NOT change, the timings must not either.
	// A gap here would mean the win above is measuring something other than the
	// join order — a warmer cache, an artifact of running ON second.
	for _, o := range controls {
		if o.ratio > 2.0 || o.ratio < 0.5 {
			t.Errorf("%s: the plan did not change but the timing did: off=%s on=%s (%.2fx)\n"+
				"  Identical plans over identical rows should time the same. A gap here means\n"+
				"  the win reported above is not attributable to the join order.",
				o.name, o.off, o.on, o.ratio)
		}
	}
}

// planFor returns an arrangement's EXPLAIN under one setting, so a failure
// message names the plans rather than only the times.
func planFor(arrangements []*statsJoinArrangement, name string, on bool) string {
	for _, a := range arrangements {
		if a.name == name {
			if on {
				return a.planOn
			}
			return a.planOff
		}
	}
	return "(unknown)"
}

// buildStatsJoinArrangement creates the schema, loads it, collects statistics,
// and captures both EXPLAINs. `a` and `b` join on b.a_id = a.id, with an index
// on b.a_id and a's primary key on the other side — so either table can drive,
// and only its row count says which should.
func buildStatsJoinArrangement(t *testing.T, ctx context.Context, arr *statsJoinArrangement) {
	t.Helper()
	h := newStressHarness(t, "statsjoin_"+arr.name)
	h.createSchema(
		"CREATE TABLE a (id BIGINT, v BIGINT, PRIMARY KEY (id))" +
			" CREATE TABLE b (id BIGINT, a_id BIGINT, PRIMARY KEY (id))" +
			" CREATE INDEX b_by_a ON b (a_id)")

	h.bulkInsert("a", arr.aRows, func(i int) string {
		return fmt.Sprintf("(%d, %d)", i, i)
	})
	h.bulkInsert("b", arr.bRows, func(i int) string {
		return fmt.Sprintf("(%d, %d)", i, i%arr.aRows)
	})

	arr.off = openStatsJoinDB(t, h.dbPath, false)
	arr.on = openStatsJoinDB(t, h.dbPath, true)

	// Collect through the ON connection — the same Conn.Raw path `frl stats
	// collect` uses, so this exercises the shipped code rather than a test-only
	// shortcut. Collection happens once and BOTH settings then read the same
	// stored bytes; OFF ignoring statistics that are demonstrably present is
	// what makes the opt-in gate part of this measurement.
	conn, err := arr.on.Conn(ctx)
	if err != nil {
		t.Fatalf("%s: conn: %v", arr.name, err)
	}
	defer conn.Close()
	var report *recordlayer.CollectionReport
	if rawErr := conn.Raw(func(dc any) error {
		ec, ok := dc.(*embedded.EmbeddedConnection)
		if !ok {
			return fmt.Errorf("driver conn is %T, not *embedded.EmbeddedConnection", dc)
		}
		var e error
		report, e = ec.CollectStatistics(ctx, recordlayer.CollectOptions{BatchSize: 5000})
		return e
	}); rawErr != nil {
		t.Fatalf("%s: collect: %v", arr.name, rawErr)
	}
	// Exactness at this scale is the collector's whole premise — a sampled
	// estimate would be within a few percent and that is not what was built.
	if got := report.Collected["A"].Count; got != int64(arr.aRows) {
		t.Fatalf("%s: collected |a|=%d, want %d", arr.name, got, arr.aRows)
	}
	if got := report.Collected["B"].Count; got != int64(arr.bRows) {
		t.Fatalf("%s: collected |b|=%d, want %d", arr.name, got, arr.bRows)
	}
	t.Logf("%-8s loaded a=%d b=%d, scanned %d records",
		arr.name, arr.aRows, arr.bRows, report.RecordsScanned)

	arr.planOff = explainStatsJoin(t, ctx, arr.off)
	arr.planOn = explainStatsJoin(t, ctx, arr.on)
}

func openStatsJoinDB(t *testing.T, dbPath string, useStats bool) *sql.DB {
	t.Helper()
	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=main", dbPath, clusterFilePath)
	if useStats {
		dsn += "&planner_statistics=true"
	}
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("open %q: %v", dsn, err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func explainStatsJoin(t *testing.T, ctx context.Context, db *sql.DB) string {
	t.Helper()
	var plan string
	if err := db.QueryRowContext(ctx,
		"EXPLAIN SELECT a.v, b.id FROM a, b WHERE b.a_id = a.id").Scan(&plan); err != nil {
		t.Fatalf("explain: %v", err)
	}
	return strings.ToUpper(plan)
}

// timeStatsJoin runs the join and drains every row. Draining matters: a lazy
// cursor makes the query return before the join has done its work, and the
// timing would then be of the plan handshake rather than the plan.
func timeStatsJoin(t *testing.T, ctx context.Context, db *sql.DB, query string) (time.Duration, int) {
	t.Helper()
	start := time.Now()
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var v, id int64
		if scanErr := rows.Scan(&v, &id); scanErr != nil {
			t.Fatalf("scan: %v", scanErr)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return time.Since(start), n
}
