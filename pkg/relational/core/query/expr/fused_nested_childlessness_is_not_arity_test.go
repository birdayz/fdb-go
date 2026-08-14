package expr_test

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/expr"
	"fdb.dev/pkg/relational/core/query/semantic"
)

type exprCodedResolutionError interface {
	Code() values.ResolutionErrorCode
}

// TestFusedNestedReferencesCarryExactOwners proves RFC-232's replacement for
// the retired childless/lazy carrier contract: local and correlated nested
// references both publish a complete path over an exact QOV. Correlation is an
// owner property; path arity is stated independently by the ordinal vector.
func TestFusedNestedReferencesCarryExactOwners(t *testing.T) {
	t.Parallel()

	tbl := &semantic.StaticTable{
		TableName: semantic.ParseQualifiedName("T", false),
		TableColumns: []semantic.Column{
			{Id: semantic.NewUnquoted("id"), Type: "INT"},
			{
				Id: semantic.NewUnquoted("n"), Type: "RECORD", Nullable: true,
				StructFields: []semantic.Column{
					{Id: semantic.NewUnquoted("sk"), Type: "INT"},
					{Id: semantic.NewUnquoted("co"), Type: "STRING"},
				},
			},
		},
	}
	inner := &semantic.StaticTable{
		TableName:    semantic.ParseQualifiedName("U", false),
		TableColumns: []semantic.Column{{Id: semantic.NewUnquoted("uid"), Type: "INT"}},
	}
	analyzer := semantic.NewAnalyzer(semantic.NewInMemoryCatalog(tbl, inner), false)

	localScope := semantic.NewScope(nil)
	if err := localScope.AddSource(semantic.ScopeSource{
		Table: tbl, Alias: semantic.NewUnquoted("t"), CorrelationName: "T",
	}); err != nil {
		t.Fatal(err)
	}
	local, err := expr.New(analyzer, localScope).ResolveIdentifierPath([]semantic.Identifier{
		semantic.NewUnquoted("n"), semantic.NewUnquoted("sk"),
	})
	if err != nil {
		t.Fatalf("resolve local n.sk: %v", err)
	}

	outerScope := semantic.NewScope(nil)
	if err := outerScope.AddSource(semantic.ScopeSource{
		Table: tbl, Alias: semantic.NewUnquoted("t"), CorrelationName: "T",
	}); err != nil {
		t.Fatal(err)
	}
	innerScope := semantic.NewScope(outerScope)
	if err := innerScope.AddSource(semantic.ScopeSource{
		Table: inner, Alias: semantic.NewUnquoted("u"), CorrelationName: "U",
	}); err != nil {
		t.Fatal(err)
	}
	correlated, err := expr.New(analyzer, innerScope).ResolveIdentifierPath([]semantic.Identifier{
		semantic.NewUnquoted("t"), semantic.NewUnquoted("n"), semantic.NewUnquoted("sk"),
	})
	if err != nil {
		t.Fatalf("resolve correlated t.n.sk: %v", err)
	}

	for name, value := range map[string]values.Value{
		"local": local, "correlated": correlated,
	} {
		value := value
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			field := mustExprField(t, value)
			if got := field.Path().Ordinals(); len(got) != 2 || got[0] != 1 || got[1] != 0 {
				t.Fatalf("path = %v, want [1 0] (N.SK)", got)
			}
			owner := mustExprQOV(t, field.ChildValue())
			if owner.Correlation().Name() != "T" {
				t.Fatalf("owner correlation = %q, want T", owner.Correlation().Name())
			}
			if owner.FlowedType().Code() != values.TypeCodeRecord {
				t.Fatalf("owner flowed type = %v, want exact record", owner.FlowedType())
			}

			// Mutation-sensitive negative proof: changing the root ordinal to an
			// impossible slot is rejected atomically, rather than publishing a
			// partially resolved/lazy carrier.
			mutated, resolveErr := values.ResolveFieldOrdinals(owner, []int{2, 0})
			if mutated != nil {
				t.Fatalf("invalid ordinal mutation returned partial value %T", mutated)
			}
			var coded exprCodedResolutionError
			if !errors.As(resolveErr, &coded) || coded.Code() != values.FieldOutOfRange {
				t.Fatalf("invalid ordinal mutation error = %v, want FieldOutOfRange", resolveErr)
			}
		})
	}
}
