package embedded

import (
	"errors"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// TestAsSelectIndex_RejectsWindowedAggregate pins the front-end pre-pass on the
// index-DDL surface.
//
// The OVER clause does not survive aggregate lowering — that is precisely why
// the query path checks the PARSE TREE before it builds anything. Index DDL
// planned its SELECT with the plan visitor directly and never ran the pre-pass,
// so `SUM(v) OVER (PARTITION BY g)` lowered to a bare `SUM(v)` and the
// generator PERSISTED a global sum index. That is worse than a wrong query
// result: the wrong semantics are written into the schema template and every
// subsequent read of that index is wrong.
//
// This is the dimension the existing AS-SELECT aggregate goldens could not
// probe — every one of them declares an aggregate WITHOUT an OVER clause, so
// they agree with the buggy and the fixed generator alike.
func TestAsSelectIndex_RejectsWindowedAggregate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		ddl  string
	}{
		{"sum_partition_by", "CREATE INDEX gidx AS SELECT SUM(a1) OVER (PARTITION BY a2) FROM t1"},
		{"sum_empty_over", "CREATE INDEX gidx AS SELECT SUM(a1) OVER () FROM t1"},
		{"count_partition_by", "CREATE INDEX gidx AS SELECT COUNT(a1) OVER (PARTITION BY a2) FROM t1"},
		{"max_partition_by_with_group", "CREATE INDEX gidx AS SELECT MAX(a1) OVER (PARTITION BY a2) FROM t1 GROUP BY a2"},
		{"windowed_in_where", "CREATE INDEX gidx AS SELECT a1 FROM t1 WHERE a1 > SUM(a2) OVER (PARTITION BY a3)"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tmpl, err := buildSchemaTemplateFromDDL(asSelectGoldenDDL + "\n" + tc.ddl)
			if err == nil {
				var got string
				for _, idx := range tmpl.Underlying().GetAllIndexes() {
					if idx.Name == "GIDX" {
						got = idx.Type
					}
				}
				t.Fatalf("%s was ACCEPTED and persisted an index of type %q — the OVER "+
					"clause was silently dropped and the stored index has semantics "+
					"unrelated to the declaration; it must be rejected", tc.ddl, got)
			}
			var apiErr *api.Error
			if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUnsupportedQuery {
				t.Fatalf("want %s (unsupported query), got %v", api.ErrCodeUnsupportedQuery, err)
			}
			if !strings.Contains(err.Error(), "windowed aggregate") {
				t.Fatalf("rejection must name the construct; got %v", err)
			}
		})
	}
}

// TestAsSelectIndex_NonWindowedAggregateStillBuilds is the negative half: the
// pre-pass keys on the OVER clause, not on the aggregate, so ordinary aggregate
// index definitions must still build. Without this a "fix" that rejects every
// aggregate would pass the test above.
func TestAsSelectIndex_NonWindowedAggregateStillBuilds(t *testing.T) {
	t.Parallel()

	for _, ddl := range []string{
		"CREATE INDEX gidx AS SELECT a2, SUM(a1) FROM t1 GROUP BY a2",
		"CREATE INDEX gidx AS SELECT a2, COUNT(a1) FROM t1 GROUP BY a2",
		"CREATE INDEX gidx AS SELECT a1 FROM t1 ORDER BY a1",
	} {
		if _, err := buildSchemaTemplateFromDDL(asSelectGoldenDDL + "\n" + ddl); err != nil {
			t.Errorf("%s must still build — the pre-pass keys on OVER, not on the "+
				"aggregate itself: %v", ddl, err)
		}
	}
}
