package query

// Recursive-CTE truth pins. A join over a
// RECURSIVE-CTE REFERENCE (the cteExprScope temp-table scan) gates
// ordinal: the reference is an ordinal-eligible arity-1
// leaf whose seed leg type comes from cteColumnsScope. These pins turn that
// from drift-prone folklore (two header comments once claimed the class was a
// name-model survivor) into asserted behavior, both directions.

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

func TestRecursiveRefJoinGatesOrdinal(t *testing.T) {
	t.Parallel()
	tr := newDisjointUnnestTranslator(t)
	// The recursive-branch translation state: the reference pre-registered as
	// a temp-table scan (presence keys the gate arm) with its OUTPUT columns.
	tr.cteExprScope["R"] = nil
	tr.cteColumnsScope["R"] = []values.Field{
		{Name: "RID", FieldType: values.NotNullLong, Ordinal: 0},
	}

	j := logical.NewJoin(scan("R", "r"), scan("SRC", "s"), logical.JoinInner, "")
	d := tr.ordinalWedgeGateDecide(j)
	if !d.Gated || d.Arity != 2 {
		t.Fatalf("recursive-reference join gate = %+v, want Gated arity 2", d)
	}

	// The seed's reference leg is TYPED from cteColumnsScope.
	legs := tr.legsOfGatedJoin(j)
	rv, _ := tr.buildOrdinalJoinResultValue(legs)
	if rv == nil {
		t.Fatal("the recursive-reference join must build the ordinal seed")
	}
	rc := rv.(*values.RecordConstructorValue)
	if rc.Fields[0].Name != "RID" {
		t.Fatalf("seed leads with %q, want the reference leg's cteColumnsScope column RID", rc.Fields[0].Name)
	}
	fv := exactTestFieldView(t, rc.Fields[0].Value)
	qov, ok := values.AsQuantifiedObjectValue(fv.ChildValue())
	if !ok || !strings.EqualFold(qov.Correlation().Name(), "r") {
		t.Fatalf("reference-leg QOV = %v, want correlation r", qov)
	}

	// The OTHER direction: the recursive DEFINITION node in leg position
	// stays poison — defensively; the shape is production-unreachable
	// (derived-table WITH parses to 42F01 "table does not exist").
	recDef := logical.NewCTE("R2", scan("SRC", "s2"), scan("R2", "d"), true)
	jDef := logical.NewJoin(recDef, scan("AUX", "x"), logical.JoinInner, "")
	if dd := tr.ordinalWedgeGateDecide(jDef); dd.Gated {
		t.Fatal("a recursive DEFINITION node in leg position must stay poison (defensive; production-unreachable)")
	}
}

// Recursive-leg normalization is now an ordinal projection over the leg's
// exact flowed row. Output names are aliases only; neither a dotted alias nor a
// computed rendering is parsed back into a correlation.
func TestNormalizeRecursiveLegUsesExactOrdinalAuthority(t *testing.T) {
	t.Parallel()
	rowType := &values.RecordType{Fields: []values.Field{
		{Name: "LEFT", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: "RIGHT", Ordinal: 1, FieldType: values.NotNullString},
	}}
	scanExpr, err := expressions.NewFullUnorderedScanExpression([]string{"T"}, rowType)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	tr := newGateTranslator(t)
	normalizedExpr := tr.normalizeLegToOutputColumns(scanExpr, []string{"A.B", "(T.RIGHT)"})
	if normalizedExpr == nil {
		t.Fatalf("normalize recursive leg: %v", tr.translateErr)
	}
	normalized, ok := normalizedExpr.(*expressions.LogicalProjectionExpression)
	if !ok {
		t.Fatalf("normalized leg = %T, want logical projection", normalizedExpr)
	}
	aliases := normalized.GetAliases()
	if len(aliases) != 2 || aliases[0] != "A.B" || aliases[1] != "(T.RIGHT)" {
		t.Fatalf("normalization aliases = %v", aliases)
	}
	for ordinal, value := range normalized.GetProjectedValues() {
		field := exactTestFieldView(t, value)
		if got := field.Path().Ordinals(); len(got) != 1 || got[0] != ordinal {
			t.Fatalf("normalized slot %d path = %v, want [%d]", ordinal, got, ordinal)
		}
		owner, ownerOK := values.AsQuantifiedObjectValue(field.ChildValue())
		if !ownerOK || owner.Correlation() != normalized.GetInner().GetAlias() {
			t.Fatalf("normalized slot %d owner = %v, want inner quantifier %s",
				ordinal, owner, normalized.GetInner().GetAlias())
		}
	}
}

