package embedded

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/query/logical"
	"fdb.dev/pkg/relational/core/query/semantic"
)

func TestBoundAdmissionKeepsLexicalPolicyWithPrivateParent(t *testing.T) {
	t.Parallel()
	for _, shadowing := range []bool{false, true} {
		t.Run(map[bool]string{false: "private_parent_is_unambiguous", true: "unnest_frame_collision"}[shadowing], func(t *testing.T) {
			t.Parallel()
			owner, md := clauseTestOwner(t)
			scope := semantic.NewScope(nil)
			for _, alias := range []string{"O", "P"} {
				source, ok := exactVirtualScopeSource(alias, logical.NewScan("T", alias), md, nil, nil)
				if !ok {
					t.Fatal("missing source type")
				}
				source.CorrelationName = "PRIVATE_" + alias
				source.Shadowing = shadowing
				if err := scope.AddSource(source); err != nil {
					t.Fatal(err)
				}
			}
			owner.outerScope, owner.outerScopes = scope, scope.Sources()
			sql := "SELECT p.id FROM t o, t j WHERE o.id > 0"
			if shadowing {
				sql = "SELECT 1 FROM t o, t j WHERE o.id > 0"
			}
			q, err := parseQueryFromSelect(t, sql)
			if err != nil {
				t.Fatal(err)
			}
			bound, err := owner.bindQuery(q)
			if err != nil {
				t.Fatal(err)
			}
			if bound.correlated() == shadowing {
				t.Fatal("dependency control did not distinguish admission from correlation")
			}
			_, err = lowerBoundExists(bound)
			if !shadowing {
				// Ordinary private parents are not scope-ambiguous merely because
				// their lexical names repeat. Existing multi-source planning limits
				// remain separate (the driver's minted-middle 0AF00 sentinel).
				if err != nil {
					t.Fatalf("private parent was treated as a lexical binding: %v", err)
				}
				return
			}
			var unsupported *CorrelatedExistsError
			if !errors.As(err, &unsupported) || !unsupported.Unsupported {
				t.Fatalf("private parent widened UNNEST admission: %v", err)
			}
		})
	}
}

func TestBoundOnLaterShadowUsesOriginalParentIdentity(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ name, source string }{
		{"catalog", "t"},
		{"derived", "(SELECT id FROM t)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			owner, md := clauseTestOwner(t)
			source, ok := exactVirtualScopeSource("O", logical.NewScan("T", "O"), md, nil, nil)
			if !ok {
				t.Fatal("missing source type")
			}
			source.CorrelationName = "PRIVATE_O"
			scope := semantic.NewScope(nil)
			if err := scope.AddSource(source); err != nil {
				t.Fatal(err)
			}
			owner.outerScope, owner.outerScopes = scope, scope.Sources()
			q, err := parseQueryFromSelect(t, "SELECT a.id FROM t a JOIN t b ON b.id = o.id JOIN "+test.source+" o ON o.id = a.id")
			if err != nil {
				t.Fatal(err)
			}
			bound, err := owner.bindQuery(q)
			if err != nil {
				t.Fatal(err)
			}
			var joins []*logical.LogicalJoin
			var visit func(logical.LogicalOperator)
			visit = func(op logical.LogicalOperator) {
				if join, ok := op.(*logical.LogicalJoin); ok {
					joins = append(joins, join)
				}
				for _, child := range op.Children() {
					visit(child)
				}
			}
			visit(bound.plan)
			if len(joins) != 2 || joins[1].BoundOn == nil || len(joins[1].BoundOn.VisibleBindings) != 2 {
				t.Fatalf("lost left-to-current ON origin: %v", joins)
			}
			refs := predicates.GetCorrelatedToOfPredicate(joins[1].BoundOn.Predicate)
			if _, present := refs[values.NamedCorrelationIdentifier("PRIVATE_O")]; !present {
				t.Fatalf("early ON rebound to later source: %v", refs)
			}
			_, err = lowerBoundExists(bound)
			var unsupported *CorrelatedExistsError
			if !errors.As(err, &unsupported) || !unsupported.Unsupported || unsupported.Message != "correlated EXISTS: a JOIN ON references an alias reused as a later inner join source (outer/inner alias collision) is not supported" {
				t.Fatalf("later-shadow admission was lost: %v", err)
			}
		})
	}
}

