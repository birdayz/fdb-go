package values

import (
	"testing"
)

// The exact channel answers "are these two flowed types equal" from immutable
// handles instead of from a rebuilt Type graph. That substitution is only sound
// if handle arithmetic computes the SAME RELATION as Type.Equals — and the
// tempting shortcut, handle POINTER identity, does not. Interning keys on the
// record/enum NAME and Type.Equals deliberately ignores it, so two Equals-equal
// records get two handles. Substituting identity would report them different:
// a false negative, in the plan-changing direction.
//
// Neither TestRecordTypeEqualsIgnoresRecordName nor
// TestExactInterningKeepsRecordNamesApart fails under that substitution. Each
// observes one side of the asymmetry and neither observes a COMPARISON, which
// is how the wrong premise survived a full review lap. This file observes the
// comparison, over a corpus built to contain the disagreements.
//
// The corpus is compared PAIRWISE and TOTALLY: every ordered pair, both
// relations, no sampling. That is what makes it a differential test rather than
// a list of cases someone thought of — a shape whose two answers diverge fails
// here even if nobody predicted that shape.

// differentialCorpusEntry is one type in the corpus plus a label for failures.
type differentialCorpusEntry struct {
	label string
	typ   Type
}

// differentialCorpus builds the type population the pairwise sweep runs over.
//
// It is built to contain, deliberately and by name, every shape where the
// candidate fast paths could disagree with Equals:
//
//   - records differing ONLY in RecordName, and enums differing ONLY in
//     EnumName — Equals-EQUAL, handle-DISTINCT. These are the pairs that refute
//     pointer identity.
//   - the same record shape reached twice by two independently-built graphs —
//     Equals-equal and (through interning) handle-identical, so the sweep sees
//     both outcomes of the identity relation and not just the negative one.
//   - anyRecord against the concrete zero-field record, which share a TypeCode
//     and are not equal.
//   - nullability at every level: top-level, field, array element.
//   - field ORDER, field NAME, arity, enum value order and enum value NUMBER,
//     each varied one attribute at a time so a failure names the attribute.
func differentialCorpus() []differentialCorpusEntry {
	prim := func(code TypeCode, nullable bool) Type {
		return &PrimitiveType{TypeCode: code, Nullable: nullable}
	}
	rec := func(name string, nullable bool, fields ...Field) Type {
		// Copy the variadic slice before numbering it. Go allocates a fresh slice
		// for a spread of individual arguments, so today every caller here is
		// safe — but `rec(n, false, existing...)` passes the caller's slice
		// straight through, and the ordinals would be written into it. A one-line
		// copy removes the hazard instead of resting on how the callers happen to
		// spell themselves.
		owned := make([]Field, len(fields))
		copy(owned, fields)
		for i := range owned {
			owned[i].Ordinal = i
		}
		return &RecordType{RecordName: name, Nullable: nullable, Fields: owned}
	}
	field := func(name string, typ Type) Field { return Field{Name: name, FieldType: typ} }

	var corpus []differentialCorpusEntry
	add := func(label string, typ Type) {
		corpus = append(corpus, differentialCorpusEntry{label: label, typ: typ})
	}

	// Primitives across every admitted code, both nullabilities. The codes are
	// listed explicitly rather than derived from a range over TypeCode: the
	// unadmitted codes (UNKNOWN/ANY/NONE and the structured ones) are refused by
	// snapshotExactType, and a corpus that silently dropped them would look the
	// same as one that covered them.
	for _, code := range []TypeCode{
		TypeCodeNull, TypeCodeBoolean, TypeCodeInt, TypeCodeLong,
		TypeCodeFloat, TypeCodeDouble, TypeCodeString, TypeCodeBytes,
		TypeCodeVersion, TypeCodeUuid, TypeCodeDate, TypeCodeTimestamp,
	} {
		add(code.String()+"/notnull", prim(code, false))
		add(code.String()+"/nullable", prim(code, true))
	}

	// anyRecord vs the concrete unit record: same TypeCode, not equal.
	add("anyRecord/notnull", anyRecordType{})
	add("anyRecord/nullable", anyRecordType{nullable: true})
	add("record/empty/notnull", rec("", false))
	add("record/empty/nullable", rec("", true))

	// THE PAIRS THAT REFUTE POINTER IDENTITY. Same shape, different provenance
	// name: Equals says equal, interning says two objects.
	add("record/namedA", rec("A", false, field("ID", prim(TypeCodeLong, false))))
	add("record/namedB", rec("B", false, field("ID", prim(TypeCodeLong, false))))
	add("record/unnamed", rec("", false, field("ID", prim(TypeCodeLong, false))))

	// The same shape reached by an independently-constructed graph, so the sweep
	// observes handle identity holding as well as failing.
	add("record/namedA/rebuilt", rec("A", false, field("ID", prim(TypeCodeLong, false))))

	// One attribute at a time off record/namedA.
	add("record/fieldNameDiffers", rec("A", false, field("PK", prim(TypeCodeLong, false))))
	add("record/fieldTypeDiffers", rec("A", false, field("ID", prim(TypeCodeInt, false))))
	add("record/fieldNullableDiffers", rec("A", false, field("ID", prim(TypeCodeLong, true))))
	add("record/topNullableDiffers", rec("A", true, field("ID", prim(TypeCodeLong, false))))
	add("record/arityDiffers", rec("A", false,
		field("ID", prim(TypeCodeLong, false)),
		field("VAL", prim(TypeCodeString, true))))
	add("record/fieldOrderDiffers", rec("A", false,
		field("VAL", prim(TypeCodeString, true)),
		field("ID", prim(TypeCodeLong, false))))
	add("record/duplicateFieldNames", rec("A", false,
		field("ID", prim(TypeCodeLong, false)),
		field("ID", prim(TypeCodeLong, false))))

	// Nesting, so a disagreement below the root is reachable.
	add("record/nested/namedInner", rec("Outer", false,
		field("INNER", rec("A", false, field("ID", prim(TypeCodeLong, false))))))
	add("record/nested/differentlyNamedInner", rec("Outer", false,
		field("INNER", rec("B", false, field("ID", prim(TypeCodeLong, false))))))

	// Enums: the EnumName pair is the second refutation of pointer identity.
	enumVals := []EnumValue{{Name: "RED", Number: 0}, {Name: "GREEN", Number: 1}}
	reordered := []EnumValue{{Name: "GREEN", Number: 1}, {Name: "RED", Number: 0}}
	renumbered := []EnumValue{{Name: "RED", Number: 0}, {Name: "GREEN", Number: 7}}
	add("enum/namedA", &EnumType{EnumName: "Colour", Values: enumVals})
	add("enum/namedB", &EnumType{EnumName: "Shade", Values: enumVals})
	add("enum/unnamed", &EnumType{Values: enumVals})
	add("enum/nullable", &EnumType{EnumName: "Colour", Nullable: true, Values: enumVals})
	add("enum/reordered", &EnumType{EnumName: "Colour", Values: reordered})
	add("enum/renumbered", &EnumType{EnumName: "Colour", Values: renumbered})
	add("enum/shorter", &EnumType{EnumName: "Colour", Values: enumVals[:1]})
	add("enum/empty", &EnumType{EnumName: "Colour"})

	// Arrays: element identity and both nullability positions.
	add("array/long/notnull", &ArrayType{ElementType: prim(TypeCodeLong, false)})
	add("array/long/nullable", &ArrayType{Nullable: true, ElementType: prim(TypeCodeLong, false)})
	add("array/nullableLong", &ArrayType{ElementType: prim(TypeCodeLong, true)})
	add("array/int", &ArrayType{ElementType: prim(TypeCodeInt, false)})
	add("array/recordNamedA", &ArrayType{
		ElementType: rec("A", false, field("ID", prim(TypeCodeLong, false))),
	})
	add("array/recordNamedB", &ArrayType{
		ElementType: rec("B", false, field("ID", prim(TypeCodeLong, false))),
	})

	// Relations. RelationType.Equals ignores nullability because the type has
	// none to ignore — IsNullable is a constant false — so the canonical
	// encoding's nullable byte cannot diverge from Equals here. The arm is in
	// the corpus so that stays MEASURED rather than reasoned: adding a nullable
	// bit to RelationType would break this sweep.
	add("relation/recordNamedA", &RelationType{
		InnerType: rec("A", false, field("ID", prim(TypeCodeLong, false))),
	})
	add("relation/recordNamedB", &RelationType{
		InnerType: rec("B", false, field("ID", prim(TypeCodeLong, false))),
	})
	add("relation/long", &RelationType{InnerType: prim(TypeCodeLong, false)})

	return corpus
}

