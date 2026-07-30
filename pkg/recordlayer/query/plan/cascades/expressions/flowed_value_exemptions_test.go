package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// typedInnerQuantifier is a quantifier over a member that STATES a two-column
// row. Every test below depends on the inner being typed: an exemption pin over
// an untyped inner cannot tell a stripped type from an absent one, and would pass
// with the exemption removed.
func typedInnerQuantifier(t *testing.T, alias string) (Quantifier, *values.RecordType) {
	t.Helper()
	row := rowOfTypes("A", values.NotNullLong, "B", values.NotNullString)
	q := NamedForEachQuantifier(values.NamedCorrelationIdentifier(alias),
		InitialOf(&typedStubExpr{name: "src" + alias, typ: row}))
	if _, typed := q.GetFlowedObjectValue().Type().(*values.RecordType); !typed {
		t.Fatal("fixture: GetFlowedObjectValue did not type the value, so every pin in " +
			"this file is vacuous — a site with nothing to strip cannot be shown to strip it")
	}
	return q, row
}

// TestFlowedValueExemptionsStateNoRowType pins the expressions that pass their
// inner's flowed object through while producing a DIFFERENT row, and must
// therefore state no type until they can state the right one.
//
// The pattern is LogicalProjectionExpression's, and the reason it has to be
// repeated per site rather than asserted once is that each site's REAL fix is
// different work with a different owner. What they share is the failure mode: the
// passthrough was inherited from Java at a point where Java does not actually pass
// through, and while the flowed accessor carried no type the inherited contract
// asserted nothing. Typing the accessor turned every one of them from an inert
// wrong SHAPE into a stated wrong ROW, and a stated row is believed — a downstream
// reader takes the inner's multi-leg row as this operator's output and then
// refuses to serve a source-relative ordinal against it.
//
// Each case names what Java states instead, because "state nothing" is only
// defensible while the right answer is known and booked.
func TestFlowedValueExemptionsStateNoRowType(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		build   func(inner Quantifier) RelationalExpression
		javaIs  string
		realFix string
	}{
		{
			name: "GroupByExpression",
			build: func(inner Quantifier) RelationalExpression {
				return NewGroupByExpression(
					[]values.Value{values.NewQuantifiedObjectValue(inner.GetAlias())},
					nil, inner)
			},
			javaIs: "resultValueFunction.apply(groupingValue, aggregateValue) — the OUTPUT " +
				"row of grouping + aggregate columns (GroupByExpression.java:129/152)",
			realFix: "build the output row from GetGroupingKeys and GetAggregates (CQ-59)",
		},
		{
			name: "InsertExpression",
			build: func(inner Quantifier) RelationalExpression {
				return NewInsertExpression(inner, "T", values.UnknownType)
			},
			javaIs:  "new QueriedValue(targetType) — the TARGET row (InsertExpression.java:71)",
			realFix: "values.NewQueriedValue(e.targetType), which this expression already holds",
		},
		{
			name: "UpdateExpression",
			build: func(inner Quantifier) RelationalExpression {
				return NewUpdateExpression(inner, "T", nil)
			},
			javaIs: "new QueriedValue(RECORD<OLD: innerRow, NEW: targetType>) — the " +
				"before/after PAIR (UpdateExpression.java:84, :209-213)",
			realFix: "the two-field OLD/NEW record, once the expression carries the target type",
		},
	} {
		inner, innerRow := typedInnerQuantifier(t, "IN")
		rv := tc.build(inner).GetResultValue()
		qov, isQOV := rv.(*values.QuantifiedObjectValue)
		if !isQOV {
			t.Errorf("%s: result value is a %T; the passthrough shape changed and this pin "+
				"no longer describes the site", tc.name, rv)
			continue
		}
		if qov.Correlation != inner.GetAlias() {
			t.Errorf("%s: result value is over %s, want the inner alias %s",
				tc.name, qov.Correlation.Name(), inner.GetAlias().Name())
		}
		if _, stated := qov.Typ.(*values.RecordType); stated {
			t.Errorf("%s: the result value STATES the row type %v.\n"+
				"  That row is the INNER's (%v), and this operator does not flow its inner's\n"+
				"  row. Java states %s.\n"+
				"  While the flowed accessor was untyped the passthrough asserted nothing;\n"+
				"  typed, it asserts a row the operator does not produce, and a reader that\n"+
				"  believes it reads every slot at the wrong depth.\n"+
				"  Fix the VALUE before typing it: %s.",
				tc.name, qov.Typ, innerRow, tc.javaIs, tc.realFix)
		}
	}
}

