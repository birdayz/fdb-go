package sqldriver_test

// End-to-end coverage for RFC-208's runtime physical range sets. These cases
// deliberately constrain a suffix after one or more float equalities: a
// terminal signed-zero widening or a residual-only/base-scan answer would not
// exercise the changed contract.

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/core/metadata"
)

func TestFDB_RuntimeRangeSetLimitThroughFilterAndDistinct(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	const path = "/testdb_range_budget"
	setup := openTestDB(t, path)
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE "+path)
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE range_budget "+
		"CREATE TABLE t (id BIGINT, v DOUBLE, w BIGINT, payload STRING, PRIMARY KEY (id)) "+
		"CREATE INDEX rb_vw ON t (v, w)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA "+path+"/s WITH TEMPLATE range_budget")
	t.Cleanup(func() {
		for _, ddl := range []string{"DROP SCHEMA " + path + "/s", "DROP SCHEMA TEMPLATE range_budget", "DROP DATABASE " + path} {
			if _, err := setup.ExecContext(ctx, ddl); err != nil {
				t.Errorf("cleanup %s: %v", ddl, err)
			}
		}
	})
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", path, clusterFilePath))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// The first physical sign branch starts with three rows that the filter
	// rejects, or that DISTINCT collapses. A result cap of three is not a safe
	// raw-row cap below either operator. The suffix equality forces range-set
	// enumeration of both signs, rather than terminal-zero widening.
	mwjoMustExec(t, db, ctx, "INSERT INTO t VALUES "+
		"(1,-0.0,5,'a'),(2,-0.0,5,'a'),(3,-0.0,5,'a'),"+
		"(4,-0.0,5,'b'),(5,0.0,5,'b'),(6,0.0,5,'c')")
	for _, tc := range []struct {
		name, query, operator, want string
	}{
		{"filter", "SELECT id FROM t WHERE v = 0 AND w = 5 AND payload <> 'a' LIMIT 3", "PredicatesFilter(", "[4 5 6]"},
		{"distinct", "SELECT DISTINCT payload FROM t WHERE v = 0 AND w = 5 LIMIT 3", "Distinct(", "[a b c]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan := planExplainVia(t, ctx, db, tc.query)
			for _, part := range []string{"Limit(3", tc.operator, "IndexScan(RB_VW", "[=, =]"} {
				if !strings.Contains(plan, part) {
					t.Fatalf("plan %s does not exercise %s above the range set (missing %q)", plan, tc.operator, part)
				}
			}
			t.Logf("range-set limit plan: %s", plan)
			rows, err := db.QueryContext(ctx, tc.query)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var got []string
			for rows.Next() {
				var value string
				if err := rows.Scan(&value); err != nil {
					t.Fatal(err)
				}
				got = append(got, value)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			sort.Strings(got)
			if fmt.Sprint(got) != tc.want {
				t.Fatalf("bounded %s returned %v, want %s", tc.operator, got, tc.want)
			}
		})
	}
}

