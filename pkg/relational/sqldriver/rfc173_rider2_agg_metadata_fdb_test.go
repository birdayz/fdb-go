package sqldriver_test

import (
	"fmt"
	"testing"
)

// TestFDB_RFC173Rider2_AggOutputMetadata pins Java-parity for the RESULT-SET
// column METADATA (name + type) of a GROUP BY, closing the item-1 c4 probe's
// two live-verified (4.12.11.0) drifts:
//
//	(a) a QUALIFIED group key (`d.dname`) labels the output column the BARE
//	    `DNAME`, never `D.DNAME`. Java clears the qualifier on the top-level
//	    projection (Expression.clearQualifier); Go now carries the bare name as
//	    ColumnDef.Label (display) while keeping the qualified Name as the datum
//	    lookup key the aggregate cursor writes — so the value still resolves.
//	(b) a group key from a FAR join leg reports its real column TYPE (STRING),
//	    not UNKNOWN. Java reads the type off the flowed join-output record; Go
//	    now resolves the group key against ALL join-leaf descriptors instead of
//	    only the first (which missed a second-leg key).
//
// (c) is DELIBERATELY not changed: Go labels an unaliased `COUNT(*)` with the
// descriptive `COUNT(*)` where Java mints a positional `_N`. The plandiff
// ConformColumns harness already accepts a descriptive Go label against Java's
// anonymous `_N` when the type matches, so the descriptive label is a
// conformance-blessed read-side nicety (wire-neutral) — this test pins it stays
// descriptive so an accidental change is caught.
func TestFDB_RFC173Rider2_AggOutputMetadata(t *testing.T) {
	t.Parallel()
	db, ctx := gojDB(t, "rider2meta")

	// colMeta returns "NAME|TYPE" per result column, in order.
	colMeta := func(t *testing.T, q string) []string {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		cts, err := rows.ColumnTypes()
		if err != nil {
			t.Fatalf("columnTypes %q: %v", q, err)
		}
		out := make([]string, len(cts))
		for i, ct := range cts {
			out[i] = ct.Name() + "|" + ct.DatabaseTypeName()
		}
		return out
	}
	assertMeta := func(t *testing.T, q string, want []string) {
		t.Helper()
		got := colMeta(t, q)
		if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", want) {
			t.Fatalf("column metadata %q\n got=%v\nwant=%v", q, got, want)
		}
	}

	t.Run("(a)+(b) qualified group key over join: bare label + resolved far-leg type", func(t *testing.T) {
		// d.dname: label BARE "DNAME" (not "D.DNAME"), type STRING (dname lives on
		// the far dept leg — resolved, not UNKNOWN).
		assertMeta(t, `SELECT d.dname, COUNT(*) FROM emp AS e INNER JOIN dept AS d ON e.did = d.did GROUP BY d.dname`,
			[]string{"DNAME|STRING", "COUNT(*)|BIGINT"})
	})

	t.Run("bare group key over join also resolves its far-leg type", func(t *testing.T) {
		assertMeta(t, `SELECT dname, COUNT(*) FROM emp AS e INNER JOIN dept AS d ON e.did = d.did GROUP BY dname`,
			[]string{"DNAME|STRING", "COUNT(*)|BIGINT"})
	})

	t.Run("aggregate column: bare-key fix does not disturb the aggregate label/type", func(t *testing.T) {
		// Control: the group-key label/type fix must leave the aggregate column
		// untouched. Go keeps its descriptive `MAX(E.SALARY)` label (a Go read-
		// side nicety vs Java's `_1`) and the first-leg-resolved BIGINT type.
		assertMeta(t, `SELECT d.dname, MAX(e.salary) FROM emp AS e INNER JOIN dept AS d ON e.did = d.did GROUP BY d.dname`,
			[]string{"DNAME|STRING", "MAX(E.SALARY)|BIGINT"})
	})

	t.Run("values still flow: the qualified datum key resolves (not NULL)", func(t *testing.T) {
		// The load-bearing pin behind the Label/Name split: the display label is
		// bare but the row is still keyed by the qualified Name, so the grouped
		// values are correct — never the NULLs a naive qualifier-strip would cause.
		got := gojRead(t, ctx, db,
			"SELECT d.dname, COUNT(*), MAX(e.salary) FROM emp AS e INNER JOIN dept AS d ON e.did = d.did GROUP BY d.dname ORDER BY d.dname")
		want := []gojRow{{"eng", 2, 100}, {"sales", 1, 80}}
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("grouped values over join: got %+v, want %+v", got, want)
		}
	})

	t.Run("single-table group key control keeps its bare label and type", func(t *testing.T) {
		assertMeta(t, `SELECT ename, COUNT(*) FROM emp AS e GROUP BY ename`,
			[]string{"ENAME|STRING", "COUNT(*)|BIGINT"})
	})

	t.Run("(c) unaliased COUNT(*) keeps its descriptive label (accepted by ConformColumns)", func(t *testing.T) {
		// Intentional divergence: Java mints `_1`, Go keeps the informative
		// `COUNT(*)`; the conformance harness accepts it (type must still match).
		// Pinned so an accidental relabel is caught.
		got := colMeta(t, `SELECT dname, COUNT(*) FROM emp AS e INNER JOIN dept AS d ON e.did = d.did GROUP BY dname`)
		if len(got) != 2 || got[1] != "COUNT(*)|BIGINT" {
			t.Fatalf("unaliased COUNT(*): got %v, want the descriptive COUNT(*)|BIGINT label", got)
		}
	})
}
