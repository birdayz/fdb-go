package embedded

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/query"
	"fdb.dev/pkg/relational/core/query/logical"
)

func firstWhereFilter(op logical.LogicalOperator) *logical.LogicalFilter {
	for cur := op; cur != nil; {
		if filter, ok := cur.(*logical.LogicalFilter); ok {
			return filter
		}
		children := cur.Children()
		if len(children) != 1 {
			return nil
		}
		cur = children[0]
	}
	return nil
}

func TestPlanVisitorWherePredicateInstallation(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)

	tests := []struct {
		name       string
		sql        string
		wantExists int
	}{
		{
			name: "comparison",
			sql:  "SELECT * FROM Order WHERE price > 5",
		},
		{
			name: "correlated_exists",
			sql: "SELECT * FROM Order WHERE price > 5 AND " +
				"EXISTS (SELECT 1 FROM Customer WHERE Customer.price = Order.price)",
			wantExists: 1,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			op, err := NewPlanVisitor(md).VisitQuery(parseQuery(t, test.sql))
			if err != nil {
				t.Fatalf("VisitQuery: %v", err)
			}
			filter := firstWhereFilter(op)
			if filter == nil {
				t.Fatalf("WHERE filter missing from logical plan:\n%s", op.Explain(""))
			}
			if filter.Predicate == nil {
				t.Fatalf("WHERE predicate was not installed (text=%q)", filter.PredicateText)
			}
			if got := len(filter.ExistsSubqueries); got != test.wantExists {
				t.Fatalf("EXISTS subqueries = %d, want %d", got, test.wantExists)
			}
		})
	}
}

// A resolver decline is reachable for a syntactically valid but unsupported
// predicate shape. The visitor intentionally retains the canonical text filter
// for diagnostics; the Cascades translator must decline that tree rather than
// translating its input and accidentally executing an unfiltered scan.
func TestPlanVisitorWherePredicateDeclineFailsClosed(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	op, err := NewPlanVisitor(md).VisitQuery(parseQuery(t,
		"SELECT * FROM Order WHERE FROBNICATE(price) = 1"))
	if err != nil {
		t.Fatalf("VisitQuery: %v", err)
	}

	filter := firstWhereFilter(op)
	if filter == nil {
		t.Fatalf("declined WHERE lost its filter:\n%s", op.Explain(""))
	}
	if filter.Predicate != nil {
		t.Fatalf("unsupported predicate unexpectedly constructed: %s", filter.Predicate.Explain())
	}
	if filter.PredicateText == "" {
		t.Fatal("declined WHERE lost its canonical predicate text")
	}

	ref, _, translateErr := query.TranslateToCascadesWithError(op, md)
	if translateErr != nil {
		t.Fatalf("unexpected classified translation error: %v", translateErr)
	}
	if ref != nil {
		t.Fatal("text-only WHERE translated; query could execute without its predicate")
	}
}

func TestInstallFirstWherePredicateFailsClosedWithoutFilter(t *testing.T) {
	t.Parallel()
	err := installFirstWherePredicate(
		logical.NewScan("Order", ""),
		predicates.NewConstantPredicate(predicates.TriTrue),
	)
	if err == nil {
		t.Fatal("expected missing WHERE filter to fail closed")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %T %v, want typed *api.Error", err, err)
	}
	if apiErr.Code != api.ErrCodeUnsupportedQuery {
		t.Fatalf("SQLSTATE = %s, want %s", apiErr.Code, api.ErrCodeUnsupportedQuery)
	}
}
