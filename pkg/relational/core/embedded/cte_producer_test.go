package embedded

import (
	"errors"
	"maps"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/query/logical"
)

func testCTERegistry(bodies map[string]logical.LogicalOperator) logical.CTERegistry {
	registry := logical.CTERegistry{}
	for name, body := range bodies {
		cte := logical.NewCTE(name, body, nil, false)
		logical.BindCTESources(cte, logical.CTERegistry{})
		registry = registry.With(cte.CTEProducer)
	}
	return registry
}

func TestRetainedCTEProducerOriginDependencies(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, sql string
		free      bool
	}{
		{"referenced", "WITH a(x) AS (SELECT o.id FROM t s), b AS (SELECT * FROM a) SELECT x FROM b", true},
		{"unused", "WITH a(x) AS (SELECT o.id FROM t s), b AS (SELECT * FROM a) SELECT id FROM t i", false},
		{"retained_through_shadow", "WITH a(x) AS (SELECT o.id FROM t s), b AS (SELECT * FROM a) SELECT 1 FROM t i WHERE EXISTS (WITH a(x) AS (SELECT id FROM t z) SELECT 1 FROM b WHERE x = i.id)", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			owner, _ := clauseTestOwner(t)
			query, err := parseQueryFromSelect(t, test.sql)
			if err != nil {
				t.Fatal(err)
			}
			bound, err := owner.bindQuery(query)
			if err != nil {
				t.Fatal(err)
			}
			want := make(bindingSet)
			if test.free {
				want[values.NamedCorrelationIdentifier("O")] = struct{}{}
			}
			if !maps.Equal(bound.free, want) || bound.correlated() != test.free {
				t.Fatalf("producer-origin free dependencies = %v, want %v", bound.free, want)
			}
		})
	}
}

func TestRetainedCTESQLConsumersKeepDistinctBindings(t *testing.T) {
	t.Parallel()
	owner, _ := clauseTestOwner(t)
	query, err := parseQueryFromSelect(t, "WITH c AS (SELECT id FROM t) SELECT l.id FROM c l, c r")
	if err != nil {
		t.Fatal(err)
	}
	bound, err := owner.bindQuery(query)
	if err != nil {
		t.Fatal(err)
	}
	declaration, ok := bound.plan.(*logical.LogicalCTE)
	if !ok {
		t.Fatalf("missing retained declaration: %T", bound.plan)
	}
	left := logical.FindVisibleScan(declaration.Main, "L")
	right := logical.FindVisibleScan(declaration.Main, "R")
	if left == nil || right == nil || left.Source.Producer() != declaration.CTEProducer || right.Source.Producer() != declaration.CTEProducer {
		t.Fatal("SQL consumers do not share their declaration's prepared producer")
	}
	if left.Binding == "" || right.Binding == "" || left.Binding == right.Binding || left.Alias != "L" || right.Alias != "R" {
		t.Fatalf("consumer identities or aliases lost: left=%+v right=%+v", left, right)
	}
	if len(bound.free) != 0 {
		t.Fatalf("local producer consumers introduced a free correlation: %v", bound.free)
	}
}

func TestRetainedRecursiveCTEReusesPreparedSeed(t *testing.T) {
	t.Parallel()
	owner, _ := clauseTestOwner(t)
	visitor, err := owner.newSubqueryVisitor()
	if err != nil {
		t.Fatal(err)
	}
	query, err := parseQueryFromSelect(t, "WITH RECURSIVE r(x) AS (SELECT id FROM t seed UNION ALL SELECT x FROM r step WHERE x < 3) SELECT x FROM r result")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := visitor.VisitQuery(query)
	if err != nil {
		t.Fatal(err)
	}
	declaration := plan.(*logical.LogicalCTE)
	union := declaration.Body().(*logical.LogicalUnion)
	seed := logical.FindVisibleScan(union.Inputs[0], "SEED")
	step := logical.FindVisibleScan(union.Inputs[1], "STEP")
	result := logical.FindVisibleScan(declaration.Main, "RESULT")
	if seed == nil || step == nil || result == nil || !seed.Source.Resolved() || seed.Source.Producer() != nil || step.Source.Producer() != declaration.CTEProducer || result.Source.Producer() != declaration.CTEProducer {
		t.Fatal("recursive seed, temporary source, or final consumer lost its owner")
	}
	bindings := map[string]struct{}{seed.Binding: {}, step.Binding: {}, result.Binding: {}}
	if len(bindings) != 3 || seed.Binding == "" || step.Binding == "" || result.Binding == "" {
		t.Fatalf("recursive consumers share a binding: %v", bindings)
	}
	for allocated := range visitor.bindings.reserved {
		if strings.HasPrefix(allocated, "Q$BOUND") {
			if _, retained := bindings[allocated]; !retained {
				t.Fatalf("preparation allocated an abandoned source binding %s; seed was rebuilt", allocated)
			}
		}
	}
}

