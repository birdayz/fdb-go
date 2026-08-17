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
// composite and primitive shapes a QOV can flow.
//
// It pins the CONTRACT, not the mechanism, and the distinction matters because
// both halves route through the interner: with the shortcut disabled outright,
// SnapshotExactType(value.Type()) lands back on the same interned node and every
// assertion here still holds. The mechanism is pinned separately by
// TestTheShortcutReadsTheHandleRatherThanDerivingIt below.
//
// The differently-named rows here are shape coverage, NOT the name axis. Dropping
// the record name from the intern key leaves this test green — measured; it
// reddens TestExactInterningKeepsRecordNamesApart,
// TestRFC232QOVSnapshotsAndDefensivelyThawsItsType and one arm of
// TestTranslateProjectionInputNameNormalizationToCorrelationIsExactAndFailClosed,
// which are where that axis actually lives.
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
		// A VALUE, not a second relationship. Comparing the two handles to each
		// other adds nothing once the pointer check above has passed — they are
		// the same object, so Equals is x.Equals(x) — and it reads like an
		// independent check. What is independent is whether the handle describes
		// the type it was ASKED about: a shortcut that consistently answered some
		// OTHER interned node would satisfy both relationships and fail here.
		if got := SharedExactType(shortcut); !got.Equals(typ) {
			t.Errorf("over %s the handle describes %s", DescribeType(typ), DescribeType(got))
		}
		// And for a record the NAME has to survive, which is the axis Equals
		// deliberately ignores: canonical identity excludes the record name, so
		// only thawing it back can catch a handle that carries the wrong one.
		if want, ok := typ.(*RecordType); ok {
			got, isRecord := SharedExactType(shortcut).(*RecordType)
			if !isRecord {
				t.Errorf("over %s the handle thawed to a non-record %s",
					DescribeType(typ), DescribeType(SharedExactType(shortcut)))
			} else if got.RecordName != want.RecordName {
				t.Errorf("over %s the handle reports record name %q, want %q",
					DescribeType(typ), got.RecordName, want.RecordName)
			}
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

// TestEveryExactNodeIsInterned is the invariant ExactTypeForValue's shortcut
// rests on, made checkable.
//
// The shortcut is only equivalent to the round trip because a QOV's handle is
// the SAME OBJECT the round trip would land on, and that is true only while every
// path that builds an exact node routes through the intern table. Two paths did
// not — the record-constructor type and the nullability-widened copy — and the
// consequence was invisible: an un-interned node is a correct TYPE, so nothing
// answers wrongly; it just compares unequal (children compare by pointer) to an
// identical interned one, silently defeating the sharing of everything above it,
// and folds a zero intern hash into one hot bucket.
//
// internHashValue is the witness: internedExactType stamps it, and only it does.
func TestEveryExactNodeIsInterned(t *testing.T) {
	t.Parallel()

	record := NewRecordType("T", false, []Field{
		{Name: "ID", FieldType: NotNullLong, Ordinal: 0},
		{Name: "NESTED", FieldType: NewRecordType("N", true, []Field{
			{Name: "X", FieldType: NullableString, Ordinal: 0},
		}), Ordinal: 1},
	})

	handle, err := SnapshotExactType(record)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	assertInterned(t, "snapshot", handle.(*exactType))

	relation, err := ExactRelationOf(record)
	if err != nil {
		t.Fatalf("relation: %v", err)
	}
	assertInterned(t, "relation", relation.(*exactType))

	// A record-constructor type: built from already-exact children rather than
	// from an ordinary Type graph.
	qov, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("q"), record)
	if err != nil {
		t.Fatalf("QOV: %v", err)
	}
	field, err := ResolveFieldAccess(qov, []FieldRequest{mustFieldByName(t, "ID")})
	if err != nil {
		t.Fatalf("resolve ID: %v", err)
	}
	constructor := NewRecordConstructorValue(RecordConstructorField{Name: "ID", Value: field})
	constructed, err := exactRecordConstructorType(constructor)
	if err != nil {
		t.Fatalf("exactRecordConstructorType: %v", err)
	}
	assertInterned(t, "record constructor", constructed)

	// A nullability-widened copy, on a record and on a primitive.
	assertInterned(t, "widened record", exactWithNullability(handle.(*exactType), true))
	primitive, err := SnapshotExactType(NotNullLong)
	if err != nil {
		t.Fatalf("primitive snapshot: %v", err)
	}
	assertInterned(t, "widened primitive", exactWithNullability(primitive.(*exactType), true))

	// And the widened copy is SHARED: asking twice yields one object, which is what
	// the intern table buys and what a bypassing path loses.
	first := exactWithNullability(handle.(*exactType), true)
	second := exactWithNullability(handle.(*exactType), true)
	if first != second {
		t.Error("two widened copies of one handle are two objects, so the widened " +
			"node is not interned")
	}
	if first.nullable == handle.(*exactType).nullable {
		t.Error("widening did not change nullability, so this test proves nothing")
	}
}