// snapshotCorpus snapshots every entry, failing loudly on a rejection rather
// than dropping the entry. A corpus that silently shrinks is the empty-set trap:
// the sweep still reports green, over a population that no longer contains the
// shape the test was written for.
func snapshotCorpus(t *testing.T, corpus []differentialCorpusEntry) []*exactType {
	t.Helper()
	handles := make([]*exactType, len(corpus))
	for i, entry := range corpus {
		handle, err := SnapshotExactType(entry.typ)
		if err != nil {
			t.Fatalf("corpus entry %q was refused by SnapshotExactType: %v\n"+
				"every entry must be snapshottable; a dropped entry silently shrinks "+
				"the population this sweep reports green over", entry.label, err)
		}
		handles[i] = handle.(*exactType)
	}
	return handles
}

// TestExactEqualityIsTypeEqualityOverTheWholeCorpus is the pin RFC-233 requires:
// over every ordered pair, exactTypesEqual and Type.Equals agree.
//
// It is stated over ORDERED pairs, not unordered, because Equals is implemented
// per concrete type and an asymmetric implementation is a real failure mode —
// anyRecordType.Equals type-asserts on the receiver's own type, so a mismatched
// pair takes two different code paths depending on which side is the receiver.
func TestExactEqualityIsTypeEqualityOverTheWholeCorpus(t *testing.T) {
	t.Parallel()
	corpus := differentialCorpus()
	handles := snapshotCorpus(t, corpus)

	// 58 is the population as built, so the floor is EXACT on the low side:
	// adding a shape is free, losing one fails. A round-number floor set below
	// the real size lets the corpus decay silently down to it.
	const corpusFloor = 58
	if len(corpus) < corpusFloor {
		t.Fatalf("corpus has %d entries, floor is %d — the sweep's strength is its "+
			"population, and a shrunken corpus reports green over the shapes it lost",
			len(corpus), corpusFloor)
	}

	var (
		agreedTrue  int
		agreedFalse int
	)
	for i := range corpus {
		for j := range corpus {
			want := corpus[i].typ.Equals(corpus[j].typ)
			got := exactTypesEqual(handles[i], handles[j])
			if got != want {
				t.Errorf("exactTypesEqual(%s, %s) = %v, but Equals = %v\n"+
					"the exact channel's equality must be the SAME RELATION as "+
					"Type.Equals; a disagreement here is a wrong planner answer, not "+
					"a slow one", corpus[i].label, corpus[j].label, got, want)
				continue
			}
			if want {
				agreedTrue++
			} else {
				agreedFalse++
			}
		}
	}

	// Both outcomes must be populated. A sweep that only ever agreed on FALSE
	// would pass with exactTypesEqual hard-wired to false.
	if agreedTrue == 0 || agreedFalse == 0 {
		t.Fatalf("degenerate sweep: %d true agreements, %d false — both must be "+
			"non-zero or a constant-returning implementation passes", agreedTrue, agreedFalse)
	}
}

