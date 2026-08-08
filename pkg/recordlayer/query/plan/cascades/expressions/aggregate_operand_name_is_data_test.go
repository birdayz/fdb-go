package expressions_test

// The aggregate output-column name derives from DATA (AggregateSpec.OperandName,
// the parse text captured once at the sole production mint), never from a
// re-read of the operand Value's leaf `.Field` (RFC-197 item 5, `contract:`).
//
// WHY THIS DIMENSION WAS UNPROBED. The retired `case *values.FieldValue: opName =
// v.Field` arm was a SECOND copy of the Value→name rendering rule, and it agreed
// with the authority (values.ColumnNameValue) on every shape the suite happened
// to build — a CHILDLESS FieldValue renders to its own leaf either way. The two
// copies part company only on a QUALIFIED operand: the authority renders
// `T.V`, the leaf read renders bare `V`. So two aggregates over same-leaf columns
// of different quantifiers both spelled `SUM(V)`, and since the aggregate
// output-ordinal map is keyed by that spelling and is last-wins, one of the two
// output slots was unaddressable. That is the RFC's opening bug class — two
// columns sharing a leaf name treated as one — inside a naming authority.
//
// Java is the spec and it settles the axis twice over. Where Java keeps a column
// name it stores it AS DATA at construction and reads it back with a getter:
// Column.of(Optional<String>, value) -> Field.of(type, fieldNameOptional)
// (Column.java:81-82, Type.java:2908-2910), read via Type.java:2750-2763. It
// never re-derives a name from the Value. Where Java keeps no name it keeps NONE
// — an unaliased aggregate is Column.unnamedOf (GroupByExpression.java:754) and
// surfaces positionally as `_0` (Type.java:2645-2651,
// RelationalStructMetaData.java:81-89), and nothing in the relational layer ever
// matches that label back (SemanticAnalyzer.lookupAlias skips unnamed
// expressions, SemanticAnalyzer.java:521-523; the group-by pull-up binds by loop
// index, CompensateRecordConstructorRule.java:73-95). Go's rendered `COUNT(X)`
// spelling has no upstream contract behind it at all, which is exactly why it
// must not be allowed to DECIDE anything.
//
// NEGATIVE RESULT, pinned below and load-bearing for the classification: no SQL
// query reaches the Value-derived fallback at all today. The sole production mint
// (cascades_translator.go) always sets OperandName from the captured parse text,
// measured as zero empty-OperandName mints and zero fallback entries across all
// of ./pkg/relational/... including the real-FDB sqldriver suite. The collapse
// was therefore latent, not shipped — and it is reachable the moment any producer
// builds a spec without parse text, which is precisely what the unit corpus
// already does.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// qualifiedOperand builds the operand shape a resolver-bound `SUM(alias.col)`
// carries: a FieldValue whose child is the quantifier it reads from.
func qualifiedOperand(alias, leaf string) *values.FieldValue {
	return &values.FieldValue{
		Field: leaf,
		Child: values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(alias)),
		Typ:   values.UnknownType,
	}
}

// The collision dimension: same leaf, two quantifiers, no parse text.
func TestAggregateResultColumnName_SameLeafDifferentQuantifiersStayDistinct(t *testing.T) {
	t.Parallel()

	left := expressions.AggregateSpec{Function: expressions.AggSum, Operand: qualifiedOperand("T", "V")}
	right := expressions.AggregateSpec{Function: expressions.AggSum, Operand: qualifiedOperand("U", "V")}

	gotLeft := expressions.AggregateResultColumnName(left)
	gotRight := expressions.AggregateResultColumnName(right)

	if gotLeft == gotRight {
		t.Fatalf("SUM(T.V) and SUM(U.V) both render %q.\n\n"+
			"Two aggregates over same-leaf columns of DIFFERENT quantifiers are two "+
			"columns, and this is THE naming authority for them. The aggregate "+
			"output-ordinal map (groupByOutputOrdinals, cascades_translator.go) is "+
			"keyed by this string and is last-wins, so one identity collapses onto "+
			"the other's slot. A `.Field` leaf read reintroduces exactly this: it "+
			"renders the qualified operand bare. Derive the operand text from the "+
			"spec's OperandName data, or from values.ColumnNameValue — the ONE "+
			"rendering every output-naming site shares — never from a second copy "+
			"of the rule.", gotLeft)
	}
	if want := "SUM(T.V)"; gotLeft != want {
		t.Errorf("SUM(T.V) rendered %q, want %q — the qualifier is the only thing "+
			"separating the two identities, so dropping it is the collision", gotLeft, want)
	}
	if want := "SUM(U.V)"; gotRight != want {
		t.Errorf("SUM(U.V) rendered %q, want %q", gotRight, want)
	}
}

// OperandName is DATA and it WINS. This is the Java axis (name stored at
// construction, never re-derived) and it is what makes the shape above latent
// rather than shipped: production always supplies the text.
func TestAggregateResultColumnName_OperandNameDataWinsOverTheValue(t *testing.T) {
	t.Parallel()

	// The spec carries parse text that deliberately DISAGREES with anything the
	// operand Value could render, so a re-derivation cannot pass by coincidence.
	spec := expressions.AggregateSpec{
		Function:    expressions.AggSum,
		Operand:     qualifiedOperand("T", "V"),
		OperandName: "PRICE * QTY",
	}
	if got, want := expressions.AggregateResultColumnName(spec), "SUM(PRICE*QTY)"; got != want {
		t.Fatalf("AggregateResultColumnName = %q, want %q.\n\n"+
			"The captured parse text is the name-as-data channel (Java's "+
			"Column.of(Optional<String>, value), Column.java:81-82). It is the "+
			"authority; the operand Value is not consulted while it is present.",
			got, want)
	}
}

// The shapes the retired leaf read got RIGHT still render identically, so the
// conversion is a removal of a divergent duplicate and not a re-spelling.
func TestAggregateResultColumnName_UnqualifiedAndStarSpellingsUnchanged(t *testing.T) {
	t.Parallel()

	bare := expressions.AggregateSpec{
		Function: expressions.AggCount,
		Operand:  &values.FieldValue{Field: "AMOUNT", Typ: values.UnknownType},
	}
	if got, want := expressions.AggregateResultColumnName(bare), "COUNT(AMOUNT)"; got != want {
		t.Errorf("childless operand rendered %q, want %q — the two rendering copies "+
			"agreed on this shape, so removing one must not move it", got, want)
	}

	star := expressions.AggregateSpec{
		Function: expressions.AggCount,
		Operand:  &values.ConstantValue{Value: nil, Typ: values.UnknownType},
	}
	if got, want := expressions.AggregateResultColumnName(star), "COUNT(*)"; got != want {
		t.Errorf("COUNT(*) rendered %q, want %q", got, want)
	}

	none := expressions.AggregateSpec{Function: expressions.AggCount}
	if got, want := expressions.AggregateResultColumnName(none), "COUNT(?)"; got != want {
		t.Errorf("operand-less spec rendered %q, want %q", got, want)
	}
}
