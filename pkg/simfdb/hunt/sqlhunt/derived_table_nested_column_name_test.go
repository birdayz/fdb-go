package sqlhunt

import (
	"context"
	"testing"
)

// TestDerivedTableNamesANestedReferenceByItsLeaf pins that a derived table
// whose projection is a NESTED column reference exposes that column to the
// enclosing query under the name the enclosing query resolves.
//
// Two authorities describe the same row here, and they used to disagree. The
// semantic scope registers `(SELECT A.B, C AS Q, W.X …) AS u` as U(B, Q, X) —
// it must, because that is what `u.x` and `WHERE b < 8` resolve against. The
// plan flowed U as RECORD(B, Q, A.W.X), because the dotted rendering is the
// internal slot key that keeps two members of one struct root apart inside a
// projection. Java agrees with the scope: its plan for this query is
// `MAP (_.B AS B, _.C AS Q, _.W.X AS X)`.
//
// Nothing compared the two spellings as long as every U-rooted value was
// rewritten onto the carrier before execution, so the disagreement sat latent
// and green. It surfaces as a runtime `edge lookup U: read as
// RECORD(B:INT,Q:DOUBLE,X:INT), declared RECORD(B:INT,Q:DOUBLE,A.W.X:INT)` on
// valid SQL the moment a U-rooted value survives to the binder.
//
// The WHERE is what makes the shape lethal and is therefore not decoration:
// without it the projection resolves and the row types are never compared, so
// `SELECT u.x FROM (…) AS u` passed throughout. The filter is what puts a
// U-rooted predicate in front of the binder.
func TestDerivedTableNamesANestedReferenceByItsLeaf(t *testing.T) {
	t.Parallel()
	h, err := qcNewHarness(940001)
	if err != nil {
		t.Fatalf("harness: %v", err)
	}
	defer h.close()

	const inner = "SELECT A.B, C AS Q, W.X FROM VALUES (1, 2.0, (3, 4, 'foo')), " +
		"(10, 90.2, (5, 6.0, 'bar')) AS A(B, C, W(X, Y, Z))"

	for _, probe := range []struct {
		what     string
		query    string
		wantCols []string
		wantRows int
	}{
		{
			"a filter over the derived table puts a U-rooted predicate at the binder",
			"SELECT * FROM (" + inner + ") AS u WHERE b < 8",
			[]string{"B", "Q", "X"},
			1,
		},
		{
			"the qualified read of the nested column resolves by its leaf",
			"SELECT u.x FROM (" + inner + ") AS u",
			[]string{"X"},
			2,
		},
		{
			"and so does the unqualified one",
			"SELECT x FROM (" + inner + ") AS u WHERE x > 3",
			[]string{"X"},
			1,
		},
	} {
		rows, queryErr := h.db.QueryContext(context.Background(), probe.query)
		if queryErr != nil {
			t.Errorf("%s: %v\n  query: %s", probe.what, queryErr, probe.query)
			continue
		}
		cols, colErr := rows.Columns()
		if colErr != nil {
			rows.Close() //nolint:errcheck
			t.Errorf("%s: columns: %v", probe.what, colErr)
			continue
		}
		n := 0
		for rows.Next() {
			n++
		}
		rowsErr := rows.Err()
		rows.Close() //nolint:errcheck
		if rowsErr != nil {
			t.Errorf("%s: rows: %v", probe.what, rowsErr)
			continue
		}
		if len(cols) != len(probe.wantCols) {
			t.Errorf("%s: columns %v, want %v", probe.what, cols, probe.wantCols)
			continue
		}
		for i := range cols {
			if cols[i] != probe.wantCols[i] {
				t.Errorf("%s: column %d is %q, want %q — a dotted label here means the "+
					"internal slot key crossed into SQL", probe.what, i, cols[i], probe.wantCols[i])
			}
		}
		if n != probe.wantRows {
			t.Errorf("%s: %d rows, want %d", probe.what, n, probe.wantRows)
		}
	}
}