// TestPointerIdentityWouldBeWrongAndThisCorpusProvesIt is the vacuity guard for
// the sweep above, and it is the arm that actually refutes RFC-233 v1.
//
// The sweep would still pass if the corpus happened to contain no pair where
// Equals and handle identity disagree — it would just be measuring nothing about
// the substitution the RFC rejects. So the disagreement is asserted directly:
// at least one Equals-EQUAL pair must hold two DISTINCT handles.
//
// The failure message names what breaks, because the natural "cleanup" here is
// to make interning name-insensitive, which would silently disarm the sweep by
// emptying this set rather than by breaking it.
func TestPointerIdentityWouldBeWrongAndThisCorpusProvesIt(t *testing.T) {
	t.Parallel()
	corpus := differentialCorpus()
	handles := snapshotCorpus(t, corpus)

	type disagreement struct{ left, right string }
	var refutations []disagreement
	for i := range corpus {
		for j := range corpus {
			if i == j {
				continue
			}
			if corpus[i].typ.Equals(corpus[j].typ) && handles[i] != handles[j] {
				refutations = append(refutations, disagreement{corpus[i].label, corpus[j].label})
			}
		}
	}
	if len(refutations) == 0 {
		t.Fatalf("no Equals-equal pair in the corpus holds two distinct handles.\n" +
			"The differential sweep is then vacuous with respect to the substitution " +
			"RFC-233 rejects: pointer identity would pass it. Either the corpus lost " +
			"its name-differing record and enum pairs, or interning stopped keying on " +
			"the name — in which case exact_type_intern.go's contract changed and this " +
			"file, RFC-233 §3.1 and TestExactInterningKeepsRecordNamesApart all need " +
			"rereading before anything is 'simplified'.")
	}

	// Name the expected refutations so a corpus that keeps SOME disagreement but
	// loses the record or the enum one is still caught. Both are cited by
	// RFC-233 §3.1; losing either leaves half the argument unpinned.
	sawRecord, sawEnum := false, false
	for _, r := range refutations {
		if r.left == "record/namedA" && r.right == "record/namedB" {
			sawRecord = true
		}
		if r.left == "enum/namedA" && r.right == "enum/namedB" {
			sawEnum = true
		}
	}
	if !sawRecord {
		t.Errorf("record/namedA vs record/namedB no longer disagree; RecordType.Equals " +
			"ignoring RecordName is half of RFC-233 §3.1 and is now unpinned here")
	}
	if !sawEnum {
		t.Errorf("enum/namedA vs enum/namedB no longer disagree; EnumType.Equals " +
			"ignoring EnumName is the other half and is now unpinned here")
	}
}

// TestExactRowShapesAgreeIsQuantifiedRowShapesAgree pins the second substitution
// RFC-233 makes: exactRowShapesAgree replaces QuantifiedRowShapesAgree.
//
// The two are NOT the same code. QRSA normalises with
// left.Equals(WithNullability(right, left.IsNullable())); exactRowShapesAgree
// hand-walks fields and enum values when the top-level bits differ. "Same rule"
// is a claim about two independent implementations, which is exactly the claim a
// differential sweep exists to check.
//
// Nothing is excluded from this sweep. The first version of it dropped the
// three RELATION entries, because QuantifiedRowShapesAgree used to PANIC on a
// relation operand — and an exclusion written to route around a crash is an
// exclusion that hides one. It is a measurement gap of exactly the shape
// CLAUDE.md calls a green over an empty set: the sweep reported agreement over a
// region where one side had no answer at all.
//
// QuantifiedRowShapesAgree is total now (see typeShapesAgreeBelowTheTop), so the
// region is swept rather than skipped, and the count is asserted at zero so the
// next person to hit a panic here cannot make it go away the same way.
func TestExactRowShapesAgreeIsQuantifiedRowShapesAgree(t *testing.T) {
	t.Parallel()
	corpus := differentialCorpus()
	handles := snapshotCorpus(t, corpus)

	comparable := make([]int, 0, len(corpus))
	for i, entry := range corpus {
		if entry.typ.Code() == TypeCodeRelation {
			continue
		}
		comparable = append(comparable, i)
	}
	if excluded := len(corpus) - len(comparable); excluded != 3 {
		t.Fatalf("expected exactly 3 RELATION entries in the corpus, got %d", excluded)
	}
	// Sweep EVERYTHING; `comparable` is computed only to assert the relation
	// entries are present and are no longer being routed around.
	comparable = comparable[:0]
	for i := range corpus {
		comparable = append(comparable, i)
	}

	var agreedTrue, agreedFalse int
	for _, i := range comparable {
		for _, j := range comparable {
			want := QuantifiedRowShapesAgree(corpus[i].typ, corpus[j].typ)
			got := exactRowShapesAgree(handles[i], handles[j])
			if got != want {
				t.Errorf("exactRowShapesAgree(%s, %s) = %v, but QuantifiedRowShapesAgree = %v",
					corpus[i].label, corpus[j].label, got, want)
				continue
			}
			if want {
				agreedTrue++
			} else {
				agreedFalse++
			}
		}
	}
	if agreedTrue == 0 || agreedFalse == 0 {
		t.Fatalf("degenerate sweep: %d true agreements, %d false", agreedTrue, agreedFalse)
	}

	// The nullability tolerance is the entire reason this pair is not just
	// exactTypesEqual, so assert it fires rather than trusting the sweep to have
	// happened across it: at least one pair must agree on SHAPE while disagreeing
	// on exact equality.
	tolerated := 0
	for _, i := range comparable {
		for _, j := range comparable {
			if exactRowShapesAgree(handles[i], handles[j]) && !exactTypesEqual(handles[i], handles[j]) {
				tolerated++
			}
		}
	}
	if tolerated == 0 {
		t.Fatal("no pair in the corpus is shape-equal but not exactly equal, so this " +
			"sweep never exercised the top-level nullability tolerance that " +
			"distinguishes exactRowShapesAgree from exactTypesEqual")
	}
}

