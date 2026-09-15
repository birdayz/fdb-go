package embedded

import (
	"database/sql/driver"
	"fmt"
	"math"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/parser"
	"fdb.dev/pkg/relational/core/query/expr"
	"fdb.dev/pkg/relational/core/query/semantic"
)

func TestSubstituteParamsFiniteFloat(t *testing.T) {
	t.Parallel()
	for _, value := range []float64{
		math.Copysign(0, -1), 0, 3, -3, 3.5, 9007199254740992,
		math.SmallestNonzeroFloat64, -math.SmallestNonzeroFloat64,
		math.MaxFloat64, -math.MaxFloat64,
	} {
		t.Run(fmt.Sprintf("%016x", math.Float64bits(value)), func(t *testing.T) {
			t.Parallel()
			assertFiniteFloatParameter(t, value)
		})
	}
}

func assertFiniteFloatParameter(t *testing.T, value float64) {
	t.Helper()
	rendered, err := substituteParams("?", []driver.NamedValue{{Ordinal: 1, Value: value}})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parser.ParseExpression(rendered)
	if err != nil {
		t.Fatal(err)
	}
	resolver := expr.New(semantic.NewAnalyzer(semantic.NewInMemoryCatalog(), false), semantic.NewScope(nil))
	resolved, err := resolver.WalkExpression(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Type().Code() != values.TypeCodeDouble {
		t.Errorf("float64(%v) rendered %q resolves to %v, want DOUBLE", value, rendered, resolved.Type())
	}
	evaluated, err := resolved.Evaluate(nil)
	if err != nil {
		t.Fatal(err)
	}
	result, ok := evaluated.(float64)
	if !ok || math.Float64bits(result) != math.Float64bits(value) {
		t.Errorf("float64(%v) rendered %q resolves to %T(%v), want identical floating bits", value, rendered, evaluated, evaluated)
	}
}

func FuzzSubstituteParamsFiniteFloat(f *testing.F) {
	for _, value := range []float64{math.Copysign(0, -1), 0, 3, -3, 3.5, math.SmallestNonzeroFloat64, math.MaxFloat64} {
		f.Add(math.Float64bits(value))
	}
	f.Fuzz(func(t *testing.T, bits uint64) {
		t.Parallel()
		value := math.Float64frombits(bits)
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return // The non-finite CAST transport has a separate admission policy.
		}
		assertFiniteFloatParameter(t, value)
	})
}
