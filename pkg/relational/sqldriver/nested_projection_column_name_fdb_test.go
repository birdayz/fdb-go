package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_NestedProjectionColumnNameIsThePath pins RFC-229 §2.3 where it is
// USER-VISIBLE: the label `database/sql`'s Rows.Columns() surfaces for a nested
// projected reference.
//
// MEASURED BEFORE THE FIX: `SELECT n.sk, n.co FROM t1` returned columns
// [N N] — two identical labels over correct, DIFFERENT data. The resolver fuses
// `n.sk` into one FieldValue whose Field is the struct ROOT `N`, and every
// output-naming authority read that root, so both members of the struct were
// named after their container.
//
// Java is the spec and it does not have the defect, structurally rather than
// carefully: SemanticAnalyzer.lookupNestedField performs the identical fuse
// (SemanticAnalyzer.java:598, FieldValue.ofFieldsAndFuseIfPossible) and then
// names the resulting Expression by the REQUESTED IDENTIFIER `n.sk`
// (SemanticAnalyzer.java:599) rather than by the fused Value. The top-level
// projection clears the qualifier (LogicalOperator.java:238 →
// Expression.clearQualifier → Identifier.withoutQualifier, Identifier.java:101),
// so Java's user-visible labels are SK and CO. Go now answers the same.
//
// The row assertions are the control that stops this passing for the wrong
// reason. A namer that produced distinct labels by falling back to positional
// `_0`/`_1`, or one that renamed the slots without keeping each label attached
// to the column it names, would satisfy "the labels differ" and fail here.
func TestFDB_NestedProjectionColumnNameIsThePath(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/npcn"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE npcn_tmpl "+
			"CREATE TYPE AS STRUCT gst (sk BIGINT, co BIGINT) "+
			// t1 declares BOTH a flat `sk` and a struct member `n.sk`, so the
			// bare-leaf channel is live: `stripColumnQualifier`/`parseColRef().bare()`
			// probe the table descriptor for SK, and a nested path whose leaf
			// collides with a real column is the shape that channel could confuse.
			"CREATE TABLE t1 (id BIGINT, sk BIGINT, n gst, PRIMARY KEY (id)) "+
			"CREATE TABLE t2 (id2 BIGINT, v BIGINT, PRIMARY KEY (id2))"); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	// A struct member name must be a valid protobuf identifier, and that is what
	// keeps explainValueOrdinals' `#`-doubling escape out of the NAME path: the
	// nested arm mints its slot name from that renderer, so a member spelled
	// `a#1` would be named `N.A##1`. It cannot exist. Pinned here rather than
	// asserted in a comment, because the escape's safety argument rests on it —
	// if DDL ever admits `#`, the path must be minted from the accessors instead.
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE npcn_hash_tmpl "+
			"CREATE TYPE AS STRUCT hst (\"a#1\" BIGINT, b BIGINT) "+
			"CREATE TABLE h1 (id BIGINT, n hst, PRIMARY KEY (id))"); err == nil {
		t.Errorf("DDL now ACCEPTS a struct member named `a#1`.\n" +
			"  That re-arms the explain escape in the NAME path: " +
			"values.NestedResolvedPath mints an output slot name from " +
			"explainValueOrdinals, which doubles every `#` in a path step, so " +
			"`n.\"a#1\"` would be named N.A##1 — a slot key the user never " +
			"spelled. Mint the path from the ResolvedAccessors directly instead " +
			"of from the explain renderer, and fix the note at values.go's " +
			"explainValueOrdinals that cites this refusal as the reason it is safe.")
	} else if !strings.Contains(err.Error(), "not a valid protobuf identifier") {
		t.Errorf("a struct member named `a#1` was refused for an unexpected reason: %v\n"+
			"  want a protobuf-identifier rejection. A refusal for some other "+
			"reason would let this pin pass while the constraint it relies on was "+
			"gone.", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE npcn_tmpl"); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// sk is deliberately NOT a function of co and vice versa, so a label bound
	// to the wrong slot changes the answer rather than merely the spelling.
	if _, err := db.ExecContext(ctx,
		"INSERT INTO t1 VALUES (1, 99, (10, 21)), (2, 98, (11, 22))"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	// ONE row, so a two-source cross product keeps t1's row count and cardinality
	// cannot be mistaken for a naming effect.
	if _, err := db.ExecContext(ctx, "INSERT INTO t2 VALUES (100, 7)"); err != nil {
		t.Fatalf("INSERT t2: %v", err)
	}

	query := func(t *testing.T, q string) ([]string, []string) {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			t.Fatalf("%s: Columns: %v", q, err)
		}
		var out []string
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatalf("%s: scan: %v", q, err)
			}
			cells := make([]string, len(vals))
			for i, v := range vals {
				cells[i] = fmt.Sprint(v)
			}
			out = append(out, strings.Join(cells, "|"))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("%s: iterate: %v", q, err)
		}
		return cols, out
	}

	for _, tc := range []struct {
		name     string
		sql      string
		wantCols []string
		wantRows []string
		why      string
	}{{
		name:     "two members of one struct root",
		sql:      "SELECT n.sk, n.co FROM t1 ORDER BY id",
		wantCols: []string{"SK", "CO"},
		wantRows: []string{"10|21", "11|22"},
		why: "THE REGRESSION THIS FILE EXISTS FOR. Before RFC-229 §2.3 both " +
			"columns were labelled N, because the namer read the fused " +
			"reference's struct ROOT instead of its resolved path.",
	}, {
		name:     "a single nested member",
		sql:      "SELECT n.sk FROM t1 ORDER BY id",
		wantCols: []string{"SK"},
		wantRows: []string{"10", "11"},
		why: "one column cannot collide with anything, so this catches a fix " +
			"that only kicks in when two nested columns are present — the label " +
			"is wrong for `SELECT n.sk` alone too, and was N.",
	}, {
		name:     "nested beside a flat column",
		sql:      "SELECT id, n.co FROM t1 ORDER BY id",
		wantCols: []string{"ID", "CO"},
		wantRows: []string{"1|21", "2|22"},
		why: "CONTROL — a FLAT reference must keep its bare Field. A fix that " +
			"routed every reference through the path renderer would still pass " +
			"the two cases above and change this one.",
	}, {
		name:     "an explicit alias still wins",
		sql:      "SELECT n.sk AS first, n.co AS second FROM t1 ORDER BY id",
		wantCols: []string{"FIRST", "SECOND"},
		wantRows: []string{"10|21", "11|22"},
		why: "CONTROL — the user's AS is the entire output-name authority and " +
			"§2.3 must not reach past it.",
	}, {
		name:     "a flat sk beside the nested n.sk",
		sql:      "SELECT sk, n.sk FROM t1 ORDER BY id",
		wantCols: []string{"SK", "SK"},
		wantRows: []string{"99|10", "98|11"},
		why: "THE BARE-LEAF COLLISION CHANNEL, driven on a schema that arms it. " +
			"§2.3 made the nested slot's internal name N.SK, whose BARE LEAF is " +
			"SK — the same leaf as a real declared column on the same table. The " +
			"sites that probe a table descriptor by bare leaf " +
			"(stripColumnQualifier, parseColRef().bare()) therefore see SK twice. " +
			"MEASURED: the labels collide (Java's getColumnLabel is the " +
			"unqualified leaf too, so that is the agreed behaviour and is pinned " +
			"separately by TestFDB_DuplicateBareLeafKeepsTwoColumns) and the ROWS " +
			"STAY CORRECT — 99 is t1.sk and 10 is n.sk, never each other. Reading " +
			"the same value twice, or swapping them, is the failure this catches.",
	}, {
		name:     "the nested member first, flat sk second",
		sql:      "SELECT n.sk, sk FROM t1 ORDER BY id",
		wantCols: []string{"SK", "SK"},
		wantRows: []string{"10|99", "11|98"},
		why: "ORDER CONTROL on the same collision — a last-wins name-keyed map " +
			"resolves both slots to whichever was written last, which is invisible " +
			"in one ordering and loud in the other.",
	}, {
		name:     "two sources, one nested member",
		sql:      "SELECT n.sk FROM t1, t2 ORDER BY t1.id",
		wantCols: []string{"SK"},
		wantRows: []string{"10", "11"},
		why: "THE QUALIFIED ARM, which nothing drove until now. With >=2 FROM " +
			"sources the resolver emits every reference through its quantifier, so " +
			"the fused nested node carries QOV(T1) and the INTERNAL slot name " +
			"becomes T1.N.SK rather than N.SK — MEASURED via EXPLAIN, which reads " +
			"Project([T1.N#1.SK#0], NestedLoopJoin(...)) here against " +
			"Project([N#1.SK#0], Scan(T1)) for the single-source form. The " +
			"user-visible LABEL is SK either way, because the display side strips " +
			"to the bare leaf exactly as Java clears the qualifier at the " +
			"top-level projection (Identifier.withoutQualifier, Identifier.java:101). " +
			"Writer and reader both derive from values.OutputColumnName, so they " +
			"moved together and the query ANSWERS — that is what this case proves.",
	}, {
		name:     "two sources, two members of one struct root",
		sql:      "SELECT n.sk, n.co FROM t1, t2 ORDER BY t1.id",
		wantCols: []string{"SK", "CO"},
		wantRows: []string{"10|21", "11|22"},
		why: "the collision case ON the qualified arm. Both slots are qualified " +
			"T1.N.*, so a fix that only distinguished members in the unqualified " +
			"shape would emit two T1.N slots here and duplicate the labels again.",
	}, {
		name:     "two sources, nested beside the other table's flat column",
		sql:      "SELECT n.sk, v FROM t1, t2 ORDER BY t1.id",
		wantCols: []string{"SK", "V"},
		wantRows: []string{"10|7", "11|7"},
		why: "CONTROL across sources — the flat column from the OTHER table must " +
			"keep its bare label and its own value. A namer that qualified the " +
			"display label, or that bound a label to the wrong source's slot, " +
			"changes this row.",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cols, out := query(t, tc.sql)
			if strings.Join(cols, ",") != strings.Join(tc.wantCols, ",") {
				t.Errorf("%s\n  columns = %v, want %v\n  %s", tc.sql, cols, tc.wantCols, tc.why)
			}
			if strings.Join(out, " ") != strings.Join(tc.wantRows, " ") {
				t.Errorf("%s\n  rows = %v, want %v\n  a label bound to the wrong "+
					"slot moves the DATA, not only the spelling", tc.sql, out, tc.wantRows)
			}
		})
	}

	// HOW THE QUALIFIED ARM IS *NOT* REACHED, pinned because the obvious guess is
	// wrong and a reader who assumes it would write an unreachable test. The
	// three-part spelling `t1.n.sk` does not resolve at all — the qualified arm
	// above is reached by the BARE `n.sk` under a multi-source FROM. If this ever
	// starts resolving, the qualified arm gains a second entry point and the
	// EXPLAIN spelling asserted above must be re-measured for it.
	t.Run("the three-part qualified spelling does not resolve", func(t *testing.T) {
		t.Parallel()
		for _, q := range []string{
			"SELECT t1.n.sk FROM t1, t2",
			"SELECT t1.n.sk, t1.n.co FROM t1, t2",
		} {
			rows, err := db.QueryContext(ctx, q)
			if err == nil {
				rows.Close()
				t.Errorf("%s now RESOLVES.\n"+
					"  This file assumes the qualified nested arm is reached only "+
					"through the bare `n.sk` under a multi-source FROM. A "+
					"table-qualified struct descent is a second entry point: "+
					"re-measure its EXPLAIN spelling and its user-visible label "+
					"before assuming they match the cases above.", q)
				continue
			}
			if !strings.Contains(err.Error(), "42703") {
				t.Errorf("%s was refused with %v, want 42703 (undefined_column).\n"+
					"  A refusal for an unrelated reason would let this pin pass "+
					"while the shape it describes had changed.", q, err)
			}
		}
	})

	// A RECURSIVE CTE whose seed leg projects a struct member. The leg
	// normalization reads the leg's output by its PHYSICAL name, and RFC-229 §2.3
	// changed that name from the struct root `N` to the path `N.SK` — so this
	// shape exercises legPhysicalOutputNames' verbatimField classification, which
	// decides whether a dot in the name is a QUALIFIER to split on.
	//
	// MEASURED both ways: with §2.3's predicate disabled this query FAILS with
	// `XX000: result row carries no positional output row aligned to column "N"`,
	// so §2.3 FIXES this shape rather than breaking it. It still needs a test,
	// because the name is now dotted and a first-dot split here would mint
	// QOV("N") — a correlation named after a struct root that exists nowhere.
	t.Run("a recursive CTE leg projecting a struct member", func(t *testing.T) {
		t.Parallel()
		rows, err := db.QueryContext(ctx,
			"WITH RECURSIVE r AS (SELECT n.sk FROM t1 WHERE id = 1 "+
				"UNION ALL SELECT sk + 1 FROM r WHERE sk < 13) SELECT * FROM r")
		if err != nil {
			t.Fatalf("recursive CTE over a nested seed projection: %v\n"+
				"  The leg normalization reads the seed's output by its physical "+
				"name, which §2.3 made the dotted path N.SK. If that name is "+
				"classified as a QUALIFIED identifier the first-dot split mints "+
				"QOV(\"N\") — a correlation named after a struct root — instead of "+
				"reading emitted slot i by ordinal.", err)
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			t.Fatalf("Columns: %v", err)
		}
		if len(cols) != 1 || cols[0] != "SK" {
			t.Errorf("recursive CTE columns = %v, want [SK]", cols)
		}
		var got []int64
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got = append(got, v)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate: %v", err)
		}
		// The RECURSION DEPTH is the assertion that matters: a leg wrap that
		// reads the wrong slot returns NULL and the recursion stalls at the seed
		// instead of counting to 13. Row values, not just row count.
		if fmt.Sprint(got) != "[10 11 12 13]" {
			t.Errorf("recursive CTE rows = %v, want [10 11 12 13] — a stall at "+
				"[10] means the leg wrap read a slot that was not there", got)
		}
	})
}