// legacyQuantifiedRowShapesAgree is the expression QuantifiedRowShapesAgree used
// to evaluate, kept HERE rather than deleted because it is the reference the
// replacement is differentially checked against.
//
// It returns (answer, panicked). Its panics are the point: they are what made
// the old form partial, and a reference that crashed the test process could not
// be used as a reference at all.
func legacyQuantifiedRowShapesAgree(left, right Type) (agree, panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			agree, panicked = false, true
		}
	}()
	if left == nil || right == nil {
		return false, false
	}
	if left.IsNullable() == right.IsNullable() {
		return left.Equals(right), false
	}
	return left.Equals(WithNullability(right, left.IsNullable())), false
}

// panicProneTypes are the three classes WithNullability refuses to flip, which
// is what made the old comparison partial. NONE and ANY are NOT snapshottable —
// snapshotExactType refuses them as placeholders — so they cannot appear in the
// exact-handle corpus and are only reachable through the ordinary-Type surface,
// which is exactly the surface QuantifiedRowShapesAgree exposes.
func panicProneTypes() []differentialCorpusEntry {
	return []differentialCorpusEntry{
		{label: "relation/long", typ: &RelationType{InnerType: &PrimitiveType{TypeCode: TypeCodeLong}}},
		{label: "relation/record", typ: &RelationType{InnerType: &RecordType{
			Fields: []Field{{Name: "ID", FieldType: &PrimitiveType{TypeCode: TypeCodeLong}}},
		}}},
		{label: "none", typ: NoneType},
		{label: "any", typ: AnyType},
	}
}

// TestQuantifiedRowShapesAgreeIsTotalWhereItUsedToCrash pins the behaviour change
// this comparison's rewrite makes, in both directions at once: WHERE the old
// expression had an answer the new one gives the same answer, and where the old
// one had NO answer the new one answers false.
//
// Splitting those two claims would let either half pass alone. A rewrite that
// returned a constant false is total and never disagrees with a panic; a rewrite
// that kept the panic agrees everywhere it answers. Only the pair is a proof.
//
// The panic classes are asserted to be NON-EMPTY and enumerated by class, because
// the whole justification for the rewrite is that the old form crashed on inputs
// its callers can produce. If that set ever empties — someone gives RELATION a
// nullable bit, or WithNullability stops guarding NONE/ANY — this test would
// otherwise keep passing while measuring nothing.
func TestQuantifiedRowShapesAgreeIsTotalWhereItUsedToCrash(t *testing.T) {
	t.Parallel()

	corpus := append(differentialCorpus(), panicProneTypes()...)

	var (
		answered  int
		disagreed int
		crashed   []string
	)
	for i := range corpus {
		for j := range corpus {
			want, panicked := legacyQuantifiedRowShapesAgree(corpus[i].typ, corpus[j].typ)
			got := QuantifiedRowShapesAgree(corpus[i].typ, corpus[j].typ)
			if panicked {
				crashed = append(crashed, corpus[i].label+" vs "+corpus[j].label)
				if got {
					// The direction matters. Turning a crash into TRUE would let a
					// rule newly fire; turning it into FALSE can only stop one.
					t.Errorf("QuantifiedRowShapesAgree(%s, %s) = true where the old "+
						"expression panicked — a crash may only become a refusal, never "+
						"an agreement, or the rewrite lets a rule fire that never could",
						corpus[i].label, corpus[j].label)
				}
				continue
			}
			answered++
			if got != want {
				disagreed++
				t.Errorf("QuantifiedRowShapesAgree(%s, %s) = %v, old expression = %v",
					corpus[i].label, corpus[j].label, got, want)
			}
		}
	}

	if len(crashed) == 0 {
		t.Fatal("the old expression never panicked over this corpus, so this test " +
			"measured nothing about the totality it exists to pin. Either the " +
			"RELATION/NONE/ANY entries were dropped from panicProneTypes, or " +
			"WithNullability stopped refusing to flip them — in which case " +
			"typeShapesAgreeBelowTheTop's justification needs rereading, not this test " +
			"relaxing")
	}
	if answered == 0 {
		t.Fatal("the old expression panicked on EVERY pair, so the agreement half of " +
			"this test is vacuous")
	}

	// Enumerate the classes by name. A set that keeps the relation crashes while
	// losing NONE and ANY would still be non-empty above, and would silently stop
	// covering the two classes nobody found by hand.
	byClass := map[string]int{}
	for _, pair := range crashed {
		switch {
		case containsWord(pair, "none"):
			byClass["none"]++
		case containsWord(pair, "any"):
			byClass["any"]++
		case containsWord(pair, "relation"):
			byClass["relation"]++
		}
	}
	for _, class := range []string{"relation", "none", "any"} {
		if byClass[class] == 0 {
			t.Errorf("no crashing pair in class %q; WithNullability refuses to flip "+
				"RELATION, NONE and ANY and all three are reachable through this "+
				"comparison, so all three must be represented", class)
		}
	}
}

// containsWord reports whether label contains word as a whole path segment of a
// "left vs right" pair label. Substring matching would let "relation/record"
// count as the record class.
func containsWord(pair, word string) bool {
	start := 0
	for i := 0; i <= len(pair); i++ {
		if i == len(pair) || pair[i] == '/' || pair[i] == ' ' {
			if pair[start:i] == word {
				return true
			}
			start = i + 1
		}
	}
	return false
}

