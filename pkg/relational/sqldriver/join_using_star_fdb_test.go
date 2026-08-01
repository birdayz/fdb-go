package sqldriver_test

// JOIN … USING hides the RIGHT-side copy of each USING column — Java parity.
//
// Java's resolveJoinUsingClause (QueryVisitor.java:397-420) marks the right
// copy hidden (Expression.asHidden); star expansion filters hidden
// (SemanticAnalyzer.expandStar → nonEphemeralVisible, bare `*` and `alias.*`
// alike), and UNQUALIFIED resolution skips hidden attributes
// (SemanticAnalyzer.java:468) so a bare reference to the USING column
// resolves the LEFT copy instead of being ambiguous, while the QUALIFIED
// right copy stays addressable. Live-Java measurement pinned in
// conformance's JoinUsingStarJavaProbe; the corpus witness
// (join-tests.yamsql `select * from ja join jb using(c1) join jd
// using(c1)`) is DDL-blocked on master (AS-SELECT value indexes, PR #577).

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_JoinUsingStarHidesRightColumns(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_usingstar")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_usingstar")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE usingstar "+
			"CREATE TABLE ja (c1 BIGINT NOT NULL, a2 STRING, PRIMARY KEY (c1)) "+
			"CREATE TABLE jb (c1 BIGINT NOT NULL, b2 STRING, PRIMARY KEY (c1)) "+
			"CREATE TABLE jd (c1 BIGINT NOT NULL, d2 STRING, PRIMARY KEY (c1))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_usingstar/s WITH TEMPLATE usingstar")
	dsn := fmt.Sprintf("fdbsql:///testdb_usingstar?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mwjoMustExec(t, db, ctx, "INSERT INTO ja VALUES (1, 'a1'), (2, 'a2')")
	mwjoMustExec(t, db, ctx, "INSERT INTO jb VALUES (1, 'b1'), (3, 'b3')")
	mwjoMustExec(t, db, ctx, "INSERT INTO jd VALUES (1, 'd1'), (2, 'd2')")

	// run returns (column labels, rows-as-strings).
	run := func(t *testing.T, q string) ([]string, []string) {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			t.Fatalf("columns %q: %v", q, err)
		}
		var out []string
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		for rows.Next() {
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			parts := make([]string, len(vals))
			for i, v := range vals {
				if b, ok := v.([]byte); ok {
					parts[i] = string(b)
				} else {
					parts[i] = fmt.Sprintf("%v", v)
				}
			}
			out = append(out, strings.Join(parts, "|"))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows %q: %v", q, err)
		}
		return cols, out
	}

	t.Run("bare star hides right USING copies", func(t *testing.T) {
		t.Parallel()
		cols, got := run(t, "SELECT * FROM ja JOIN jb USING (c1)")
		wantCols := []string{"C1", "A2", "B2"}
		if strings.Join(cols, ",") != strings.Join(wantCols, ",") {
			t.Fatalf("columns = %v, want %v", cols, wantCols)
		}
		if len(got) != 1 || got[0] != "1|a1|b1" {
			t.Fatalf("rows = %v, want [1|a1|b1]", got)
		}
	})

	t.Run("chained USING hides each right copy", func(t *testing.T) {
		t.Parallel()
		// The corpus reproducer's shape (join-tests.yamsql).
		cols, got := run(t, "SELECT * FROM ja JOIN jb USING (c1) JOIN jd USING (c1)")
		wantCols := []string{"C1", "A2", "B2", "D2"}
		if strings.Join(cols, ",") != strings.Join(wantCols, ",") {
			t.Fatalf("columns = %v, want %v", cols, wantCols)
		}
		if len(got) != 1 || got[0] != "1|a1|b1|d1" {
			t.Fatalf("rows = %v, want [1|a1|b1|d1]", got)
		}
	})

	t.Run("bare USING column resolves the left copy", func(t *testing.T) {
		t.Parallel()
		// The hidden right copy is skipped by UNQUALIFIED resolution
		// (SemanticAnalyzer.java:468) — no 42702, the left copy answers.
		cols, got := run(t, "SELECT c1 FROM ja JOIN jb USING (c1)")
		if strings.Join(cols, ",") != "C1" {
			t.Fatalf("columns = %v, want [C1]", cols)
		}
		if len(got) != 1 || got[0] != "1" {
			t.Fatalf("rows = %v, want [1]", got)
		}
	})

	t.Run("qualified right copy stays addressable", func(t *testing.T) {
		t.Parallel()
		_, got := run(t, "SELECT jb.c1 FROM ja JOIN jb USING (c1)")
		if len(got) != 1 || got[0] != "1" {
			t.Fatalf("rows = %v, want [1]", got)
		}
	})

	t.Run("ORDER BY bare USING column resolves", func(t *testing.T) {
		t.Parallel()
		_, got := run(t, "SELECT b2 FROM ja JOIN jb USING (c1) ORDER BY c1")
		if len(got) != 1 || got[0] != "b1" {
			t.Fatalf("rows = %v, want [b1]", got)
		}
	})

	t.Run("qualified star over the right leg hides its USING copy", func(t *testing.T) {
		t.Parallel()
		cols, got := run(t, "SELECT jb.* FROM ja JOIN jb USING (c1)")
		if strings.Join(cols, ",") != "B2" {
			t.Fatalf("columns = %v, want [B2]", cols)
		}
		if len(got) != 1 || got[0] != "b1" {
			t.Fatalf("rows = %v, want [b1]", got)
		}
	})

	t.Run("LEFT JOIN USING star hides and pads", func(t *testing.T) {
		t.Parallel()
		cols, got := run(t, "SELECT * FROM ja LEFT JOIN jb USING (c1) ORDER BY c1")
		wantCols := []string{"C1", "A2", "B2"}
		if strings.Join(cols, ",") != strings.Join(wantCols, ",") {
			t.Fatalf("columns = %v, want %v", cols, wantCols)
		}
		if len(got) != 2 || got[0] != "1|a1|b1" || got[1] != "2|a2|<nil>" {
			t.Fatalf("rows = %v, want [1|a1|b1 2|a2|<nil>]", got)
		}
	})

	t.Run("ON join keeps both copies", func(t *testing.T) {
		t.Parallel()
		cols, _ := run(t, "SELECT * FROM ja JOIN jb ON ja.c1 = jb.c1")
		wantCols := []string{"C1", "A2", "C1", "B2"}
		if strings.Join(cols, ",") != strings.Join(wantCols, ",") {
			t.Fatalf("columns = %v, want %v", cols, wantCols)
		}
	})
}
