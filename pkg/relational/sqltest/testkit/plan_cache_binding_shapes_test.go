package testkit

import (
	"database/sql"
	"database/sql/driver"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/core/embedded"
)

// Every binding shape plans, in production's bound-literal form, for both of
// its bindings; the FDB table runs only in test-full.
func TestPlanCacheBindingShapesPlan(t *testing.T) {
	t.Parallel()
	if len(PlanCacheBindingShapes) < 16 {
		t.Fatalf("binding-shape table shrank to %d rows", len(PlanCacheBindingShapes))
	}
	for _, tc := range PlanCacheBindingShapes {
		for _, args := range [][]any{tc.First, tc.Second} {
			named := make([]driver.NamedValue, len(args))
			for i, arg := range args {
				named[i] = driver.NamedValue{Ordinal: i + 1, Value: arg}
				if n, ok := arg.(sql.NamedArg); ok {
					named[i] = driver.NamedValue{Name: n.Name, Ordinal: i + 1, Value: n.Value}
				}
			}
			if _, err := embedded.PlanPhysicalForTestWithBoundLiterals(tc.Query, PlanCacheBindingShapesSchema, named, nil); err != nil {
				t.Errorf("%s %v: %v", tc.Name, args, err)
			}
		}
	}
}

// Pins the lexer fact the coalesce row works around: `/0` is one identifier
// token, so `1/0` inside a call is a syntax error in both engines.
func TestPlanCacheBindingShapesUnspacedDivision(t *testing.T) {
	t.Parallel()
	_, err := embedded.PlanPhysicalForTestWithBoundLiterals("SELECT id FROM T WHERE COALESCE(5, 1/0) > 4",
		PlanCacheBindingShapesSchema, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "42601") {
		t.Fatalf("COALESCE(5, 1/0): err=%v, want 42601", err)
	}
}
