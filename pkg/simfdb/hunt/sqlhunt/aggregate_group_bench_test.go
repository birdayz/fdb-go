package sqlhunt

import (
	"context"
	"database/sql"
	"fmt"
	"sync/atomic"
	"testing"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/sqldriver"
	"fdb.dev/pkg/simfdb"
)

// The aggregate-index GROUP BY path over SimFDB, so a PER-GROUP cost question can
// be asked without the 1M stress suite's two and a half minutes of container
// ingest. It is the same planner, the same cursors and the same Value evaluation a
// container run uses; only the storage engine and the network are gone.
//
// It is written against the SQL surface for the same reason scan_bench_test.go is:
// a per-group regression is a RATIO between two trees, and a benchmark that only
// compiles on one of them cannot state one.

var aggKeyCounter atomic.Uint64

const (
	aggBenchGroups   = 2_000
	aggBenchPerGroup = 10
)

// aggBenchHarness builds a schema with a grouped SUM index and its COUNT(*)
// companion — the pair the aggregate-index match needs before it will answer a
// grouped SUM at all (a grouped SUM index cannot decide group existence on its
// own) — and fills it with aggBenchGroups groups of aggBenchPerGroup rows.
func aggBenchHarness(b *testing.B) *sql.DB {
	b.Helper()

	env := dst.NewSim(uint64(aggBenchGroups))
	env.Buggify = dst.DisabledBuggifier()
	simDB := recordlayer.NewFDBDatabaseWithBackend(simfdb.New(env)).SetEnv(env)
	simDB.SetStoreStateCache(recordlayer.NewMetaDataVersionStampStoreStateCache())

	key := fmt.Sprintf("sim://aggbench/%d", aggKeyCounter.Add(1))
	b.Cleanup(sqldriver.RegisterBackend(key, simDB))

	setup, err := sql.Open("fdbsql", "fdbsql://"+qcDBPath+"?cluster_file="+key)
	if err != nil {
		b.Fatalf("open setup: %v", err)
	}
	setup.SetMaxOpenConns(1)
	b.Cleanup(func() { setup.Close() })

	ctx := context.Background()
	for _, stmt := range []string{
		"CREATE DATABASE " + qcDBPath,
		"CREATE SCHEMA TEMPLATE aggtmpl " +
			"CREATE TABLE orders (id BIGINT, customer_id BIGINT, amount BIGINT, PRIMARY KEY (id)) " +
			"CREATE INDEX sum_amount_by_customer AS SELECT SUM(amount) FROM orders GROUP BY customer_id " +
			"CREATE INDEX count_by_customer AS SELECT COUNT(*) FROM orders GROUP BY customer_id",
		"CREATE SCHEMA " + qcDBPath + "/s WITH TEMPLATE aggtmpl",
	} {
		if _, err := setup.ExecContext(ctx, stmt); err != nil {
			b.Fatalf("setup %q: %v", stmt, err)
		}
	}

	db, err := sql.Open("fdbsql", "fdbsql://"+qcDBPath+"?cluster_file="+key+"&schema=s")
	if err != nil {
		b.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	b.Cleanup(func() { db.Close() })

	const perTx = 250
	id := 0
	for start := 0; start < aggBenchGroups*aggBenchPerGroup; start += perTx {
		stmt := "INSERT INTO orders VALUES "
		for i := 0; i < perTx && start+i < aggBenchGroups*aggBenchPerGroup; i++ {
			if i > 0 {
				stmt += ","
			}
			stmt += fmt.Sprintf("(%d,%d,%d)", id, id%aggBenchGroups, 1+id%97)
			id++
		}
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			b.Fatalf("insert at %d: %v", start, err)
		}
	}
	return db
}

func benchAggGroups(b *testing.B, query string, wantRows int) {
	db := aggBenchHarness(b)
	if got := drain(b, db, query); got != wantRows {
		b.Fatalf("warmup returned %d rows, want %d — the benchmark is not measuring "+
			"the shape it names", got, wantRows)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := drain(b, db, query); got != wantRows {
			b.Fatalf("iteration %d returned %d rows, want %d", i, got, wantRows)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(wantRows), "groups/op")
}

// BenchmarkAggregateGroupsHaving is the stress suite's "GROUP BY customer
// HAVING" — every group read out of the merged aggregate indexes and filtered.
func BenchmarkAggregateGroupsHaving(b *testing.B) {
	// Every amount is at least 1, so every group SUMs above the threshold and the
	// filter keeps ALL of them. That is deliberate: it makes this a per-GROUP
	// instrument rather than a filter-selectivity one, and the row-count check in
	// benchAggGroups fails loudly if the data ever stops satisfying it.
	benchAggGroups(b,
		"SELECT customer_id, SUM(amount) FROM orders GROUP BY customer_id "+
			"HAVING SUM(amount) > 0 ORDER BY customer_id",
		aggBenchGroups)
}

// BenchmarkAggregateGroupsPlain is the same read without the HAVING filter, so
// the filter's own contribution can be separated from the merge's.
func BenchmarkAggregateGroupsPlain(b *testing.B) {
	benchAggGroups(b,
		"SELECT customer_id, SUM(amount) FROM orders GROUP BY customer_id ORDER BY customer_id",
		aggBenchGroups)
}
