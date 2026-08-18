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
		for i := range fields {
			fields[i].Ordinal = i
		}
		return &RecordType{RecordName: name, Nullable: nullable, Fields: fields}
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
// WithNullability panics on a RelationType, so relation entries are excluded
// from the reference side — and the exclusion is COUNTED, because silently
// skipping entries is how a sweep ends up reporting green over three of them.
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
		t.Fatalf("expected exactly 3 relation entries excluded from the QRSA "+
			"reference sweep (WithNullability panics on RELATION), got %d — the "+
			"corpus changed and the exclusion has to be re-justified", excluded)
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