func TestBoundExistsSetOperationAdmission(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, sql string
		reject    bool
	}{
		{"correlated_union_all", "SELECT i.id FROM t i WHERE i.id = o.id UNION ALL SELECT j.id FROM t j WHERE j.id = o.id", true},
		{"correlated_union_distinct", "SELECT i.id FROM t i WHERE i.id = o.id UNION SELECT j.id FROM t j WHERE j.id = o.id", true},
		{"correlated_union_cte_envelope", "WITH c AS (SELECT id FROM t) SELECT id FROM c WHERE id = o.id UNION ALL SELECT id FROM t WHERE id = o.id", true},
		{"independent_union", "SELECT id FROM t UNION ALL SELECT id FROM t", false},
		{"correlated_derived_union", "SELECT d.id FROM (SELECT id FROM t UNION ALL SELECT id FROM t) d WHERE d.id = o.id", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			owner, _ := clauseTestOwner(t)
			q, err := parseQueryFromSelect(t, test.sql)
			if err != nil {
				t.Fatal(err)
			}
			clause := owner.newClause()
			alias, typ, err := clause.BuildExists(q)
			if test.reject {
				// UNION DISTINCT is rejected by the shared query visitor before
				// correlation classification, matching Java visitSetQuery.
				if test.name == "correlated_union_distinct" {
					var typed *api.Error
					if !errors.As(err, &typed) || typed.Code != api.ErrCodeUnsupportedQuery || typed.Message != "only UNION ALL is supported" {
						t.Fatalf("UNION DISTINCT syntax rejection = %v", err)
					}
				} else {
					var unsupported *CorrelatedExistsError
					if !errors.As(err, &unsupported) || !unsupported.Unsupported || unsupported.Message != "correlated EXISTS: unsupported query body shape" {
						t.Fatalf("correlated set-operation admission = %v, want existing body-shape rejection", err)
					}
				}
				if len(clause.subqueries)+len(owner.subqueries)+len(owner.scalarSubqueries)+len(owner.correlatedScalarSubqueries) != 0 {
					t.Fatal("rejected set operation published an attachment")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			value, err := values.NewExistsValue(alias, typ)
			if err != nil {
				t.Fatal(err)
			}
			if err := clause.admitPredicate(predicates.ExistsValueToQueryPredicate(value)); err != nil || len(owner.subqueries) != 1 {
				t.Fatalf("supported set-operation placement = %v", err)
			}
		})
	}
}

func TestBoundOnFailureDoesNotPublish(t *testing.T) {
	t.Parallel()
	owner, _ := clauseTestOwner(t)
	q, err := parseQueryFromSelect(t, "SELECT a.id FROM t a LEFT JOIN t b ON b.id = o.id")
	if err != nil {
		t.Fatal(err)
	}
	clause := owner.newClause()
	_, _, err = clause.BuildExists(q)
	var unsupported *CorrelatedExistsError
	if !errors.As(err, &unsupported) || !unsupported.Unsupported {
		t.Fatalf("correlated OUTER ON = %v", err)
	}
	if len(owner.subqueries)+len(owner.scalarSubqueries)+len(owner.correlatedScalarSubqueries) != 0 {
		t.Fatal("failed ON published an edge")
	}
	valid, err := parseQueryFromSelect(t, "SELECT id FROM t")
	if err != nil {
		t.Fatal(err)
	}
	clause = owner.newClause()
	alias, typ, err := clause.BuildExists(valid)
	if err != nil {
		t.Fatal(err)
	}
	value, err := values.NewExistsValue(alias, typ)
	if err != nil {
		t.Fatal(err)
	}
	if err := clause.admitPredicate(predicates.ExistsValueToQueryPredicate(value)); err != nil || len(owner.subqueries) != 1 {
		t.Fatalf("valid sibling after ON failure = %v", err)
	}
	if err := owner.subqueries[0].ValidateAdmission(); err != nil {
		t.Fatal(err)
	}
}
