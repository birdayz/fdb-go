package embedded

import (
	"errors"
	"maps"
	"reflect"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/query"
	"fdb.dev/pkg/relational/core/query/logical"
	"fdb.dev/pkg/relational/core/query/semantic"
)

func clauseTestOwner(t *testing.T) (*existsSubqueryPlanner, *recordlayer.RecordMetaData) {
	t.Helper()
	template, err := buildSchemaTemplateFromDDL("CREATE TABLE t (id BIGINT, PRIMARY KEY (id))")
	if err != nil {
		t.Fatal(err)
	}
	md := template.Underlying()
	source, ok := exactVirtualScopeSource("O", logical.NewScan("T", "O"), md, nil, nil)
	if !ok {
		t.Fatal("outer table has no exact scope")
	}
	scope := semantic.NewScope(nil)
	if err := scope.AddSource(source); err != nil {
		t.Fatal(err)
	}
	return &existsSubqueryPlanner{md: md, outerScope: scope, outerScopes: scope.Sources()}, md
}

func TestSubqueryClauseAdmissionUsesOneRetainedGraph(t *testing.T) {
	t.Parallel()
	owner, md := clauseTestOwner(t)
	q, err := parseQueryFromSelect(t, "SELECT 1 FROM t m WHERE o.id > 0 AND EXISTS (SELECT 1 FROM t n)")
	if err != nil {
		t.Fatal(err)
	}
	bound, err := owner.bindQuery(q)
	if err != nil {
		t.Fatal(err)
	}
	freeBefore := maps.Clone(bound.free)
	if len(freeBefore) != 1 {
		t.Fatalf("child free set = %v, want only O", freeBefore)
	}
	if _, ok := freeBefore[values.NamedCorrelationIdentifier("O")]; !ok {
		t.Fatalf("child lost outer dependency: %v", freeBefore)
	}
	lowered, err := lowerBoundExists(bound)
	if err != nil {
		t.Fatal(err)
	}
	if lowered.constraint != logical.ExistsPositivePredicateOnly {
		t.Fatalf("child constraint = %v, want positive predicate only", lowered.constraint)
	}
	input, err := query.LowerExistsInput(lowered.plan, md, lowered.retained...)
	if err != nil {
		t.Fatal(err)
	}
	edge := logical.ExistsSubquery{Alias: owner.mintSubqueryAlias(), Plan: lowered.plan, Input: input, FlowedType: input.ResultType(), JoinPredicate: lowered.join, Constraint: lowered.constraint}
	exists, err := values.NewExistsValue(edge.Alias, edge.FlowedType)
	if err != nil {
		t.Fatal(err)
	}
	positive := predicates.ExistsValueToQueryPredicate(exists)
	for _, name := range []string{"where_positive", "on_positive", "where_negative", "on_negative", "projection_positive", "projection_negative", "mixed_uses", "unknown_use"} {
		t.Run(name, func(t *testing.T) {
			// Sequential subtests intentionally consume the SAME immutable input;
			// no child construction or memo-property mutation occurs here.
			p, _ := clauseTestOwner(t)
			p.subqueries = []logical.ExistsSubquery{{Alias: values.NamedCorrelationIdentifier("EXISTING_EXISTS")}}
			p.scalarSubqueries = []logical.ScalarSubquery{{Alias: values.NamedCorrelationIdentifier("EXISTING_SCALAR")}}
			p.correlatedScalarSubqueries = []logical.CorrelatedScalarSubquery{{Alias: values.NamedCorrelationIdentifier("EXISTING_CORRELATED")}}
			beforeExists := append([]logical.ExistsSubquery(nil), p.subqueries...)
			beforeScalars := append([]logical.ScalarSubquery(nil), p.scalarSubqueries...)
			beforeCorrelated := append([]logical.CorrelatedScalarSubquery(nil), p.correlatedScalarSubqueries...)
			clause := p.newClause()
			clause.subqueries = []logical.ExistsSubquery{edge}
			clause.scalarSubqueries = []logical.ScalarSubquery{{Alias: values.NamedCorrelationIdentifier("PRIVATE_SCALAR"), Plan: logical.NewScan("T", "S")}}
			clause.correlatedScalarSubqueries = []logical.CorrelatedScalarSubquery{{Alias: values.NamedCorrelationIdentifier("PRIVATE_CORRELATED"), InnerPlan: logical.NewScan("T", "C")}}
			var admission error
			switch name {
			case "where_positive", "on_positive":
				admission = clause.admitPredicate(positive)
			case "where_negative", "on_negative":
				admission = clause.admitPredicate(predicates.NewNot(positive))
			case "projection_positive":
				admission = clause.admitValues([]values.Value{exists})
			case "projection_negative":
				admission = clause.admitValues([]values.Value{&values.NotValue{Child: exists}})
			case "mixed_uses":
				admission = clause.admitPredicate(predicates.NewAnd(positive, predicates.NewNot(positive)))
			case "unknown_use":
				admission = clause.admitValues([]values.Value{exists.Value})
			}
			wantSuccess := name == "where_positive" || name == "on_positive"
			if wantSuccess {
				if admission != nil || len(p.subqueries) != 2 || len(p.scalarSubqueries) != 2 || len(p.correlatedScalarSubqueries) != 2 {
					t.Fatalf("complete admission: err=%v counts=%d/%d/%d", admission, len(p.subqueries), len(p.scalarSubqueries), len(p.correlatedScalarSubqueries))
				}
				if again := clause.admitPredicate(positive); again == nil || len(p.subqueries) != 2 {
					t.Fatal("same clause published twice")
				}
			} else {
				var typed *api.Error
				if !errors.As(admission, &typed) || typed.Code != api.ErrCodeUnsupportedOperation {
					t.Fatalf("admission = %v, want typed 0A000", admission)
				}
				if !reflect.DeepEqual(beforeExists, p.subqueries) || !reflect.DeepEqual(beforeScalars, p.scalarSubqueries) || !reflect.DeepEqual(beforeCorrelated, p.correlatedScalarSubqueries) {
					t.Fatal("failed admission changed the owner's existing registrations")
				}
				// The SAME owner must remain usable after the rejection.
				sibling := p.newClause()
				simple, err := parseQueryFromSelect(t, "SELECT 1 FROM t")
				if err != nil {
					t.Fatal(err)
				}
				alias, typ, err := sibling.BuildExists(simple)
				if err != nil {
					t.Fatal(err)
				}
				value, err := values.NewExistsValue(alias, typ)
				if err != nil {
					t.Fatal(err)
				}
				if err := sibling.admitPredicate(predicates.ExistsValueToQueryPredicate(value)); err != nil || len(p.subqueries) != 2 {
					t.Fatalf("same-owner sibling could not publish once: %v", err)
				}
			}
			if clause.subqueries[0].Input.Reference() != input.Reference() || clause.subqueries[0].Plan != lowered.plan || !clause.subqueries[0].FlowedType.Equals(input.ResultType()) || !maps.Equal(freeBefore, bound.free) {
				t.Fatal("consumer admission changed child identity, type, or dependencies")
			}
		})
	}
}

