package embedded

import (
	"errors"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/query/logical"
)

// TestIndexAggregatesReachTheTranslatedGraph: MIN_EVER, MAX_EVER and
// BITMAP_CONSTRUCT_AGG translate into the group by's aggregates, as Java's
// function catalog resolves them into the graph queries and index definitions
// share. MAX_EVER reads its index; MIN_EVER has no matching index and cannot
// stream. BITMAP_CONSTRUCT_AGG and MAX both have streaming accumulators.
func TestIndexAggregatesReachTheTranslatedGraph(t *testing.T) {
	t.Parallel()
	const ddl = `CREATE TABLE t1 (id BIGINT, col1 BIGINT, col2 BIGINT, PRIMARY KEY (id))
		CREATE INDEX mx AS SELECT max_ever(col2) FROM t1 GROUP BY col1
		CREATE INDEX bm AS SELECT bitmap_construct_agg(bitmap_bit_position(id)) AS bitmap, col1, bitmap_bucket_offset(id) AS offset FROM t1 GROUP BY col1, bitmap_bucket_offset(id)`
	for _, tc := range []struct {
		sql     string
		fn      expressions.AggregateFunction
		planned bool
	}{
		{"SELECT max_ever(col2) FROM t1 GROUP BY col1", expressions.AggMaxEver, true},
		{"SELECT min_ever(col2) FROM t1 GROUP BY col1", expressions.AggMinEver, false},
		{"SELECT col1, bitmap_construct_agg(bitmap_bit_position(id)) AS bitmap, bitmap_bucket_offset(id) AS offset " +
			"FROM t1 GROUP BY col1, bitmap_bucket_offset(id)", expressions.AggBitmapConstructAgg, true},
		{"SELECT max(col2) FROM t1 GROUP BY col1", expressions.AggMax, true},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			t.Parallel()
			var found []expressions.AggregateFunction
			observe := func(_ logical.LogicalOperator, ref *expressions.Reference) {
				var walk func(r *expressions.Reference)
				walk = func(r *expressions.Reference) {
					for _, m := range r.Members() {
						if gb, ok := m.(*expressions.GroupByExpression); ok {
							for _, a := range gb.GetAggregates() {
								found = append(found, a.Function)
							}
						}
						for _, q := range m.GetQuantifiers() {
							walk(q.GetRangesOver())
						}
					}
				}
				walk(ref)
			}
			plan, _, err := planPhysicalForTestObserved(tc.sql, ddl, nil, false, nil, plannerOptionsFrom(nil), observe)
			if len(found) != 1 || found[0] != tc.fn {
				t.Fatalf("the translated group by holds %v, want [%v]", found, tc.fn)
			}
			if tc.planned {
				if err != nil || plan == nil {
					t.Fatalf("did not plan: %v", err)
				}
				if tc.fn == expressions.AggMaxEver && !strings.Contains(plan.Explain(), "AggregateIndex(MAX_EVER, MX") {
					t.Fatalf("max_ever is not served by its index: %s", plan.Explain())
				}
				return
			}
			var ae *api.Error
			if plan != nil || !errors.As(err, &ae) || ae.Code != api.ErrCodeUnsupportedQuery {
				t.Fatalf("planned %v, err %v; want 0AF00 with no plan", plan, err)
			}
		})
	}
}
