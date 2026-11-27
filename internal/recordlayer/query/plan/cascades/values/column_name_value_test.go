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

	lazy := newFlatFieldValue("AMOUNT", UnknownType)
	baked := newFieldValueWithResolvedOrdinal("AMOUNT", 2, UnknownType)

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
	sum := &ArithmeticValue{Op: OpMul, Left: baked, Right: newFieldValueWithResolvedOrdinal("QTY", 3, UnknownType)}
	if got := ColumnNameValue(sum); got != "(AMOUNT * QTY)" {
		t.Fatalf("ColumnNameValue(computed) = %q, want (AMOUNT * QTY)", got)
	}
	if got := ExplainValue(sum); got != "(AMOUNT#2 * QTY#3)" {
		t.Fatalf("ExplainValue(computed) = %q, want (AMOUNT#2 * QTY#3)", got)
	}

	// Multi-accessor baked path: every step renders name-only in the
	// column-name form, name#ordinal in the explain form.
	multi := &fieldValue{
		Field: "CITY",
		Typ:   UnknownType,
		Resolved: &fieldPath{Accessors: []resolvedAccessor{
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

func TestColumnNameValue_SuppressesOnlyTaggedCurrentOwner(t *testing.T) {
	t.Parallel()

	row := NewRecordType("", false, []Field{{
		Name: "SORT_KEY", FieldType: NullableLong, Ordinal: 0,
	}})
	layout, err := NewOrdinalLayoutForCarrierType(
		row,
		[]OrdinalTileSpec{{Start: 0, Width: 1, Kind: OrdinalTileFlat}},
		nil,
	)
	if err != nil {
		t.Fatalf("current layout: %v", err)
	}
	currentField, err := ResolveFieldOrdinals(layout.Carrier(), []int{0})
	if err != nil {
		t.Fatalf("current field: %v", err)
	}
	if got := ColumnNameValue(currentField); got != "SORT_KEY" {
		t.Fatalf("tagged-current column name = %q, want SORT_KEY", got)
	}

	// Render text is not identity: a user-named `_current` alias remains a
	// real qualifier and cannot acquire the tagged owner's privilege.
	named, err := NewQuantifiedObjectValue(
		NamedCorrelationIdentifier("_current"), row,
	)
	if err != nil {
		t.Fatalf("named carrier: %v", err)
	}
	namedField, err := ResolveFieldOrdinals(named, []int{0})
	if err != nil {
		t.Fatalf("named field: %v", err)
	}
	if got := ColumnNameValue(namedField); got != "_current.SORT_KEY" {
		t.Fatalf("named _current column name = %q, want _current.SORT_KEY", got)
	}
}

func TestProjectionOutputIdentityKey_DistinguishesAliasFromDerivedExpression(t *testing.T) {
	t.Parallel()
	computed := &ArithmeticValue{
		Op:    OpAdd,
		Left:  newFieldValueWithResolvedOrdinal("X", 0, UnknownType),
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
	build := func(rootName string) *fieldValue {
		return &fieldValue{
			Field: "CITY",
			Typ:   UnknownType,
			Resolved: &fieldPath{Accessors: []resolvedAccessor{
				{Field: rootName, Ordinal: 0},
				{Field: "CITY", Ordinal: 1},
			}},
		}
	}
	left, right := build("ADDRESS"), build("HOME")
	if !SemanticEqualsUnderAliasMap(left, right, mustAliasMap(t)) {
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

	lazy := newFlatFieldValue("sort_key", UnknownType)
	baked3 := newFieldValueWithResolvedOrdinal("SORT_KEY", 3, UnknownType)
	baked4 := newFieldValueWithResolvedOrdinal("SORT_KEY", 4, UnknownType)
	nested := &fieldValue{
		Field: "SORT_KEY",
		Typ:   UnknownType,
		Resolved: &fieldPath{Accessors: []resolvedAccessor{
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
	qualified := newCorrelatedFieldValueWithResolvedOrdinal(
		mustQOV(t, alias), "NAME", 1, UnknownType)
	local := newFlatFieldValue("name", UnknownType)
	otherOrdinal := newFieldValueWithResolvedOrdinal("NAME", 2, UnknownType)
	nested := &fieldValue{
		Field: "CITY",
		Typ:   UnknownType,
		Child: &fieldValue{
			Field: "ADDR",
			Typ:   UnknownType,
			Child: mustQOV(t, alias),
		},
	}
	topLevelCity := newFlatFieldValue("CITY", UnknownType)

	if !CanBridgeOrderingValueRoots(qualified, local) {
		t.Fatal("a child-scoped qualified key should bridge to its source-local candidate key")
	}
	if CanBridgeOrderingValueRoots(qualified, otherOrdinal) {
		t.Fatal("cross-root values with different baked ordinals must not bridge")
	}
	if CanBridgeOrderingValueRoots(
		newFieldValueWithResolvedOrdinal("NAME", 1, UnknownType),
		otherOrdinal,
	) {
		t.Fatal("two source-local baked ordinals must not bridge")
	}
	if CanBridgeOrderingValueRoots(
		newFieldValue(mustQOV(t, alias), "NAME", UnknownType),
		newFieldValue(mustQOV(t, otherAlias), "NAME", UnknownType),
	) {
		t.Fatal("two QOV-rooted values are ambiguous and must not bridge")
	}
	if CanBridgeOrderingValueRoots(nested, topLevelCity) {
		t.Fatal("a nested path must not bridge to a same-leaf top-level key")
	}
}

func TestCanBridgeOrderingValueRoots_CurrentPhaseBoundary(t *testing.T) {
	t.Parallel()

	stringType := NotNullString
	recordType := &RecordType{Fields: []Field{
		{Name: "ADDRESS", Ordinal: 0, FieldType: &RecordType{Fields: []Field{{Name: "NAME", Ordinal: 0, FieldType: stringType}}}},
		{Name: "NAME", Ordinal: 1, FieldType: stringType},
		{Name: "OTHER", Ordinal: 2, FieldType: stringType},
	}}
	newCurrentRoot := func() *quantifiedObjectValue {
		layout, err := NewOrdinalLayoutForCarrierType(recordType, []OrdinalTileSpec{{
			Start: 0,
			Width: len(recordType.Fields),
			Kind:  OrdinalTileFlat,
		}}, nil)
		if err != nil {
			t.Fatalf("NewOrdinalLayoutForCarrierType: %v", err)
		}
		root, ok := layout.Carrier().(*quantifiedObjectValue)
		if !ok {
			t.Fatalf("layout carrier = %T, want values-owned QOV", layout.Carrier())
		}
		return root
	}
	fieldAt := func(root Value, ordinals ...int) Value {
		field, err := ResolveFieldOrdinals(root, ordinals)
		if err != nil {
			t.Fatalf("ResolveFieldOrdinals(%v): %v", ordinals, err)
		}
		return field
	}

	currentRoot := newCurrentRoot()
	currentName := fieldAt(currentRoot, 1)
	namedA := mustQOV(t, NamedCorrelationIdentifier("request-a"), recordType)
	namedB := mustQOV(t, NamedCorrelationIdentifier("request-b"), recordType)
	namedName := fieldAt(namedA, 1)
	if !CanBridgeOrderingValueRoots(currentName, namedName) ||
		!CanBridgeOrderingValueRoots(namedName, currentName) {
		t.Fatal("same typed full path did not bridge across current and named phases")
	}
	if CanBridgeOrderingValueRoots(namedName, fieldAt(namedB, 1)) {
		t.Fatal("two named roots bridged without an explicit alias translation")
	}

	if !CanBridgeOrderingValueRoots(currentName, fieldAt(currentRoot, 1)) {
		t.Fatal("structurally identical values over one current owner handle did not match")
	}
	otherCurrent := newCurrentRoot()
	if CanBridgeOrderingValueRoots(currentName, fieldAt(otherCurrent, 1)) {
		t.Fatal("equal-looking current roots from distinct owner phases bridged")
	}

	if CanBridgeOrderingValueRoots(currentName, fieldAt(namedA, 0, 0)) {
		t.Fatal("different full accessor paths bridged")
	}
	if CanBridgeOrderingValueRoots(
		currentName,
		newCorrelatedFieldValueWithResolvedOrdinal(namedA, "NAME", 2, stringType),
	) {
		t.Fatal("different resolved ordinals bridged")
	}
	if CanBridgeOrderingValueRoots(
		currentName,
		newCorrelatedFieldValueWithResolvedOrdinal(namedA, "NAME", 1, NullableString),
	) {
		t.Fatal("different result types bridged")
	}

	forgery := mustQOV(t, NamedCorrelationIdentifier(CurrentCorrelation().Name()), recordType)
	if CanBridgeOrderingValueRoots(currentName, fieldAt(forgery, 1)) {
		t.Fatal("named _current forgery acquired tagged-current bridge authority")
	}
}