func TestSubqueryClauseKnownTruthAdmissionBeforePublication(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, sql string
		truth     predicates.TriBool
	}{
		{"aggregate_true", "SELECT COUNT(*) FROM t i WHERE i.id = o.id", predicates.TriTrue},
		{"aggregate_offset_false", "SELECT COUNT(*) FROM t i WHERE i.id = o.id LIMIT 1 OFFSET 1", predicates.TriFalse},
		{"empty_limit_false", "SELECT i.id FROM t i WHERE i.id = o.id LIMIT 0", predicates.TriFalse},
	} {
		for _, use := range []string{"predicate", "negated_predicate", "projection", "negated_projection"} {
			t.Run(test.name+"/"+use, func(t *testing.T) {
				t.Parallel()
				owner, _ := clauseTestOwner(t)
				owner.subqueries = []logical.ExistsSubquery{{Alias: values.NamedCorrelationIdentifier("EXISTING_EXISTS")}}
				owner.scalarSubqueries = []logical.ScalarSubquery{{Alias: values.NamedCorrelationIdentifier("EXISTING_SCALAR")}}
				owner.correlatedScalarSubqueries = []logical.CorrelatedScalarSubquery{{Alias: values.NamedCorrelationIdentifier("EXISTING_CORRELATED")}}
				existsBefore := append([]logical.ExistsSubquery(nil), owner.subqueries...)
				scalarBefore := append([]logical.ScalarSubquery(nil), owner.scalarSubqueries...)
				correlatedBefore := append([]logical.CorrelatedScalarSubquery(nil), owner.correlatedScalarSubqueries...)
				q, err := parseQueryFromSelect(t, test.sql)
				if err != nil {
					t.Fatal(err)
				}
				clause := owner.newClause()
				alias, typ, err := clause.BuildExists(q)
				if err != nil {
					t.Fatal(err)
				}
				if len(clause.subqueries) != 1 || clause.subqueries[0].KnownTruth != test.truth {
					t.Fatalf("child did not retain expected cardinality truth %v: %+v", test.truth, clause.subqueries)
				}
				value, err := values.NewExistsValue(alias, typ)
				if err != nil {
					t.Fatal(err)
				}
				switch use {
				case "predicate":
					err = clause.admitPredicate(predicates.ExistsValueToQueryPredicate(value))
				case "negated_predicate":
					err = clause.admitPredicate(predicates.NewNot(predicates.ExistsValueToQueryPredicate(value)))
				case "projection":
					err = clause.admitValues([]values.Value{value})
				case "negated_projection":
					err = clause.admitValues([]values.Value{&values.NotValue{Child: value}})
				}
				if use == "predicate" || use == "negated_predicate" {
					if err != nil || !clause.published || len(owner.subqueries) != 2 {
						t.Fatalf("known-truth predicate must remain admitted: %v", err)
					}
					return
				}
				var typed *api.Error
				if !errors.As(err, &typed) || typed.Code != api.ErrCodeUnsupportedQuery || typed.Message != "a projected cardinality-known EXISTS is not yet supported" {
					t.Errorf("projected known truth = %v, want existing typed rejection", err)
				}
				if clause.published || !reflect.DeepEqual(existsBefore, owner.subqueries) || !reflect.DeepEqual(scalarBefore, owner.scalarSubqueries) || !reflect.DeepEqual(correlatedBefore, owner.correlatedScalarSubqueries) {
					t.Fatal("rejected projection changed the owner's registrations")
				}
				// A data-dependent projected sibling still uses this same owner.
				q, err = parseQueryFromSelect(t, "SELECT i.id FROM t i WHERE i.id = o.id")
				if err != nil {
					t.Fatal(err)
				}
				sibling := owner.newClause()
				alias, typ, err = sibling.BuildExists(q)
				if err != nil {
					t.Fatal(err)
				}
				value, err = values.NewExistsValue(alias, typ)
				if err != nil {
					t.Fatal(err)
				}
				if err := sibling.admitValues([]values.Value{value}); err != nil || len(owner.subqueries) != 2 || !sibling.published {
					t.Fatalf("projected sibling after rejection = %v", err)
				}
				if !reflect.DeepEqual(existsBefore, owner.subqueries[:1]) || !reflect.DeepEqual(scalarBefore, owner.scalarSubqueries) || !reflect.DeepEqual(correlatedBefore, owner.correlatedScalarSubqueries) {
					t.Fatal("sibling changed previous registrations")
				}
			})
		}
	}
}

