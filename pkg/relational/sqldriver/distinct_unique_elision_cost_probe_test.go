package sqldriver_test

// MEASUREMENT PROBE for the secondary-UNIQUE DISTINCT-elision RFC.
//
// distinctEliminatedByUniqueKey (rule_implement_distinct_final.go) declines to
// admit a SECONDARY UNIQUE index as a proof that a DISTINCT is redundant. This
// probe measures what that decline costs on a real store: the plan shape with
// and without the DISTINCT keyword, the wall-clock delta, and whether the
// DISTINCT blocks anything structurally (covering-index-only access, LIMIT
// pushdown / early termination).

import (
	"context"
	"database/sql"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/core/metadata"
)

const duecRows = 100000

func TestFDB_DistinctUniqueElisionCostProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_duec")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_duec")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE duec "+
			"CREATE TABLE users (id BIGINT, email STRING, payload STRING, PRIMARY KEY (id)) "+
			"CREATE UNIQUE INDEX by_email ON users (email)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_duec/s WITH TEMPLATE duec")
	dsn := fmt.Sprintf("fdbsql:///testdb_duec?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(16)

	// ---- load ----------------------------------------------------------
	const batch = 250
	const workers = 8
	loadStart := time.Now()
	var wg sync.WaitGroup
	errCh := make(chan error, workers)
	per := duecRows / workers
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			lo := w * per
			for base := lo; base < lo+per; base += batch {
				var sb strings.Builder
				sb.WriteString("INSERT INTO users (id, email, payload) VALUES ")
				for i := 0; i < batch; i++ {
					id := base + i
					if i > 0 {
						sb.WriteString(",")
					}
					fmt.Fprintf(&sb, "(%d, 'user%07d@example.com', 'pad-%07d-xxxxxxxxxxxxxxxxxxxx')", id, id, id)
				}
				if _, e := db.ExecContext(ctx, sb.String()); e != nil {
					errCh <- fmt.Errorf("insert batch at %d: %w", base, e)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Fatalf("load: %v", e)
	}
	loadDur := time.Since(loadStart)

	var loaded int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&loaded); err != nil {
		t.Fatalf("count: %v", err)
	}
	t.Logf("LOAD: %d rows in %v (%.0f rows/s); COUNT(*) = %d",
		duecRows, loadDur.Round(time.Millisecond),
		float64(duecRows)/loadDur.Seconds(), loaded)

	// ---- EXPLAIN -------------------------------------------------------
	queries := []struct{ name, sql string }{
		{"distinct", "SELECT DISTINCT email FROM users"},
		{"no_distinct", "SELECT email FROM users"},
		{"distinct_order", "SELECT DISTINCT email FROM users ORDER BY email"},
		{"no_distinct_order", "SELECT email FROM users ORDER BY email"},
		{"distinct_order_limit", "SELECT DISTINCT email FROM users ORDER BY email LIMIT 10"},
		{"no_distinct_order_limit", "SELECT email FROM users ORDER BY email LIMIT 10"},
		{"distinct_limit", "SELECT DISTINCT email FROM users LIMIT 10"},
		{"no_distinct_limit", "SELECT email FROM users LIMIT 10"},
	}
	explains := make(map[string]string, len(queries))
	for _, q := range queries {
		explains[q.name] = explainPlan(t, ctx, db, q.sql)
		t.Logf("EXPLAIN %-24s %s\n    => %s", q.name, q.sql, explains[q.name])
	}

	// The measurements below are only interpretable against the plan shapes they
	// were taken on, so those shapes are ASSERTED rather than logged. Each
	// assertion is a fact RFC-210 rests on, and each failure message names what
	// changed.
	if !strings.Contains(explains["distinct"], "Distinct(") {
		t.Fatalf("SELECT DISTINCT over a secondary-UNIQUE column no longer carries a "+
			"physical distinct: %s\nThe secondary-UNIQUE elision decline "+
			"(distinctEliminatedByUniqueKey) has been lifted. This probe must be "+
			"updated to assert the ELIDED shape and to enforce RFC-210's perf "+
			"criterion, not deleted.", explains["distinct"])
	}
	// The elision's output shape. That it is a base-record Scan and NOT
	// IndexScan(BY_EMAIL) is the refutation of "the unique index would answer the
	// query directly" — measured at 0.33s here versus 6.95s for the index path.
	if strings.Contains(explains["no_distinct"], "Distinct(") ||
		!strings.Contains(explains["no_distinct"], "Scan(USERS)") ||
		strings.Contains(explains["no_distinct"], "IndexScan") {
		t.Fatalf("the no-DISTINCT shape is no longer a plain base-record scan: %s\n"+
			"RFC-210 §2.1's cost comparison compares the DISTINCT plan against THIS "+
			"plan; if the access path moved, the comparison measures something else.",
			explains["no_distinct"])
	}
	if !strings.Contains(explains["distinct_order"], "IndexScan(BY_EMAIL") {
		t.Fatalf("ORDER BY email no longer reaches BY_EMAIL: %s", explains["distinct_order"])
	}

	// ---- timing --------------------------------------------------------
	// run executes q, returning the row count, the wall clock, the bytes this
	// goroutine's execution cumulatively allocated (TotalAlloc delta — the
	// hash-set churn shows up here), and the peak process HeapInuse sampled
	// every 2ms while the query runs.
	run := func(q string) (int, time.Duration, uint64, uint64) {
		runtime.GC()
		var m0 runtime.MemStats
		runtime.ReadMemStats(&m0)
		var peak uint64
		stop := make(chan struct{})
		var sampWG sync.WaitGroup
		sampWG.Add(1)
		go func() {
			defer sampWG.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				var ms runtime.MemStats
				runtime.ReadMemStats(&ms)
				if ms.HeapInuse > peak {
					peak = ms.HeapInuse
				}
				time.Sleep(2 * time.Millisecond)
			}
		}()
		start := time.Now()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		n := 0
		var s sql.NullString
		for rows.Next() {
			if err := rows.Scan(&s); err != nil {
				t.Fatalf("scan: %v", err)
			}
			n++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err %q: %v", q, err)
		}
		rows.Close()
		d := time.Since(start)
		close(stop)
		sampWG.Wait()
		var m1 runtime.MemStats
		runtime.ReadMemStats(&m1)
		return n, d, m1.TotalAlloc - m0.TotalAlloc, peak
	}

	const reps = 7
	type result struct {
		name     string
		n        int
		durs     []time.Duration
		median   time.Duration
		medAlloc uint64
		medPeak  uint64
	}
	var results []result
	for _, q := range queries {
		r := result{name: q.name}
		var allocs, peaks []uint64
		for i := 0; i < reps; i++ {
			n, d, a, p := run(q.sql)
			r.n = n
			r.durs = append(r.durs, d)
			allocs = append(allocs, a)
			peaks = append(peaks, p)
		}
		sorted := append([]time.Duration(nil), r.durs...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		r.median = sorted[len(sorted)/2]
		sort.Slice(allocs, func(i, j int) bool { return allocs[i] < allocs[j] })
		sort.Slice(peaks, func(i, j int) bool { return peaks[i] < peaks[j] })
		r.medAlloc = allocs[len(allocs)/2]
		r.medPeak = peaks[len(peaks)/2]
		results = append(results, r)
	}
	for _, r := range results {
		t.Logf("TIMING %-24s rows=%-7d median=%-12v medAllocMiB=%-8.1f medPeakHeapMiB=%-8.1f all=%v",
			r.name, r.n, r.median.Round(time.Millisecond),
			float64(r.medAlloc)/(1<<20), float64(r.medPeak)/(1<<20), r.durs)
	}

	// ---- paged (multi-page) cost ---------------------------------------
	// The unbounded runs above finish inside one page, so they never charge
	// the hash-distinct's continuation: executeHashDistinct serializes EVERY
	// seen key into DistinctHashContinuation and rebuilds the set from it on
	// resume. Forcing a scanned-rows limit makes the 100k query span ~10 pages
	// and exposes that O(distinct-values) per-page cost.
	const pageScanLimit = 10000
	pconn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, int64(pageScanLimit)).Build())
	})
	runPaged := func(q string) (int, time.Duration, uint64) {
		runtime.GC()
		var m0 runtime.MemStats
		runtime.ReadMemStats(&m0)
		start := time.Now()
		rows, err := pconn.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("paged query %q: %v", q, err)
		}
		n := 0
		var s sql.NullString
		for rows.Next() {
			if err := rows.Scan(&s); err != nil {
				t.Fatalf("scan: %v", err)
			}
			n++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("paged rows.Err %q: %v", q, err)
		}
		rows.Close()
		d := time.Since(start)
		var m1 runtime.MemStats
		runtime.ReadMemStats(&m1)
		return n, d, m1.TotalAlloc - m0.TotalAlloc
	}
	for _, q := range []struct{ name, sql string }{
		{"paged_distinct", "SELECT DISTINCT email FROM users"},
		{"paged_no_distinct", "SELECT email FROM users"},
	} {
		var ds []time.Duration
		var as []uint64
		var n int
		for i := 0; i < 5; i++ {
			nn, d, a := runPaged(q.sql)
			n = nn
			ds = append(ds, d)
			as = append(as, a)
		}
		sorted := append([]time.Duration(nil), ds...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		sort.Slice(as, func(i, j int) bool { return as[i] < as[j] })
		t.Logf("PAGED(scanLimit=%d) %-20s rows=%-7d median=%-12v medAllocMiB=%-8.1f all=%v",
			pageScanLimit, q.name, n, sorted[len(sorted)/2].Round(time.Millisecond),
			float64(as[len(as)/2])/(1<<20), ds)
	}

	// ---- executor variant ---------------------------------------------
	// Same shape as the SQL schema above, built through the metadata builder so
	// the planned plan object is reachable and RecordQueryDistinctPlan.Streaming
	// can be read (EXPLAIN deliberately does not render it).
	cols := []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("EMAIL", api.NewStringType(false), 2),
		metadata.NewColumnSpec("PAYLOAD", api.NewStringType(false), 3),
	}
	b := metadata.NewSchemaTemplateBuilder().SetName("duec")
	b.AddTable("USERS", cols, []string{"ID"})
	b.AddIndex("USERS", "BY_EMAIL", []string{"EMAIL"}, true)
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("build metadata: %v", err)
	}
	md := tmpl.Underlying()
	if idx := md.GetIndex("BY_EMAIL"); idx == nil || !idx.IsUnique() {
		t.Fatal("BY_EMAIL missing or not unique")
	}
	for _, q := range queries {
		p, e := embedded.PlanRecordQueryWithMetadata(q.sql, md, nil)
		if e != nil {
			t.Logf("VARIANT %-24s plan error: %v", q.name, e)
			continue
		}
		found := false
		streaming := false
		plans.Walk(p, func(node plans.RecordQueryPlan) bool {
			if d, ok := node.(*plans.RecordQueryDistinctPlan); ok {
				found = true
				streaming = d.Streaming
			}
			return true
		})
		t.Logf("VARIANT %-24s distinctPresent=%v streaming=%v plan=%s", q.name, found, streaming, p.Explain())
		// Which executor variant fires decides which cost the decline actually
		// pays, and EXPLAIN does not render it — so it is asserted here. The
		// unordered shape gets the HASH set, whose seen-key set is serialized into
		// every continuation page (executeHashDistinct); that is where RFC-210
		// §2.1's +72%/+267MiB paged figure comes from. The ordered shape gets the
		// streaming variant, which carries no seen-key set — and is why that RFC
		// sets no timing criterion on the ordered shape.
		switch q.name {
		case "distinct":
			if !found || streaming {
				t.Fatalf("unordered SELECT DISTINCT: want the hash variant "+
					"(distinctPresent=true, streaming=false), got present=%v streaming=%v",
					found, streaming)
			}
		case "distinct_order":
			if !found || !streaming {
				t.Fatalf("ordered SELECT DISTINCT: want the streaming variant "+
					"(distinctPresent=true, streaming=true), got present=%v streaming=%v",
					found, streaming)
			}
		case "no_distinct", "no_distinct_order":
			if found {
				t.Fatalf("%s should carry no distinct operator at all", q.name)
			}
		}
	}
}