func TestFDB_RuntimeSignedZeroRangeSetAccessPaths(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_rszr")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_rszr")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE rszr "+
		"CREATE TABLE d (id BIGINT, v DOUBLE, w BIGINT, payload STRING, PRIMARY KEY (id)) "+
		"CREATE INDEX d_vw ON d (v, w) "+
		"CREATE TABLE f (id BIGINT, v FLOAT, w BIGINT, payload STRING, PRIMARY KEY (id)) "+
		"CREATE INDEX f_vw ON f (v, w) "+
		"CREATE TABLE m (id BIGINT, v1 DOUBLE, v2 FLOAT, w BIGINT, payload STRING, PRIMARY KEY (id)) "+
		"CREATE INDEX m_v1v2w ON m (v1, v2, w) "+
		"CREATE TABLE pfx (id BIGINT, g BIGINT, v DOUBLE, w BIGINT, PRIMARY KEY (id)) "+
		"CREATE INDEX pfx_gvw ON pfx (g, v, w) "+
		"CREATE TABLE pkd (v DOUBLE, w BIGINT, id BIGINT, payload STRING, PRIMARY KEY (v, w, id)) "+
		"CREATE TABLE u (id BIGINT, v DOUBLE, w BIGINT, PRIMARY KEY (id)) "+
		"CREATE UNIQUE INDEX u_vw ON u (v, w) "+
		"CREATE TABLE o (id BIGINT, kd DOUBLE, kf FLOAT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_rszr/s WITH TEMPLATE rszr")
	dsn := fmt.Sprintf("fdbsql:///testdb_rszr?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// DOUBLE and FLOAT corpora each contain both exact target signs, the two
	// broad-interval flank rows, an ordinary nonzero, and two NULL suffixes.
	mwjoMustExec(t, db, ctx, "INSERT INTO d (id,v,w,payload) VALUES "+
		"(1,-0.0,5,'d-neg'),(2,0.0,5,'d-pos'),(3,-0.0,9,'d-high'),"+
		"(4,0.0,1,'d-low'),(5,7.0,5,'d-seven'),"+
		"(6,-0.0,NULL,'d-neg-null'),(7,0.0,NULL,'d-pos-null')")
	mwjoMustExec(t, db, ctx, "INSERT INTO f (id,v,w,payload) VALUES "+
		"(101,-0.0,5,'f-neg'),(102,0.0,5,'f-pos'),(103,-0.0,9,'f-high'),"+
		"(104,0.0,1,'f-low'),(105,7.0,5,'f-seven')")

	// One row for every physical sign choice of (DOUBLE,FLOAT), plus flanks.
	// IDs follow tuple order: (--), (-+), (+-), (++).
	mwjoMustExec(t, db, ctx, "INSERT INTO m (id,v1,v2,w,payload) VALUES "+
		"(201,-0.0,-0.0,5,'mm'),(202,-0.0,0.0,5,'mp'),"+
		"(203,0.0,-0.0,5,'pm'),(204,0.0,0.0,5,'pp'),"+
		"(205,-0.0,-0.0,9,'high'),(206,0.0,0.0,1,'low')")
	mwjoMustExec(t, db, ctx, "INSERT INTO pfx (id,g,v,w) VALUES "+
		"(211,1,-0.0,5),(212,1,0.0,5),(213,1,-0.0,9),(214,1,0.0,1),"+
		"(215,2,-0.0,5),(216,2,0.0,5)")

	mwjoMustExec(t, db, ctx, "INSERT INTO pkd (v,w,id,payload) VALUES "+
		"(-0.0,5,301,'pk-neg'),(0.0,5,302,'pk-pos'),"+
		"(-0.0,9,303,'pk-high'),(0.0,1,304,'pk-low')")
	mwjoMustExec(t, db, ctx, "INSERT INTO u (id,v,w) VALUES (401,-0.0,5),(402,0.0,5),(403,7.0,5)")
	mwjoMustExec(t, db, ctx, "INSERT INTO o (id,kd,kf) VALUES "+
		"(10,0.0,-0.0),(11,-0.0,0.0),(20,7.0,7.0),(30,NULL,NULL)")

	idsDB := func(t *testing.T, q string, args ...any) []int64 {
		t.Helper()
		rows, queryErr := db.QueryContext(ctx, q, args...)
		if queryErr != nil {
			t.Fatalf("query %q: %v", q, queryErr)
		}
		defer func() { _ = rows.Close() }()
		var out []int64
		for rows.Next() {
			var id int64
			if scanErr := rows.Scan(&id); scanErr != nil {
				t.Fatalf("scan: %v", scanErr)
			}
			out = append(out, id)
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			t.Fatalf("rows: %v", rowsErr)
		}
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out
	}
	assertIDs := func(t *testing.T, q string, want []int64, args ...any) {
		t.Helper()
		if got := idsDB(t, q, args...); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("%s args=%v = %v, want %v", q, args, got, want)
		}
	}

	for _, tc := range []struct {
		name string
		q    string
		args []any
		want []int64
	}{
		{"double_param_positive", "SELECT id FROM d WHERE v = ? AND w = 5", []any{0.0}, []int64{1, 2}},
		{"double_param_negative", "SELECT id FROM d WHERE v = ? AND w = 5", []any{math.Copysign(0, -1)}, []int64{1, 2}},
		{"float_param_positive", "SELECT id FROM f WHERE v = ? AND w = 5", []any{0.0}, []int64{101, 102}},
		{"float_param_negative", "SELECT id FROM f WHERE v = ? AND w = 5", []any{math.Copysign(0, -1)}, []int64{101, 102}},
		{"float_coalesce_param", "SELECT id FROM f WHERE v = COALESCE(?, 7.0) AND w = 5", []any{0.0}, []int64{101, 102}},
		{"double_suffix_inequality", "SELECT id FROM d WHERE v = ? AND w >= 5", []any{0.0}, []int64{1, 2, 3}},
		{"double_suffix_is_null", "SELECT id FROM d WHERE v = ? AND w IS NULL", []any{math.Copysign(0, -1)}, []int64{6, 7}},
		{"four_sign_branches_positive", "SELECT id FROM m WHERE v1 = ? AND v2 = ? AND w = 5", []any{0.0, 0.0}, []int64{201, 202, 203, 204}},
		{"four_sign_branches_negative", "SELECT id FROM m WHERE v1 = ? AND v2 = ? AND w = 5", []any{math.Copysign(0, -1), math.Copysign(0, -1)}, []int64{201, 202, 203, 204}},
		{"fixed_prefix_middle_zero", "SELECT id FROM pfx WHERE g = 1 AND v = ? AND w = 5", []any{0.0}, []int64{211, 212}},
		{"composite_float_primary_key", "SELECT id FROM pkd WHERE v = ? AND w = 5", []any{0.0}, []int64{301, 302}},
		{"in_forward", "SELECT id FROM d WHERE v IN (-0.0,0.0) AND w = 5", nil, []int64{1, 2}},
		{"in_reverse", "SELECT id FROM d WHERE v IN (0.0,-0.0) AND w = 5", nil, []int64{1, 2}},
		{"in_repeated", "SELECT id FROM d WHERE v IN (-0.0,0.0,-0.0) AND w = 5", nil, []int64{1, 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertIDs(t, tc.q, tc.want, tc.args...)
		})
	}

	// The physical column type, not the driver's rendering of an untyped zero,
	// chooses FLOAT vs DOUBLE tuple packing. Both suffix constraints stay SARGed.
	for _, tc := range []struct {
		q     string
		index string
	}{
		{"SELECT id FROM d WHERE v = 0 AND w = 5", "D_VW"},
		{"SELECT id FROM f WHERE v = 0 AND w = 5", "F_VW"},
		{"SELECT id FROM m WHERE v1 = 0 AND v2 = 0 AND w = 5", "M_V1V2W"},
		{"SELECT id FROM pfx WHERE g = 1 AND v = 0 AND w = 5", "PFX_GVW"},
	} {
		plan := planExplainVia(t, ctx, db, tc.q)
		if !strings.Contains(plan, "IndexScan("+tc.index) || !strings.Contains(plan, "[=, =") {
			t.Fatalf("%s plan = %s\nwant %s with the equality suffix retained", tc.q, plan, tc.index)
		}
	}

	// Covering and fetch are each applied once above the combined physical
	// ranges. The two shapes must return the same two logical matches.
	coveringQ := "SELECT id FROM d WHERE v = 0 AND w = 5"
	coveringPlan := planExplainVia(t, ctx, db, coveringQ)
	if !strings.Contains(coveringPlan, "COVERING") || strings.Contains(coveringPlan, "Fetch(") {
		t.Fatalf("covering plan = %s, want one COVERING D_VW scan and no Fetch", coveringPlan)
	}
	nonCoveringQ := "SELECT payload FROM d WHERE v = 0 AND w = 5"
	nonCoveringPlan := planExplainVia(t, ctx, db, nonCoveringQ)
	// PAYLOAD is outside D_VW's entry, so this scan must read base records.
	// Asserted as the property, not as the literal `Fetch(IndexScan(D_VW` — a
	// bare IndexScan is a fetching scan, so the old string tested the rendering
	// and went red on a plan that reads exactly the records it should.
	assertScanReadsBaseRecords(t, nonCoveringPlan, "IndexScan(D_VW")
	var payloads []string
	rows, err := db.QueryContext(ctx, nonCoveringQ)
	if err != nil {
		t.Fatalf("non-covering query: %v", err)
	}
	for rows.Next() {
		var payload string
		if scanErr := rows.Scan(&payload); scanErr != nil {
			t.Fatalf("non-covering scan: %v", scanErr)
		}
		payloads = append(payloads, payload)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		t.Fatalf("non-covering rows: %v", rowsErr)
	}
	_ = rows.Close()
	sort.Strings(payloads)
	if fmt.Sprint(payloads) != fmt.Sprint([]string{"d-neg", "d-pos"}) {
		t.Fatalf("non-covering payloads = %v, want [d-neg d-pos]", payloads)
	}

	// A UNIQUE composite index may contain both physical signs. Logical equality
	// returns both, and a scalar subquery must therefore reject the two-row result
	// rather than relying on a one-row point-probe proof.
	assertIDs(t, "SELECT id FROM u WHERE v = 0 AND w = 5", []int64{401, 402})
	var scalarID int64
	scalarErr := db.QueryRowContext(ctx,
		"SELECT (SELECT id FROM u WHERE v = 0 AND w = 5) FROM o WHERE id = 10").Scan(&scalarID)
	if scalarErr == nil || !strings.Contains(scalarErr.Error(), "21000") {
		t.Fatalf("two-sign UNIQUE scalar subquery error = %v, want SQLSTATE 21000", scalarErr)
	}

	// Force a transaction/page stop after every scanned row. The resumed stream
	// must cross all four sign branches with no gap or duplicate, and SQL
	// LIMIT/OFFSET must be global rather than resetting for each branch.
	pagedConn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, 1).
			Build())
	})
	const windowQ = "SELECT id FROM m WHERE v1 = 0 AND v2 = 0 AND w = 5 " +
		"ORDER BY v1, v2, w, id LIMIT 2 OFFSET 1"
	windowRows, err := pagedConn.QueryContext(ctx, windowQ)
	if err != nil {
		t.Fatalf("paged range-set window: %v", err)
	}
	var window []int64
	for windowRows.Next() {
		var id int64
		if scanErr := windowRows.Scan(&id); scanErr != nil {
			t.Fatalf("paged range-set scan: %v", scanErr)
		}
		window = append(window, id)
	}
	if rowsErr := windowRows.Err(); rowsErr != nil {
		t.Fatalf("paged range-set rows: %v", rowsErr)
	}
	_ = windowRows.Close()
	if fmt.Sprint(window) != fmt.Sprint([]int64{202, 203}) {
		t.Fatalf("paged four-branch LIMIT/OFFSET = %v, want [202 203]", window)
	}
}

