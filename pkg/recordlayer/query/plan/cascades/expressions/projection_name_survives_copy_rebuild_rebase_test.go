package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestProjectionColumnNameSurvivesEveryArm is RFC-229 §2.1 stated as a test
// rather than as a field: a projected column's output NAME must survive every
// copy, rebuild and rebase, exactly as `Resolved` does.
//
// §2.1 asks for a name SLOT. This drives the contract that slot would exist to
// guarantee, against the channel that carries the name today — the projected
// Value plus its alias, read through values.OutputColumnName. That order is
// deliberate: a slot added beside a derivation that already satisfies the
// contract is a SECOND source of truth, and two channels that can disagree is
// the defect class RFC-229 exists to close, not a fix for it. So the contract
// is pinned first and the slot is owed only where the contract FAILS.
//
// MEASURED RESULT, recorded here because it is the finding rather than a
// preamble: the name survives all four arms for every FIELD REFERENCE, flat and
// nested. It does NOT survive the rebase arm for a COMPUTED expression, whose
// rendering gains `#ordinal` discriminators when its operands bake. That is
// RFC-229 §0's pre/post-bake asymmetry, characterized at
// values/projection_namer_nested_collapse_test.go, and it is out of §2.3's
// nested-only scope — so it is asserted here as the boundary of the contract,
// not silently omitted from it.
//
// A test that only drove the field arms would report the contract as universal
// and be wrong. A test that only drove the computed arm would report it as
// broken and be wrong the other way. Both populations are driven.
func TestProjectionColumnNameSurvivesEveryArm(t *testing.T) {
	t.Parallel()

	corr := values.NamedCorrelationIdentifier("Q0")
	renamed := values.NamedCorrelationIdentifier("Q1")
	inner := NamedForEachQuantifier(corr, nil)

	nested := func(leaf string, ordinal int) *values.FieldValue {
		return &values.FieldValue{Field: "N", Typ: values.UnknownType, Resolved: &values.FieldPath{
			Accessors: []values.ResolvedAccessor{
				{Field: "N", Ordinal: 0}, {Field: leaf, Ordinal: ordinal},
			},
		}}
	}

	for _, tc := range []struct {
		name string
		val  values.Value
		// alias is the user's AS, "" for none.
		alias string
		want  string
		// survivesRebase records the MEASURED answer for the rebase arm. It is
		// a field rather than a constant because the two populations genuinely
		// differ, and collapsing them would hide which one is which.
		survivesRebase bool
		why            string
	}{{
		name: "flat field reference", val: values.NewFlatFieldValue("ID", values.UnknownType),
		want: "ID", survivesRebase: true,
		why: "Field is carried data; nothing in a rebase rewrites it",
	}, {
		name: "nested field reference", val: nested("SK", 1),
		want: "N.SK", survivesRebase: true,
		why: "the name is the resolved PATH, and Resolved is the very field " +
			"whose preserve-on-copy contract §2.1 asks the name to inherit — so " +
			"for this population the name IS already carried data",
	}, {
		name: "nested sibling of the same struct root", val: nested("CO", 2),
		want: "N.CO", survivesRebase: true,
		why: "the sibling is driven too: a rebase that dropped Resolved would " +
			"collapse BOTH onto N and the single-column case could not tell",
	}, {
		name: "an explicit alias", val: nested("SK", 1), alias: "first",
		want: "FIRST", survivesRebase: true,
		why: "the alias is the entire output-name authority and is carried on " +
			"the projection, not derived from the Value at all",
	}, {
		name: "a computed expression over a LAZY operand",
		val: &values.ArithmeticValue{
			Left:  values.NewFlatFieldValue("N", values.UnknownType),
			Right: &values.ConstantValue{Value: int64(1)},
			Op:    values.OpAdd,
		},
		want: "(N + 1)", survivesRebase: true,
		why: "a correlation rename alone does not bake an ordinal, so the " +
			"rendering is stable across THIS rebase — the instability is the " +
			"BAKE, driven separately below",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			aliases := []string{tc.alias}
			p := NewLogicalProjectionExpressionWithAliasProvenance(
				[]values.Value{tc.val}, aliases, []bool{false}, inner)

			nameOf := func(e *LogicalProjectionExpression) string {
				vals, als := e.GetProjectedValues(), e.GetAliases()
				if len(vals) != 1 {
					t.Fatalf("projection lost its slot: %d values", len(vals))
				}
				a := ""
				if len(als) > 0 {
					a = als[0]
				}
				return values.OutputColumnName(vals[0], a)
			}

			// ARM 1 — MINTED.
			if got := nameOf(p); got != tc.want {
				t.Fatalf("minted name = %q, want %q (%s)", got, tc.want, tc.why)
			}

			// ARM 2 — COPIED. The child edge is rewired on every memo rewrite;
			// this is the arm a field-by-field struct literal silently breaks.
			copied := p.WithQuantifiers([]Quantifier{NamedForEachQuantifier(renamed, nil)}).(*LogicalProjectionExpression)
			if got := nameOf(copied); got != tc.want {
				t.Fatalf("name after WithQuantifiers = %q, want %q — a copy that "+
					"rewires the child must change nothing else", got, tc.want)
			}

			// ARM 3 — REBUILT through the constructor from the accessors, which
			// is what every rewrite rule does (plan_extraction, the merge rule,
			// the limit push-through).
			rebuilt := NewLogicalProjectionExpressionWithAliasProvenance(
				p.GetProjectedValues(), p.GetAliases(), p.GetAliasMinted(), inner)
			if got := nameOf(rebuilt); got != tc.want {
				t.Fatalf("name after rebuild = %q, want %q — a rule that hands an "+
					"existing projection's columns to a new expression must not "+
					"rename them", got, tc.want)
			}

			// ARM 4 — REBASED. The projected Values are translated to a new
			// correlation and the projection rebuilt on them, which is what the
			// join and left-outer-existential lowerings do.
			rebasedVals := []values.Value{
				values.RebaseValue(tc.val, values.AliasMap{corr: renamed}),
			}
			rebased := NewLogicalProjectionExpressionWithAliasProvenance(
				rebasedVals, p.GetAliases(), p.GetAliasMinted(), inner)
			got := nameOf(rebased)
			if tc.survivesRebase && got != tc.want {
				t.Fatalf("name after rebase = %q, want %q — %s.\n"+
					"  THIS IS THE §2.1 CONTRACT BREAKING. A column that survives a "+
					"rebase with its ordinal intact and its name dropped is the bug "+
					"in a new place: the executor keys the emitted slot by one "+
					"spelling and every downstream re-reader looks it up by the "+
					"other.", got, tc.want, tc.why)
			}
		})
	}

	// THE ONE PLACE THE CONTRACT DOES NOT HOLD, asserted rather than omitted.
	// A computed projection's rendering changes when its operands BAKE, so a
	// name derived after the bake disagrees with one derived before it. §2.3 is
	// nested-only and deliberately does not touch this arm; what keeps it off a
	// user is isPlainColumnRef in the result-set layer, and the label such a
	// column actually receives is the positional `_i`.
	//
	// If this ever starts holding, the derivation has become stable across the
	// bake and RFC-229 §0's second defect is closed — say so there rather than
	// deleting this.
	t.Run("a computed expression does NOT survive the ordinal bake", func(t *testing.T) {
		t.Parallel()
		lazy := &values.ArithmeticValue{
			Left:  values.NewFlatFieldValue("N", values.UnknownType),
			Right: &values.ConstantValue{Value: int64(1)},
			Op:    values.OpAdd,
		}
		baked := &values.ArithmeticValue{
			Left:  values.NewFieldValueWithResolvedOrdinal("N", 0, values.UnknownType),
			Right: &values.ConstantValue{Value: int64(1)},
			Op:    values.OpAdd,
		}
		before := values.OutputColumnName(lazy, "")
		after := values.OutputColumnName(baked, "")
		if before == after {
			t.Fatalf("a computed projection now names itself %q on BOTH sides of "+
				"the bake. The §2.1 contract has become universal — widen the "+
				"table above to assert it for the computed population too, and "+
				"close RFC-229 §0's pre/post-bake defect.", before)
		}
		// The control that stops this passing for the wrong reason: the FIELD
		// population must still be stable across the same bake, or the
		// asymmetry has spread rather than been isolated.
		if a, b := values.OutputColumnName(values.NewFlatFieldValue("N", values.UnknownType), ""),
			values.OutputColumnName(values.NewFieldValueWithResolvedOrdinal("N", 0, values.UnknownType), ""); a != b {
			t.Fatalf("a PLAIN field reference now names itself %q lazy and %q "+
				"baked — the bake asymmetry has spread to the population the "+
				"table above asserts is stable", a, b)
		}
	})
}