// TestQuantifiedRowShapesAgreeAllocatesNothing pins the second half of the
// rewrite's justification. The old expression called WithNullability, which
// builds a whole new RecordType to answer a boolean — on a comparison the
// planner runs per rule firing and the executor runs per row.
//
// A performance property with no test is a property that silently regresses:
// reintroducing a normalising rebuild would pass every correctness arm above.
func TestQuantifiedRowShapesAgreeAllocatesNothing(t *testing.T) {
	t.Parallel()

	// A record pair that takes the differing-bits arm — the arm that used to
	// allocate — and agrees, so the walk runs to completion rather than
	// short-circuiting on the first field.
	fields := []Field{
		{Name: "ID", FieldType: &PrimitiveType{TypeCode: TypeCodeLong}, Ordinal: 0},
		{Name: "VAL", FieldType: &PrimitiveType{TypeCode: TypeCodeString, Nullable: true}, Ordinal: 1},
	}
	left := &RecordType{RecordName: "R", Nullable: true, Fields: fields}
	right := &RecordType{RecordName: "R", Nullable: false, Fields: fields}
	if !QuantifiedRowShapesAgree(left, right) {
		t.Fatal("the probe pair must AGREE, or the walk short-circuits and the " +
			"allocation measurement covers only the first field")
	}

	// testing.Benchmark rather than testing.AllocsPerRun: this test runs under
	// t.Parallel() as every test here must, and AllocsPerRun panics outright when
	// called from a parallel test. Benchmark measures the same quantity with no
	// such restriction.
	result := testing.Benchmark(func(b *testing.B) {
		var agree bool
		for i := 0; i < b.N; i++ {
			agree = QuantifiedRowShapesAgree(left, right)
		}
		if !agree {
			b.Fatal("unexpected disagreement")
		}
	})
	if result.N == 0 {
		t.Fatal("the benchmark ran zero iterations, so its allocation figure " +
			"describes nothing")
	}
	if allocs := result.AllocsPerOp(); allocs != 0 {
		t.Errorf("QuantifiedRowShapesAgree allocated %d objects per call on the "+
			"differing-nullability arm over %d iterations; it must allocate none. "+
			"A normalising rebuild has been reintroduced", allocs, result.N)
	}
}

// TestTypeEqualsIsSymmetricOverTheCorpus pins the property that lets the
// comparison call sites be rewritten without checking each one's orientation.
//
// About a third of the sites RFC-233 converts had the ordinary type on the left
// and the quantifier on the right — `window.Equals(root.FlowedType())` — and the
// helper they route to takes the quantifier first. That rewrite is only sound if
// Equals is symmetric. It is, because every implementation type-asserts the
// operand to its own concrete type before comparing anything, so a mismatched
// pair is refused by whichever side is the receiver. But "is" is a claim about
// six implementations, and a seventh could be added.
//
// The panic-prone entries are included here because symmetry has to hold on the
// placeholder types too: Equals itself never builds a type, so unlike
// QuantifiedRowShapesAgree there is nothing here that could refuse to answer.
func TestTypeEqualsIsSymmetricOverTheCorpus(t *testing.T) {
	t.Parallel()
	corpus := append(differentialCorpus(), panicProneTypes()...)

	var symmetricTrue, symmetricFalse int
	for i := range corpus {
		for j := range corpus {
			forward := corpus[i].typ.Equals(corpus[j].typ)
			backward := corpus[j].typ.Equals(corpus[i].typ)
			if forward != backward {
				t.Errorf("Equals is asymmetric: %s.Equals(%s) = %v but "+
					"%s.Equals(%s) = %v — every call site RFC-233 rewrote with the "+
					"operands swapped is now suspect",
					corpus[i].label, corpus[j].label, forward,
					corpus[j].label, corpus[i].label, backward)
				continue
			}
			if forward {
				symmetricTrue++
			} else {
				symmetricFalse++
			}
		}
	}
	if symmetricTrue == 0 || symmetricFalse == 0 {
		t.Fatalf("degenerate sweep: %d symmetric-true, %d symmetric-false — both "+
			"must be non-zero, or an implementation returning a constant is symmetric "+
			"and this proves nothing", symmetricTrue, symmetricFalse)
	}
}

// TestFlowedTypeHelpersAgreeWithTheSpellingTheyReplace is the differential for
// the two public helpers themselves, over real quantified object values rather
// than over bare types.
//
// The arithmetic is already pinned above; what is NOT pinned there is the
// PLUMBING — that FlowedTypesEqual reaches the right handle, that
// FlowedTypeEquals hands the shared graph to Equals in a state equal to the
// fresh one, and that neither confuses the two operands. Those are the mistakes
// a rewrite across 33 call sites actually makes, and none of them would show up
// in a sweep over types.
func TestFlowedTypeHelpersAgreeWithTheSpellingTheyReplace(t *testing.T) {
	t.Parallel()

	// Only object-or-scalar roots: NewQuantifiedObjectValue refuses NULL and
	// RELATION at the root, so the corpus is filtered and the filter is COUNTED.
	corpus := differentialCorpus()
	type qovEntry struct {
		label string
		typ   Type
		qov   QuantifiedObjectValue
	}
	var entries []qovEntry
	for i, entry := range corpus {
		qov, err := NewQuantifiedObjectValue(
			NamedCorrelationIdentifier("q"+uitoa(uint64(i))), entry.typ)
		if err != nil {
			continue
		}
		entries = append(entries, qovEntry{label: entry.label, typ: entry.typ, qov: qov})
	}
	if rejected := len(corpus) - len(entries); rejected != 5 {
		t.Fatalf("expected exactly 5 corpus entries refused as QOV roots (3 RELATION "+
			"+ 2 NULL primitives), got %d — the corpus changed and this filter has to "+
			"be re-derived rather than relaxed", rejected)
	}

	var agreedTrue, agreedFalse int
	for i := range entries {
		for j := range entries {
			want := entries[i].typ.Equals(entries[j].typ)

			if got := FlowedTypesEqual(entries[i].qov, entries[j].qov); got != want {
				t.Errorf("FlowedTypesEqual(%s, %s) = %v, want %v",
					entries[i].label, entries[j].label, got, want)
			}
			// Both orientations of the mixed helper, because the call sites use
			// both and the symmetry pin above is what licenses that.
			if got := FlowedTypeEquals(entries[i].qov, entries[j].typ); got != want {
				t.Errorf("FlowedTypeEquals(%s, type %s) = %v, want %v",
					entries[i].label, entries[j].label, got, want)
			}
			if got := FlowedTypeEquals(entries[j].qov, entries[i].typ); got != want {
				t.Errorf("FlowedTypeEquals(%s, type %s) = %v, want %v",
					entries[j].label, entries[i].label, got, want)
			}
			if want {
				agreedTrue++
			} else {
				agreedFalse++
			}
		}
	}
	if agreedTrue == 0 || agreedFalse == 0 {
		t.Fatalf("degenerate sweep: %d true, %d false", agreedTrue, agreedFalse)
	}
}

