package query

// White-box pins for the at-translation LEFT/RIGHT ordinal seed: the
// declaration-order rule (a join's seed keeps FROM order regardless of join
// kind — only the null-supplying ROLE moves) and the null-supplying-leg
// nullability rule (record-level, not per-column).

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// TestLeftOuterSeedShape pins the gated LEFT/RIGHT box seed:
//   - the NULL-SUPPLYING leg's QOV is RECORD-LEVEL nullable (Java's
//     type.withNullability(true) at the pull-up — never per-column), the
//     preserved leg's QOV is not;
//   - DECLARATION order regardless of join kind — a RIGHT join's seed leads
//     with its LEFT operand's run while the null-supplying ROLE moves to
//     that leg;
//   - a clustered preserved leg (a comma cluster on the preserved side)
//     GATES at the box root too, and the gated LEFT box is itself ELIGIBLE
//     as a leg of an enclosing cluster.
func TestLeftOuterSeedShape(t *testing.T) {
	t.Parallel()
	tr := newDisjointUnnestTranslator(t)

	checkSeed := func(t *testing.T, kind logical.JoinKind, nullSide string) {
		t.Helper()
		j := logical.NewJoin(scan("SRC", "s"), scan("AUX", "x"), kind, "")
		d := tr.ordinalWedgeGateDecide(j)
		if !d.Gated || d.Arity != 2 {
			t.Fatalf("single-source %v box gate = %+v, want Gated arity 2", kind, d)
		}
		legs := tr.legsOfGatedJoin(j)
		if len(legs) != 2 || !strings.EqualFold(legs[0].alias, "s") || !strings.EqualFold(legs[1].alias, "x") {
			t.Fatalf("legs = %+v, want DECLARATION order [s x] (only the ROLE moves)", legs)
		}
		rv, _ := tr.buildOrdinalJoinResultValue(legs)
		if rv == nil {
			t.Fatal("gated outer box must build the ordinal seed")
		}
		rc := rv.(*values.RecordConstructorValue)
		// Field order: SRC's run (SID, ARR) then AUX's (XID, V) — declaration.
		if rc.Fields[0].Name != "SID" || rc.Fields[2].Name != "XID" {
			t.Fatalf("seed field order = [%s .. %s ..], want declaration [SID .. XID ..]", rc.Fields[0].Name, rc.Fields[2].Name)
		}
		for _, f := range rc.Fields {
			fv := f.Value.(*values.FieldValue)
			qov := fv.Child.(*values.QuantifiedObjectValue)
			// Key on the STORED Typ: QOV.Type() blanket-wraps nullable (a
			// pre-existing Go-side rule), so the record-level marker is only
			// observable on q.Typ — which is what seed consumers read.
			rt := qov.Typ.(*values.RecordType)
			wantNullable := strings.EqualFold(qov.Correlation.Name(), nullSide)
			if rt.Nullable != wantNullable {
				t.Fatalf("%v box: leg %s record-level nullable = %v, want %v (nullability lives on the null-supplying QOV's RECORD type)",
					kind, qov.Correlation, rt.Nullable, wantNullable)
			}
		}
	}
	checkSeed(t, logical.JoinLeft, "x")  // LEFT: right operand null-supplies
	checkSeed(t, logical.JoinRight, "s") // RIGHT: left operand null-supplies

	// A comma cluster as the preserved leg GATES at the box root too — its
	// buried sources are nameable per-leg via binding-keyed windows and
	// positional binders, so there is no need to fall back to the name model.
	clustered := logical.NewJoin(inner(scan("SRC", "s"), scan("AUX", "x")),
		scan("AUX2", "y"), logical.JoinLeft, "")
	if d := tr.ordinalWedgeGateDecide(clustered); !d.Gated {
		t.Fatalf("a LEFT box with a CLUSTERED preserved leg must GATE, got %+v", d)
	}

	// The gated LEFT box is itself ELIGIBLE as a leg of an enclosing
	// cluster — its arity (preserved + 1) is accounted for so the enclosing
	// seed and the post-flattening arrangement agree.
	leftBox := logical.NewJoin(scan("SRC", "s"), scan("AUX", "x"), logical.JoinLeft, "")
	if !tr.ordinalEligible(leftBox) {
		t.Fatal("a LEFT box must be ELIGIBLE as a leg")
	}
}
