package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_GroupByNestedPathRejected pins a NEGATIVE result, and the reason it is
// worth a test file is that it is the only thing standing between a latent
// wrong-rows defect and a live one.
//
// The defect, unreachable today. The resolver FUSES a nested reference: `n.sk`
// becomes ONE FieldValue{Field:"N", Resolved:[N,SK]}. AggregateKeyColumnName
// (pkg/recordlayer/query/plan/cascades/expressions/group_by.go) is the single
// naming authority for a grouping key and renders the flat ROOT —
// strings.ToUpper(fv.Field) — so it answers "N" for `n.sk` AND for `n.co`. The
// translator keys grouping columns by that name and the later key overwrites the
// earlier, so `GROUP BY n.sk, n.co` would collapse to ONE grouping column and
// return too few groups.
//
// That is byte-for-byte the sort-key defect RFC-227 fixed. The fix there was to
// name the hidden column by the path it reads (sortKeyExtraColumnName now calls
// values.ColumnNameValue, which renders the full path). The group-key half was
// never fixed, and the two sit one screen apart:
//
//	sortKeyExtraColumnName  -> values.ColumnNameValue(fv)   // full PATH  (fixed)
//	AggregateKeyColumnName  -> strings.ToUpper(fv.Field)    // flat ROOT  (not)
//
// What makes it unreachable is measured here, not assumed: the SQL layer
// rejects a nested path as a grouping key outright with 42703, so no query can
// reach the collapse. Nothing pinned that rejection before this file.
//
// THIS TEST IS THE TRIPWIRE. If nested-path GROUP BY is ever implemented — and
// it is a real gap, since ORDER BY over the same path works (RFC-227) and the
// asymmetry between the two is not a design decision anyone recorded — then
// AggregateKeyColumnName MUST be converted to the resolved path IN THE SAME
// CHANGE. Implementing the feature alone silently arms the collapse, and the
// symptom is missing groups, which no existing test would catch.
func TestFDB_GroupByNestedPathRejected(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/gnk"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE gnk_tmpl "+
			"CREATE TYPE AS STRUCT gst (sk BIGINT, co BIGINT) "+
			"CREATE TABLE t1 (id BIGINT, n gst, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE gnk_tmpl"); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// (sk, co) pairs (1,1) (1,2) (2,1) (2,2) (1,1). Seeded so that IF the two-key
	// query below ever plans, the assertion can distinguish a correct 4 groups
	// from a collapsed 2 — the data is not incidental, it is what makes the
	// tripwire able to state the consequence.
	if _, err := db.ExecContext(ctx,
		"INSERT INTO t1 VALUES (1, (1, 1)), (2, (1, 2)), (3, (2, 1)), (4, (2, 2)), (5, (1, 1))"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	// A nested path in ORDER BY works — this is the asymmetry, asserted rather
	// than described, so "GROUP BY simply does not do nested paths" cannot be
	// mistaken for a general limitation on nested paths.
	rows, err := db.QueryContext(ctx, "SELECT id FROM t1 ORDER BY n.sk, n.co")
	if err != nil {
		t.Fatalf("ORDER BY over a nested path stopped working: %v\n"+
			"  The premise of this file is that GROUP BY is the odd one out. If "+
			"ORDER BY now rejects the same path too, the gap is elsewhere and this "+
			"test is describing the wrong asymmetry.", err)
	}
	n := 0
	for rows.Next() {
		n++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("ORDER BY nested path iterate: %v", err)
	}
	if n != 5 {
		t.Fatalf("ORDER BY over a nested path returned %d rows, want 5", n)
	}

	// THE GATE. Each of these must be refused, and refused for the reason stated
	// — a wrong code would mean the query is being turned away by something
	// unrelated, which would let this file pass while the real gate was gone.
	for _, q := range []string{
		"SELECT n.sk, COUNT(*) FROM t1 GROUP BY n.sk",
		"SELECT n.sk, n.co, COUNT(*) FROM t1 GROUP BY n.sk, n.co",
	} {
		_, err := db.QueryContext(ctx, q)
		if err == nil {
			t.Fatalf("NESTED-PATH GROUP BY NOW PLANS — THE COLLAPSE IS ARMED.\n"+
				"  query: %s\n\n"+
				"  This test exists because AggregateKeyColumnName "+
				"(expressions/group_by.go) renders a grouping key as the flat struct "+
				"ROOT, strings.ToUpper(fv.Field). A fused nested reference carries "+
				"Field=\"N\" for BOTH n.sk and n.co, so two grouping columns take one "+
				"output name and the later overwrites the earlier: "+
				"`GROUP BY n.sk, n.co` returns 2 groups where the data has 4.\n\n"+
				"  If you implemented nested-path GROUP BY, convert "+
				"AggregateKeyColumnName (and its mirror aggregateGroupKeyOutputName "+
				"in embedded/logical_predicate.go) to the RESOLVED PATH first — "+
				"values.ColumnNameValue, exactly as sortKeyExtraColumnName does since "+
				"RFC-227 — then replace this test with one asserting 4 groups for the "+
				"two-key query and 2 for each single-key query.", q)
		}
		if !strings.Contains(err.Error(), "42703") {
			t.Fatalf("nested-path GROUP BY refused with the WRONG error.\n"+
				"  query: %s\n  got:   %v\n  want:  42703 (undefined_column).\n"+
				"  A refusal for an unrelated reason would let this tripwire pass "+
				"while the gate it watches was gone.", q, err)
		}
	}
}
