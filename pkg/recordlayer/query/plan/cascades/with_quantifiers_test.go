package cascades

import (
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func mustWithQuantifiers(
	t testing.TB,
	expression expressions.RelationalExpression,
	quantifiers []expressions.Quantifier,
) expressions.RelationalExpression {
	t.Helper()
	rebuilt, err := expression.WithQuantifiers(quantifiers)
	if err != nil {
		t.Fatalf("WithQuantifiers() unexpected error: %v", err)
	}
	if rebuilt == nil {
		t.Fatal("WithQuantifiers() returned a nil expression without an error")
	}
	return rebuilt
}

func requireTestQuantifierArity(testDouble string, got, want int) error {
	if got == want {
		return nil
	}
	return fmt.Errorf("%w: %s requires %d, got %d", expressions.ErrQuantifierArity, testDouble, want, got)
}

func mustExistentialAlias(
	t testing.TB,
	alias values.CorrelationIdentifier,
	flowed ...values.Type,
) *predicates.ExistentialValuePredicate {
	t.Helper()
	typ := values.NullableLong
	if len(flowed) > 1 {
		t.Fatalf("mustExistentialAlias accepts at most one flowed type, got %d", len(flowed))
	}
	if len(flowed) == 1 {
		typ = flowed[0]
	}
	predicate, err := predicates.NewExistentialAlias(alias, typ)
	if err != nil {
		t.Fatalf("NewExistentialAlias(%q, %v): %v", alias, typ, err)
	}
	return predicate
}
