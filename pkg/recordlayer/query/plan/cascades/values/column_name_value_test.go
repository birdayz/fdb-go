package values

import "testing"

// TestColumnNameValue_IgnoresBakedOrdinals pins the rendering split between
// ExplainValue and ColumnNameValue: ExplainValue keeps the baked `#<ordinal>`
// accessor discriminator (EXPLAIN/debug — collapsing two different reads is
// a bug there), while ColumnNameValue — the NAME-derivation rendering every
// output-column-name derivation goes through — renders the SAME text for a
// lazy and a baked reference. Without the split, plan-time ordinal binding
// would change derived column names ("SUM(AMOUNT)" vs "SUM(AMOUNT#2)") and
// break the naming lockstep between layers that render different instances
// of one reference.
func TestColumnNameValue_IgnoresBakedOrdinals(t *testing.T) {
	t.Parallel()

	lazy := NewFlatFieldValue("AMOUNT", UnknownType)
	baked := NewFieldValueWithResolvedOrdinal("AMOUNT", 2, UnknownType)

	if got := ExplainValue(baked); got != "AMOUNT#2" {
		t.Fatalf("ExplainValue(baked) = %q, want AMOUNT#2 (ordinal discriminator kept)", got)
	}
	if got := ColumnNameValue(baked); got != "AMOUNT" {
		t.Fatalf("ColumnNameValue(baked) = %q, want AMOUNT (ordinal-free)", got)
	}
	if ColumnNameValue(lazy) != ColumnNameValue(baked) {
		t.Fatalf("lazy vs baked ColumnNameValue drift: %q vs %q",
			ColumnNameValue(lazy), ColumnNameValue(baked))
	}

	// A COMPUTED tree over baked references: the composite rendering must be
	// ordinal-free end-to-end (the aggregate-operand naming case).
	sum := &ArithmeticValue{Op: OpMul, Left: baked, Right: NewFieldValueWithResolvedOrdinal("QTY", 3, UnknownType)}
	if got := ColumnNameValue(sum); got != "(AMOUNT * QTY)" {
		t.Fatalf("ColumnNameValue(computed) = %q, want (AMOUNT * QTY)", got)
	}
	if got := ExplainValue(sum); got != "(AMOUNT#2 * QTY#3)" {
		t.Fatalf("ExplainValue(computed) = %q, want (AMOUNT#2 * QTY#3)", got)
	}

	// Multi-accessor baked path: every step renders name-only in the
	// column-name form, name#ordinal in the explain form.
	multi := &FieldValue{
		Field: "CITY",
		Typ:   UnknownType,
		Resolved: &FieldPath{Accessors: []ResolvedAccessor{
			{Field: "ADDR", Ordinal: 1},
			{Field: "CITY", Ordinal: 0},
		}},
	}
	if got := ColumnNameValue(multi); got != "ADDR.CITY" {
		t.Fatalf("ColumnNameValue(multi) = %q, want ADDR.CITY", got)
	}
	if got := ExplainValue(multi); got != "ADDR#1.CITY#0" {
		t.Fatalf("ExplainValue(multi) = %q, want ADDR#1.CITY#0", got)
	}
}

func TestProjectionOutputIdentityKey_DistinguishesAliasFromDerivedExpression(t *testing.T) {
	t.Parallel()
	computed := &ArithmeticValue{
		Op:    OpAdd,
		Left:  NewFieldValueWithResolvedOrdinal("X", 0, UnknownType),
		Right: &ConstantValue{Value: int64(1), Typ: NotNullLong},
	}
	aliasedName := OutputColumnName(computed, "X")
	derivedName := OutputColumnName(computed, "")
	if aliasedName == derivedName {
		t.Fatalf("test precondition failed: alias and derived output names both %q", aliasedName)
	}
	if ProjectionOutputIdentityKey(computed, "X") == ProjectionOutputIdentityKey(computed, "") {
		t.Fatalf("identity key collapsed explicit alias %q with derived name %q", aliasedName, derivedName)
	}
}

