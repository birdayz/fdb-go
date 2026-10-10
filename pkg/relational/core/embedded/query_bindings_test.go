package embedded

import (
	"context"
	"database/sql/driver"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/core/parser"
)

func TestQueryBindingsEquivalence(t *testing.T) {
	t.Parallel()
	normalize := func(sql string, args ...any) queryBindings {
		t.Helper()
		root, err := parser.Parse(sql)
		if err != nil {
			t.Fatal(err)
		}
		named := make([]driver.NamedValue, len(args))
		for i, arg := range args {
			named[i] = driver.NamedValue{Ordinal: i + 1, Value: arg}
		}
		_, releaseParams, err := bindStatementParameters(root, named)
		if err != nil {
			t.Fatal(err)
		}
		defer releaseParams()
		bindings, release, err := normalizeQueryBindings(root, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		return bindings
	}
	for _, pair := range []struct {
		name, first, second   string
		firstArgs, secondArgs []any
		share                 bool
	}{
		{"literal values", "SELECT v FROM t WHERE id=1", "select v from t where id=2", nil, nil, true},
		{"quoted strings", "SELECT 'a' 'b' FROM t", "SELECT 'c d' FROM t", nil, nil, true},
		{"parameter values", "SELECT v FROM t WHERE id=?", "SELECT v FROM t WHERE id=?", []any{int64(1)}, []any{int64(2)}, true},
		{"parameter type", "SELECT ? FROM t", "SELECT ? FROM t", []any{int32(1)}, []any{int64(1)}, false},
		{"null type", "SELECT ? FROM t", "SELECT ? FROM t", []any{nil}, []any{""}, false},
		{"boolean evaluation", "SELECT ? FROM t", "SELECT ? FROM t", []any{true}, []any{false}, false},
		{"boolean literal evaluation", "SELECT TRUE FROM t", "SELECT FALSE FROM t", nil, nil, false},
		{"coalesce specialization", "SELECT id FROM t WHERE COALESCE(? / ?, ?) IS NULL", "SELECT id FROM t WHERE COALESCE(? / ?, ?) IS NULL", []any{int64(1), int64(0), int64(5)}, []any{int64(1), int64(1), int64(5)}, false},
		{"coalesce repeat", "SELECT id FROM t WHERE COALESCE(? / ?, ?) IS NULL", "SELECT id FROM t WHERE COALESCE(? / ?, ?) IS NULL", []any{int64(1), int64(0), int64(5)}, []any{int64(1), int64(0), int64(5)}, true},
		{"equal partition", "SELECT ? + ? FROM t", "SELECT ? + ? FROM t", []any{int64(1), int64(1)}, []any{int64(2), int64(2)}, true},
		{"unequal partition", "SELECT ? + ? FROM t", "SELECT ? + ? FROM t", []any{int64(1), int64(1)}, []any{int64(1), int64(2)}, false},
		{"limit values", "SELECT id FROM t LIMIT ?", "SELECT id FROM t LIMIT ?", []any{int64(1)}, []any{int64(2)}, false},
		{"in array expansion", "SELECT id FROM t WHERE id IN ?", "SELECT id FROM t WHERE id IN ?", []any{[]int64{1, 2}}, []any{[]int64{2, 3}}, false},
		{"in list values", "SELECT id FROM t WHERE id IN (1,2)", "SELECT id FROM t WHERE id IN (2,3)", nil, nil, false},
		{"sort ordinals", "SELECT id,v FROM t ORDER BY 1", "SELECT id,v FROM t ORDER BY 2", nil, nil, false},
		{"group ordinals", "SELECT id,v FROM t GROUP BY 1", "SELECT id,v FROM t GROUP BY 2", nil, nil, false},
		{"quoted identity", "SELECT id FROM \"a\" WHERE id=1", "SELECT id FROM \"A\" WHERE id=2", nil, nil, false},
	} {
		a, b := normalize(pair.first, pair.firstArgs...), normalize(pair.second, pair.secondArgs...)
		if same := a.text == b.text && a.equivalence == b.equivalence; same != pair.share {
			t.Errorf("%s: share=%v, want %v; text %q / %q equivalence %q / %q", pair.name, same, pair.share, a.text, b.text, a.equivalence, b.equivalence)
		}
	}
}

func TestQueryBindingsDuplicateConstraintsAreImplications(t *testing.T) {
	t.Parallel()
	for _, engineWide := range []bool{false, true} {
		logger := &captureLogger{}
		g, md := newLoggingGenerator(t, ordersSchema, logger)
		if engineWide {
			g.cache = NewRelationalPlanCache(nil)
		}
		var first *cascadesPlan
		for _, args := range [][2]int64{{1, 2}, {3, 3}, {4, 5}} {
			q := parseQuery(t, "SELECT ? + ? FROM orders")
			_, release, err := bindStatementParameters(q, []driver.NamedValue{
				{Ordinal: 1, Value: args[0]}, {Ordinal: 2, Value: args[1]},
			})
			if err != nil {
				t.Fatal(err)
			}
			plan, err := g.planSelectCascades(context.Background(), q, md, true, statementOptions{})
			release()
			if err != nil {
				t.Fatal(err)
			}
			p := plan.(*cascadesPlan)
			if p.constants["0"] != args[0] || p.constants["1"] != args[1] {
				t.Fatalf("engineWide=%v: stable input positions lost: %v", engineWide, p.constants)
			}
			if first == nil {
				first = p
			} else if p.physicalPlan != first.physicalPlan || logger.events[len(logger.events)-1].Cache != PlanCacheHit {
				t.Fatalf("engineWide=%v args=%v: a plan without duplicate equalities must accept equal and unequal values", engineWide, args)
			}
		}
	}
}

func TestQueryBindingConstraintChecksOnlyRequiredEqualities(t *testing.T) {
	t.Parallel()
	first := queryBindings{literals: []queryLiteralBinding{
		{typeName: "LONG", valueKey: "1", equalTo: 0},
		{typeName: "LONG", valueKey: "1", equalTo: 0},
		{typeName: "LONG", valueKey: "2", equalTo: 2},
		{typeName: "BOOLEAN", valueKey: "true", equalTo: 3, exact: true},
	}}
	constraint := first.constraint()
	if constraint.literals[0].valueKey != "" || constraint.literals[3].valueKey != "true" {
		t.Fatal("constraint retained an unconstrained value or lost an exact value")
	}
	for _, tc := range []struct {
		name string
		edit func(*queryBindings)
		want bool
	}{
		{"same", func(*queryBindings) {}, true},
		{"new equal values", func(b *queryBindings) { b.literals[0].valueKey, b.literals[1].valueKey = "3", "3" }, true},
		{"new additional equality", func(b *queryBindings) { b.literals[2].valueKey = "1" }, true},
		{"broken equality", func(b *queryBindings) { b.literals[1].valueKey = "2" }, false},
		{"wrong type", func(b *queryBindings) { b.literals[2].typeName = "DOUBLE" }, false},
		{"wrong exact value", func(b *queryBindings) { b.literals[3].valueKey = "false" }, false},
		{"missing position", func(b *queryBindings) { b.literals = b.literals[:3] }, false},
	} {
		incoming := queryBindings{literals: append([]queryLiteralBinding(nil), first.literals...)}
		tc.edit(&incoming)
		if got := constraint.accepts(incoming); got != tc.want {
			t.Errorf("%s: accepts=%v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestQueryBindingsRestoresParameterScope(t *testing.T) {
	t.Parallel()
	q := parseQuery(t, "SELECT ? FROM orders")
	_, releaseParams, err := bindStatementParameters(q, []driver.NamedValue{{Ordinal: 1, Value: int64(7)}})
	if err != nil {
		t.Fatal(err)
	}
	defer releaseParams()
	bindings, release, err := normalizeQueryBindings(q, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings.constants) != 1 || bindings.constants["0"] != int64(7) {
		t.Fatalf("execution pool=%v", bindings.constants)
	}
	// The same tree can be planned again, including EXPLAIN's re-entry.
	release()
	second, releaseSecond, err := normalizeQueryBindings(q, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseSecond()
	if second.equivalence != bindings.equivalence || second.constants["0"] != int64(7) {
		t.Fatalf("restored binding changed: %+v / %+v", bindings, second)
	}
}

func TestQueryBindingsReusesPlanNotExecutionPool(t *testing.T) {
	t.Parallel()
	logger := &captureLogger{}
	g, md := newLoggingGenerator(t, ordersSchema, logger)
	var first *cascadesPlan
	for i, sql := range []string{"SELECT id FROM orders WHERE id=7", "SELECT id FROM orders WHERE id=8"} {
		plan, err := g.planSelectCascades(context.Background(), parseQuery(t, sql), md, true, statementOptions{})
		if err != nil {
			t.Fatal(err)
		}
		p := plan.(*cascadesPlan)
		if p.constants["0"] != int64(7+i) {
			t.Fatalf("execution %d carries %v", i, p.constants)
		}
		if first == nil {
			first = p
		} else if first.physicalPlan != p.physicalPlan || first.constants["0"] != int64(7) {
			t.Fatal("cache did not reuse the immutable plan with independent execution bindings")
		}
	}
	if logger.events[1].Cache != PlanCacheHit {
		t.Fatal("a different literal missed the cache")
	}
}

func TestQueryBindingsAggregateLiteralReuse(t *testing.T) {
	t.Parallel()
	logger := &captureLogger{}
	g, md := newLoggingGenerator(t, "CREATE TABLE T (id BIGINT, g BIGINT, PRIMARY KEY(id)) CREATE INDEX i_count AS SELECT COUNT(*) FROM T GROUP BY g", logger)
	for i, sql := range []string{"SELECT COUNT(1) FROM T GROUP BY g", "SELECT COUNT(2) FROM T GROUP BY g"} {
		plan, err := g.planSelectCascades(context.Background(), parseQuery(t, sql), md, true, statementOptions{})
		if err != nil {
			t.Fatal(err)
		}
		p := plan.(*cascadesPlan)
		// Unaliased computed columns publish positional labels, not the
		// aggregate's internal COUNT(1) row field name.
		if len(p.outputLabels) != 1 || p.outputLabels[0] != "" {
			t.Fatalf("query %d labels=%v, want one positional label", i, p.outputLabels)
		}
		if i == 1 && logger.events[i].Cache != PlanCacheHit {
			t.Fatal("literal-independent COUNT did not reuse its plan")
		}
		if !strings.Contains(p.Explain(), "AggregateIndex(") {
			t.Fatalf("COUNT of a non-null runtime constant lost the count index: %s", p.Explain())
		}
	}
}

func TestQueryBindingsConstrainsSparseIndexProof(t *testing.T) {
	t.Parallel()
	logger := &captureLogger{}
	g, md := newLoggingGenerator(t, "CREATE TABLE T (id BIGINT, v BIGINT, PRIMARY KEY(id)) CREATE INDEX i AS SELECT v FROM T WHERE v < 200", logger)
	for i, sql := range []string{"SELECT v FROM T WHERE v < 100", "SELECT v FROM T WHERE v < 453"} {
		plan, err := g.planSelectCascades(context.Background(), parseQuery(t, sql), md, true, statementOptions{})
		if err != nil {
			t.Fatal(err)
		}
		usesSparse := strings.Contains(plan.Explain(), "IndexScan(I")
		if usesSparse != (i == 0) {
			t.Fatalf("query %d: unsound or lost sparse-index proof: %s", i, plan.Explain())
		}
		if logger.events[i].Cache != PlanCacheMiss {
			t.Fatal("different values reused a literal-dependent index proof")
		}
	}
}
