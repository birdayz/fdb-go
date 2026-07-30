package executor

import (
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// bindMergedOuterLegs runs ONCE PER OUTER ROW of every FlatMap whose outer is a
// merged concat, so its cost is multiplied by the outer cardinality of every
// clustered-box gather in the workload. The shapes benchmarked here are the ones
// measured on the real-FDB corpus: two legs is by far the commonest, and the
// wide cases exist so the per-leg terms show up separately from the fixed ones.
//
// What the benchmark is watching for is ALLOCATION, not nanoseconds. The path
// used to build, per outer row: a []legSpan slice, one []values.Field copy and
// one *values.RecordType per leg (the type feeding a diagnostic that only the
// error path reads), a claimed map, and K successive whole-map copies of the
// evaluation context's bindings — one per leg, each copying every binding
// already present. The last term is the one that scales with the ENCLOSING
// context rather than with the row, which is how a two-leg join in a deeply
// correlated query pays the most.
func benchMergedRow(nLegs, colsPerLeg int) *PositionalRow {
	fields := make([]values.Field, 0, nLegs*colsPerLeg)
	legs := make([]values.RecordTypeLeg, 0, nLegs)
	slots := make([]any, 0, nLegs*colsPerLeg)
	for l := 0; l < nLegs; l++ {
		alias := values.NamedCorrelationIdentifier(fmt.Sprintf("LEG%d", l))
		legs = append(legs, values.NewRecordTypeLeg(alias, alias.Name(), l*colsPerLeg, colsPerLeg))
		for c := 0; c < colsPerLeg; c++ {
			fields = append(fields, values.Field{
				Name:      fmt.Sprintf("L%dC%d", l, c),
				FieldType: values.NotNullLong,
				Ordinal:   l*colsPerLeg + c,
			})
			slots = append(slots, int64(l*colsPerLeg+c))
		}
	}
	return &PositionalRow{Type: &values.RecordType{Fields: fields, Legs: legs}, Slots: slots}
}

// benchEnclosingContext is the context the FlatMap binds over. depth is the
// number of correlations already bound by enclosing FlatMaps — the term the
// per-leg WithBinding copies multiply against.
func benchEnclosingContext(depth int) *EvaluationContext {
	ec := EmptyEvaluationContext()
	for i := 0; i < depth; i++ {
		ec = ec.WithBinding(values.NamedCorrelationIdentifier(fmt.Sprintf("OUTER%d", i)), int64(i))
	}
	return ec
}

func BenchmarkBindMergedOuterLegs(b *testing.B) {
	joinAlias := values.UniqueCorrelationIdentifier()

	for _, tc := range []struct {
		name              string
		nLegs, cols, deep int
	}{
		// The corpus's dominant shape: two legs of a clustered box, shallow.
		{"2legs_4cols_depth1", 2, 4, 1},
		// The same shape inside a correlated subquery — the enclosing bindings
		// are what the per-leg map copies duplicate.
		{"2legs_4cols_depth8", 2, 4, 8},
		{"3legs_4cols_depth8", 3, 4, 8},
		// Wide legs: isolates the per-leg []Field + *RecordType terms.
		{"2legs_16cols_depth1", 2, 16, 1},
		{"6legs_8cols_depth8", 6, 8, 8},
	} {
		row := benchMergedRow(tc.nLegs, tc.cols)
		base := benchEnclosingContext(tc.deep)
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				ec := bindMergedOuterLegs(base, row, joinAlias)
				if ec == nil {
					b.Fatal("nil context")
				}
			}
		})
	}
}

// The DECLINE path — a row with no legs — is the commonest call of all on the
// corpus (a plain single-source outer). It must stay allocation-free.
func BenchmarkBindMergedOuterLegs_DeclinesNoLegs(b *testing.B) {
	joinAlias := values.UniqueCorrelationIdentifier()
	row := &PositionalRow{
		Type:  &values.RecordType{Fields: []values.Field{{Name: "K", FieldType: values.NotNullLong}}},
		Slots: []any{int64(1)},
	}
	base := benchEnclosingContext(4)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if ec := bindMergedOuterLegs(base, row, joinAlias); ec == nil {
			b.Fatal("nil context")
		}
	}
}
