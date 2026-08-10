package embedded

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// nestedGroupKey builds the shape the SQL resolver produces for `n.sk`: ONE
// FieldValue whose Field is the struct ROOT with a multi-accessor resolved path.
// Java's resolver fuses identically (SemanticAnalyzer.java:598) — which is why
// the root cannot be the column's identity in either engine.
func nestedGroupKey(leaf string, ordinal int) *values.FieldValue {
	return &values.FieldValue{Field: "N", Resolved: &values.FieldPath{
		Accessors: []values.ResolvedAccessor{{Field: "N", Ordinal: 0}, {Field: leaf, Ordinal: ordinal}},
	}}
}

// TestAggregateGroupKeyMirrorsTakeTheNestedPath drives the two group-key naming
// mirrors that live in this package, for the shape SQL cannot reach yet.
//
// THIS IS A UNIT PIN PRECISELY BECAUSE THE CORPUS CANNOT REACH THE ARM. Nested-
// path GROUP BY is refused with 42703 (pinned at
// sqldriver/groupby_nested_key_collapse_fdb_test.go), so a full-suite run
// exercises only the flat arm and this conversion would ship untested — and its
// first real firing, whenever that feature lands, would be read as a finding
// rather than as an untested branch. Both arms and both controls are driven
// here from explicit state instead.
//
// The three mirrors that must agree are named at buildAggColumns' own comment:
// the executor's aggKeyName (→ expressions.AggregateKeyColumnName),
// aggregateGroupKeyOutputName below, and buildAggColumns' ColumnDef derivation.
// The first is pinned in the expressions package; the other two are here.
func TestAggregateGroupKeyMirrorsTakeTheNestedPath(t *testing.T) {
	t.Parallel()

	sk := nestedGroupKey("SK", 1)
	co := nestedGroupKey("CO", 2)

	t.Run("aggregateGroupKeyOutputName", func(t *testing.T) {
		t.Parallel()
		gotSK, gotCO := aggregateGroupKeyOutputName(sk), aggregateGroupKeyOutputName(co)
		if gotSK == gotCO {
			t.Fatalf("n.sk and n.co both name the aggregate output column %q — this "+
				"mirror is reading the flat struct root again while its authority "+
				"(expressions.AggregateKeyColumnName) reads the path. A post-aggregate "+
				"reference rebased through this name then addresses a slot the "+
				"executor keyed differently, and serves NULL.", gotSK)
		}
		if gotSK != "N.SK" || gotCO != "N.CO" {
			t.Fatalf("aggregateGroupKeyOutputName = %q / %q, want N.SK / N.CO", gotSK, gotCO)
		}
		if got := aggregateGroupKeyOutputName(sk); got != expressions.AggregateKeyColumnName(sk) {
			t.Fatalf("the mirror answers %q where its authority answers %q — a mirror "+
				"that disagrees on ONE shape is the entire defect class RFC-229 closes",
				got, expressions.AggregateKeyColumnName(sk))
		}
	})

	t.Run("aggregateGroupKeyOutputName keeps the flat arms", func(t *testing.T) {
		t.Parallel()
		// CONTROL. Without these the test above passes for an implementation
		// that routed EVERY key through the path renderer, which would spell a
		// lateral-unnest shadowing key `V.V` where the executor keys it `V`
		// (RFC-142) — a wrong answer, not a wrong label.
		flat := values.NewFlatFieldValue("STATUS", values.UnknownType)
		if got := aggregateGroupKeyOutputName(flat); got != "STATUS" {
			t.Fatalf("a FLAT group key now names its column %q, want STATUS", got)
		}
		qualified := values.NewFieldValue(
			values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("Q$DUP1")),
			"QID", values.UnknownType)
		if got := aggregateGroupKeyOutputName(qualified); got != "QID" {
			t.Fatalf("a CORRELATION-qualified group key now names its column %q, want QID", got)
		}
	})

	t.Run("buildAggColumns ColumnDef", func(t *testing.T) {
		t.Parallel()
		cols := buildAggColumns([]values.Value{sk, co}, nil, nil)
		if len(cols) != 2 {
			t.Fatalf("buildAggColumns returned %d columns for 2 group keys", len(cols))
		}
		if cols[0].Name == cols[1].Name {
			t.Fatalf("both group-key ColumnDefs are named %q — the datum lookup key "+
				"collides, so the second column reads the first's value out of the "+
				"row map", cols[0].Name)
		}
		if cols[0].Name != "N.SK" || cols[1].Name != "N.CO" {
			t.Fatalf("ColumnDef.Name = %q / %q, want N.SK / N.CO — the Name is the "+
				"key the aggregate cursor WRITES under, so it must equal "+
				"expressions.AggregateKeyColumnName exactly", cols[0].Name, cols[1].Name)
		}
		// The DISPLAY label is the bare leaf, which is what Rows.Columns()
		// surfaces. Java clears the qualifier on the top-level projection
		// (Identifier.withoutQualifier, Identifier.java:101-106), so `n.sk`
		// shows as SK.
		if cols[0].Label != "SK" || cols[1].Label != "CO" {
			t.Fatalf("ColumnDef.Label = %q / %q, want SK / CO — the qualifier must "+
				"never leak into user-visible metadata, and the leaf must come from "+
				"the resolved PATH, not from the struct root", cols[0].Label, cols[1].Label)
		}
	})

	t.Run("buildAggColumns keeps a flat key bare and unlabelled", func(t *testing.T) {
		t.Parallel()
		// CONTROL. A flat key's Name is already bare, so it gets NO label —
		// the label is only minted when it differs from the name. A widened
		// nested arm would start emitting one here.
		cols := buildAggColumns(
			[]values.Value{values.NewFlatFieldValue("STATUS", values.UnknownType)}, nil, nil)
		if len(cols) != 1 || cols[0].Name != "STATUS" || cols[0].Label != "" {
			t.Fatalf("flat group key ColumnDef = %+v, want Name=STATUS Label=\"\"", cols)
		}
	})
}
