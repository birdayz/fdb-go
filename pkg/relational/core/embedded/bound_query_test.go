package embedded

import (
	"errors"
	"maps"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/query/logical"
	"fdb.dev/pkg/relational/core/query/semantic"
)

// LIMIT/OFFSET are Go extensions. A parameter that reaches query construction
// unresolved must not become the absent-limit/zero-offset sentinel, including
// UNION's separate right-branch construction path.
func TestBoundQueryUnresolvedPagination(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		"SELECT id FROM t LIMIT ?",
		"SELECT id FROM t LIMIT 1 OFFSET ?",
		"SELECT id FROM t UNION ALL SELECT id FROM t LIMIT ?",
		"SELECT id FROM t UNION ALL SELECT id FROM t LIMIT 1 OFFSET ?",
		"SELECT d.id FROM (SELECT id FROM t LIMIT ?) d",
		"WITH c AS (SELECT id FROM t LIMIT 1 OFFSET ?) SELECT id FROM c",
	} {
		for _, nested := range []bool{false, true} {
			t.Run(body+map[bool]string{false: "/root", true: "/nested"}[nested], func(t *testing.T) {
				t.Parallel()
				owner, md := clauseTestOwner(t)
				q, err := parseQueryFromSelect(t, body)
				if err != nil {
					t.Fatal(err)
				}
				if nested {
					_, err = owner.bindQuery(q)
				} else {
					_, err = NewPlanVisitor(md).VisitQuery(q)
				}
				var typed *api.Error
				if !errors.As(err, &typed) || typed.Code != api.ErrCodeUnsupportedQuery {
					t.Fatalf("unresolved pagination = %v, want typed unsupported query", err)
				}
			})
		}
	}
}

func TestBoundQueryDependenciesFollowOwnedEdges(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, sql string
		outer     bool
	}{
		{"projection", "SELECT o.id FROM t i", true},
		{"independent_scalar", "SELECT 1 FROM t i WHERE (SELECT MAX(s.id) FROM t s) > 0", false},
		{"correlated_scalar", "SELECT 1 FROM t i WHERE (SELECT MAX(s.id) FROM t s WHERE s.id = o.id) > 0", true},
		{"scalar_local_dependency", "SELECT 1 FROM t i WHERE (SELECT MAX(s.id) FROM t s WHERE s.id = i.id) > 0", false},
		{"nested_exists", "SELECT 1 FROM t i WHERE EXISTS (SELECT 1 FROM t n WHERE n.id = o.id)", true},
		{"cte_capture_with_local_shadow", "WITH c AS (SELECT o.id AS v FROM t s) SELECT o.v FROM c o", true},
		{"unused_definition", "WITH c AS (SELECT o.id AS v FROM t s) SELECT id FROM t i", false},
		{"union_second_branch", "SELECT id FROM t i UNION ALL SELECT o.id FROM t j", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			owner, _ := clauseTestOwner(t)
			q, err := parseQueryFromSelect(t, test.sql)
			if err != nil {
				t.Fatal(err)
			}
			bound, err := owner.bindQuery(q)
			if err != nil {
				t.Fatal(err)
			}
			want := make(bindingSet)
			if test.outer {
				want[values.NamedCorrelationIdentifier("O")] = struct{}{}
			}
			if !maps.Equal(bound.free, want) || bound.correlated() != test.outer {
				t.Fatalf("free=%v correlated=%v, want %v", bound.free, bound.correlated(), want)
			}
		})
	}
}

