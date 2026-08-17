package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestLogicalProjection_AliasProvenanceExcludedFromIdentity is the logical-layer
// twin of the plan-level pin: the per-slot alias provenance records WHO named an
// output, not what it computes, so it stays out of EqualsWithoutChildren and
// HashCodeWithoutChildren. Same contract values.FieldPath.FrontierPinned carries.
//
// The paired negative keeps it honest: the alias STRING is still identity, so a
// pass here cannot come from the output schema having left the key entirely.
func TestLogicalProjection_AliasProvenanceExcludedFromIdentity(t *testing.T) {
	t.Parallel()
	inner := ForEachQuantifier(InitialOf(&leafScan{name: "T"}))
	v := testFieldAt("A.K", 0, values.NullableLong)

	minted := mustExpression(NewLogicalProjectionExpressionWithAliasProvenance(
		[]values.Value{v}, []string{"A.K"}, []bool{true}, inner))
	minted = mustExpression(minted.WithAliasSources([]values.ProjectionAliasSource{
		values.NewProjectionAliasSource(values.NamedCorrelationIdentifier("A")),
	}))

	userWritten := mustExpression(NewLogicalProjectionExpressionWithAliases(
		[]values.Value{v}, []string{"A.K"}, inner))

	if !minted.EqualsWithoutChildren(userWritten, EmptyAliasMap()) {
		t.Error("a projection differing only in alias provenance must be the same memo member")
	}
	if minted.HashCodeWithoutChildren() != userWritten.HashCodeWithoutChildren() {
		t.Error("equal expressions must hash equal (memo invariant)")
	}
	otherSource := mustExpression(mustExpression(NewLogicalProjectionExpressionWithAliasProvenance(
		[]values.Value{v}, []string{"A.K"}, []bool{true}, inner)).WithAliasSources(
		[]values.ProjectionAliasSource{
			values.NewProjectionAliasSource(values.NamedCorrelationIdentifier("Z")),
		}))
	if !minted.EqualsWithoutChildren(otherSource, EmptyAliasMap()) ||
		minted.HashCodeWithoutChildren() != otherSource.HashCodeWithoutChildren() {
		t.Error("structured alias source is metadata provenance and must stay out of memo identity")
	}
	if _, err := userWritten.WithAliasSources([]values.ProjectionAliasSource{
		values.NewProjectionAliasSource(values.NamedCorrelationIdentifier("A")),
	}); err == nil {
		t.Error("a user-named slot accepted a machinery alias source")
	}

	renamed := mustExpression(NewLogicalProjectionExpressionWithAliases(
		[]values.Value{v}, []string{"K"}, inner))

	if minted.EqualsWithoutChildren(renamed, EmptyAliasMap()) {
		t.Error("the alias STRING is still a memo discriminator")
	}
}

// TestLogicalProjection_AliasProvenanceSurvivesWithQuantifiers pins the copy path
// the memo drives on every child rewrite. This one is worth its own test because
// WithQuantifiers used to build a field-by-field struct literal: it listed the
// three fields that existed when it was written, so any field added afterwards
// was dropped on every rewrite, silently and everywhere.
func TestLogicalProjection_AliasProvenanceSurvivesWithQuantifiers(t *testing.T) {
	t.Parallel()
	inner := ForEachQuantifier(InitialOf(&leafScan{name: "T"}))
	other := ForEachQuantifier(InitialOf(&leafScan{name: "T"}))
	v := testFieldAt("A.K", 0, values.NullableLong)

	p := mustExpression(NewLogicalProjectionExpressionWithAliasProvenance(
		[]values.Value{v}, []string{"A.K"}, []bool{true}, inner))
	p = mustExpression(p.WithAliasSources([]values.ProjectionAliasSource{
		values.NewProjectionAliasSource(values.NamedCorrelationIdentifier("A")),
	}))

	rewired, ok := mustWithQuantifiers(t, p, []Quantifier{other}).(*LogicalProjectionExpression)
	if !ok {
		t.Fatal("WithQuantifiers must return a *LogicalProjectionExpression")
	}
	if got := rewired.GetAliasMinted(); len(got) != 1 || !got[0] {
		t.Errorf("WithQuantifiers dropped the alias provenance: got %v, want [true]", got)
	}
	// The other name-carrying field must survive the same rewrite, so a
	// regression cannot hide by dropping both together.
	if got := rewired.GetAliases(); len(got) != 1 || got[0] != "A.K" {
		t.Errorf("WithQuantifiers dropped the aliases: got %v, want [A.K]", got)
	}
	if got := rewired.GetAliasSources(); len(got) != 1 || !got[0].Present ||
		got[0].Source != values.NamedCorrelationIdentifier("A") {
		t.Errorf("WithQuantifiers dropped the structured alias source: got %+v", got)
	}
	got := rewired.GetAliasSources()
	got[0] = values.NewProjectionAliasSource(values.NamedCorrelationIdentifier("MUTATED"))
	if original := rewired.GetAliasSources(); original[0].Source != values.NamedCorrelationIdentifier("A") {
		t.Errorf("GetAliasSources exposed mutable storage: %+v", original)
	}
}

func TestLogicalProjection_AliasSourceValidationRejectsMalformedVectors(t *testing.T) {
	t.Parallel()
	inner := ForEachQuantifier(InitialOf(&leafScan{name: "T"}))
	v := testFieldAt("A.K", 0, values.NullableLong)
	p := mustExpression(NewLogicalProjectionExpressionWithAliasProvenance(
		[]values.Value{v}, []string{"A.K"}, []bool{true}, inner))

	tests := []struct {
		name    string
		sources []values.ProjectionAliasSource
	}{
		{
			name: "too many slots",
			sources: []values.ProjectionAliasSource{
				values.NewProjectionAliasSource(values.NamedCorrelationIdentifier("A")),
				{},
			},
		},
		{
			name: "present zero source",
			sources: []values.ProjectionAliasSource{
				{Present: true},
			},
		},
		{
			name: "absent hidden source",
			sources: []values.ProjectionAliasSource{
				{Source: values.NamedCorrelationIdentifier("A")},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := p.WithAliasSources(test.sources); err == nil {
				t.Fatal("malformed structured alias-source vector was accepted")
			}
		})
	}
}

func TestLogicalProjection_NamedOutputSchemaIsIdentityAndSurvivesRewire(t *testing.T) {
	t.Parallel()
	inner := ForEachQuantifier(InitialOf(&leafScan{name: "T"}))
	value := values.NewBooleanValue(true)

	at := mustExpression(NewLogicalProjectionExpressionWithOutputSchema(
		[]values.Value{value}, []string{"AT"}, []bool{true}, []string{"AT"}, inner))
	val := mustExpression(NewLogicalProjectionExpressionWithOutputSchema(
		[]values.Value{value}, []string{"VAL"}, []bool{true}, []string{"VAL"}, inner))
	if at.EqualsWithoutChildren(val, EmptyAliasMap()) {
		t.Fatal("projections with different frozen output schemas reported equal")
	}
	if at.HashCodeWithoutChildren() == val.HashCodeWithoutChildren() {
		t.Fatal("projections with different frozen output schemas produced the same hash")
	}

	rewired := mustWithQuantifiers(t, at,
		[]Quantifier{ForEachQuantifier(InitialOf(&leafScan{name: "U"}))}).(*LogicalProjectionExpression)
	if got := rewired.GetOutputNames(); len(got) != 1 || got[0] != "AT" {
		t.Fatalf("WithQuantifiers changed frozen output schema: %v", got)
	}
}