func TestRecursiveCTEConsumerBridgeWidensOnlyTheDeclaredPositionalRoot(t *testing.T) {
	t.Parallel()
	declaredRow := &values.RecordType{Fields: []values.Field{
		{Name: "NAME", Ordinal: 0, FieldType: values.NullableString},
		{Name: "LEVEL", Ordinal: 1, FieldType: values.NotNullInt},
	}}
	commonRow := &values.RecordType{Fields: []values.Field{
		{Name: "NAME", Ordinal: 0, FieldType: values.NullableString},
		{Name: "LEVEL", Ordinal: 1, FieldType: values.NullableInt},
	}}
	declaration := exactTestQOV(t, "ORG_LEVELS", declaredRow)
	target := exactTestQOV(t, "ORG_LEVELS", commonRow)

	for ordinal, wantType := range []values.Type{values.NullableString, values.NullableInt} {
		original := exactTestField(t, declaration, ordinal)
		translated, err := translateRecursiveCTEConsumerValue(original, declaration, target)
		if err != nil {
			t.Fatalf("translate slot %d: %v", ordinal, err)
		}
		field := exactTestFieldView(t, translated)
		if !field.ResultType().Equals(wantType) {
			t.Fatalf("translated slot %d type = %s, want %s", ordinal, field.ResultType(), wantType)
		}
		owner, ok := values.AsQuantifiedObjectValue(field.ChildValue())
		if !ok || !owner.FlowedType().Equals(commonRow) {
			t.Fatalf("translated slot %d owner = %v, want exact common row %s", ordinal, owner, commonRow)
		}
	}

	nestedDeclared := &values.RecordType{Fields: []values.Field{{
		Name: "PAYLOAD", Ordinal: 0, FieldType: &values.RecordType{Fields: []values.Field{{
			Name: "LEVEL", Ordinal: 0, FieldType: values.NotNullInt,
		}}},
	}}}
	nestedCommon := &values.RecordType{Fields: []values.Field{{
		Name: "PAYLOAD", Ordinal: 0, FieldType: &values.RecordType{Fields: []values.Field{{
			Name: "LEVEL", Ordinal: 0, FieldType: values.NullableInt,
		}}},
	}}}
	nestedDeclaration := exactTestQOV(t, "NESTED_LEVELS", nestedDeclared)
	nestedTarget := exactTestQOV(t, "NESTED_LEVELS", nestedCommon)
	nested, err := translateRecursiveCTEConsumerValue(
		exactTestField(t, nestedDeclaration, 0, 0), nestedDeclaration, nestedTarget)
	if err != nil {
		t.Fatalf("translate nested LEVEL: %v", err)
	}
	nestedField := exactTestFieldView(t, nested)
	if got := nestedField.Path().Ordinals(); len(got) != 2 || got[0] != 0 || got[1] != 0 ||
		!nestedField.ResultType().Equals(values.NullableInt) {
		t.Fatalf("nested LEVEL path/type = %v/%s, want [0 0]/nullable INT", got, nestedField.ResultType())
	}

	foreign := exactTestField(t, exactTestQOV(t, "FOREIGN", declaredRow), 1)
	unchanged, err := translateRecursiveCTEConsumerValue(foreign, declaration, target)
	if err != nil || unchanged != foreign {
		t.Fatalf("foreign window = (%v, %v), want pointer-stable unchanged", unchanged, err)
	}

	reordered := &values.RecordType{Fields: []values.Field{
		{Name: "LEVEL", Ordinal: 0, FieldType: values.NullableInt},
		{Name: "NAME", Ordinal: 1, FieldType: values.NullableString},
	}}
	if translated, translateErr := translateRecursiveCTEConsumerValue(
		exactTestField(t, declaration, 1), declaration, exactTestQOV(t, "ORG_LEVELS", reordered)); translated != nil || translateErr == nil {
		t.Fatalf("reordered common row = (%v, %v), want exact rejection", translated, translateErr)
	}

	incompatible := &values.RecordType{Fields: []values.Field{
		{Name: "NAME", Ordinal: 0, FieldType: values.NullableString},
		{Name: "LEVEL", Ordinal: 1, FieldType: values.NotNullString},
	}}
	if translated, translateErr := translateRecursiveCTEConsumerValue(
		exactTestField(t, declaration, 1), declaration, exactTestQOV(t, "ORG_LEVELS", incompatible)); translated != nil || translateErr == nil {
		t.Fatalf("incompatible common row = (%v, %v), want exact rejection", translated, translateErr)
	}
}

