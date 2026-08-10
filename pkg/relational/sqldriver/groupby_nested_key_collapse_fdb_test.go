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
			"CREATE TABLE t1 (id BIGINT, n gst, PRIMARY KEY (id)) "+
			// t2 differs from t1 in ONE way: it also declares a FLAT column whose
			// name equals the struct member's LEAF. That single difference is
			// what arms the escape pinned at the bottom of this file, so the two
			// tables are the controlled comparison rather than two fixtures.
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
	// t2 MUST be non-empty. The escape pinned at the bottom of this file
	// surfaces from the EXECUTOR, so an empty table hides it completely: the
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
		if !strings.Contains(err.Error(), "42703") {
			t.Fatalf("nested-path GROUP BY refused with the WRONG error.\n"+
				"  query: %s\n  got:   %v\n  want:  42703 (undefined_column).\n"+
				"  A refusal for an unrelated reason would let this tripwire pass "+
				"while the gate it watches was gone.", q, err)
		}
	}

	// A SEPARATE, PRE-EXISTING DEFECT, pinned at its CURRENT behaviour rather
	// than at the behaviour it should have.
	//
	// WHAT ACTUALLY DEFEATS THE REFUSAL — measured, and it is not what it first
	// looked like. `SELECT n.sk FROM t1 GROUP BY n.sk` over t1, which declares
	// only `n`, IS refused cleanly with 42703; dropping the aggregate is not by
	// itself enough. The escape needs a table that ALSO declares a FLAT column
	// whose name equals the path's LEAF. Over t2 (id, sk, n) the same query is
	// NOT refused: it plans and dies in the EXECUTOR with
	//
	//   ordinal resolution: field "N.SK" not resolvable in the runtime row
	//   (ordinal -1, row columns [ID SK N]) — malformed plan
	//
	// The reading that follows from the pair: the semantic layer's validation of
	// the grouping key is satisfied by the BARE LEAF, so `n.sk` is accepted on
	// the strength of the unrelated flat `sk`, and the mismatch only surfaces
	// when the executor tries to resolve the real path. A user error reported as
	// internal state — the same class as any missing semantic refusal that
	// surfaces from the executor.
	//
	// It is NOT this change's regression: MEASURED identically with RFC-229
	// §2.3's predicate disabled, so the naming conversion neither caused it nor
	// can fix it, and fixing it here would mean changing refusal semantics inside
	// a naming change.
	//
	// It is pinned rather than described because an earlier version of this
	// file's header asserted that "the SQL layer rejects a nested path as a
	// grouping key with 42703" full stop, and that claim was too broad — it held
	// for every shape anyone had tried and not for this one. A watched gap
	// survives the next refactor; a described one does not.
	t.Run("a flat column sharing the leaf name defeats the refusal", func(t *testing.T) {
		t.Parallel()
		// CONTROL FIRST: the same query over the table WITHOUT the flat `sk` is
		// refused properly. Without this the assertions below would read as
		// "non-aggregate GROUP BY is broken", which is false and would send the
		// eventual fix to the wrong layer.
		if _, err := db.QueryContext(ctx, "SELECT n.sk FROM t1 GROUP BY n.sk"); err == nil ||
			!strings.Contains(err.Error(), "42703") {
			t.Fatalf("the CONTROL moved: `SELECT n.sk FROM t1 GROUP BY n.sk` over a "+
				"table with no flat `sk` should still be a clean 42703, got %v.\n"+
				"  The defect below is specifically about a flat column sharing the "+
				"path's LEAF name; if the control fails too, the gap is wider than "+
				"this pin describes and its diagnosis is wrong.", err)
		}
		const q = "SELECT n.sk FROM t2 GROUP BY n.sk"
		rows, err := db.QueryContext(ctx, q)
		if err == nil {
			rows.Close()
			t.Fatalf("%s now SUCCEEDS.\n"+
				"  If it returns correct groups, nested-path GROUP BY has landed "+
				"for this shape — convert the gate above too. If it returns rows "+
				"without grouping correctly, that is worse than the error it used "+
				"to raise.", q)
		}
		if strings.Contains(err.Error(), "42703") {
			t.Fatalf("%s is now refused with 42703 — THE DEFECT THIS PINS IS FIXED.\n"+
				"  A nested grouping key over a table declaring a flat column of the "+
				"same leaf name now gets the same clean "+
				"undefined_column refusal the aggregate shape always got. Delete "+
				"this sub-test and fold the query into the gate's list above.", q)
		}
		if !strings.Contains(err.Error(), "not resolvable in the runtime row") {
			t.Fatalf("%s failed with an UNEXPECTED error: %v\n"+
				"  This pin expects the known defect — a plan-time escape that "+
				"surfaces as an executor ordinal-resolution failure. A different "+
				"error means the shape moved and this pin has stopped watching "+
				"what it names. Want either 42703 (fixed) or the runtime "+
				"'not resolvable in the runtime row' (still broken).", q, err)
		}
		// The defect stated as the assertion it should one day become: the
		// grouping key is a user-supplied column reference the semantic layer
		// declined to validate, so the correct answer is 42703 at plan time, not
		// XX000-class internal state from the executor.
		t.Logf("KNOWN DEFECT (pre-existing, not RFC-229's): %s reports a user "+
			"error as internal state:\n  %v\n  It should be a clean 42703 at the "+
			"semantic layer, as the aggregate shape is. Booked separately; fixing "+
			"it here would mean changing refusal semantics inside a naming change.", q, err)
	})
}