// TestFlowedTypeHelpersAnswerRatherThanCrash pins the one behavioural difference
// the rewrite introduces: an unusable operand.
//
// `left.FlowedType().Equals(right.FlowedType())` panicked when the LEFT value
// could not state a row — FlowedType returns a nil Type and the method call goes
// through a nil interface — and answered false when the RIGHT one could not. The
// helpers answer false for both. That is the fail-safe direction at every call
// site (rebuild, refuse the rewrite, miss the lookup) and it is asserted rather
// than assumed, because "returns false on nil" is exactly the property a later
// nil-guard cleanup would delete as dead.
func TestFlowedTypeHelpersAnswerRatherThanCrash(t *testing.T) {
	t.Parallel()

	row := &RecordType{RecordName: "R", Fields: []Field{
		{Name: "ID", FieldType: &PrimitiveType{TypeCode: TypeCodeLong}, Ordinal: 0},
	}}
	good, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("q"), row)
	if err != nil {
		t.Fatalf("building the usable side failed: %v", err)
	}
	var unusable QuantifiedObjectValue // nil interface

	// The old spelling really did crash on the left operand. Established here
	// rather than asserted, so the justification for the change is measured.
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the replaced expression did NOT panic on a nil left operand, " +
					"so the totality claim in FlowedTypesEqual's doc is stale and the " +
					"doc needs correcting, not this test deleting")
			}
		}()
		_ = unusable.FlowedType().Equals(good.FlowedType())
	}()

	if FlowedTypesEqual(unusable, good) {
		t.Error("FlowedTypesEqual(nil, good) must be false")
	}
	if FlowedTypesEqual(good, unusable) {
		t.Error("FlowedTypesEqual(good, nil) must be false")
	}
	if FlowedTypesEqual(unusable, unusable) {
		t.Error("FlowedTypesEqual(nil, nil) must be false: two values that cannot " +
			"state a row are not thereby the SAME row")
	}
	if FlowedTypeEquals(unusable, row) {
		t.Error("FlowedTypeEquals(nil, row) must be false")
	}
	if FlowedTypeEquals(good, nil) {
		t.Error("FlowedTypeEquals(good, nil) must be false")
	}
	if !FlowedTypeEquals(good, row) {
		t.Error("FlowedTypeEquals(good, its own row) must be true — the guards above " +
			"must not have swallowed the positive case")
	}
}

// TestQuantifiedRowShapesAgreeIsSymmetric pins the property the executor's
// conversion relies on. Six call sites had the declared row on the LEFT and the
// quantifier on the right — `QuantifiedRowShapesAgree(b.outerType,
// exact.FlowedType())` — and route to a helper that takes the quantifier first.
//
// It is a separate claim from TestTypeEqualsIsSymmetricOverTheCorpus, because
// the differing-bits arm is a hand-written walk rather than a dispatch through
// Equals: it switches on the LEFT operand's concrete type and type-asserts the
// right, so an asymmetry there would be invisible to the Equals sweep.
//
// The panic-prone entries are included: QuantifiedRowShapesAgree is total now,
// so it has an answer for every pair, and "symmetric" has to hold on the pairs
// that used to crash too — those are exactly the ones whose two orientations
// took different code paths before.
func TestQuantifiedRowShapesAgreeIsSymmetric(t *testing.T) {
	t.Parallel()
	corpus := append(differentialCorpus(), panicProneTypes()...)

	var agreeing, disagreeing int
	for i := range corpus {
		for j := range corpus {
			forward := QuantifiedRowShapesAgree(corpus[i].typ, corpus[j].typ)
			backward := QuantifiedRowShapesAgree(corpus[j].typ, corpus[i].typ)
			if forward != backward {
				t.Errorf("QuantifiedRowShapesAgree is asymmetric on (%s, %s): %v vs %v "+
					"— the six executor call sites rewritten with the operands swapped "+
					"are now wrong", corpus[i].label, corpus[j].label, forward, backward)
				continue
			}
			if forward {
				agreeing++
			} else {
				disagreeing++
			}
		}
	}
	if agreeing == 0 || disagreeing == 0 {
		t.Fatalf("degenerate sweep: %d agreeing, %d disagreeing", agreeing, disagreeing)
	}
}

