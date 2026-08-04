package sqldriver_test

// The planner may only build a scan over an index the store can READ.
//
// This is Java's MetaDataPlanContext.forRootReference filter
// (MetaDataPlanContext.java:194-199), fed by
// PlanContext.Builder.getReadableIndexes (PlanContext.java:236-247) — a filter
// on the INDEX LIST, applied before any match candidate is created. Go carried
// no readable-index view at all until RFC-209 needed one, so a WRITE_ONLY or
// DISABLED index was planned into a scan that then failed at EXECUTION with
// IndexNotReadableError: a query erroring where a correct, slower plan existed.
//
// The tests below set the index state on the real store the SQL driver wrote,
// by writing the same key the maintainer writes
// (subspace[IndexStateSpaceKey][indexName] = Tuple{int64(state)}), and then ask
// the driver to plan. Writing the on-disk contract rather than reconstructing
// the metadata to call MarkIndexWriteOnly is deliberate: the encoding IS what
// the planner reads, so a change to it must break this test.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

// setIndexStateRaw writes an index's state directly into the schema's store,
// exactly as FDBRecordStore.setIndexState does. state READABLE clears the key,
// because READABLE is the default and only the exceptions are stored.
func setIndexStateRaw(t *testing.T, dbPath, schema, indexName string, state recordlayer.IndexState) {
	t.Helper()
	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	ss := subspace.Sub().Sub(tuple.Tuple{dbPath, strings.ToUpper(schema)}).Sub(recordlayer.IndexStateSpaceKey)
	key := ss.Pack(tuple.Tuple{indexName})
	if _, err := rawDB.Transact(func(tr fdb.WritableTransaction) (any, error) {
		if state == recordlayer.IndexStateReadable {
			tr.Clear(key)
			return nil, nil
		}
		tr.Set(key, tuple.Tuple{int64(state)}.Pack())
		return nil, nil
	}); err != nil {
		t.Fatalf("set index state %s=%v: %v", indexName, state, err)
	}
}

