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
// layer refuses a nested path as a grouping key, so the feature is a genuine gap
// and this is the pin on the refusal. When nested-path GROUP BY is implemented,
// the naming prerequisite is already met — replace the gate below with the
// assertions its failure message names, and check nothing else re-derives a
// group-key name from `fv.Field`.
//
// THE REFUSAL IS 0AF00, AND IT USED TO BE SOMETHING ELSE ENTIRELY. This file
// previously recorded it as 42703, and the sub-test that lived below pinned a
// shape that ESCAPED it. Both had the same cause: nothing validated the grouping
// KEY. The 42703 came from validateGroupByProjection's existence check, which
// compares a PROJECTED column's BARE LEAF against the union of source field
// names — so the refusal held only while the leaf named nothing. A table also
// declaring a flat `sk` let `GROUP BY n.sk` through on the strength of that
// unrelated column, and `SELECT COUNT(*) ... GROUP BY n.sk` escaped even without
// one, because with nothing projected the check never ran at all. Those shapes
// planned and died in the executor as "not resolvable in the runtime row —
// malformed plan".
//
// The key is now validated as a key (rejectNestedPathGroupKey,
// embedded/logical_predicate.go), which is why the escaping query is folded into
// the gate below rather than pinned as a defect, and why the expected code is
// 0AF00: the reference is well-formed and answers in SELECT, WHERE and ORDER BY,
// so it is an unsupported FEATURE, not an undefined column. Measured against the
// live Java server at tag 4.12.11.0 by
// conformance/nested_groupby_key_java_probe_test.go: given an index over the
// path, JAVA ANSWERS a nested grouping key — `SELECT COUNT(*) FROM T_NG3 GROUP
// BY n.sk` returns [[2] [1]], the same rows as its indexed FLAT twin — and
// reserves 42703 for a qualifier that resolves to nothing. So this is a
// capability Java has and Go lacks, which is precisely what UNSUPPORTED_QUERY
// reports. 0AF00 is also the code Java's own visitGroupByItem spends on a GROUP
// BY item it will not take (ExpressionVisitor.java:251). Note that a Java
// PLANNER DECLINE is not evidence either way here: Java's Cascades has no
// physical sort, so it declines a FLAT grouping key just as readily when no
// index supplies the ordering. The wider shape
// coverage lives in groupby_nested_path_refused_fdb_test.go; what this file
// keeps watching is the collapse-ordering constraint above.
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
			"CREATE TABLE t1 (id BIGINT, n gst, PRIMARY KEY (id)) "+
			// t2 differs from t1 in ONE way: it also declares a FLAT column whose
			// name equals the struct member's LEAF. That single difference is
			// what used to arm the escape now folded into the gate below, so the
			// two tables are the controlled comparison rather than two fixtures.
			"CREATE TABLE t2 (id BIGINT, sk BIGINT, n gst, PRIMARY KEY (id))"); err != nil {
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
	// t2 MUST be non-empty. The escape now folded into the gate below surfaced
	// from the EXECUTOR, so an empty table hides it completely: the
	// plan is built, no row is ever evaluated, and the query returns zero rows
	// with no error — a green that proves only that the table was empty. The
	// flat `sk` values are disjoint from the struct's so a wrong-slot read is
	// visible if the shape ever starts answering.
	if _, err := db.ExecContext(ctx,
		"INSERT INTO t2 VALUES (1, 90, (1, 1)), (2, 91, (1, 2)), (3, 92, (2, 1))"); err != nil {
		t.Fatalf("INSERT t2: %v", err)
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
		// Folded in from a sub-test that pinned this exact query as an ESCAPE:
		// over t2, whose flat `sk` shares the path leaf, it used to plan and die
		// in the executor. It is now refused with the rest.
		"SELECT n.sk FROM t2 GROUP BY n.sk",
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
				"not any fourth site a new feature might add.\n\n"+
				"  THE FOURTH SITE EXISTS AND IS NAMED HERE so nobody has to hunt "+
				"for it: RecordQueryStreamingAggregationPlan.HintOrdering "+
				"(pkg/recordlayer/query/plan/plans/ordering.go:1155) synthesizes a "+
				"PROVIDED ordering key as FieldValue{Field: AggregateKeyColumnName(k)}, "+
				"and RichOrdering.orderingKeyFor "+
				"(pkg/recordlayer/query/plan/cascades/properties/rich_ordering.go:364-365) "+
				"matches a REQUESTED key against those provided keys through "+
				"values.ExplainValue — a STRING. So group-key naming reaches ordering "+
				"MATCHING through a rendering, not through Value identity. Once a "+
				"nested key is admitted, the provided key is a flat single-accessor "+
				"FieldValue whose Field is the dotted path \"N.SK\", while the "+
				"requested key is the real fused multi-accessor reference rendering "+
				"as \"N#0.SK#1\" (both renderings MEASURED). Two spellings that do "+
				"not meet make the match fail SILENTLY: an ordering the aggregation "+
				"really provides goes unrecognised, so the cost is a redundant sort "+
				"rather than a wrong answer — which is exactly why it needs naming "+
				"rather than discovering. Convert the ordering side too, or prove "+
				"the spellings meet.", q)
		}
		if !strings.Contains(err.Error(), "0AF00") {
			t.Fatalf("nested-path GROUP BY refused with the WRONG error.\n"+
				"  query: %s\n  got:   %v\n  want:  0AF00 (unsupported_query).\n"+
				"  A refusal for an unrelated reason would let this tripwire pass "+
				"while the gate it watches was gone.", q, err)
		}
	}
}