func TestRecursiveCTECommonRowPrecedesSelfScanAndConsumerBinding(t *testing.T) {
	t.Parallel()
	constantInt := func(value int32) values.Value {
		return &values.ConstantValue{Value: value, Typ: values.NotNullInt}
	}
	declaredRow := &values.RecordType{Fields: []values.Field{
		{Name: "LEVEL", Ordinal: 0, FieldType: values.NotNullInt},
	}}

	seed := logical.NewProject(scan("Order", "o"), []string{"LEVEL"}, nil)
	seed.ProjectedValues = []values.Value{constantInt(0)}
	recursive := logical.NewProject(scan("WALK", "w"), []string{"LEVEL"}, nil)
	recursive.ProjectedValues = []values.Value{&values.ArithmeticValue{
		Op:    values.OpAdd,
		Left:  exactTestField(t, exactTestQOV(t, "W", declaredRow), 0),
		Right: constantInt(1),
	}}
	main := logical.NewProject(scan("WALK", "r"), []string{"LEVEL"}, nil)
	main.ProjectedValues = []values.Value{
		exactTestField(t, exactTestQOV(t, "R", declaredRow), 0),
	}
	cte := logical.NewCTE("WALK",
		logical.NewUnion([]logical.LogicalOperator{seed, recursive}, false), main, true)

	tr := newGateTranslator(t)
	translated := tr.translateRecursiveCTE(cte)
	if translated == nil {
		t.Fatalf("translate nullable recursive CTE: %v", tr.translateErr)
	}
	projection, ok := translated.(*expressions.LogicalProjectionExpression)
	if !ok {
		t.Fatalf("main expression = %T, want logical projection", translated)
	}
	mainLevel := exactTestFieldView(t, projection.GetProjectedValues()[0])
	if !mainLevel.ResultType().Equals(values.NullableInt) {
		t.Fatalf("main LEVEL type = %s, want nullable INT", mainLevel.ResultType())
	}
	mainOwner, ok := values.AsQuantifiedObjectValue(mainLevel.ChildValue())
	if !ok || !mainOwner.FlowedType().Equals(&values.RecordType{Fields: []values.Field{
		{Name: "LEVEL", Ordinal: 0, FieldType: values.NullableInt},
	}}) {
		t.Fatalf("main LEVEL owner = %v, want exact common nullable row", mainOwner)
	}

	recursiveUnion, ok := projection.GetInner().GetRangesOver().Get().(*expressions.RecursiveUnionExpression)
	if !ok {
		t.Fatalf("main child = %T, want RecursiveUnionExpression",
			projection.GetInner().GetRangesOver().Get())
	}
	if !recursiveUnion.GetResultValue().Type().Equals(mainOwner.FlowedType()) {
		t.Fatalf("recursive union type = %s, want main common owner %s",
			recursiveUnion.GetResultValue().Type(), mainOwner.FlowedType())
	}
	var tempScan *expressions.TempTableScanExpression
	var findTempScan func(expressions.RelationalExpression)
	findTempScan = func(expression expressions.RelationalExpression) {
		if expression == nil || tempScan != nil {
			return
		}
		if scanExpression, isScan := expression.(*expressions.TempTableScanExpression); isScan {
			tempScan = scanExpression
			return
		}
		for _, quantifier := range expression.GetQuantifiers() {
			if rangesOver := quantifier.GetRangesOver(); rangesOver != nil {
				findTempScan(rangesOver.Get())
			}
		}
	}
	findTempScan(recursiveUnion.GetRecursiveState().GetRangesOver().Get())
	if tempScan == nil || !tempScan.GetResultValue().Type().Equals(mainOwner.FlowedType()) {
		t.Fatalf("recursive self scan = %v, want common nullable row %s", tempScan, mainOwner.FlowedType())
	}

	incompatibleRecursive := logical.NewProject(scan("WALK", "w"), []string{"LEVEL"}, nil)
	incompatibleRecursive.ProjectedValues = []values.Value{
		&values.ConstantValue{Value: "wrong", Typ: values.NotNullString},
	}
	incompatibleCTE := logical.NewCTE("WALK",
		logical.NewUnion([]logical.LogicalOperator{seed, incompatibleRecursive}, false), main, true)
	incompatibleTranslator := newGateTranslator(t)
	if got := incompatibleTranslator.translateRecursiveCTE(incompatibleCTE); got != nil ||
		incompatibleTranslator.translateErr == nil ||
		!strings.Contains(incompatibleTranslator.translateErr.Error(), "incompatible") {
		t.Fatalf("incompatible recursive leg = (%T, %v), want typed rejection",
			got, incompatibleTranslator.translateErr)
	}
}