// TestFDB_NonReadableIndexIsNotAMatchCandidate pins the gate on both
// non-readable states, on a VALUE index, with the answer checked in every arm.
//
// The two assertions are a pair and neither alone is the property:
//
//   - the plan must stop naming the index (otherwise the gate did nothing), and
//   - the query must still return the right rows (otherwise the gate traded an
//     execution error for a wrong answer, which is worse).
func TestFDB_NonReadableIndexIsNotAMatchCandidate(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_idxread")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_idxread")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE idxread "+
			"CREATE TABLE t (pk BIGINT, c BIGINT, v BIGINT, PRIMARY KEY (pk)) "+
			"CREATE INDEX t_by_c ON t(c)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_idxread/s WITH TEMPLATE idxread")
	dsn := fmt.Sprintf("fdbsql:///testdb_idxread?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (pk,c,v) VALUES (1,10,100),(2,20,200),(3,10,300)")

	// Every statement below runs on ONE pinned connection, and that is
	// load-bearing rather than tidiness. The plan cache lives on the
	// EmbeddedConnection, and database/sql hands out a pooled connection per
	// call — so a test that goes through the pool re-plans almost every time and
	// never exercises the cache at all. Measured: with the readable-index view
	// removed from the plan-cache key, the pooled version of this test still
	// passed, because the stale entry was simply never consulted. Pinning the
	// connection is what makes the cache-key assertion below able to fail.
	//
	// It also matches how the staleness actually arises: setIndexStateRaw writes
	// the state behind the driver's back, so no DDL runs and nothing invalidates
	// the cache. The key is the only thing standing between a withdrawn index
	// and a plan that still scans it.
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("pin connection: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	const q = "SELECT pk, v FROM t WHERE c = 10"
	rowsFor := func(t *testing.T) string {
		t.Helper()
		rows, qErr := conn.QueryContext(ctx, q)
		if qErr != nil {
			t.Fatalf("query: %v", qErr)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var pk, v int64
			if sErr := rows.Scan(&pk, &v); sErr != nil {
				t.Fatalf("scan: %v", sErr)
			}
			out = append(out, fmt.Sprintf("[%d %d]", pk, v))
		}
		if rErr := rows.Err(); rErr != nil {
			t.Fatalf("rows.Err: %v", rErr)
		}
		return strings.Join(out, " ")
	}
	planFor := func(t *testing.T) string {
		t.Helper()
		var plan string
		if pErr := conn.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&plan); pErr != nil {
			t.Fatalf("EXPLAIN: %v", pErr)
		}
		return plan
	}

	const wantRows = "[1 100] [3 300]"

	// Baseline: the index is READABLE and the planner uses it. Without this the
	// later assertions could pass on a query that never had an index plan.
	readablePlan := planFor(t)
	if !strings.Contains(readablePlan, "T_BY_C") {
		t.Fatalf("baseline: a readable index is not being used, so this test cannot "+
			"observe it being withdrawn.\n  plan: %s", readablePlan)
	}
	if got := rowsFor(t); got != wantRows {
		t.Fatalf("baseline rows: got %s, want %s", got, wantRows)
	}

	// These three arms are also what holds the readable-index view OUT of any
	// staleness. The baseline above plans the index in, so by the time an arm
	// withdraws it there is already a cached plan naming it; if the view the
	// planner consults were served from anything that survives the state change,
	// the arm would keep seeing the old plan. Measured: adding a session-scoped
	// memo over fetchReadableIndexes keyed on the METADATA VERSION reddens all
	// three arms below and leaves the "restored" subtest green — index state
	// lives in its own subspace and does not bump md.Version(), so such a memo
	// serves a stale view for the life of the session. Anyone reaching for that
	// memo to buy back the per-query read (measured at ~1.2ms of a ~5.5ms cached
	// SELECT — see fetchReadableIndexes) will be stopped here, by these arms.
	for _, tc := range []struct {
		name  string
		state recordlayer.IndexState
	}{
		{"write-only", recordlayer.IndexStateWriteOnly},
		{"disabled", recordlayer.IndexStateDisabled},
		// READABLE_UNIQUE_PENDING is SCANNABLE — IndexState.isScannable() returns
		// true for it (IndexState.java:94-96) — and is still excluded, because
		// Java gates the planner on isReadable(), which is strictly READABLE
		// (RecordStoreState.java:172-174). The distinction is the whole point of
		// the state: the index is fully built but its uniqueness is unproven, and
		// the planner is entitled to assume uniqueness from a unique index. A
		// gate written against isScannable() would admit it and pass every other
		// arm of this test, so this arm is the only thing holding the strict
		// reading in place.
		{"readable-unique-pending", recordlayer.IndexStateReadableUniquePending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setIndexStateRaw(t, "/testdb_idxread", "s", "T_BY_C", tc.state)
			t.Cleanup(func() {
				setIndexStateRaw(t, "/testdb_idxread", "s", "T_BY_C", recordlayer.IndexStateReadable)
			})

			plan := planFor(t)
			if strings.Contains(plan, "T_BY_C") {
				t.Fatalf("a %v index is still planned into a scan.\n  plan: %s\n"+
					"Java filters the index LIST before creating match candidates "+
					"(MetaDataPlanContext.java:194-199); planning over it here means the query "+
					"fails at execution with IndexNotReadableError where a full scan would "+
					"have answered it correctly.", tc.state, plan)
			}
			if got := rowsFor(t); got != wantRows {
				t.Fatalf("with the index %v the answer changed: got %s, want %s\n"+
					"Withdrawing an index must change the PLAN and nothing else — trading an "+
					"execution error for a wrong answer is strictly worse than the error.",
					tc.state, got, wantRows)
			}
		})
	}

	// Restored: withdrawing an index must be REVERSIBLE. This arm covers the
	// direction the arms above cannot — a plan cached while the index was
	// withdrawn must not outlive the withdrawal.
	//
	// It does NOT cover the other direction, and it used to be credited with
	// doing so. Under a mutation that makes the readable-index view stale (a
	// memo keyed on metadata version), this subtest PASSES: it only ever asks
	// for the index to come back, and a stale view that still says "readable"
	// answers that correctly by accident. The withdrawn arms above are what
	// redden. Keep both — they fail on opposite mutations.
	t.Run("restored", func(t *testing.T) {
		plan := planFor(t)
		if !strings.Contains(plan, "T_BY_C") {
			t.Fatalf("the index is READABLE again but the planner still refuses it.\n  plan: %s\n"+
				"A plan cached while it was withdrawn is being served past the restore: the "+
				"readable-index view is part of the plan-cache key precisely so that cannot happen.", plan)
		}
		if got := rowsFor(t); got != wantRows {
			t.Fatalf("restored rows: got %s, want %s", got, wantRows)
		}
	})
}