// TestAcceptedQuantifiersAlwaysStateARow pins the invariant three call sites now
// rely on to drop a nil check: a value AsQuantifiedObjectValue ACCEPTS always has
// a flowed handle, so FlowedExactType on it is never nil.
//
// It spans two functions and nothing else observes it. Without this, dropping
// `flowed == nil` from AsQuantifiedObjectValue's refusal — which reads like a
// redundant guard, since the constructor already requires a handle — turns
// `FlowedExactType(qov).Code()` at those sites into a nil-interface panic.
func TestAcceptedQuantifiersAlwaysStateARow(t *testing.T) {
	t.Parallel()

	corpus := differentialCorpus()
	accepted := 0
	for i, entry := range corpus {
		qov, err := NewQuantifiedObjectValue(
			NamedCorrelationIdentifier("q"+uitoa(uint64(i))), entry.typ)
		if err != nil {
			continue
		}
		if _, ok := AsQuantifiedObjectValue(qov); !ok {
			t.Fatalf("%s: a constructed QOV was refused by AsQuantifiedObjectValue", entry.label)
		}
		accepted++
		if FlowedExactType(qov) == nil {
			t.Errorf("%s: AsQuantifiedObjectValue accepted it but FlowedExactType is nil — "+
				"the call sites that dropped their nil check now panic", entry.label)
		}
	}
	if accepted == 0 {
		t.Fatal("no corpus entry produced an accepted QOV, so this pinned nothing")
	}

	// The refusal side, because the invariant is a BICONDITIONAL in use: the
	// sites drop the nil check only because a handle-less value never gets past
	// AsQuantifiedObjectValue in the first place.
	handleless := &quantifiedObjectValue{correlation: NamedCorrelationIdentifier("q")}
	if _, ok := AsQuantifiedObjectValue(handleless); ok {
		t.Error("AsQuantifiedObjectValue accepted a value with no flowed handle; every " +
			"site that dropped its nil check is now a panic waiting for that value")
	}
	if FlowedExactType(handleless) != nil {
		t.Error("FlowedExactType must answer nil for a value with no flowed handle")
	}
}

// TestHandleAccessorsMatchTheThawedGraph pins the equality that licenses roughly
// nine substitutions of `handle.Type().Code()` by `handle.Code()` and
// `.Type().IsNullable()` by `.IsNullable()`.
//
// The accessors read the snapshot's own fields; the thawed graph is built from
// those same fields by a SEPARATE switch in thaw(). Two derivations of one fact,
// which is exactly the shape that drifts — thaw's RELATION arm, for instance,
// drops nullability entirely because RelationType has none to carry, and the
// handle's nullable field does not.
func TestHandleAccessorsMatchTheThawedGraph(t *testing.T) {
	t.Parallel()

	corpus := differentialCorpus()
	handles := snapshotCorpus(t, corpus)
	checked := 0
	for i, entry := range corpus {
		thawed := handles[i].Type()
		if thawed == nil {
			t.Fatalf("%s: Type() is nil for a snapshotted handle", entry.label)
		}
		if got, want := handles[i].Code(), thawed.Code(); got != want {
			t.Errorf("%s: handle.Code() = %v but Type().Code() = %v", entry.label, got, want)
		}
		if got, want := handles[i].IsNullable(), thawed.IsNullable(); got != want {
			t.Errorf("%s: handle.IsNullable() = %v but Type().IsNullable() = %v",
				entry.label, got, want)
		}
		checked++
	}
	if checked < corpusFloorForAccessors {
		t.Fatalf("checked %d handles, floor is %d", checked, corpusFloorForAccessors)
	}

	// A nil handle must answer rather than panic, because the substitution sites
	// call these on a handle whose nil-ness they have just tested — or, at the
	// three sites that dropped that test, on one the invariant above says cannot
	// be nil. Both readings need a total accessor.
	var absent *exactType
	if got := absent.Code(); got != TypeCodeUnknown {
		t.Errorf("nil handle Code() = %v, want TypeCodeUnknown — a handle that states "+
			"no type has no other honest answer", got)
	}
	if absent.IsNullable() {
		t.Error("nil handle IsNullable() must be false: absence of a type is not a nullable type")
	}
}

// corpusFloorForAccessors keeps the accessor sweep from silently shrinking. It
// is the corpus size as built, so adding a shape is free and losing one fails.
const corpusFloorForAccessors = 58

