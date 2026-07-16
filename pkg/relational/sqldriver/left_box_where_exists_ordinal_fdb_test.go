package sqldriver_test

// A single-source LEFT box under WHERE-EXISTS resolves POSITIONALLY: the
// below-FOD correlation predicate bakes to an ofOrdinal read over the box's
// merged row, the box row flows through the identity existential FlatMap
// unchanged, and the result set reads it per-slot.
//
// The duplicate-name test below is the proof the ordinal path fired (correct
// rows alone don't distinguish it, since a name-keyed lookup is also correct
// when column names don't collide): `SELECT *` over a LEFT box whose two legs
// share column names keeps EACH leg's value in its own slot, while a
// name-keyed lookup would conflate them last-leg-wins. The la/lb ID+GID
// collision is load-bearing.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestFDB_LeftBoxWhereExistsOrdinal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/i2c4_lift"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	const tmpl = "i2c4_lift_tmpl"
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE "+tmpl+
		" CREATE TABLE la (id BIGINT, gid BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE lb (id BIGINT, gid BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE lg (gid BIGINT, label STRING, PRIMARY KEY (gid))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE "+tmpl); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// la: group 10 (has lb match + lg match), group 20 (has lb match, NO lg),
	// group 30 (NO lb match → null-extends, NO lg).
	if _, err := db.ExecContext(ctx, "INSERT INTO la VALUES (1, 10), (2, 20), (3, 30)"); err != nil {
		t.Fatalf("seed la: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO lb VALUES (100, 10), (101, 20)"); err != nil {
		t.Fatalf("seed lb: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO lg VALUES (10, 'a')"); err != nil {
		t.Fatalf("seed lg: %v", err)
	}

	allRows := func(t *testing.T, q string) []string {
		t.Helper()
		r, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query: %v\n  sql: %s", err, q)
		}
		defer r.Close()
		cols, err := r.Columns()
		if err != nil {
			t.Fatalf("columns: %v", err)
		}
		var got []string
		for r.Next() {
			vals := make([]any, len(cols))
			for i := range vals {
				vals[i] = new(any)
			}
			if err := r.Scan(vals...); err != nil {
				t.Fatalf("scan: %v", err)
			}
			parts := make([]string, len(cols))
			for i, v := range vals {
				cell := *(v.(*any))
				if cell == nil {
					parts[i] = "<NULL>"
				} else {
					parts[i] = fmt.Sprint(cell)
				}
			}
			got = append(got, strings.Join(parts, "|"))
		}
		if err := r.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		sort.Strings(got)
		return got
	}
	want := func(t *testing.T, label, q string, exp []string) {
		t.Helper()
		got := allRows(t, q)
		if len(got) != len(exp) {
			t.Errorf("%s: rows = %v, want %v\n  sql: %s", label, got, exp, q)
			return
		}
		for i := range exp {
			if got[i] != exp[i] {
				t.Errorf("%s: rows = %v, want %v\n  sql: %s", label, got, exp, q)
				return
			}
		}
	}

	// (A) The duplicate-name test through the LEFT box + EXISTS: the four
	// columns [a.ID a.GID b.ID b.GID] each in their own slot. Group 10
	// passes EXISTS (lg has gid 10). Ordinal keeps a.ID=1 AND b.ID=100
	// distinct; a name-keyed lookup would conflate them last-leg-wins.
	want(t, "A/star+EXISTS",
		"SELECT * FROM la a LEFT JOIN lb b ON a.gid = b.gid WHERE EXISTS (SELECT 1 FROM lg g WHERE g.gid = a.gid)",
		[]string{"1|10|100|10"})

	// (B) NOT EXISTS: groups 20 and 30. Group 30 NULL-extends (no lb match) —
	// the LEFT null-extension must survive (b columns NULL).
	want(t, "B/star+NOT EXISTS",
		"SELECT * FROM la a LEFT JOIN lb b ON a.gid = b.gid WHERE NOT EXISTS (SELECT 1 FROM lg g WHERE g.gid = a.gid)",
		[]string{"2|20|101|20", "3|30|<NULL>|<NULL>"})

	// (C) Correlation into the PRESERVED (left) leg, both polarities — the box
	// quantifier is named by the rightmost leg, so this binds under the other
	// alias through the ordinal window.
	want(t, "C/EXISTS-preserved",
		"SELECT a.id, b.id FROM la a LEFT JOIN lb b ON a.gid = b.gid WHERE EXISTS (SELECT 1 FROM lg g WHERE g.gid = a.gid)",
		[]string{"1|100"})

	// (D) Qualified projection both polarities — the name model resolved these
	// via qualified keys; they must stay identical under the ordinal seed.
	want(t, "D/qualified NOT EXISTS",
		"SELECT a.id, b.id FROM la a LEFT JOIN lb b ON a.gid = b.gid WHERE NOT EXISTS (SELECT 1 FROM lg g WHERE g.gid = a.gid)",
		[]string{"2|101", "3|<NULL>"})

	// (F) The FAST-PATH WRONG-LEG regression. Join on ID, correlate EXISTS on
	// GID — GID DIFFERS between the legs, so reading the wrong leg flips the
	// answer (unlike a join ON gid, where the legs share the value and a
	// wrong-leg read stays correct by luck). The existential fast path built
	// QOV(box).<bareCol> = the box's rightmost leg
	// (lb), reading b.gid instead of a.gid; on a colliding column name that is
	// wrong rows. la(1,gid=100)/la(2,gid=200), lb(1,gid=999), lg(gid=100): only
	// a.gid=100 matches lg, so the answer is {1}; the wrong-leg read (b.gid=999)
	// returned {}. Fixed by declining the fast path to the below-FOD rebase.
	{
		if _, err := db.ExecContext(ctx, "INSERT INTO lb VALUES (1, 999)"); err != nil {
			t.Fatalf("seed lb extra: %v", err)
		}
		// la already has (1,10),(2,20),(3,30); add distinct-gid rows for the
		// ID-join discriminator using a fresh pair of ids.
		if _, err := db.ExecContext(ctx, "INSERT INTO la VALUES (7, 100), (8, 200)"); err != nil {
			t.Fatalf("seed la extra: %v", err)
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO lb VALUES (7, 999)"); err != nil {
			t.Fatalf("seed lb extra2: %v", err)
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO lg VALUES (100, 'x')"); err != nil {
			t.Fatalf("seed lg extra: %v", err)
		}
		// a.id=7: a.gid=100 (lg has 100 → EXISTS), b.gid=999 (no lg). a.id=8:
		// a.gid=200 (no lg). Correct (reads a.gid): {7}; wrong-leg (b.gid): {}.
		want(t, "F/fast-path buried-leg (join on id, EXISTS on gid)",
			"SELECT a.id FROM la a LEFT JOIN lb b ON a.id = b.id WHERE a.id >= 7 AND EXISTS (SELECT 1 FROM lg g WHERE g.gid = a.gid)",
			[]string{"7"})
	}

	// (E) EXPLAIN determinism ×10, both polarities.
	for _, q := range []string{
		"SELECT * FROM la a LEFT JOIN lb b ON a.gid = b.gid WHERE EXISTS (SELECT 1 FROM lg g WHERE g.gid = a.gid)",
		"SELECT * FROM la a LEFT JOIN lb b ON a.gid = b.gid WHERE NOT EXISTS (SELECT 1 FROM lg g WHERE g.gid = a.gid)",
	} {
		var first string
		if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&first); err != nil {
			t.Fatalf("explain: %v", err)
		}
		for i := 0; i < 9; i++ {
			var again string
			if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&again); err != nil {
				t.Fatalf("explain %d: %v", i, err)
			}
			if again != first {
				t.Errorf("EXPLAIN nondeterministic:\n  first: %s\n  again: %s", first, again)
				break
			}
		}
	}
}