// assertInterned requires that node is THE shared object for its content — the one
// a fresh lookup returns — not merely that it looks interned.
//
// It used to check `internHashValue != 0` and its doc claimed that field is the
// witness because "only internedExactType stamps it". That was false: the static
// primitive table stamps it directly too (see sharedPrimitiveExactTypes), and so
// does sharedPrimitiveExactType's fail-closed fallback. The consequence was not
// theoretical — this helper was called on a widened primitive, which WAS a duplicate
// object, and passed, because the duplicate carries a perfectly good hash. A witness
// that any construction path can forge is not a witness.
//
// So look the node up and require POINTER IDENTITY, against whichever table owns its
// kind. That is the property the sharing actually rests on, and it is the only one
// that can distinguish "interned" from "equal to something interned".
func assertInterned(t *testing.T, what string, node *exactType) {
	t.Helper()
	if node == nil {
		t.Fatalf("%s: nil node", what)
	}
	if node.internHashValue == 0 {
		t.Errorf("%s: node carries no intern hash at all", what)
	}
	if _, isPrimitive := sharedPrimitiveExactTypes[node.code]; isPrimitive &&
		len(node.fields) == 0 && node.element == nil &&
		len(node.enumValues) == 0 && !node.anyRecord {
		// Primitives are owned by the STATIC table, which is never in the shards.
		if shared := sharedPrimitiveExactType(node.code, node.nullable); shared != node {
			t.Errorf("%s: primitive %v/nullable=%v is a DUPLICATE of the shared static "+
				"node (%p vs %p); it is exactTypesEqual and carries the same intern "+
				"hash, so only pointer identity can see this — and every composite "+
				"built over it stops matching the identical composite built over the "+
				"shared node, because children compare by POINTER",
				what, node.code, node.nullable, node, shared)
		}
	} else {
		// Composites are owned by the intern shards. Rebuild the probe and require
		// the lookup to return this very object.
		probe := exactProbe{
			code: node.code, nullable: node.nullable, anyRecord: node.anyRecord,
			name: node.name, element: node.element, enumValues: node.enumValues,
		}
		for i := range node.fields {
			probe.srcFields = append(probe.srcFields,
				Field{Name: node.fields[i].name, Ordinal: node.fields[i].ordinal})
			probe.children = append(probe.children, node.fields[i].typ)
		}
		hash := probe.internHash()
		shard := &exactInterned[hash%exactInternShards]
		shard.mu.RLock()
		found := lookupInternedLocked(shard, hash, &probe)
		shard.mu.RUnlock()
		if found != node {
			t.Errorf("%s: the intern table does not return this object for its own "+
				"content (%p, table has %p); an equal type can therefore exist twice "+
				"and the pointer-compared children of everything above it stop matching",
				what, node, found)
		}
	}
	for i := range node.fields {
		assertInterned(t, what+".field["+node.fields[i].name+"]", node.fields[i].typ)
	}
	if node.element != nil {
		assertInterned(t, what+".element", node.element)
	}
}

// TestTheShortcutReadsTheHandleRatherThanDerivingIt pins the MECHANISM the test
// above cannot see.
//
// ExactTypeForValue's saving is entirely in not calling value.Type(): that thaws a
// whole ordinary Type graph and then walks it to rebuild a node the value is
// already holding. Interning makes the long way round land on the same object, so
// no comparison of the two answers can tell whether the shortcut ran — disable it
// and the equivalence test stays green. exactTypeOfValue IS the shortcut, so pin
// it directly: it must recognize a QOV and hand back the value's own field.
//
// The failure this guards is pure cost, and therefore silent by construction: the
// derivation is correct, just per-call. On a 200-plan IN-list sweep the shortcut's
// callers ask once per baked field resolution.
func TestTheShortcutReadsTheHandleRatherThanDerivingIt(t *testing.T) {
	t.Parallel()

	qov, err := NewQuantifiedObjectValue(
		NamedCorrelationIdentifier("q"), exactShortcutRecordType("SHORTCUT_MECHANISM"))
	if err != nil {
		t.Fatalf("QOV: %v", err)
	}
	carried, ok := exactTypeOfValue(qov)
	if !ok {
		t.Fatal("the shortcut declined a QOV that carries a handle, so every caller " +
			"thaws the type graph and re-snapshots it to learn what the value already knows")
	}
	if carried != qov.(*quantifiedObjectValue).flowed {
		t.Error("the shortcut answered something other than the value's own handle")
	}
	answer, err := ExactTypeForValue(qov)
	if err != nil {
		t.Fatalf("ExactTypeForValue: %v", err)
	}
	if answer != carried {
		t.Error("ExactTypeForValue did not return what the shortcut found")
	}

	// And it declines what it cannot answer from a handle, rather than guessing:
	// a non-QOV has to fall through to the derivation.
	if _, ok := exactTypeOfValue(&ConstantValue{Value: int64(3), Typ: NotNullLong}); ok {
		t.Error("the shortcut claimed a handle on a value that carries none")
	}
}