func TestFDB_RuntimeSignedZeroCorrelatedFloatAndDouble(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := setupPlanShapeDB(t, "rsz_corr_widths",
		"CREATE TABLE d (id BIGINT, v DOUBLE, w BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX d_vw ON d (v, w) "+
			"CREATE TABLE f (id BIGINT, v FLOAT, w BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX f_vw ON f (v, w) "+
			"CREATE TABLE o (id BIGINT, kd DOUBLE, kf FLOAT, PRIMARY KEY (id))")
	mwjoMustExec(t, db, ctx, "INSERT INTO d VALUES (1,-0.0,5),(2,0.0,5),(3,-0.0,9),(4,0.0,1)")
	mwjoMustExec(t, db, ctx, "INSERT INTO f VALUES (101,-0.0,5),(102,0.0,5),(103,-0.0,9),(104,0.0,1)")
	mwjoMustExec(t, db, ctx, "INSERT INTO o VALUES (10,0.0,-0.0),(11,-0.0,0.0),(30,NULL,NULL)")

	for _, tc := range []struct {
		name  string
		q     string
		index string
		want  []int64
	}{
		{"double_outer_positive", "SELECT d.id FROM d,o WHERE d.v=o.kd AND d.w=5 AND o.id=10", "D_VW", []int64{1, 2}},
		{"double_outer_negative", "SELECT d.id FROM d,o WHERE d.v=o.kd AND d.w=5 AND o.id=11", "D_VW", []int64{1, 2}},
		{"float_outer_negative", "SELECT f.id FROM f,o WHERE f.v=o.kf AND f.w=5 AND o.id=10", "F_VW", []int64{101, 102}},
		{"float_outer_positive", "SELECT f.id FROM f,o WHERE f.v=o.kf AND f.w=5 AND o.id=11", "F_VW", []int64{101, 102}},
		{"double_outer_null", "SELECT d.id FROM d,o WHERE d.v=o.kd AND d.w=5 AND o.id=30", "D_VW", nil},
		{"float_outer_null", "SELECT f.id FROM f,o WHERE f.v=o.kf AND f.w=5 AND o.id=30", "F_VW", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := planExplainVia(t, ctx, db, tc.q)
			if !strings.Contains(plan, tc.index) || !strings.Contains(plan, "[=, =]") {
				t.Fatalf("plan = %s\nwant correlated %s composite probe with its suffix", plan, tc.index)
			}
			rows, queryErr := db.QueryContext(ctx, tc.q)
			if queryErr != nil {
				t.Fatalf("query: %v", queryErr)
			}
			var got []int64
			for rows.Next() {
				var id int64
				if scanErr := rows.Scan(&id); scanErr != nil {
					t.Fatalf("scan: %v", scanErr)
				}
				got = append(got, id)
			}
			if rowsErr := rows.Err(); rowsErr != nil {
				t.Fatalf("rows: %v", rowsErr)
			}
			_ = rows.Close()
			sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("%s = %v, want %v", tc.q, got, tc.want)
			}
		})
	}
}

