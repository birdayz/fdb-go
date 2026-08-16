package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestProjectionColumnNameCopyRebuildAndRebase pins projection naming over
// admitted, fully resolved FieldValues. Copying or rebuilding preserves the
// name. Rebasing changes a correlation-qualified derived name to the target
// binder, while an explicit output alias remains authoritative.
func TestProjectionColumnNameCopyRebuildAndRebase(t *testing.T) {
	t.Parallel()

	corr := values.NamedCorrelationIdentifier("Q0")
	renamed := values.NamedCorrelationIdentifier("Q1")
	inner := NamedForEachQuantifier(corr, nil)
	nestedType := rowOfTypes("N", rowOfTypes("SK", values.NotNullLong, "CO", values.NotNullLong))
	nested := func(leaf string) values.Value {
		base := mustExpression(values.NewQuantifiedObjectValue(corr, nestedType))
		return mustResolvedField(base, "N", leaf)
	}

	for _, testCase := range []struct {
		name        string
		value       values.Value
		alias       string
		want        string
		wantRebased string
	}{
		{
			name:        "flat field reference",
			value:       testField("ID", values.NotNullLong),
			want:        "ID",
			wantRebased: "ID",
		},
		{
			name:        "nested field reference",
			value:       nested("SK"),
			want:        "Q0.N.SK",
			wantRebased: "Q1.N.SK",
		},
		{
			name:        "nested sibling",
			value:       nested("CO"),
			want:        "Q0.N.CO",
			wantRebased: "Q1.N.CO",
		},
		{
			name:        "explicit alias",
			value:       nested("SK"),
			alias:       "first",
			want:        "FIRST",
			wantRebased: "FIRST",
		},
		{
			// ORDINAL-FREE, like the nested cases above it. This case used to
			// expect `(TEST_FIELD.N#0 + 1)`, which made the composite arm the
			// only naming authority whose answer moved when its operands were
			// bound to ordinals — the nested arm has always named `Q0.N.SK`
			// rather than `Q0.N#0.SK#1`. Its operand here is fully resolved, so
			// the two spellings are exactly the pre- and post-bake forms of one
			// name, and the ordinal-bearing one is the one this row was pinning.
			name: "computed exact field",
			value: &values.ArithmeticValue{
				Left:  testField("N", values.NotNullLong),
				Right: &values.ConstantValue{Value: int64(1)},
				Op:    values.OpAdd,
			},
			want:        "(TEST_FIELD.N + 1)",
			wantRebased: "(TEST_FIELD.N + 1)",
		},
	} {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			projection := mustExpression(NewLogicalProjectionExpressionWithAliasProvenance(
				[]values.Value{testCase.value}, []string{testCase.alias}, []bool{false}, inner))
			nameOf := func(expression *LogicalProjectionExpression) string {
				projected := expression.GetProjectedValues()
				aliases := expression.GetAliases()
				if len(projected) != 1 {
					t.Fatalf("projection has %d slots, want 1", len(projected))
				}
				alias := ""
				if len(aliases) != 0 {
					alias = aliases[0]
				}
				return values.OutputColumnName(projected[0], alias)
			}

			if got := nameOf(projection); got != testCase.want {
				t.Fatalf("minted name = %q, want %q", got, testCase.want)
			}
			copied := mustWithQuantifiers(t, projection,
				[]Quantifier{NamedForEachQuantifier(renamed, nil)}).(*LogicalProjectionExpression)
			if got := nameOf(copied); got != testCase.want {
				t.Fatalf("name after WithQuantifiers = %q, want %q", got, testCase.want)
			}
			rebuilt := mustExpression(NewLogicalProjectionExpressionWithAliasProvenance(
				projection.GetProjectedValues(), projection.GetAliases(), projection.GetAliasMinted(), inner))
			if got := nameOf(rebuilt); got != testCase.want {
				t.Fatalf("name after rebuild = %q, want %q", got, testCase.want)
			}

			aliasMap := mustExpression(values.NewAliasMap([]values.AliasPair{{Source: corr, Target: renamed}}))
			rebased := mustExpression(NewLogicalProjectionExpressionWithAliasProvenance(
				[]values.Value{values.RebaseValue(testCase.value, aliasMap)},
				projection.GetAliases(), projection.GetAliasMinted(), inner))
			if got := nameOf(rebased); got != testCase.wantRebased {
				t.Fatalf("name after rebase = %q, want %q", got, testCase.wantRebased)
			}
		})
	}
}

func TestProjectionColumnNameExactRequestsCanonicalize(t *testing.T) {
	t.Parallel()

	byName := &values.ArithmeticValue{
		Left:  testField("N", values.NotNullLong),
		Right: &values.ConstantValue{Value: int64(1)},
		Op:    values.OpAdd,
	}
	byOrdinal := &values.ArithmeticValue{
		Left:  testFieldAt("N", 0, values.NotNullLong),
		Right: &values.ConstantValue{Value: int64(1)},
		Op:    values.OpAdd,
	}
	if a, b := values.OutputColumnName(byName, ""), values.OutputColumnName(byOrdinal, ""); a != b {
		t.Fatalf("equivalent exact field resolutions produced names %q and %q", a, b)
	}
	if !values.SemanticEqualsUnderAliasMap(byName, byOrdinal, values.EmptyAliasMap()) {
		t.Fatal("name and ordinal requests against one exact root did not canonicalize")
	}
}