func TestBoundDependenciesDoNotBindDefinitionToMain(t *testing.T) {
	t.Parallel()
	outer := values.NamedCorrelationIdentifier("O")
	value, err := values.NewQuantifiedObjectValue(outer, values.NullableLong)
	if err != nil {
		t.Fatal(err)
	}
	body := &logical.LogicalProject{Input: logical.NewScan("T", "S"), ProjectedValues: []values.Value{value}}
	// Same spelling deliberately models a definition built in an earlier frame.
	// Main's scan binds its own output, never the definition's free reference.
	cte := &logical.LogicalCTE{
		Main:        logical.NewScan("C", "O"),
		CTEProducer: logical.NewCTE("C", body, nil, false).CTEProducer,
	}
	property, err := boundDependencies(cte, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(property.free, bindingSet{outer: {}}) {
		t.Fatalf("definition capture lost: %v", property.free)
	}
}

func TestBoundExistsResidualDependencies(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, sql string
		outer     bool
		scalars   int
	}{
		{"projection_only", "SELECT o.id FROM t i", false, 0},
		{"projection_scalar_only", "SELECT (SELECT MAX(s.id) FROM t s WHERE s.id = o.id) FROM t i", false, 0},
		{"where_scalar_survives_projection", "SELECT o.id FROM t i WHERE (SELECT MAX(s.id) FROM t s) > 0", false, 1},
		{"where_scalar_and_outer", "SELECT 1 FROM t i WHERE (SELECT MAX(s.id) FROM t s) > o.id", true, 1},
		{"nested_exists", "SELECT 1 FROM t i WHERE EXISTS (SELECT 1 FROM t n WHERE n.id = o.id)", true, 0},
		{"known_truth", "SELECT COUNT(*) FROM t i WHERE i.id = o.id", false, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			owner, _ := clauseTestOwner(t)
			q, err := parseQueryFromSelect(t, test.sql)
			if err != nil {
				t.Fatal(err)
			}
			bound, err := owner.bindQuery(q)
			if err != nil {
				t.Fatal(err)
			}
			before := maps.Clone(bound.free)
			if !bound.correlated() {
				t.Fatal("control lost its pre-elision outer dependency")
			}
			lowered, err := lowerBoundExists(bound)
			if err != nil {
				t.Fatal(err)
			}
			want := make(bindingSet)
			if test.outer {
				want[values.NamedCorrelationIdentifier("O")] = struct{}{}
			}
			if !maps.Equal(lowered.free, want) || len(lowered.scalars) != test.scalars {
				t.Fatalf("residual free=%v scalars=%d, want %v/%d", lowered.free, len(lowered.scalars), want, test.scalars)
			}
			if !maps.Equal(before, bound.free) {
				t.Fatal("lowering mutated the bound dependency snapshot")
			}
		})
	}
}