func TestProjectionOutputIdentityKey_IncludesMultiAccessorDisplayPath(t *testing.T) {
	t.Parallel()
	build := func(rootName string) *FieldValue {
		return &FieldValue{
			Field: "CITY",
			Typ:   UnknownType,
			Resolved: &FieldPath{Accessors: []ResolvedAccessor{
				{Field: rootName, Ordinal: 0},
				{Field: "CITY", Ordinal: 1},
			}},
		}
	}
	left, right := build("ADDRESS"), build("HOME")
	if !SemanticEqualsUnderAliasMap(left, right, AliasMap{}) {
		t.Fatal("test requires equal ordinal paths with different display names to be semantically equal")
	}
	if ExplainValue(left) == ExplainValue(right) {
		t.Fatalf("test precondition failed: multi-accessor renderings both %q", ExplainValue(left))
	}
	if ProjectionOutputIdentityKey(left, "") == ProjectionOutputIdentityKey(right, "") {
		t.Fatalf("identity key collapsed rendered paths %q and %q", ExplainValue(left), ExplainValue(right))
	}
}

func TestCanBridgeOrderingFieldValues_PreservesOrdinalIdentity(t *testing.T) {
	t.Parallel()

	lazy := NewFlatFieldValue("sort_key", UnknownType)
	baked3 := NewFieldValueWithResolvedOrdinal("SORT_KEY", 3, UnknownType)
	baked4 := NewFieldValueWithResolvedOrdinal("SORT_KEY", 4, UnknownType)
	nested := &FieldValue{
		Field: "SORT_KEY",
		Typ:   UnknownType,
		Resolved: &FieldPath{Accessors: []ResolvedAccessor{
			{Field: "ROW", Ordinal: 0},
			{Field: "SORT_KEY", Ordinal: 3},
		}},
	}

	if !CanBridgeOrderingFieldValues(lazy, baked3) {
		t.Fatal("lazy and single-accessor baked forms of one flat column should bridge")
	}
	if CanBridgeOrderingFieldValues(baked3, baked4) {
		t.Fatal("two differently baked ordinals must never bridge by display name")
	}
	if CanBridgeOrderingFieldValues(lazy, nested) {
		t.Fatal("a nested baked path must not bridge to a flat lazy field")
	}
}

func TestCanBridgeOrderingValueRoots_QualifiedToSourceLocal(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("C")
	otherAlias := NamedCorrelationIdentifier("D")
	qualified := NewCorrelatedFieldValueWithResolvedOrdinal(
		NewQuantifiedObjectValue(alias), "NAME", 1, UnknownType)
	local := NewFlatFieldValue("name", UnknownType)
	otherOrdinal := NewFieldValueWithResolvedOrdinal("NAME", 2, UnknownType)
	nested := &FieldValue{
		Field: "CITY",
		Typ:   UnknownType,
		Child: &FieldValue{
			Field: "ADDR",
			Typ:   UnknownType,
			Child: NewQuantifiedObjectValue(alias),
		},
	}
	topLevelCity := NewFlatFieldValue("CITY", UnknownType)

	if !CanBridgeOrderingValueRoots(qualified, local) {
		t.Fatal("a child-scoped qualified key should bridge to its source-local candidate key")
	}
	if CanBridgeOrderingValueRoots(qualified, otherOrdinal) {
		t.Fatal("cross-root values with different baked ordinals must not bridge")
	}
	if CanBridgeOrderingValueRoots(
		NewFieldValueWithResolvedOrdinal("NAME", 1, UnknownType),
		otherOrdinal,
	) {
		t.Fatal("two source-local baked ordinals must not bridge")
	}
	if CanBridgeOrderingValueRoots(
		NewFieldValue(NewQuantifiedObjectValue(alias), "NAME", UnknownType),
		NewFieldValue(NewQuantifiedObjectValue(otherAlias), "NAME", UnknownType),
	) {
		t.Fatal("two QOV-rooted values are ambiguous and must not bridge")
	}
	if CanBridgeOrderingValueRoots(nested, topLevelCity) {
		t.Fatal("a nested path must not bridge to a same-leaf top-level key")
	}
}
