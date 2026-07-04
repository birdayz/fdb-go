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