// Unlike database/sql's automatic pagination, one ExecutePlan invocation cannot
// hide an incorrectly capped filter/distinct child by resuming its empty page.
func TestFDB_RuntimeRangeSetFilterDistinctOneExecution(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		t.Fatal(err)
	}
	db := recordlayer.NewFDBDatabase(rawDB)
	ks := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
	builder := metadata.NewSchemaTemplateBuilder().SetName("range_budget_direct")
	builder.AddTable("T", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("V", api.NewDoubleType(false), 2),
		metadata.NewColumnSpec("W", api.NewLongType(false), 3),
		metadata.NewColumnSpec("PAYLOAD", api.NewStringType(false), 4),
	}, []string{"ID"})
	builder.AddIndex("T", "RB_VW", []string{"V", "W"}, false)
	template, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	md := template.Underlying()
	open := func(rtx *recordlayer.FDBRecordContext) (*recordlayer.FDBRecordStore, error) {
		return recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
	}
	_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := open(rtx)
		if err != nil {
			return nil, err
		}
		desc := md.GetRecordType("T").Descriptor
		for i, payload := range []string{"a", "a", "a", "b", "b", "c"} {
			zero := float64(0)
			if i < 4 {
				zero = math.Copysign(0, -1)
			}
			record := dynamicpb.NewMessage(desc)
			record.Set(desc.Fields().ByName("ID"), protoreflect.ValueOfInt64(int64(i+1)))
			record.Set(desc.Fields().ByName("V"), protoreflect.ValueOfFloat64(zero))
			record.Set(desc.Fields().ByName("W"), protoreflect.ValueOfInt64(5))
			record.Set(desc.Fields().ByName("PAYLOAD"), protoreflect.ValueOfString(payload))
			if _, err := store.SaveRecord(record); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, sql, operator, want string }{
		{"filter", "SELECT id FROM t WHERE v = 0 AND w = 5 AND payload <> 'a' LIMIT 3", "PredicatesFilter(", "[4 5 6]"},
		{"distinct", "SELECT DISTINCT payload FROM t WHERE v = 0 AND w = 5 LIMIT 3", "Distinct(", "[a b c]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan, err := embedded.PlanRecordQueryWithMetadata(tc.sql, md, properties.FixedStatistics{Cardinality: 1_000_000})
			if err != nil {
				t.Fatal(err)
			}
			for _, part := range []string{"Limit(3", tc.operator, "IndexScan(RB_VW", "[=, =]"} {
				if !strings.Contains(plan.Explain(), part) {
					t.Fatalf("plan %s does not exercise %s (missing %q)", plan.Explain(), tc.operator, part)
				}
			}
			t.Logf("single-execution range-set plan: %s", plan.Explain())
			_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, err := open(rtx)
				if err != nil {
					return nil, err
				}
				cursor, err := executor.ExecutePlan(ctx, plan, store, executor.EmptyEvaluationContext(), nil,
					recordlayer.DefaultExecuteProperties().WithReturnedRowLimit(3))
				if err != nil {
					return nil, err
				}
				defer cursor.Close()
				var got []string
				for {
					row, err := cursor.OnNext(ctx)
					if err != nil {
						return nil, err
					}
					if !row.HasNext() {
						if row.GetNoNextReason() != recordlayer.ReturnLimitReached || row.GetContinuation().IsEnd() {
							t.Fatalf("single execution stopped with %v/end=%v, want resumable returned-row limit", row.GetNoNextReason(), row.GetContinuation().IsEnd())
						}
						break
					}
					value, ok := row.GetValue().Positional.Get(0)
					if !ok {
						t.Fatal("query result lost its projected column")
					}
					got = append(got, fmt.Sprint(value))
				}
				sort.Strings(got)
				if fmt.Sprint(got) != tc.want {
					t.Fatalf("one execution returned %v before its cap stop, want %s; raw child caps must be cleared below %s", got, tc.want, tc.operator)
				}
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
