package embedded

import (
	"errors"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
)

const sortPullupSchema = `
CREATE TABLE T (
  id BIGINT NOT NULL,
  a BIGINT,
  b BIGINT,
  c BIGINT,
  PRIMARY KEY (id)
)
`

// TestGroupedOrderBy_ComputedKeyPullsUpToOutputSlot pins the RFC-180 Y3
// sort-key pull-up: a computed ORDER BY key over a grouped select's reshaping
// projection binds to the projection OUTPUT slot (Java's
// OrderByExpression.pullUp onto the child's result value). Before the fix the
// key kept its source-scope leaf ordinals and the enforcer sort above the
// projection read a foreign slot of the projected row — a silent mis-sort when
// the stale ordinal landed in range (`ORDER BY b + 1` sorted by SUM(c)), an
// ordinal-model malformed-plan error when it didn't (`ORDER BY a + b` faulted
// on B). Row-level pins live in yamsql group_by_proj_expr.yaml; this pins that
// the shapes PLAN cleanly.
func TestGroupedOrderBy_ComputedKeyPullsUpToOutputSlot(t *testing.T) {
	t.Parallel()
	for _, sql := range []string{
		"SELECT a + b, MAX(c) FROM T GROUP BY a, b ORDER BY a + b",
		"SELECT b + 1, SUM(c) FROM T GROUP BY b ORDER BY b + 1",
		"SELECT SUM(c), a + 1 FROM T GROUP BY a ORDER BY a + 1",
	} {
		if _, err := PlanQueryForTest(sql, sortPullupSchema, nil); err != nil {
			t.Errorf("plan %q: %v", sql, err)
		}
	}
}

// TestGroupedOrderBy_UnderivableKeyDeclinesTyped pins the correct-or-loud
// guard: an ORDER BY expression over a grouped reshaping projection that is
// NOT one of the SELECT-list outputs cannot be evaluated against the projected
// row — Java widens the select with the missing expression
// (LogicalOperator.generateSelect, remainingOrderByExpressions branch; the
// widening port is the RFC-180 booked follow-up). Until then the translator
// must decline TYPED, never emit a plan whose sort key reads a foreign slot.
func TestGroupedOrderBy_UnderivableKeyDeclinesTyped(t *testing.T) {
	t.Parallel()
	_, err := PlanQueryForTest(
		"SELECT a + b, MAX(c) FROM T GROUP BY a, b ORDER BY a - b", sortPullupSchema, nil)
	if err == nil {
		t.Fatal("want typed decline for underivable grouped ORDER BY key, got nil " +
			"(if the Java-style select-widening landed, replace this pin with a row-level one)")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUnsupportedQuery {
		t.Fatalf("want *api.Error with ErrCodeUnsupportedQuery (0AF00), got %v", err)
	}
	if !strings.Contains(apiErr.Message, "not derivable from the SELECT list") {
		t.Fatalf("want the pull-up decline diagnostic, got %q", apiErr.Message)
	}
}
