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
// THE ORDERING CONSTRAINT THIS FILE ENFORCED IS NOW SATISFIED, and that changes
// what the file is for. It is no longer a tripwire over an UNCONVERTED namer; it
// is the pin on the remaining half — the refusal itself.
//
// What it used to watch. The resolver FUSES a nested reference: `n.sk` becomes
// ONE FieldValue{Field:"N", Resolved:[N,SK]}. AggregateKeyColumnName
// (pkg/recordlayer/query/plan/cascades/expressions/group_by.go) is the single
// naming authority for a grouping key and rendered the flat ROOT —
// strings.ToUpper(fv.Field) — so it answered "N" for `n.sk` AND for `n.co`. The
// translator keys grouping columns by that name and the later key overwrites the
// earlier, so `GROUP BY n.sk, n.co` would have collapsed to ONE grouping column
// and returned too few groups.
//
// That was byte-for-byte the sort-key defect RFC-227 fixed on its own side. The
// group-key half is fixed by RFC-229 §2.3: AggregateKeyColumnName, its mirror
// aggregateGroupKeyOutputName (embedded/logical_predicate.go) and the
// ColumnDef mirror (cascades_generator.go buildAggColumns) all take the
// RESOLVED PATH for a nested key, through the one shared predicate
// values.NestedResolvedPath. The conversion is pinned as a unit, driving both
// arms and both controls, at
// expressions/group_by_naming_test.go:TestAggregateKeyColumnName_NestedKeyTakesTheResolvedPath.
// It landed BEFORE the feature, deliberately: converting afterwards ships the
// collapse, and its symptom — missing groups — is silent.
//
// WHAT IS STILL PINNED HERE, and why the file survives the conversion: the SQL
// layer still rejects a nested path as a grouping key with 42703, so the feature
// is a genuine gap and nothing else measures the refusal. When nested-path GROUP
// BY is implemented, the naming prerequisite is already met — replace the gate
// below with the assertions its failure message names, and check nothing else
// re-derives a group-key name from `fv.Field`.
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
			t.Fatalf("NESTED-PATH GROUP BY NOW PLANS.\n"+
				"  query: %s\n\n"+
				"  THE NAMING PREREQUISITE IS ALREADY MET — RFC-229 §2.3 converted "+
				"AggregateKeyColumnName, aggregateGroupKeyOutputName and the "+
				"ColumnDef mirror to the RESOLVED PATH, so two members of one struct "+
				"root no longer share an output name. This is therefore an expected "+
				"and welcome state, not an armed collapse.\n\n"+
				"  What to do: replace this gate with the real assertions — 4 groups "+
				"for `GROUP BY n.sk, n.co` over the seeded (1,1)(1,2)(2,1)(2,2)(1,1), "+
				"and 2 groups for each single-key query — and keep the ORDER BY "+
				"assertion above. Confirm first that nothing has RE-DERIVED a "+
				"group-key name from fv.Field in the meantime: the unit pin at "+
				"expressions/group_by_naming_test.go covers the three authorities, "+
				"not any fourth site a new feature might add.", q)
		}
		if !strings.Contains(err.Error(), "42703") {
			t.Fatalf("nested-path GROUP BY refused with the WRONG error.\n"+
				"  query: %s\n  got:   %v\n  want:  42703 (undefined_column).\n"+
				"  A refusal for an unrelated reason would let this tripwire pass "+
				"while the gate it watches was gone.", q, err)
		}
	}
}
