package sqldriver_test

// End-to-end, real-FDB benchmarks of the IN-list plans over a secondary index
// ordered by the indexed column.
//
// BenchmarkFDB_InUnionMergeSort_* is the companion to
// pkg/recordlayer/query/executor.BenchmarkMergeSortCursor_HeapVsLinear: the
// InUnion merge driven through the full driver stack. A sorted InJoin
// satisfies the same order and the planner prefers it, so ImplementInJoinRule
// is disabled to keep the merge under measurement.
//
// BenchmarkFDB_InFetch_* compares the two plans that deliver
// `SELECT * ... WHERE g IN (...) ORDER BY g` over a non-covering index:
// Fetch(InJoin(Covering)), which Go elects, and the IN-union Java elects
// because it does not push a comparand InJoin through a fetch (DIVERGENCES.md,
// RFC-191); Go plans that union as InUnion(IndexScan), the same reads as
// Java's Fetch(InUnion(Covering)). Go's choice holds only while the InJoin is
// at least as fast at every N.

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

type inBench struct {
	numLegs, rowsPerLeg int
	query               string
	wantPlan            string
	disabled            []string
}

// runInBench seeds a g-bucketed table (numLegs buckets, rowsPerLeg rows each)
// with a secondary index on g, confirms the plan once outside the timed loop,
// then times the query and reports rows/sec.
func runInBench(b *testing.B, ib inBench) {
	b.Helper()
	if clusterFilePath == "" {
		b.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	seq := benchSeq.Add(1)
	dbPath := fmt.Sprintf("/bench_inunion_%d", seq)
	tmpl := fmt.Sprintf("bench_inunion_tmpl_%d", seq)

	setup := openBenchDB(b, dbPath)
	execOrFail(b, setup, ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath))
	execOrFail(b, setup, ctx,
		fmt.Sprintf("CREATE SCHEMA TEMPLATE %s "+
			"CREATE TABLE T (id BIGINT, g BIGINT, p STRING, PRIMARY KEY (id)) "+
			"CREATE INDEX t_g ON T (g)", tmpl))
	execOrFail(b, setup, ctx,
		fmt.Sprintf("CREATE SCHEMA %s/store WITH TEMPLATE %s", dbPath, tmpl))

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=STORE", strings.ToUpper(dbPath), clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		b.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	// One batched INSERT so setup cost doesn't scale with round trips.
	var values strings.Builder
	id := int64(1)
	for g := 1; g <= ib.numLegs; g++ {
		for r := 0; r < ib.rowsPerLeg; r++ {
			if id > 1 {
				values.WriteByte(',')
			}
			fmt.Fprintf(&values, "(%d,%d,'payload-%d')", id, g, id)
			id++
		}
	}
	execOrFail(b, db, ctx, "INSERT INTO T (id, g, p) VALUES "+values.String())

	conn, err := db.Conn(ctx)
	if err != nil {
		b.Fatalf("db.Conn: %v", err)
	}
	defer conn.Close()
	if len(ib.disabled) > 0 {
		if err := conn.Raw(func(dc any) error {
			ec, ok := dc.(*embedded.EmbeddedConnection)
			if !ok {
				return fmt.Errorf("driver conn is %T", dc)
			}
			ec.SetOptions(api.NewOptionsBuilder().Set(api.OptDisabledPlannerRules, ib.disabled).Build())
			return nil
		}); err != nil {
			b.Fatalf("disable rules: %v", err)
		}
	}

	inList := make([]string, ib.numLegs)
	for g := 1; g <= ib.numLegs; g++ {
		inList[g-1] = strconv.Itoa(g)
	}
	query := fmt.Sprintf(ib.query, strings.Join(inList, ","))

	// A plan that silently fell back to another shape would make the
	// measurement meaningless.
	var plan string
	if err := conn.QueryRowContext(ctx, "EXPLAIN "+query).Scan(&plan); err != nil {
		b.Fatalf("EXPLAIN: %v", err)
	}
	if !strings.Contains(plan, ib.wantPlan) || strings.Contains(plan, "InMemorySort") {
		b.Fatalf("query did not plan as %s, got:\n%s", ib.wantPlan, plan)
	}

	totalRows := int64(ib.numLegs * ib.rowsPerLeg)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := conn.QueryContext(ctx, query)
		if err != nil {
			b.Fatalf("iteration %d: %v", i, err)
		}
		cols, err := rows.Columns()
		if err != nil {
			b.Fatalf("columns: %v", err)
		}
		dest := make([]any, len(cols))
		for j := range dest {
			dest[j] = new(any)
		}
		var n int64
		for rows.Next() {
			if err := rows.Scan(dest...); err != nil {
				b.Fatalf("scan: %v", err)
			}
			n++
		}
		rows.Close()
		if n != totalRows {
			b.Fatalf("iteration %d: got %d rows, want %d", i, n, totalRows)
		}
	}
	b.StopTimer()
	if secs := b.Elapsed().Seconds(); secs > 0 {
		b.ReportMetric(float64(b.N)*float64(totalRows)/secs, "rows/sec")
	}
}

func benchInUnionMergeSort(b *testing.B, numLegs, rowsPerLeg int) {
	runInBench(b, inBench{
		numLegs: numLegs, rowsPerLeg: rowsPerLeg,
		query:    "SELECT id, g FROM T WHERE g IN (%s) ORDER BY g, id",
		wantPlan: "InUnion(",
		disabled: []string{"ImplementInJoinRule"},
	})
}

// BenchmarkFDB_InUnionMergeSort_N3 through _N1000 measure the InUnion merge
// at 3, 10, 100 and 1000 IN-list values, 5 rows per leg.
func BenchmarkFDB_InUnionMergeSort_N3(b *testing.B)    { benchInUnionMergeSort(b, 3, 5) }
func BenchmarkFDB_InUnionMergeSort_N10(b *testing.B)   { benchInUnionMergeSort(b, 10, 5) }
func BenchmarkFDB_InUnionMergeSort_N100(b *testing.B)  { benchInUnionMergeSort(b, 100, 5) }
func BenchmarkFDB_InUnionMergeSort_N1000(b *testing.B) { benchInUnionMergeSort(b, 1000, 5) }

func benchInFetch(b *testing.B, numLegs int, inJoin bool) {
	ib := inBench{
		numLegs: numLegs, rowsPerLeg: 5,
		query:    "SELECT * FROM T WHERE g IN (%s) ORDER BY g",
		wantPlan: "Fetch(InJoin(IndexScan(T_G, [=] COVERING)",
	}
	if !inJoin {
		ib.wantPlan = "InUnion(IndexScan(T_G, [=])"
		ib.disabled = []string{"ImplementInJoinRule"}
	}
	runInBench(b, ib)
}

func BenchmarkFDB_InFetch_InJoin_N3(b *testing.B)    { benchInFetch(b, 3, true) }
func BenchmarkFDB_InFetch_InUnion_N3(b *testing.B)   { benchInFetch(b, 3, false) }
func BenchmarkFDB_InFetch_InJoin_N10(b *testing.B)   { benchInFetch(b, 10, true) }
func BenchmarkFDB_InFetch_InUnion_N10(b *testing.B)  { benchInFetch(b, 10, false) }
func BenchmarkFDB_InFetch_InJoin_N100(b *testing.B)  { benchInFetch(b, 100, true) }
func BenchmarkFDB_InFetch_InUnion_N100(b *testing.B) { benchInFetch(b, 100, false) }