// TestRecursiveBodyGatesOrdinal is a structural sentinel over the actual
// recursive-CTE translation: a plain-join recursive body remains on the exact
// ordinal join path before its output is normalized by position.
func TestRecursiveBodyGatesOrdinal(t *testing.T) {
	t.Parallel()
	// Recursive branch: SELECT c.order_id FROM Order c, WALK w  (plain inner join
	// over the CTE self-reference — the RFC-130 recursive-join-body shape).
	bodyJoin := inner(scan("Order", "c"), scan("WALK", "w"))
	rec := logical.NewProject(bodyJoin, []string{"ORDER_ID"}, nil)
	rec.ProjectedValues = []values.Value{exactTestNamedField(t, "C", "ORDER_ID", values.NotNullLong)}
	// Seed: SELECT order_id FROM Order o.
	seed := logical.NewProject(scan("Order", "o"), []string{"ORDER_ID"}, nil)
	seed.ProjectedValues = []values.Value{exactTestNamedField(t, "O", "ORDER_ID", values.NotNullLong)}
	cte := logical.NewCTE("WALK", logical.NewUnion([]logical.LogicalOperator{seed, rec}, false), scan("WALK", ""), true)

	tr := newGateTranslator(t)
	if tr.translateRecursiveCTE(cte) == nil {
		t.Fatalf("recursive CTE translation failed: %v", tr.translateErr)
	}

	// THE sentinel: the body join gated ordinal after the FULL run (blanket lifted).
	d, ok := tr.wedgeGate[bodyJoin]
	if !ok || !d.Gated {
		t.Fatalf("recursive plain-join body join = %+v (recorded=%v), want GATED after the full translateRecursiveCTE run", d, ok)
	}
}
