package query

// RFC-173 W4-left commit 1 — white-box pins for the at-translation LEFT/RIGHT
// ordinal seed (design rulings I2 + I3).

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// TestRFC173W4Left_SeedShape pins the gated LEFT/RIGHT box seed:
//   - I3: the NULL-SUPPLYING leg's QOV is RECORD-LEVEL nullable (Java's
//     type.withNullability(true) at the pull-up — never per-column), the
//     preserved leg's QOV is not;
//   - I2: DECLARATION order regardless of join kind — a RIGHT join's seed
//     leads with its LEFT operand's run while the null-supplying ROLE moves
//     to that leg;
//   - the single-source gate scope: a clustered leg declines (joined-
//     preserved stays name-model until the mixed-nesting commit).
func TestRFC173W4Left_SeedShape(t *testing.T) {
	t.Parallel()
	tr := newDisjointUnnestTranslator(t)

	checkSeed := func(t *testing.T, kind logical.JoinKind, nullSide string) {
		t.Helper()
		j := logical.NewJoin(scan("SRC", "s"), scan("AUX", "x"), kind, "")
		d := tr.ordinalWedgeGateDecide(j)
		if !d.Gated || d.Arity != 2 {
			t.Fatalf("single-source %v box gate = %+v, want Gated arity 2 (W4-left)", kind, d)
		}
		legs := tr.legsOfGatedJoin(j)
		if len(legs) != 2 || !strings.EqualFold(legs[0].alias, "s") || !strings.EqualFold(legs[1].alias, "x") {
			t.Fatalf("legs = %+v, want DECLARATION order [s x] (I2: only the ROLE moves)", legs)
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
			// pre-existing Go-side rule), so the I3 record-level marker is
			// only observable on q.Typ — which is what seed consumers read.
			rt := qov.Typ.(*values.RecordType)
			wantNullable := strings.EqualFold(qov.Correlation.Name(), nullSide)
			if rt.Nullable != wantNullable {
				t.Fatalf("%v box: leg %s record-level nullable = %v, want %v (I3: nullability on the null-supplying QOV's RECORD type)",
					kind, qov.Correlation, rt.Nullable, wantNullable)
			}
		}
	}
	checkSeed(t, logical.JoinLeft, "x")  // LEFT: right operand null-supplies
	checkSeed(t, logical.JoinRight, "s") // RIGHT: left operand null-supplies

	// The clustered-leg decline (gate scope): a comma cluster as the
	// preserved leg keeps the box name-model.
	clustered := logical.NewJoin(inner(scan("SRC", "s"), scan("AUX", "x")),
		scan("AUX2", "y"), logical.JoinLeft, "")
	if d := tr.ordinalWedgeGateDecide(clustered); d.Gated {
		t.Fatal("a LEFT box with a CLUSTERED leg must stay name-model (joined-preserved; mixed-nesting commit's charter)")
	}

	// Leg-position ineligibility: the gated-as-root LEFT box is NOT
	// merge-opaque (RewriteOuterJoinRule dissolves it), so as a LEG it stays
	// ineligible and an enclosing cluster stays name-model.
	leftBox := logical.NewJoin(scan("SRC", "s"), scan("AUX", "x"), logical.JoinLeft, "")
	if tr.ordinalEligible(leftBox) {
		t.Fatal("a LEFT box must stay INELIGIBLE as a leg (dissolves and merges across the boundary — the W3b drift class)")
	}
}
