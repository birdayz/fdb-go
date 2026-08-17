package values

import "testing"

// Three shortcuts take a fact off a value's own exact handle instead of deriving
// it again from a thawed Type graph: ExactTypeForValue, OrdinalDomainOfQuantified
// and newPullUpOutputQOVForSource. Each is only sound because the long way round
// provably lands back on the SAME interned node, and each is silent when it
// breaks — a shortcut answering a different handle would keep planning and
// produce a memo that dedups differently, not an error. These pin the
// equivalence, not the saving.

func exactShortcutRecordType(name string) Type {
	return NewRecordType(name, false, []Field{
		{Name: "ID", FieldType: NotNullLong, Ordinal: 0},
		{Name: "VAL", FieldType: NullableString, Ordinal: 1},
	})
}

// TestExactTypeForValueIsTheRoundTripItReplaces drives the equivalence over the
// shapes whose identity is FINER than canonical equality. The intern table keys
// on the record/enum NAME as well as the shape, because thaw rebuilds the name
// from the node — so a same-shape different-name pair is exactly where a
// shortcut that returned "an equal handle" rather than "the same handle" would
// start reporting the wrong name.
func TestExactTypeForValueIsTheRoundTripItReplaces(t *testing.T) {
	t.Parallel()

	for _, typ := range []Type{
		exactShortcutRecordType("T"),
		exactShortcutRecordType("OTHER"),
		exactShortcutRecordType(""),
		NotNullLong,
		NullableString,
		&ArrayType{ElementType: NotNullLong},
	} {
		qov, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("q"), typ)
		if err != nil {
			t.Fatalf("QOV over %s: %v", DescribeType(typ), err)
		}
		shortcut, err := ExactTypeForValue(qov)
		if err != nil {
			t.Fatalf("ExactTypeForValue over %s: %v", DescribeType(typ), err)
		}
		roundTrip, err := SnapshotExactType(qov.Type())
		if err != nil {
			t.Fatalf("round trip over %s: %v", DescribeType(typ), err)
		}
		if shortcut != roundTrip {
			t.Errorf("over %s the shortcut handle is not the one the round trip "+
				"lands on; interning is what makes it the same object, so the memo "+
				"would start deduping two spellings of one type apart",
				DescribeType(typ))
		}
		if !SharedExactType(shortcut).Equals(SharedExactType(roundTrip)) {
			t.Errorf("over %s the two handles describe different types", DescribeType(typ))
		}
	}

	// A value that does NOT carry a handle must still be answered, by derivation.
	constant := &ConstantValue{Value: int64(3), Typ: NotNullLong}
	derived, err := ExactTypeForValue(constant)
	if err != nil {
		t.Fatalf("ExactTypeForValue over a constant: %v", err)
	}
	if derived == nil || derived.Type().Code() != TypeCodeLong {
		t.Errorf("a handle-less value was not derived: %v", derived)
	}
}

// TestOrdinalDomainMemoTracksTheTypeItWasTakenFrom pins the memo on the exact
// node. A one-entry memo that answered a NEIGHBOUR's domain would hand two
// different row shapes the same domain token, and the token is precisely what
// stops a baked ordinal being read against the wrong layout — so the failure
// would be a wrong column, silently.
func TestOrdinalDomainMemoTracksTheTypeItWasTakenFrom(t *testing.T) {
	t.Parallel()

	wide, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("w"), exactShortcutRecordType("W"))
	if err != nil {
		t.Fatalf("wide QOV: %v", err)
	}
	narrow, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("n"),
		NewRecordType("N", false, []Field{{Name: "ID", FieldType: NotNullLong, Ordinal: 0}}))
	if err != nil {
		t.Fatalf("narrow QOV: %v", err)
	}

	// Twice each, interleaved: the second read of each comes from the memo, and an
	// entry keyed on nothing would answer with whichever type was asked last.
	wideFirst := OrdinalDomainOfQuantified(wide)
	narrowFirst := OrdinalDomainOfQuantified(narrow)
	wideAgain := OrdinalDomainOfQuantified(wide)
	narrowAgain := OrdinalDomainOfQuantified(narrow)

	if !wideFirst.IsKnown() || !narrowFirst.IsKnown() {
		t.Fatal("a record row produced the unknown domain token")
	}
	if wideFirst != wideAgain || narrowFirst != narrowAgain {
		t.Error("the memo answered differently on a second read of the same type")
	}
	if wideFirst == narrowFirst {
		t.Error("two different row shapes share one domain token — a baked ordinal " +
			"could then be read against the wrong layout")
	}
	if got := OrdinalDomainOfType(wide.FlowedType()); got != wideFirst {
		t.Errorf("the memo disagrees with the derivation it replaces: %v vs %v", wideFirst, got)
	}
	if got := OrdinalDomainOfType(narrow.FlowedType()); got != narrowFirst {
		t.Errorf("the memo disagrees with the derivation it replaces: %v vs %v", narrowFirst, got)
	}
}

