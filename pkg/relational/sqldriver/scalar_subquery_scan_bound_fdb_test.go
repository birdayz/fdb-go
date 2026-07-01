package sqldriver_test

import "testing"

// TestFDB_ScalarSubqueryAsScanBound pins that an uncorrelated scalar subquery
// pushed into a PK/index scan as a range bound is evaluated with its pre-computed
// result, not NULL. Bug: scanComparisonsToTupleRange got the bare
// *EvaluationContext, but ScalarSubqueryValue.Evaluate only reads its result from
// a *RowEvalContext — so `WHERE id = (SELECT MIN(id) FROM t2)` built an `id = NULL`
// bound and returned 0 rows. Fixed via evalCtx.RowContext(nil) for scan bounds.
func TestFDB_ScalarSubqueryAsScanBound(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	db, ctx := rfc128DB(t, "ssq_scan_bound")
	for _, c := range []struct {
		q    string
		want int64
	}{
		{"SELECT id FROM t2 WHERE id = (SELECT MIN(id) FROM t2)", 1},
		{"SELECT id FROM t2 WHERE id = (SELECT MAX(id) FROM t2)", 8},
	} {
		got := getInts(t, ctx, db, c.q)
		if len(got) != 1 || got[0] != c.want {
			t.Errorf("%s = %v, want [%d] (scalar-subquery scan bound must not resolve to NULL)", c.q, got, c.want)
		}
	}
}
