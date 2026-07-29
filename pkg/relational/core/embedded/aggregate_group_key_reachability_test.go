package embedded

// What production SQL reaches fieldValueMatchesAggregateGroupKey's asymmetric
// (childless-vs-QOV) arm? MEASURED ANSWER: none.
//
// The measurement, so it can be repeated rather than believed: the matcher was
// temporarily made to panic whenever it was DECISIVE — returning true where
// values.SemanticEqualsUnderAliasMap, the first arm of the OR at every call
// site, returned false. Under that instrumentation the whole
// //pkg/relational/core/embedded suite (including the explaindiff corpus) and
// the whole //pkg/relational/sqldriver FDB suite ran green, and a targeted
// 20-shape sweep — correlated scalar subqueries grouping on an outer column,
// joins, HAVING, derived tables, CTEs, multi-key GROUP BY — produced zero
// reaching inputs.
//
// The instrumentation is not what survives; the FACTS that make it unreachable
// are, and they are pinned below. Two of them, and each is a separate re-arm:
//
//   R1 — a SINGLE-source scope mints the SAME child shape for every spelling.
//        `GROUP BY t.a` does not carry a QOV just because it was written
//        qualified: needsQualification is len(scope.Sources()) > 1
//        (expr.go:221), so on one source the QUALIFIED reference takes the same
//        bare arm the unqualified one takes and both come out CHILDLESS. The
//        pair is therefore SYMMETRIC, semantic equality decides it, and the
//        matcher is never consulted. This is the fact that refutes the obvious
//        reading of `SELECT a + 1, COUNT(*) FROM t GROUP BY t.a` as an
//        asymmetric shape.
//
//   R2 — a QOV child requires TWO OR MORE scope sources (or a parent-scope
//        correlation, which is the same gate: expr.go:221-233). The matcher's
//        asymmetric arm requires the opposite — exactly ONE inner source alias,
//        and the QOV must NAME that source. A qualified reference minted under
//        ≥2 sources fails the len(aliases) != 1 guard; one minted against a
//        PARENT correlation names an alias that is not among the aggregate's
//        inner sources and fails the sameSource lookup. Both ways out decline.
//
// If either fact changes the arm becomes reachable and the "latent" framing in
// aggregate_group_key_accessor_name_test.go has to be re-derived — that file's
// fixtures are then describing shipped behaviour, not a synthetic isolation.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/expr"
	"fdb.dev/pkg/relational/core/query/semantic"
)

func aggkTable(name string, cols ...string) *semantic.StaticTable {
	tbl := &semantic.StaticTable{TableName: semantic.ParseQualifiedName(name, false)}
	for _, c := range cols {
		tbl.TableColumns = append(tbl.TableColumns,
			semantic.Column{Id: semantic.NewUnquoted(c), Type: "INT"})
	}
	return tbl
}

// aggkChildShape reports the child shape of a resolved reference: "" for a
// childless (source-relative) bake, or the QOV's correlation name.
func aggkChildShape(t *testing.T, v values.Value) string {
	t.Helper()
	fv, ok := v.(*values.FieldValue)
	if !ok {
		t.Fatalf("resolved to %T, want *values.FieldValue", v)
	}
	switch c := fv.Child.(type) {
	case nil:
		return ""
	case *values.QuantifiedObjectValue:
		return c.Correlation.Name()
	default:
		t.Fatalf("unexpected child %T", c)
		return ""
	}
}