// TestValueChildrenMatchesChildrenIncludingNesting pins the scratch-backed walk
// helper against the interface it shortcuts: every arm must answer with the same
// child Children() would, and the walks built on it must still visit every node.
//
// The aliasing is NOT what this guards, and the distinction was established by
// mutation rather than assumed: sharing one package-level scratch across every
// nested frame leaves all four walks correct, because a one-element slice has its
// element copied out by `range` before any recursion runs. What the arm-by-arm
// comparison does catch is an arm answering with the WRONG child, and the length
// assertion catches an arm that starts writing a second one into a [1]Value.
func TestValueChildrenMatchesChildrenIncludingNesting(t *testing.T) {
	t.Parallel()

	qov, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("q"), exactShortcutRecordType("T"))
	if err != nil {
		t.Fatalf("QOV: %v", err)
	}
	field, err := ResolveFieldAccess(qov, []FieldRequest{mustFieldByName(t, "ID")})
	if err != nil {
		t.Fatalf("resolve ID: %v", err)
	}
	promoted := &PromoteValue{Child: field, Target: NullableLong}
	cast := &CastValue{Child: promoted, Target: NullableString}

	for _, v := range []Value{qov, field, promoted, cast} {
		var scratch [1]Value
		shortcut := valueChildren(v, &scratch)
		canonical := v.Children()
		if len(shortcut) != len(canonical) {
			t.Fatalf("%s: valueChildren gave %d children, Children gave %d",
				v.Name(), len(shortcut), len(canonical))
		}
		for i := range canonical {
			if shortcut[i] != canonical[i] {
				t.Errorf("%s: child %d differs from Children()", v.Name(), i)
			}
		}
		// The scratch is a [1]Value and its safety under recursion rests on that:
		// an arm answering with two elements out of one slot would be reading
		// past it.
		if len(shortcut) > 1 && &shortcut[0] == &scratch[0] {
			t.Errorf("%s: a scratch-backed answer has %d elements; the aliasing is only "+
				"safe for exactly one", v.Name(), len(shortcut))
		}
	}

	// The nested walk: every node of cast -> promoted -> field -> qov must be
	// visited exactly once.
	seen := map[Value]int{}
	WalkValue(cast, func(v Value) bool { seen[v]++; return true })
	for _, want := range []Value{cast, promoted, field, qov} {
		if seen[want] != 1 {
			t.Errorf("nested walk visited %s %d times, want 1", want.Name(), seen[want])
		}
	}
	if got := ValueSize(cast); got != 4 {
		t.Errorf("ValueSize over a 4-node chain = %d", got)
	}
	// Two independently built but equal chains must still compare equal and hash
	// alike through the shortcut walks — the equal-implies-same-hash invariant the
	// memo rests on, which a walk that dropped or duplicated a child would break.
	twinField, err := ResolveFieldAccess(qov, []FieldRequest{mustFieldByName(t, "ID")})
	if err != nil {
		t.Fatalf("resolve ID again: %v", err)
	}
	twin := &CastValue{
		Child:  &PromoteValue{Child: twinField, Target: NullableLong},
		Target: NullableString,
	}
	if !SemanticEqualsUnderAliasMap(cast, twin, EmptyAliasMap()) {
		t.Error("the equality walk no longer finds two equal chains equal")
	}
	if SemanticHashCode(cast) != SemanticHashCode(twin) {
		t.Error("two equal chains hash differently, so the memo would hold both")
	}
}

func mustFieldByName(t *testing.T, name string) FieldRequest {
	t.Helper()
	request, err := FieldByName(name)
	if err != nil {
		t.Fatalf("FieldByName(%q): %v", name, err)
	}
	return request
}

// TestPullUpOutputRootMatchesTheRoundTrip pins the pull-up shortcut: the output
// root it mints from a source's handle must be the value the thaw-and-snapshot
// spelling produced, since pull-up results are compared and interned in the memo.
func TestPullUpOutputRootMatchesTheRoundTrip(t *testing.T) {
	t.Parallel()

	source, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("q"), exactShortcutRecordType("T"))
	if err != nil {
		t.Fatalf("source QOV: %v", err)
	}
	alias := NamedCorrelationIdentifier("out")

	shortcut, err := newPullUpOutputQOVForSource(alias, source)
	if err != nil {
		t.Fatalf("newPullUpOutputQOVForSource: %v", err)
	}
	long, err := newPullUpOutputQOV(alias, source.Type())
	if err != nil {
		t.Fatalf("newPullUpOutputQOV: %v", err)
	}
	if shortcut.Correlation() != long.Correlation() {
		t.Error("the shortcut minted a different correlation")
	}
	if !SemanticEqualsUnderAliasMap(shortcut, long, EmptyAliasMap()) {
		t.Error("the shortcut root is not semantically the one the round trip mints, " +
			"so pull-up results would stop interning as one memo member")
	}
	shortcutHandle, err := ExactTypeForValue(shortcut)
	if err != nil {
		t.Fatalf("shortcut handle: %v", err)
	}
	longHandle, err := ExactTypeForValue(long)
	if err != nil {
		t.Fatalf("long handle: %v", err)
	}
	if shortcutHandle != longHandle {
		t.Error("the two roots carry different exact handles")
	}
}
