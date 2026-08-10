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
			"CREATE TABLE t1 (id BIGINT, n gst, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
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
		"INSERT INTO t1 VALUES (1, (10, 21)), (2, (11, 22))"); err != nil {
		t.Fatalf("INSERT: %v", err)
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
}