// TestDeleteResultValueStatesTheInnerRow is the other direction of the same
// family, and it is the one that keeps the exemptions from spreading by analogy.
//
// DELETE really does flow the rows it deleted: Java's DeleteExpression.java:62 is
// `inner.getFlowedObjectValue()`, which in Java is typed
// (Quantifier.java:801-803). So this site is Java-faithful and must stay typed.
// Freezing it untyped "for consistency" with its INSERT and UPDATE siblings would
// move Go away from the spec, and the siblings are exactly what makes that a
// tempting edit.
func TestDeleteResultValueStatesTheInnerRow(t *testing.T) {
	t.Parallel()

	inner, innerRow := typedInnerQuantifier(t, "IN")
	rv := NewDeleteExpression(inner, "T").GetResultValue()
	qov, isQOV := rv.(*values.QuantifiedObjectValue)
	if !isQOV {
		t.Fatalf("DELETE result value is a %T, want a quantified object over the inner", rv)
	}
	got, stated := qov.Typ.(*values.RecordType)
	if !stated {
		t.Fatalf("DELETE's result value states NO row type.\n" +
			"  Java's DeleteExpression.java:62 IS `inner.getFlowedObjectValue()`, and Java's\n" +
			"  accessor is typed — so this site is not one of the exemptions, it is the\n" +
			"  faithful one. INSERT (target row) and UPDATE (OLD/NEW pair) diverge; DELETE\n" +
			"  does not, and stripping its type to match them is a regression against the\n" +
			"  spec dressed as consistency.")
	}
	if !got.Equals(innerRow) {
		t.Errorf("DELETE's result value states %v, want the inner's row %v", got, innerRow)
	}
}

// TestSetOperationResultValueStatesChildZerosRow pins UNION and INTERSECTION as
// typed, against the reading that would freeze them.
//
// Both cite `RecordQuerySetPlan.mergeValues`, which sounds like a merge across all
// children and therefore like something child 0's row cannot be. It is not: the
// result TYPE mergeValues computes is the first non-existential quantifier's
// `getFlowedObjectType()`, under an explicit `// TODO let's just pick the first
// result type for now` (RecordQuerySetPlan.java:252-261). Child 0's row IS Java's
// stated row, so typing these sites moves Go towards the spec.
//
// The DerivedValue difference is real and separate — Java's value refers to every
// child, Go's to child 0 — but it is a difference in what the value refers to, not
// in the row it claims.
//
// "Child 0" is shorthand for what mergeValues actually picks: the first
// NON-EXISTENTIAL child. Both children being ForEach is the only shape any
// construction site builds today, and a pin over that shape alone cannot see the
// filter at all — it passes identically with the filter and with an unconditional
// index 0. The existential-first case below is what makes the assertion able to
// fail, so it is asserted here rather than left to the shape the callers happen
// to build.
func TestSetOperationResultValueStatesChildZerosRow(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		build func(qs []Quantifier) RelationalExpression
	}{
		{"LogicalUnionExpression", func(qs []Quantifier) RelationalExpression {
			return NewLogicalUnionExpression(qs)
		}},
		{"LogicalIntersectionExpression", func(qs []Quantifier) RelationalExpression {
			return NewLogicalIntersectionExpression(qs, nil)
		}},
	} {
		for _, shape := range []struct {
			name string
			// children builds the operator's children and names which of them
			// states the operator's row.
			children func(t *testing.T) (qs []Quantifier, want Quantifier, wantRow *values.RecordType)
			because  string
		}{
			{
				name: "all children ForEach",
				children: func(t *testing.T) ([]Quantifier, Quantifier, *values.RecordType) {
					first, firstRow := typedInnerQuantifier(t, "L")
					second, _ := typedInnerQuantifier(t, "R")
					return []Quantifier{first, second}, first, firstRow
				},
				because: "with every child non-existential the first one is the first " +
					"non-existential one",
			},
			{
				name: "existential child at index 0",
				children: func(t *testing.T) ([]Quantifier, Quantifier, *values.RecordType) {
					// Same inner shape on both, so the assertion turns on the KIND
					// and nothing else: an unconditional quantifiers[0] would state a
					// well-formed two-column row here, just the wrong child's.
					existentialRow := rowOfTypes("A", values.NotNullLong, "B", values.NotNullString)
					existential := NamedExistentialQuantifier(
						values.NamedCorrelationIdentifier("E"),
						InitialOf(&typedStubExpr{name: "srcE", typ: existentialRow}))
					forEach, forEachRow := typedInnerQuantifier(t, "R")
					return []Quantifier{existential, forEach}, forEach, forEachRow
				},
				because: "an Existential quantifier flows no row — it is consulted for a " +
					"boolean — so mergeValues filters it out before picking " +
					"(RecordQuerySetPlan.java:252-261)",
			},
		} {
			qs, want, wantRow := shape.children(t)

			rv := tc.build(qs).GetResultValue()
			qov, isQOV := rv.(*values.QuantifiedObjectValue)
			if !isQOV {
				t.Errorf("%s/%s: result value is a %T, want a quantified object over the "+
					"first non-existential child", tc.name, shape.name, rv)
				continue
			}
			if qov.Correlation != want.GetAlias() {
				t.Errorf("%s/%s: result value is over %s, want %s — %s",
					tc.name, shape.name, qov.Correlation.Name(), want.GetAlias().Name(),
					shape.because)
				continue
			}
			got, stated := qov.Typ.(*values.RecordType)
			if !stated {
				t.Errorf("%s/%s: the result value states NO row type.\n"+
					"  Java's mergeValues resolves the result type as the FIRST non-existential\n"+
					"  quantifier's flowed object type (RecordQuerySetPlan.java:252-261), so that\n"+
					"  child's row is the spec's answer here. This site is NOT one of the\n"+
					"  passthrough exemptions — freezing it untyped would be a regression against\n"+
					"  Java, and the mergeValues citation is what makes that look like the safe\n"+
					"  move.", tc.name, shape.name)
				continue
			}
			if !got.Equals(wantRow) {
				t.Errorf("%s/%s: states %v, want %v", tc.name, shape.name, got, wantRow)
			}
		}
	}
}