func TestBoundDependenciesRejectMissingScalarOwner(t *testing.T) {
	t.Parallel()
	owner, _ := clauseTestOwner(t)
	alias := owner.mintSubqueryAlias()
	value, err := values.NewQuantifiedObjectValue(alias, values.NullableLong)
	if err != nil {
		t.Fatal(err)
	}
	projection := &logical.LogicalProject{Input: logical.NewScan("T", "I"), ProjectedValues: []values.Value{value}}
	property, err := boundDependencies(projection, nil)
	if err != nil {
		t.Fatal(err)
	}
	var typed *api.Error
	if err := validateBoundDependencies(property.free, owner.outerScopes); !errors.As(err, &typed) || typed.Code != api.ErrCodeInternalError {
		t.Fatalf("orphan scalar classified as independent: %v", err)
	}
	projection.ScalarSubqueries = []logical.ScalarSubquery{{Alias: alias, Plan: logical.NewScan("T", "S")}}
	property, err = boundDependencies(projection, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(property.free) != 0 {
		t.Fatalf("owned independent scalar remains free: %v", property.free)
	}
	if err := validateBoundDependencies(property.free, owner.outerScopes); err != nil {
		t.Fatal(err)
	}
}

func TestBoundPrimaryUnnestIdentityPrecedesValues(t *testing.T) {
	t.Parallel()
	template, err := buildSchemaTemplateFromDDL("CREATE TABLE t (id BIGINT, tags BIGINT ARRAY, PRIMARY KEY (id))")
	if err != nil {
		t.Fatal(err)
	}
	md := template.Underlying()
	source, ok := exactVirtualScopeSource("O", logical.NewScan("T", "O"), md, nil, nil)
	if !ok {
		t.Fatal("missing parent row")
	}
	scope := semantic.NewScope(nil)
	if err := scope.AddSource(source); err != nil {
		t.Fatal(err)
	}
	owner := &existsSubqueryPlanner{md: md, outerScope: scope, outerScopes: scope.Sources()}
	seen := make(map[string]bool)
	for i := 0; i < 2; i++ {
		q, err := parseQueryFromSelect(t, `SELECT "Q$BOUND1" FROM o.tags AS "Q$BOUND1" WHERE "Q$BOUND1" = 9`)
		if err != nil {
			t.Fatal(err)
		}
		bound, err := owner.bindQuery(q)
		if err != nil {
			t.Fatal(err)
		}
		filter, ok := bound.plan.(*logical.LogicalFilter)
		if !ok {
			t.Fatalf("primary array plan = %T", bound.plan)
		}
		unnest, ok := filter.Input.(*logical.LogicalUnnest)
		if !ok {
			t.Fatalf("primary array input = %T", filter.Input)
		}
		if unnest.Binding == "" || unnest.Binding == unnest.Alias || seen[unnest.Binding] {
			t.Fatalf("primary array does not own a fresh pre-bound identity: alias=%s binding=%s", unnest.Alias, unnest.Binding)
		}
		seen[unnest.Binding] = true
		refs := predicates.GetCorrelatedToOfPredicate(filter.Predicate)
		if !maps.Equal(refs, map[values.CorrelationIdentifier]struct{}{values.NamedCorrelationIdentifier(unnest.Binding): {}}) {
			t.Fatalf("predicate bound before source identity: %v vs %s", refs, unnest.Binding)
		}
		if !maps.Equal(bound.free, bindingSet{values.NamedCorrelationIdentifier("O"): {}}) {
			t.Fatalf("collection owner lost: %v", bound.free)
		}
	}
}

func TestBoundQueryRetainsQualifyOnUnionBranches(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, sql string
		qualify   int
	}{
		{"both", "SELECT id FROM t QUALIFY id > 0 UNION ALL SELECT id FROM t QUALIFY id > 1", 2},
		{"left", "SELECT id FROM t QUALIFY id > 0 UNION ALL SELECT id FROM t WHERE id > 1", 1},
		{"right", "SELECT id FROM t WHERE id > 0 UNION ALL SELECT id FROM t QUALIFY id > 1", 1},
		{"neither", "SELECT id FROM t WHERE id > 0 UNION ALL SELECT id FROM t WHERE id > 1", 0},
		{"right_scalar", "SELECT id FROM t WHERE id < 0 UNION ALL SELECT id FROM t WHERE id = (SELECT MAX(id) FROM t) QUALIFY 1 = 0", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			owner, _ := clauseTestOwner(t)
			q, err := parseQueryFromSelect(t, test.sql)
			if err != nil {
				t.Fatal(err)
			}
			bound, err := owner.bindQuery(q)
			if err != nil {
				t.Fatal(err)
			}
			filters, qualify := 0, 0
			var visit func(logical.LogicalOperator)
			visit = func(op logical.LogicalOperator) {
				if filter, ok := op.(*logical.LogicalFilter); ok {
					filters++
					if filter.HasQualify {
						qualify++
					}
				}
				for _, child := range op.Children() {
					visit(child)
				}
			}
			visit(bound.plan)
			if filters != 2 || qualify != test.qualify {
				t.Fatalf("bound UNION filter provenance: filters=%d, QUALIFY=%d; want 2, %d", filters, qualify, test.qualify)
			}
		})
	}
}

func TestRetainQualifyProvenanceOwnership(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"filter", "aggregate", "derived_boundary", "scan", "nil"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			filter := logical.NewFilterWithPredicate(logical.NewScan("T", "I"), predicates.NewConstantPredicate(predicates.TriTrue), "")
			var root logical.LogicalOperator
			wantErr := false
			switch name {
			case "filter":
				root = filter
			case "aggregate":
				root = logical.NewProject(logical.NewAggregate(filter, nil, nil, nil, false), []string{"COUNT(*)"}, nil)
			case "derived_boundary":
				root = derivedSourceCarrier("D", "PRIVATE_D", filter)
				wantErr = true
			case "scan":
				root = logical.NewScan("T", "I")
				wantErr = true
			case "nil":
				wantErr = true
			}
			err := retainQualifyProvenance(root)
			if wantErr {
				var typed *api.Error
				if !errors.As(err, &typed) || typed.Code != api.ErrCodeInternalError {
					t.Fatalf("missing owning filter = %v, want typed internal error", err)
				}
				if filter.HasQualify {
					t.Fatal("QUALIFY provenance crossed the derived source boundary")
				}
			} else if err != nil || !filter.HasQualify {
				t.Fatalf("owning filter not marked: hasQualify=%v, err=%v", filter.HasQualify, err)
			}
		})
	}
}
