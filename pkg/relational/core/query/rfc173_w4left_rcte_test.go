package query

// RFC-173 W4-left commit 3 — the recursive-CTE truth pins. A join over a
// RECURSIVE-CTE REFERENCE (the cteExprScope temp-table scan) has gated
// ordinal since the S3 fulcrum: the reference is an ordinal-eligible arity-1
// leaf whose seed leg type comes from cteColumnsScope. These pins turn that
// from drift-prone folklore (two header comments claimed the class was a
// name-model survivor) into asserted behavior, both directions.

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

func TestRFC173W4Left_RecursiveRefJoinGatesOrdinal(t *testing.T) {
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
		t.Fatalf("recursive-reference join gate = %+v, want Gated arity 2 (ordinal since the S3 fulcrum)", d)
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
	fv := rc.Fields[0].Value.(*values.FieldValue)
	qov := fv.Child.(*values.QuantifiedObjectValue)
	if !strings.EqualFold(qov.Correlation.Name(), "r") {
		t.Fatalf("reference-leg QOV correlates to %s, want r", qov.Correlation)
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

// The recursive-CTE leg remap's dotted arm fires from the STRUCTURAL
// classification (the projected value was a plain FieldValue — its rendered
// name is an identifier by construction), never from the string's shape. A
// string-grammar discriminator here misread computed renderings twice
// (review findings): "(B.ID + 1)" split into the garbage correlation "(B",
// and a float literal's "1.5" — digit-only segments pass an [A-Z0-9_] test —
// into QOV("1"). Structurally classified, a computed value's dots are never
// qualifiers regardless of rendering shape.
func TestRFC173Rcte_RemapComputedRenderingNotSplit(t *testing.T) {
	t.Parallel()
	names := []string{"(B.ID + 1)", "1.5", "B.ID", "PLAIN"}
	plainField := []bool{false, false, true, true}
	vals := recursiveRemapValues(names, plainField, true)

	// Computed renderings (expression, float literal): NOT split — a
	// resolved-ordinal read named by the full rendering, no QOV child.
	for _, i := range []int{0, 1} {
		fv, ok := vals[i].(*values.FieldValue)
		if !ok || fv.Child != nil {
			t.Fatalf("computed rendering %q = %#v, want a flat FieldValue (no QOV split)", names[i], vals[i])
		}
		if fv.Field != names[i] {
			t.Fatalf("computed rendering read name = %q, want the full rendering %q", fv.Field, names[i])
		}
		if fv.Resolved == nil {
			t.Fatalf("computed rendering %q under ordinalReads must carry the resolved ordinal", names[i])
		}
	}

	// A plain FieldValue's genuinely-dotted lazy name: the QOV read stays.
	fv2, ok := vals[2].(*values.FieldValue)
	if !ok || fv2.Child == nil || fv2.Field != "ID" {
		t.Fatalf("qualified reference = %#v, want QOV(B).ID", vals[2])
	}

	// Bare column: resolved-ordinal read.
	fv3, ok := vals[3].(*values.FieldValue)
	if !ok || fv3.Child != nil || fv3.Field != "PLAIN" || fv3.Resolved == nil {
		t.Fatalf("bare column = %#v, want resolved-ordinal PLAIN", vals[3])
	}

	// The fallback path (nil classification — logical column names,
	// identifiers by construction): dotted names still split.
	fb := recursiveRemapValues([]string{"B.ID"}, nil, false)
	fv, ok := fb[0].(*values.FieldValue)
	if !ok || fv.Child == nil || fv.Field != "ID" {
		t.Fatalf("fallback qualified reference = %#v, want QOV(B).ID", fb[0])
	}
}

// legPhysicalOutputNames' structural classification: plain FieldValues are
// the ONLY values whose rendered name is an identifier by construction.
func TestRFC173Rcte_LegClassificationStructural(t *testing.T) {
	t.Parallel()
	lp := expressions.NewLogicalProjectionExpressionWithAliases(
		[]values.Value{
			&values.FieldValue{Field: "B.ID", Typ: values.UnknownType},
			&values.ArithmeticValue{
				Op:    values.OpAdd,
				Left:  &values.FieldValue{Field: "ID", Typ: values.UnknownType},
				Right: &values.ConstantValue{Value: int64(1), Typ: values.NullableLong},
			},
			&values.ConstantValue{Value: 1.5, Typ: values.NullableDouble},
		},
		[]string{"", "", ""},
		expressions.ForEachQuantifier(expressions.InitialOf(
			expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType))),
	)
	names, plainField, fromProjection := legPhysicalOutputNames(lp, []string{"a", "b", "c"})
	if !fromProjection {
		t.Fatal("projection-topped leg must classify fromProjection")
	}
	if len(names) != 3 || len(plainField) != 3 {
		t.Fatalf("names/classification arity = %d/%d, want 3/3", len(names), len(plainField))
	}
	if !plainField[0] || plainField[1] || plainField[2] {
		t.Fatalf("classification = %v, want [true false false] (FieldValue vs computed vs literal)", plainField)
	}
}