func TestRetainedCTEPhysicalValidationUnderShadow(t *testing.T) {
	t.Parallel()
	_, md := clauseTestOwner(t)
	physical := logical.NewScan("T", "T")
	body := logical.NewJoin(physical, &logical.LogicalUnnest{Segments: []string{"T", "ID"}, AtAlias: "O"}, logical.JoinInner, "")
	producer, err := logical.PrepareCTE("B", false, logical.CTERegistry{}, func(logical.CTERegistry) (logical.LogicalOperator, error) {
		return body, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	shadow := logical.NewCTE("T", logical.NewScan("T", "BASE"), nil, false)
	logical.BindCTESources(shadow, logical.CTERegistry{})
	registry := logical.CTERegistry{}.With(producer).With(shadow.CTEProducer)
	err = rejectAtOrdinalityOnTableWithCTEs(logical.NewScan("B", "B"), md, registry)
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeWrongObjectType {
		t.Fatalf("captured physical scalar AT source bypassed validation: %v", err)
	}
	if physical.Source.Producer() != nil || !physical.Source.Resolved() {
		t.Fatal("physical source acquired the consumer's same-named CTE")
	}
}

func TestRetainedCTEComputedColumnErrorPrecedesLaterProjection(t *testing.T) {
	t.Parallel()
	const ddl = `CREATE TABLE QCASE (id BIGINT, "KeepCase" BIGINT, plain BIGINT, PRIMARY KEY (id))
		CREATE TABLE q1 ("id" BIGINT, PRIMARY KEY ("id"))`
	const sql = `WITH RECURSIVE d AS (
		SELECT * FROM (SELECT q1."id" AS aa, QCASE.id AS bb FROM q1, QCASE WHERE q1."id" = 1
			UNION ALL SELECT q1."id" AS cc, QCASE.id AS dd FROM q1, QCASE WHERE q1."id" = 2) x
		UNION ALL SELECT d.cc + 1, d.dd FROM d WHERE d.cc < 3
	) SELECT d.cc FROM d ORDER BY d.cc`
	_, err := PlanPhysicalForTest(sql, ddl, nil)
	var typed *api.Error
	if !errors.As(err, &typed) || typed.Code != api.ErrCodeUndefinedColumn || typed.Message != `column "D.CC" does not exist` {
		t.Fatalf("recursive body lost its first computed-column error: %v; want 42703 D.CC", err)
	}
}

func TestComputedProjectionMissingColumnIsNotDeclined(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ sql, want string }{
		{`SELECT u.missing + 1, u.second FROM t u`, `column "U.MISSING" does not exist`},
		{`SELECT u."missing.name" + 1, u.second FROM t u`, `column "U.missing.name" does not exist`},
	} {
		t.Run(test.sql, func(t *testing.T) {
			t.Parallel()
			tmpl, err := buildSchemaTemplateFromDDL(`CREATE TABLE t (id BIGINT, PRIMARY KEY (id))`)
			if err != nil {
				t.Fatal(err)
			}
			q, err := parseQueryFromSelect(t, test.sql)
			if err != nil {
				t.Fatal(err)
			}
			check := func(err error) {
				t.Helper()
				var typed *api.Error
				if !errors.As(err, &typed) || typed.Code != api.ErrCodeUndefinedColumn || typed.Message != test.want {
					t.Fatalf("computed-column error = %v, want 42703 %s", err, test.want)
				}
			}
			_, err = NewPlanVisitor(tmpl.Underlying()).VisitQuery(q)
			check(err)
			_, err = buildLogicalPlanForQueryBodyWithCatalog(q.QueryExpressionBody(), tmpl.Underlying())
			check(err)
		})
	}
}

func TestRetainedNestedRecursiveCTEPlanBindings(t *testing.T) {
	t.Parallel()
	const sql = `WITH RECURSIVE r(n) AS (
		SELECT id FROM t UNION ALL SELECT n + 1 FROM r WHERE n < 3)
		SELECT n FROM r WHERE NOT EXISTS (
			WITH RECURSIVE r(n) AS (
				SELECT id FROM t WHERE id < 0 UNION ALL SELECT n + 1 FROM r WHERE n < 3)
			SELECT n FROM r) ORDER BY n`
	plan, err := PlanPhysicalForTest(sql, `CREATE TABLE t (id BIGINT, PRIMARY KEY (id))`, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantScan := values.NamedCorrelationIdentifier("RforScan")
	wantInsert := values.NamedCorrelationIdentifier("RforInsert")
	var recursive, scans int
	var walk func(plans.RecordQueryPlan)
	walk = func(node plans.RecordQueryPlan) {
		switch p := node.(type) {
		case *plans.RecordQueryRecursiveDfsJoinPlan:
			recursive++
			if p.GetPriorCorrelation() != wantScan {
				t.Fatalf("recursive prior binding = %#v, want %#v", p.GetPriorCorrelation(), wantScan)
			}
		case *plans.RecordQueryRecursiveLevelUnionPlan:
			recursive++
			if p.GetTempTableScanAlias() != wantScan || p.GetTempTableInsertAlias() != wantInsert {
				t.Fatal("recursive union lost its named scan/insert bindings")
			}
		case *plans.RecordQueryTempTableScanPlan:
			scans++
			if p.GetTempTableAlias() != wantScan {
				t.Fatalf("recursive temp scan binding = %#v, want %#v", p.GetTempTableAlias(), wantScan)
			}
		}
		for _, child := range node.GetChildren() {
			walk(child)
		}
	}
	walk(plan)
	if recursive != 2 || scans != 2 {
		t.Fatalf("nested recursive plan has %d recursive producers and %d temp scans, want 2 of each", recursive, scans)
	}
}
