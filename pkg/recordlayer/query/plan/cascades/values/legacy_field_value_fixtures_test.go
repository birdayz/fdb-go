package values

import "fmt"

// OrdinalBakeError is the loud construction-time error newFieldValueOfOrdinal
// returns when the requested ordinal cannot be resolved against the child's
// flowed type — the Go analog of Java's resolveFieldPath raising
// SemanticException(FIELD_ACCESS_INPUT_NON_RECORD_TYPE) for a non-record child
// and IndexOutOfBoundsException for an out-of-range ordinal
// (FieldValue.java:273-296). Never a silent fallback: a bake failure is a
// planner bug, not a NULL.
type OrdinalBakeError struct {
	Ordinal   int
	ChildType Type   // the child's flowed type (nil for a nil child)
	Reason    string // which precondition failed: nil child / non-record child / out of range
}

func (e *OrdinalBakeError) Error() string {
	return fmt.Sprintf("ordinal bake: cannot resolve ordinal %d: %s (child type %v)", e.Ordinal, e.Reason, e.ChildType)
}

// newFieldValueOfOrdinal constructs a BAKED FieldValue accessing the child's
// record field by ORDINAL position — Java's
// `FieldValue.ofOrdinalNumber(childValue, ordinalNumber)` (FieldValue.java:335):
// the position is resolved ONCE, here, and carried on the node (Resolved);
// resolveOrdinal returns it without re-deriving from the child type. The
// DISPLAY name (Field) and Typ are read from the child's RecordType at
// `ordinal` — the name serves diagnostics/Explain; the ordinal is
// authoritative (it survives even when a runtime row's type names disagree
// with the display name). The bake is MACHINERY-OWNED (FrontierPinned): this
// is the join / gather seed constructor.
//
// Errors loudly (Java raises; no silent fallback) when the child does not
// flow a *RecordType or the ordinal is out of range.
func newFieldValueOfOrdinal(child Value, ordinal int) (*fieldValue, error) {
	if child == nil {
		return nil, &OrdinalBakeError{Ordinal: ordinal, Reason: "nil child value"}
	}
	rt, ok := child.Type().(*RecordType)
	if !ok {
		return nil, &OrdinalBakeError{Ordinal: ordinal, ChildType: child.Type(), Reason: "child does not flow a record type"}
	}
	if ordinal < 0 || ordinal >= len(rt.Fields) {
		return nil, &OrdinalBakeError{Ordinal: ordinal, ChildType: rt, Reason: fmt.Sprintf("ordinal out of range for a %d-field record type", len(rt.Fields))}
	}
	fld := rt.Fields[ordinal]
	typ := fld.FieldType
	// Java FieldValue.computeResultType: the accessed field's type is
	// overridden NULLABLE when the child's record type is nullable — the
	// LEFT-outer null-supplying leg's record-level wrap
	// makes every column read through it nullable, because the padded row
	// serves NULL in every slot (how Java reports LEFT JOIN metadata
	// nullable without a per-column seed wrap). Keyed on the STORED Typ for
	// a QOV child: QOV.Type() blanket-wraps nullable (the pre-existing
	// pass-through rule), so the record-level marker is only observable on
	// q.Typ — the same authority every seed consumer reads.
	childNullable := rt.Nullable
	if qov, isQOV := child.(*quantifiedObjectValue); isQOV {
		if srt, isRT := qov.FlowedType().(*RecordType); isRT {
			childNullable = srt.Nullable
		}
	}
	if childNullable && typ != nil && !typ.IsNullable() {
		typ = WithNullability(typ, true)
	}
	// FrontierPinned: this constructor is the join seed's — the executor
	// supplies positional rows for every context these nodes evaluate in, so
	// these nodes only ever resolve by ordinal.
	// The domain is stamped, not passed: this constructor RESOLVES the ordinal
	// against rt itself, so rt IS the layout the ordinal indexes — the one
	// place a derived domain is a proof rather than a claim. A caller that
	// resolved elsewhere must state its own domain explicitly.
	NoteFieldValueMint(fld.Name, true)
	return &fieldValue{
		Field:    fld.Name,
		Typ:      typ,
		Child:    child,
		Resolved: newFieldPathOfSingleInDomain(fld.Name, ordinal, true, OrdinalDomainOfType(rt)),
	}, nil
}

func newFieldPathOfSingle(field string, ordinal int, frontierPinned bool) *fieldPath {
	return newFieldPathOfSingleInDomain(field, ordinal, frontierPinned, OrdinalDomain{})
}

func newFieldPathOfSingleInDomain(field string, ordinal int, frontierPinned bool, domain OrdinalDomain) *fieldPath {
	return &fieldPath{
		Accessors:      []resolvedAccessor{{Field: field, Ordinal: ordinal}},
		FrontierPinned: frontierPinned,
		Domain:         domain,
	}
}

// These constructors deliberately mint the legacy unresolved and manually
// baked FieldValue shapes exercised by same-package compatibility tests. They
// are test fixtures, not planner construction APIs: production callers must
// use the checked constructors and exact ordinal resolvers.
func newFieldValue(child Value, field string, typ Type) *fieldValue {
	NoteFieldValueMint(field, false)
	return &fieldValue{Field: field, Typ: typ, Child: child}
}

func newFlatFieldValue(field string, typ Type) *fieldValue {
	NoteFieldValueMint(field, false)
	return &fieldValue{Field: field, Typ: typ}
}

func newFieldValueWithResolvedOrdinal(field string, ordinal int, typ Type) *fieldValue {
	return newFieldValueWithResolvedOrdinalInDomain(field, ordinal, typ, OrdinalDomain{})
}

func newFieldValueWithResolvedOrdinalInDomain(field string, ordinal int, typ Type, domain OrdinalDomain) *fieldValue {
	NoteFieldValueMint(field, true)
	return &fieldValue{
		Field:    field,
		Typ:      typ,
		Resolved: newFieldPathOfSingleInDomain(field, ordinal, false, domain),
	}
}

func newCorrelatedFieldValueWithResolvedOrdinal(child Value, field string, ordinal int, typ Type) *fieldValue {
	return newCorrelatedFieldValueWithResolvedOrdinalInDomain(child, field, ordinal, typ, OrdinalDomain{})
}

func newCorrelatedFieldValueWithResolvedOrdinalInDomain(child Value, field string, ordinal int, typ Type, domain OrdinalDomain) *fieldValue {
	NoteFieldValueMint(field, true)
	return &fieldValue{
		Field:    field,
		Typ:      typ,
		Child:    child,
		Resolved: newFieldPathOfSingleInDomain(field, ordinal, false, domain),
	}
}