// TestAggregateGroupKeySingleSourceResolutionIsSymmetric pins R1.
//
// Both spellings of the same single-source column must come out with the SAME
// child shape. That is what routes the production pair to semantic equality
// instead of to the matcher, and it holds whether or not the source carries a
// correlation name — the gate is the source COUNT, not the presence of a
// correlation.
func TestAggregateGroupKeySingleSourceResolutionIsSymmetric(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		corr string
	}{
		{"no correlation name", ""},
		{"with correlation name", "T"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tbl := aggkTable("T", "id", "a", "b")
			an := semantic.NewAnalyzer(semantic.NewInMemoryCatalog(tbl), false)
			scope := semantic.NewScope(nil)
			if err := scope.AddSource(semantic.ScopeSource{
				Table:           tbl,
				Alias:           semantic.NewUnquoted("t"),
				CorrelationName: tc.corr,
			}); err != nil {
				t.Fatal(err)
			}
			r := expr.New(an, scope)

			// `SELECT a + 1 … GROUP BY t.a` — the SELECT side is bare, the
			// GROUP BY side is qualified. This is the exact pair that looks
			// asymmetric in the SQL text.
			bare, err := r.ResolveIdentifier(semantic.Identifier{}, semantic.NewUnquoted("a"))
			if err != nil {
				t.Fatalf("resolve bare a: %v", err)
			}
			qual, err := r.ResolveIdentifier(semantic.NewUnquoted("t"), semantic.NewUnquoted("a"))
			if err != nil {
				t.Fatalf("resolve qualified t.a: %v", err)
			}

			bareShape := aggkChildShape(t, bare)
			qualShape := aggkChildShape(t, qual)
			if bareShape != "" || qualShape != "" {
				t.Fatalf("single-source resolution minted a QOV child (bare=%q qualified=%q); "+
					"the qualified spelling now produces the CHILDLESS-vs-QOV asymmetry that "+
					"fieldValueMatchesAggregateGroupKey exists for. Its arm is REACHABLE from "+
					"plain single-source SQL and is no longer latent — re-derive the reachability "+
					"argument in aggregate_group_key_accessor_name_test.go",
					bareShape, qualShape)
			}

			// Symmetric AND ordinal-equal, so the OR's first arm decides.
			if !values.SemanticEqualsUnderAliasMap(bare, qual, values.AliasMap{}) {
				t.Fatal("bare and qualified single-source references are no longer semantically " +
					"equal: the group-key bind now depends on fieldValueMatchesAggregateGroupKey, " +
					"which the reachability argument assumes it does not")
			}
		})
	}
}

// TestAggregateGroupKeyQOVRequiresMultipleSources pins R2: the resolver mints a
// QOV child only when the scope holds more than one source. That is the
// condition under which the matcher's asymmetric arm refuses anyway
// (len(innerSourceAliases) != 1), which is why no single spelling can satisfy
// both the mint condition and the match condition at once.
func TestAggregateGroupKeyQOVRequiresMultipleSources(t *testing.T) {
	t.Parallel()

	tt := aggkTable("T", "id", "a", "b")
	oo := aggkTable("O", "id", "a", "k")
	an := semantic.NewAnalyzer(semantic.NewInMemoryCatalog(tt, oo), false)
	scope := semantic.NewScope(nil)
	if err := scope.AddSource(semantic.ScopeSource{
		Table: tt, Alias: semantic.NewUnquoted("t"), CorrelationName: "T",
	}); err != nil {
		t.Fatal(err)
	}
	if err := scope.AddSource(semantic.ScopeSource{
		Table: oo, Alias: semantic.NewUnquoted("o"), CorrelationName: "O",
	}); err != nil {
		t.Fatal(err)
	}
	r := expr.New(an, scope)

	// `b` is unambiguous (only T has it), but the scope is multi-source, so the
	// reference is emitted QOV-addressed.
	qual, err := r.ResolveIdentifier(semantic.NewUnquoted("t"), semantic.NewUnquoted("b"))
	if err != nil {
		t.Fatalf("resolve qualified t.b: %v", err)
	}
	if got := aggkChildShape(t, qual); got != "T" {
		t.Fatalf("multi-source qualified reference has child shape %q, want QOV(T). "+
			"If a multi-source scope stopped minting QOV children, the ONLY remaining "+
			"producer of the childless-vs-QOV asymmetry would be the parent-scope "+
			"correlation — re-derive R2", got)
	}
}