// TestTheOtherFlowedHelpersAgreeWithTheirSlowPath covers the four helpers the
// differential sweep above did not: FlowedRowShapesAgree, FlowedRowShapeEquals,
// ExactTypesEqual and QuantifierFlowsAScalarRow.
//
// Two pinned helpers out of six is the coverage the sweep actually had, in a file
// whose own doc says plumbing mistakes are "the mistakes a rewrite across 33 call
// sites actually makes". Each of these is a distinct piece of plumbing — a
// different handle reached, a different operand order, a different slow path —
// and none of them is exercised by testing the other two.
func TestTheOtherFlowedHelpersAgreeWithTheirSlowPath(t *testing.T) {
	t.Parallel()

	corpus := differentialCorpus()
	handles := snapshotCorpus(t, corpus)

	type entry struct {
		label  string
		typ    Type
		handle *exactType
		qov    QuantifiedObjectValue
	}
	var entries []entry
	for i, c := range corpus {
		qov, err := NewQuantifiedObjectValue(
			NamedCorrelationIdentifier("q"+uitoa(uint64(i))), c.typ)
		if err != nil {
			continue
		}
		entries = append(entries, entry{c.label, c.typ, handles[i], qov})
	}
	if len(entries) == 0 {
		t.Fatal("no corpus entry produced a QOV; this test pinned nothing")
	}

	var (
		shapesTrue, shapesFalse int
		exactTrue, exactFalse   int
	)
	for i := range entries {
		for j := range entries {
			wantShape := QuantifiedRowShapesAgree(entries[i].typ, entries[j].typ)
			if got := FlowedRowShapesAgree(entries[i].qov, entries[j].qov); got != wantShape {
				t.Errorf("FlowedRowShapesAgree(%s, %s) = %v, want %v",
					entries[i].label, entries[j].label, got, wantShape)
			}
			// Both orientations: six executor call sites had the declared row on
			// the left and route to this helper quantifier-first.
			if got := FlowedRowShapeEquals(entries[i].qov, entries[j].typ); got != wantShape {
				t.Errorf("FlowedRowShapeEquals(%s, type %s) = %v, want %v",
					entries[i].label, entries[j].label, got, wantShape)
			}
			if got := FlowedRowShapeEquals(entries[j].qov, entries[i].typ); got != wantShape {
				t.Errorf("FlowedRowShapeEquals(%s, type %s) = %v, want %v",
					entries[j].label, entries[i].label, got, wantShape)
			}
			if wantShape {
				shapesTrue++
			} else {
				shapesFalse++
			}

			wantExact := entries[i].typ.Equals(entries[j].typ)
			if got := ExactTypesEqual(entries[i].handle, entries[j].handle); got != wantExact {
				t.Errorf("ExactTypesEqual(%s, %s) = %v, want %v",
					entries[i].label, entries[j].label, got, wantExact)
			}
			if wantExact {
				exactTrue++
			} else {
				exactFalse++
			}
		}
	}
	if shapesTrue == 0 || shapesFalse == 0 || exactTrue == 0 || exactFalse == 0 {
		t.Fatalf("degenerate sweep: shapes %d/%d, exact %d/%d",
			shapesTrue, shapesFalse, exactTrue, exactFalse)
	}

	// QuantifierFlowsAScalarRow: a QOV whose row is not a RECORD. Both classes
	// must be populated, or the predicate could be a constant and still pass.
	scalar, record := 0, 0
	for _, e := range entries {
		got := QuantifierFlowsAScalarRow(e.qov)
		want := e.typ.Code() != TypeCodeRecord
		if got != want {
			t.Errorf("QuantifierFlowsAScalarRow(%s) = %v, want %v", e.label, got, want)
		}
		if want {
			scalar++
		} else {
			record++
		}
	}
	if scalar == 0 || record == 0 {
		t.Fatalf("QuantifierFlowsAScalarRow saw %d scalar rows and %d record rows; "+
			"both classes must be present", scalar, record)
	}
	// A non-quantifier answers false rather than panicking — the two call sites
	// hand it an arbitrary projected Value.
	if QuantifierFlowsAScalarRow(nil) {
		t.Error("QuantifierFlowsAScalarRow(nil) must be false")
	}
	if QuantifierFlowsAScalarRow(NewNullValue(&PrimitiveType{TypeCode: TypeCodeLong})) {
		t.Error("QuantifierFlowsAScalarRow of a non-quantifier must be false")
	}
	if ExactTypesEqual(nil, nil) {
		t.Error("ExactTypesEqual(nil, nil) must be false: two absent handles do not " +
			"denote one type. This differs from the retired expressions.typeEquals " +
			"shim, which answered true for two nil Types; no constructor can store a " +
			"nil handle, so the change is unreachable and is recorded, not hidden")
	}
}

// TestFlowedHelpersRefuseAValueThatCannotStateARow pins the arm the totality
// test above missed, and it is the arm that answered TRUE.
//
// The wrapper-pointer guard in FlowedTypesEqual and FlowedRowShapesAgree does not
// catch a live quantifier whose HANDLE is absent, and forwarding two nil handles
// to exactTypesEqual answers `left == right` — true. That is correct for
// exactTypesEqual, which is asked whether two handles denote one type, and wrong
// here, where the question is whether two VALUES flow the same row.
//
// It is the one direction the API promises never to take: a value that cannot
// answer must REFUSE, never agree.
func TestFlowedHelpersRefuseAValueThatCannotStateARow(t *testing.T) {
	t.Parallel()

	row := &RecordType{RecordName: "R", Fields: []Field{
		{Name: "ID", FieldType: &PrimitiveType{TypeCode: TypeCodeLong}, Ordinal: 0},
	}}
	good, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("q"), row)
	if err != nil {
		t.Fatalf("building the usable side failed: %v", err)
	}
	// A live wrapper with no handle. It cannot come from the constructor, which
	// is the reason the guard was missing and the reason this has to be built by
	// hand: the type is package-private, so this test is the only place the
	// shape can exist at all.
	var handleless QuantifiedObjectValue = &quantifiedObjectValue{
		correlation: NamedCorrelationIdentifier("h"),
	}

	cases := []struct {
		name string
		got  bool
	}{
		{"FlowedTypesEqual(handleless, handleless)", FlowedTypesEqual(handleless, handleless)},
		{"FlowedTypesEqual(handleless, good)", FlowedTypesEqual(handleless, good)},
		{"FlowedTypesEqual(good, handleless)", FlowedTypesEqual(good, handleless)},
		{"FlowedRowShapesAgree(handleless, handleless)", FlowedRowShapesAgree(handleless, handleless)},
		{"FlowedRowShapesAgree(handleless, good)", FlowedRowShapesAgree(handleless, good)},
		{"FlowedRowShapesAgree(good, handleless)", FlowedRowShapesAgree(good, handleless)},
		{"FlowedTypeEquals(handleless, row)", FlowedTypeEquals(handleless, row)},
		{"FlowedRowShapeEquals(handleless, row)", FlowedRowShapeEquals(handleless, row)},
	}
	for _, c := range cases {
		if c.got {
			t.Errorf("%s = true; a value that cannot state a row must REFUSE, never agree", c.name)
		}
	}

	// The positive control: the guards must not have swallowed the real answers.
	if !FlowedTypesEqual(good, good) || !FlowedRowShapesAgree(good, good) ||
		!FlowedTypeEquals(good, row) || !FlowedRowShapeEquals(good, row) {
		t.Error("a usable quantifier no longer agrees with itself; the refusal guards " +
			"are over-broad")
	}
}
