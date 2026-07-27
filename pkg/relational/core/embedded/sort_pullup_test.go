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

// The CQ-5 output Project is deliberately after ORDER BY, so a sort expression
// over grouped keys need not be selected to remain derivable. It evaluates on
// the complete private aggregate row, then the final Project hides it.
func TestGroupedOrderBy_NonSelectedKeyExpressionPlansBeforeOutputProject(t *testing.T) {
	t.Parallel()
	if _, err := PlanQueryForTest(
		"SELECT a + b, MAX(c) FROM T GROUP BY a, b ORDER BY a - b", sortPullupSchema, nil); err != nil {
		t.Fatalf("non-selected grouped ORDER BY expression should plan on the private aggregate row: %v", err)
	}
}

func TestGroupedOrderBy_UngroupedQualifiedColumnPreservesDiagnostic(t *testing.T) {
	t.Parallel()
	_, err := PlanQueryForTest(
		"SELECT t.b FROM T t GROUP BY t.a ORDER BY t.b", sortPullupSchema, nil)
	if err == nil {
		t.Fatal("expected grouped ORDER BY validation error")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeGroupingError {
		t.Fatalf("error = %v, want SQLSTATE 42803", err)
	}
	if !strings.Contains(apiErr.Error(), `"T.B"`) {
		t.Fatalf("error = %v, want parse-qualified column diagnostic T.B", apiErr)
	}
}
