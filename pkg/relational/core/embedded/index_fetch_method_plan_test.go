package embedded

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/indexfetch"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/api"
)

const indexFetchMethodDDL = "CREATE TABLE T (id BIGINT, a BIGINT, b BIGINT, c STRING, PRIMARY KEY (id))" +
	" CREATE INDEX idx_a ON T(a)" +
	" CREATE INDEX idx_ab ON T(a, b)" +
	" CREATE TABLE U (id BIGINT, t_id BIGINT, v BIGINT, PRIMARY KEY (id))" +
	" CREATE INDEX idx_t ON U(t_id)" +
	" CREATE INDEX sum_v AS SELECT SUM(v) FROM U GROUP BY t_id"

// indexScanUses lists every index scan in plan as "kind index method": a bare
// fetching scan, the inner of a covering scan, or an aggregate index's inner.
func indexScanUses(plan plans.RecordQueryPlan) []string {
	var out []string
	plans.Walk(plan, func(node plans.RecordQueryPlan) bool {
		var kind string
		var idx *plans.RecordQueryIndexPlan
		switch p := node.(type) {
		case *plans.RecordQueryIndexPlan:
			kind, idx = "scan", p
		case *plans.RecordQueryCoveringIndexPlan:
			kind, idx = "covering", p.GetIndexPlan()
		case *plans.RecordQueryCoveringIndexValuePlan:
			kind, idx = "covering-value", p.GetIndexPlan()
		case *plans.RecordQueryAggregateIndexPlan:
			kind, idx = "aggregate", p.GetIndexPlan()
		default:
			return true
		}
		out = append(out, fmt.Sprintf("%s %s %s", kind, idx.GetIndexName(), idx.GetIndexFetchMethod()))
		return true
	})
	slices.Sort(out)
	return out
}

func indexFetchMethodOptions(m api.IndexFetchMethod) *api.Options {
	return api.NewOptionsBuilder().Set(api.OptIndexFetchMethod, m).Build()
}

// TestIndexFetchMethod_PlansCarryTheConfiguredMethod is Java's stamping, plan
// by plan, for every value of INDEX_FETCH_METHOD: value scans (bare, reverse,
// both sides of a join, the correlated EXISTS probe, DELETE) carry the
// configured method, and so do covering scans' inners (IN, a covered
// projection, a streaming aggregate, UPDATE's deduplicated scan), which Java
// also stamps because they are BY_VALUE but executes from entries alone. An
// aggregate index's BY_GROUP scan carries SCAN_AND_FETCH
// (RecordQueryIndexPlan.java:248-251). The option never changes the plan: the
// EXPLAIN text, which does not render the method, is identical under all three.
func TestIndexFetchMethod_PlansCarryTheConfiguredMethod(t *testing.T) {
	t.Parallel()
	cases := []struct {
		sql  string
		dml  bool
		want []string // "kind index", "aggregate ..." always SCAN_AND_FETCH
	}{
		{sql: "SELECT * FROM T WHERE a = 1", want: []string{"scan IDX_A"}},
		{sql: "SELECT * FROM T WHERE a > 1 ORDER BY a DESC", want: []string{"scan IDX_A"}},
		{sql: "SELECT * FROM T WHERE a IN (1, 2)", want: []string{"covering IDX_A"}},
		{sql: "SELECT a, b FROM T WHERE a = 1", want: []string{"covering IDX_AB"}},
		{sql: "SELECT a, COUNT(*) FROM T GROUP BY a", want: []string{"covering IDX_A"}},
		{sql: "SELECT t_id, SUM(v) FROM U GROUP BY t_id", want: []string{"aggregate SUM_V"}},
		{sql: "SELECT T.c, U.v FROM T, U WHERE T.a = 1 AND U.t_id = T.id", want: []string{"scan IDX_A", "scan IDX_T"}},
		{sql: "SELECT id FROM T WHERE EXISTS (SELECT id FROM U WHERE U.t_id = T.id)", want: []string{"scan IDX_A", "scan IDX_T"}},
		{sql: "SELECT * FROM T WHERE id = 1"},
		{sql: "UPDATE T SET c = 'x' WHERE a = 1", dml: true, want: []string{"covering IDX_A"}},
		{sql: "DELETE FROM T WHERE a = 1", dml: true, want: []string{"scan IDX_A"}},
	}
	methods := map[api.IndexFetchMethod]indexfetch.Method{
		api.IndexFetchScanAndFetch:               indexfetch.ScanAndFetch,
		api.IndexFetchUseRemoteFetch:             indexfetch.UseRemoteFetch,
		api.IndexFetchUseRemoteFetchWithFallback: indexfetch.UseRemoteFetchWithFallback,
	}
	plan := func(sql string, dml bool, opts *api.Options) plans.RecordQueryPlan {
		t.Helper()
		var p plans.RecordQueryPlan
		var err error
		if dml {
			p, err = PlanPhysicalDMLForTestWithOptions(sql, indexFetchMethodDDL, nil, opts)
		} else {
			p, err = PlanPhysicalForTestWithOptions(sql, indexFetchMethodDDL, nil, opts)
		}
		if err != nil {
			t.Fatalf("plan %q: %v", sql, err)
		}
		return p
	}
	for _, c := range cases {
		baseline := plan(c.sql, c.dml, nil)
		for option, method := range methods {
			got := plan(c.sql, c.dml, indexFetchMethodOptions(option))
			if got.Explain() != baseline.Explain() {
				t.Errorf("%q under %v plans %s, unset plans %s", c.sql, option, got.Explain(), baseline.Explain())
			}
			want := make([]string, 0, len(c.want))
			for _, use := range c.want {
				carried := method
				if strings.HasPrefix(use, "aggregate ") {
					carried = indexfetch.ScanAndFetch
				}
				want = append(want, fmt.Sprintf("%s %s", use, carried))
			}
			slices.Sort(want)
			if uses := indexScanUses(got); !slices.Equal(uses, want) {
				t.Errorf("%q under %v: index scans %v, want %v (plan %s)", c.sql, option, uses, want, got.Explain())
			}
		}
		// Unset keeps Go's SCAN_AND_FETCH default (DIVERGENCES.md).
		for _, use := range indexScanUses(baseline) {
			if !strings.HasSuffix(use, " "+indexfetch.ScanAndFetch.String()) {
				t.Errorf("%q without the option carries %q, want SCAN_AND_FETCH", c.sql, use)
			}
		}
	}
}