// TestFDB_NonReadableAggregateIndexFallsBackToStreamingAggregation is the same
// gate on an AGGREGATE index, which is the shape RFC-209 §6 depends on: "a
// companion that is not readable must not be used", because an index in
// WRITE_ONLY or mid-backfill has a PARTIAL key set, and driving a group-existence
// merge from a partial key set would drop LIVE groups — a brand-new wrong answer,
// strictly worse than the phantom the RFC removes.
//
// Pinning it on the aggregate family separately from the value family is not
// duplication: they take different candidate builders
// (tryAggregateIndexCandidate vs the value-index defs), and a filter applied to
// one list and not the other would pass the value test while leaving the
// aggregate hole wide open.
func TestFDB_NonReadableAggregateIndexFallsBackToStreamingAggregation(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_idxreadagg")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_idxreadagg")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE idxreadagg "+
			"CREATE TABLE t (pk BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (pk)) "+
			"CREATE INDEX t_cnt_g AS SELECT COUNT(*) FROM t GROUP BY g")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_idxreadagg/s WITH TEMPLATE idxreadagg")
	dsn := fmt.Sprintf("fdbsql:///testdb_idxreadagg?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (pk,g,v) VALUES (1,1,10),(2,1,20),(3,2,30)")

	const q = "SELECT g, COUNT(*) FROM t GROUP BY g"
	const wantRows = "[1 2] [2 1]"
	rowsFor := func(t *testing.T) string {
		t.Helper()
		rows, qErr := db.QueryContext(ctx, q)
		if qErr != nil {
			t.Fatalf("query: %v", qErr)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var g, n int64
			if sErr := rows.Scan(&g, &n); sErr != nil {
				t.Fatalf("scan: %v", sErr)
			}
			out = append(out, fmt.Sprintf("[%d %d]", g, n))
		}
		if rErr := rows.Err(); rErr != nil {
			t.Fatalf("rows.Err: %v", rErr)
		}
		return strings.Join(out, " ")
	}
	planFor := func(t *testing.T) string {
		t.Helper()
		var plan string
		if pErr := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&plan); pErr != nil {
			t.Fatalf("EXPLAIN: %v", pErr)
		}
		return plan
	}

	if plan := planFor(t); !strings.Contains(plan, "AggregateIndex") {
		t.Fatalf("baseline: the aggregate index is not being used, so this test cannot "+
			"observe it being withdrawn.\n  plan: %s", plan)
	}
	if got := rowsFor(t); got != wantRows {
		t.Fatalf("baseline rows: got %s, want %s", got, wantRows)
	}

	setIndexStateRaw(t, "/testdb_idxreadagg", "s", "T_CNT_G", recordlayer.IndexStateWriteOnly)
	t.Cleanup(func() {
		setIndexStateRaw(t, "/testdb_idxreadagg", "s", "T_CNT_G", recordlayer.IndexStateReadable)
	})

	plan := planFor(t)
	if strings.Contains(plan, "AggregateIndex") {
		t.Fatalf("a WRITE_ONLY aggregate index is still planned into a scan.\n  plan: %s\n"+
			"A WRITE_ONLY aggregate index has a PARTIAL key set: mid-backfill it holds some "+
			"groups and not others. Serving a GROUP BY from it returns a subset of the "+
			"groups with no indication that it did.", plan)
	}
	if got := rowsFor(t); got != wantRows {
		t.Fatalf("with the aggregate index WRITE_ONLY the answer changed: got %s, want %s\n"+
			"The fallback is a streaming aggregation over base rows and must be exact.",
			got, wantRows)
	}
}