func TestSubqueryClauseCallbacksRemainPrivateAfterLaterResolutionError(t *testing.T) {
	t.Parallel()
	owner, md := clauseTestOwner(t)
	sq := parseSelect(t, "SELECT o.id FROM t o WHERE EXISTS (SELECT 1 FROM t) AND (SELECT MAX(id) FROM t) > 0 AND (SELECT MAX(i.id) FROM t i WHERE i.id = o.id) > 0 AND o.missing = 1")
	resolver := buildSelectScope(sq, md, "", nil)
	if resolver == nil {
		t.Fatal("no outer scope")
	}
	clause := owner.newClause()
	resolver.SetSubqueryPlanner(clause)
	_, err := resolver.WalkPredicate(sq.whereExpr.Expression())
	if err == nil {
		t.Fatal("missing column was accepted")
	}
	if len(clause.subqueries) != 1 || len(clause.scalarSubqueries) != 1 || len(clause.correlatedScalarSubqueries) != 1 {
		t.Fatalf("failure did not follow all three private callbacks: %d/%d/%d: %v", len(clause.subqueries), len(clause.scalarSubqueries), len(clause.correlatedScalarSubqueries), err)
	}
	if len(owner.subqueries) != 0 || len(owner.scalarSubqueries) != 0 || len(owner.correlatedScalarSubqueries) != 0 {
		t.Fatal("callbacks published before completion of their clause")
	}
	if _, err := walkSubqueryPredicate(resolver, owner, sq.whereExpr.Expression()); err == nil || len(owner.subqueries) != 0 || len(owner.scalarSubqueries) != 0 || len(owner.correlatedScalarSubqueries) != 0 {
		t.Fatalf("owner boundary leaked a failed clause: %v", err)
	}
	valid := parseSelect(t, "SELECT o.id FROM t o WHERE EXISTS (SELECT 1 FROM t)")
	if _, err := walkSubqueryPredicate(resolver, owner, valid.whereExpr.Expression()); err != nil || len(owner.subqueries) != 1 {
		t.Fatalf("same-owner sibling after resolution failure: %v", err)
	}
}

func TestSubqueryClauseHavingDoesNotStealProjectionEdges(t *testing.T) {
	t.Parallel()
	owner, md := clauseTestOwner(t)
	owner.subqueries = []logical.ExistsSubquery{{Alias: values.NamedCorrelationIdentifier("PROJECTED_EXISTS")}}
	owner.scalarSubqueries = []logical.ScalarSubquery{{Alias: values.NamedCorrelationIdentifier("PROJECTED_SCALAR")}}
	existsBefore := append([]logical.ExistsSubquery(nil), owner.subqueries...)
	scalarBefore := append([]logical.ScalarSubquery(nil), owner.scalarSubqueries...)
	sq := parseSelect(t, "SELECT COUNT(*) FROM t HAVING COUNT(*) > (SELECT MAX(id) FROM t)")
	op := buildLogicalPlanForSelect(sq)
	if op == nil {
		t.Fatal("missing grouped plan")
	}
	if err := upgradeHavingPredicate(op, sq, md, "", nil, owner); err != nil {
		t.Fatal(err)
	}
	agg := findAggregate(op)
	if agg == nil || agg.HavingPredicate == nil || len(agg.HavingScalarSubqueries) != 1 || len(agg.HavingExistsSubqueries) != 0 {
		t.Fatalf("HAVING did not publish exactly its own scalar edge: %+v", agg)
	}
	if !reflect.DeepEqual(owner.subqueries, existsBefore) || !reflect.DeepEqual(owner.scalarSubqueries, scalarBefore) {
		t.Fatal("HAVING moved registrations owned by the projection clause")
	}
}
