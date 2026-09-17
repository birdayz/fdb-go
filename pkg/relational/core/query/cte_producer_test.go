package query

import (
	"fmt"
	"slices"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

func testCTEProducer(name string, body logical.LogicalOperator) *logical.CTEProducer {
	cte := logical.NewCTE(name, body, nil, false)
	logical.BindCTESources(cte, logical.CTERegistry{})
	return cte.CTEProducer
}

func testCTERegistry(bodies map[string]logical.LogicalOperator) logical.CTERegistry {
	registry := logical.CTERegistry{}
	for name, body := range bodies {
		registry = registry.With(testCTEProducer(name, body))
	}
	return registry
}

func TestRetainedCTETranslationSharesProducerReferenceAndAliases(t *testing.T) {
	t.Parallel()
	first := logical.NewCTE("A", cteColumnAliasBodyFixture(t), nil, false, logical.CTEColumns("quoted", "a.b"))
	logical.BindCTESources(first, logical.CTERegistry{})
	defining := logical.CTERegistry{}.With(first.CTEProducer)
	builds := 0
	producer, err := logical.PrepareCTE("B", false, defining, func(logical.CTERegistry) (logical.LogicalOperator, error) {
		builds++
		return logical.NewScan("A", "BODY"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	shadow := testCTEProducer("A", logical.NewScan("MISSING", ""))
	tr := &cascadesTranslator{cteScope: defining.With(producer).With(shadow)}
	left, right := logical.NewScan("B", "L"), logical.NewScan("B", "R")
	logical.BindCTESources(logical.NewJoin(left, right, logical.JoinInner, ""), tr.cteScope)
	for _, scan := range []*logical.LogicalScan{left, right} {
		labels, err := ExactLogicalOutputLabels(scan, nil, nil)
		if err != nil || !slices.Equal(labels, []string{"quoted", "a.b"}) {
			t.Fatalf("retained labels = %v, %v", labels, err)
		}
		row, err := ExactLogicalResultType(scan, nil)
		if err != nil || row == nil {
			t.Fatalf("retained row = %v, %v", row, err)
		}
	}
	leftRef, rightRef := tr.translateRef(left), tr.translateRef(right)
	if leftRef == nil || rightRef != leftRef || builds != 1 {
		t.Fatalf("producer translated independently: left=%p right=%p builds=%d error=%v", leftRef, rightRef, builds, tr.translateErr)
	}
	leftQuantifier, rightQuantifier := expressions.ForEachQuantifier(leftRef), expressions.ForEachQuantifier(rightRef)
	if leftQuantifier.GetAlias() == rightQuantifier.GetAlias() || leftQuantifier.GetRangesOver() != rightQuantifier.GetRangesOver() {
		t.Fatal("consumer correlation identity was shared with producer identity")
	}
	if tr.cteScope.Lookup("A") != shadow || producer.DefiningRegistry().Lookup("A") != first.CTEProducer {
		t.Fatal("translation failed to restore the consumer's environment")
	}
}

func TestRetainedRecursiveCTERestoresOuterTranslation(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)
	outer := logical.NewCTE("R", nil, nil, true).CTEProducer
	outerFields := []values.Field{{Name: "OUTER", Ordinal: 0, FieldType: values.NotNullInt}}
	outerScan, err := expressions.NewTempTableScanExpression(outer.ScanBinding(), &values.RecordType{Fields: outerFields})
	if err != nil {
		t.Fatal(err)
	}
	tr.cteScope = tr.cteScope.With(outer)
	tr.cteExprScope[outer], tr.cteColumnsScope[outer] = outerScan, outerFields
	seed := logical.NewProject(logical.NewScan("Order", "S"), []string{"N"}, nil)
	seed.ProjectedValues = []values.Value{&values.ConstantValue{Value: int32(0), Typ: values.NotNullInt}}
	step := logical.NewProject(logical.NewScan("R", "SELF"), []string{"N"}, nil)
	step.ProjectedValues = []values.Value{&values.ConstantValue{Value: int32(1), Typ: values.NotNullInt}}
	inner := logical.NewCTE("R", logical.NewUnion([]logical.LogicalOperator{seed, step}, false), logical.NewScan("R", "RESULT"), true)
	result := tr.translateRecursiveCTE(inner)
	if result == nil {
		t.Fatalf("nested recursive declaration: %v", tr.translateErr)
	}
	if tr.cteScope.Lookup("R") != outer || tr.cteExprScope[outer] != outerScan || !slices.EqualFunc(tr.cteColumnsScope[outer], outerFields, func(a, b values.Field) bool { return a.Name == b.Name && a.FieldType.Equals(b.FieldType) }) {
		t.Fatal("inner recursion changed the outer temp scan or seed row")
	}
	if _, leaked := tr.cteExprScope[inner.CTEProducer]; leaked {
		t.Fatal("inner recursive temp/result registration escaped its scope")
	}
	if _, leaked := tr.cteColumnsScope[inner.CTEProducer]; leaked {
		t.Fatal("inner recursive row registration escaped its scope")
	}
}

func TestRetainedRecursiveCTEConsumerCommonRow(t *testing.T) {
	t.Parallel()
	for _, retainedOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("retained_only_%t", retainedOnly), func(t *testing.T) {
			t.Parallel()
			declared := &values.RecordType{Fields: []values.Field{{Name: "N", Ordinal: 0, FieldType: values.NotNullInt}}}
			seed := logical.NewProject(scan("Order", "S"), []string{"N"}, nil)
			seed.ProjectedValues = []values.Value{&values.ConstantValue{Value: int32(0), Typ: values.NotNullInt}}
			step := logical.NewProject(scan("R", "STEP"), []string{"N"}, nil)
			step.ProjectedValues = []values.Value{&values.ArithmeticValue{
				Op: values.OpAdd, Left: exactTestField(t, exactTestQOV(t, "STEP", declared), 0),
				Right: &values.ConstantValue{Value: int32(1), Typ: values.NotNullInt},
			}}
			main := logical.NewProject(scan("R", "RESULT"), []string{"N"}, nil)
			main.ProjectedValues = []values.Value{exactTestField(t, exactTestQOV(t, "RESULT", declared), 0)}
			declaration := logical.NewCTE("R", logical.NewUnion([]logical.LogicalOperator{seed, step}, false), main, true)
			logical.BindCTESources(declaration, logical.CTERegistry{})
			var input logical.LogicalOperator = declaration
			if retainedOnly {
				input = main
			}
			tr := newGateTranslator(t)
			ref := tr.translateRef(input)
			if ref == nil {
				t.Fatalf("recursive consumer failed: %v", tr.translateErr)
			}
			projection, ok := ref.Get().(*expressions.LogicalProjectionExpression)
			if !ok || !projection.GetProjectedValues()[0].Type().Equals(values.NullableInt) {
				t.Fatalf("recursive consumer did not adopt the common nullable row: %T", ref.Get())
			}
			other := logical.NewScan("R", "OTHER")
			other.Source = logical.CTEScanSource(declaration.CTEProducer)
			otherRef := tr.translateRef(other)
			if otherRef == nil || projection.GetInner().GetRangesOver() != otherRef {
				t.Fatal("recursive consumers do not share one lowered producer reference")
			}
			if len(tr.recursiveCTEConsumerRows) != 0 || len(tr.cteExprScope) != 0 || len(tr.cteColumnsScope) != 0 {
				t.Fatal("recursive temporary or consumer scope leaked after translation")
			}
		})
	}
}
