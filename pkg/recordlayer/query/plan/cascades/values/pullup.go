package values

// PullUpValue rewrites v so that it references the output of
// resultValue, viewed through alias.
//
// Ports the essential logic of Java's Value.pullUp rule set
// (MatchValueRule, MatchFieldValueAgainstQuantifiedObjectValueRule,
// MatchOrCompensateFieldValueRule, CompensateRecordConstructorRule)
// as a direct recursive algorithm rather than a rule-engine dispatch.
//
// Returns nil if v cannot be expressed in terms of resultValue.
//
// Examples (where resultValue = RecordConstructor(a=FV("x"), b=FV("y"))):
//
//   - v = FV("x") → FV(QOV(alias), "a") // input field "x" becomes output field "a"
//   - v = FV("y") → FV(QOV(alias), "b") // input field "y" becomes output field "b"
//   - v = resultValue → QOV(alias)       // the whole result maps to the output alias
//
// For non-RecordConstructor result values (e.g. a QuantifiedObjectValue
// passthrough), v is matched directly:
//
//   - v = resultValue → QOV(alias)
//   - v = FV("x"), resultValue = QOV(q) → FV(QOV(alias), "x")
func PullUpValue(v Value, resultValue Value, alias CorrelationIdentifier) (Value, error) {
	if v == nil || resultValue == nil {
		return nil, nil
	}

	// Case 1: v semantically equals the entire result value.
	if semanticEqual(v, resultValue) {
		return newPullUpOutputQOVForSource(alias, resultValue)
	}

	// Case 2: resultValue is a RecordConstructorValue — check whether
	// v matches one of its fields' values.
	if rc, ok := resultValue.(*RecordConstructorValue); ok {
		// The logical result program can retain a nominal source declaration
		// (T RECORD<...>) while a selected scan publishes the same exact row
		// anonymously.  The runtime alias and complete path still identify the
		// same source, but structural equality alone cannot see across that one
		// permitted declaration seam.  Reuse the result program's own QOV as
		// authority before matching its slots; conflicting/narrow/foreign roots
		// are deliberately left unchanged by the normalizer.
		normalized := v
		if !alias.isCurrent() {
			var err error
			normalized, err = TranslateLogicalSourceNameNormalizationInValue(
				v, alias, rc)
			if err != nil {
				return nil, err
			}
		}
		return pullUpThroughRecordConstructor(normalized, rc, alias)
	}

	// Case 3: resultValue is a QuantifiedObjectValue or ObjectValue —
	// a passthrough. If v is a FieldValue, field access passes
	// through with its base rebound to the output alias.
	if _, ok := resultValue.(*quantifiedObjectValue); ok {
		return pullUpThroughPassthrough(v, resultValue, alias)
	}
	if _, ok := resultValue.(*ObjectValue); ok {
		return pullUpThroughPassthrough(v, resultValue, alias)
	}

	return nil, nil
}

// pullUpThroughRecordConstructor handles the case where the result
// value is a record constructor with named fields.
//
// For each field in the constructor, check if v equals that field's
// value. If so, v can be accessed as the output field name.
//
// The emitted reference is re-framed to the RC's OUTPUT column i, so when the
// ordinal matters it is BAKED — a lazy name node over a duplicate-named RC
// output would later resolve to the FIRST same-named column regardless of
// which column matched (the duplicate-name conflation hazard). Baking is
// gated: a resolved input (whose positional identity must survive pull-up) or
// a duplicate-named RC resolves by ordinal. A non-field input over a clean-name
// RC resolves by its unique semantic name. Both paths publish only admitted,
// exact FieldValues.
func pullUpThroughRecordConstructor(v Value, rc *RecordConstructorValue, alias CorrelationIdentifier) (Value, error) {
	inBaked, inPinned := false, false
	if fv, ok := v.(*fieldValue); ok && fv.Resolved != nil {
		inBaked = true
		inPinned = fv.Resolved.FrontierPinned
	}
	for i, field := range rc.Fields {
		_, fieldOwnedByAlias := GetCorrelatedToOfValue(field.Value)[alias]
		if semanticEqual(v, field.Value) ||
			(fieldOwnedByAlias && CanBridgeOrderingValueRoots(v, field.Value)) {
			child, err := newPullUpOutputQOV(alias, rc.Type())
			if err != nil {
				return nil, err
			}
			if !inBaked && !rcHasDuplicateNames(rc) {
				// Clean names still resolve immediately against the exact output
				// row. RFC-232 does not admit a lazy name node, even when the
				// name is unique at this constructor boundary.
				var request FieldRequest
				if field.Name == "" {
					request, err = FieldByOrdinal(i)
				} else {
					request, err = FieldByName(field.Name)
				}
				if err != nil {
					return nil, err
				}
				return ResolveFieldAccess(child, []FieldRequest{request})
			}
			resolved, err := resolveFieldOrdinalInDomain(child, i, OrdinalDomainOfType(rc.Type()))
			if err != nil {
				return nil, err
			}
			if admitted, ok := resolved.(*fieldValue); ok {
				admitted.Resolved.FrontierPinned = inPinned
			}
			return resolved, nil
		}
	}
	return nil, nil
}

// rcHasDuplicateNames reports whether two RC columns share a name — the
// duplicate-name shape, constructible only by ordinal seeds.
func rcHasDuplicateNames(rc *RecordConstructorValue) bool {
	seen := make(map[string]struct{}, len(rc.Fields))
	for _, f := range rc.Fields {
		if _, dup := seen[f.Name]; dup {
			return true
		}
		seen[f.Name] = struct{}{}
	}
	return false
}

// pullUpThroughPassthrough handles pull-up through an identity-like
// result value (QOV, ObjectValue). Field accesses keep their resolved path but
// are re-anchored on the candidate alias so same-named fields from different
// sources remain correlation-distinct.
func pullUpThroughPassthrough(
	v Value,
	resultValue Value,
	alias CorrelationIdentifier,
) (Value, error) {
	fv, ok := v.(*fieldValue)
	if !ok || fv == nil || !nonNilPassthroughValue(resultValue) {
		return nil, nil
	}
	// A correlated field can pass through only the value it is rooted on.
	// Re-anchoring a field over a different QOV/ObjectValue silently changes
	// which source owns the column. This check also declines the legacy chained
	// FieldValue representation: copying only its outer node would drop the
	// inner path. Canonical fused paths are supported because their Child is
	// the passthrough root and their complete path lives in Resolved.
	if fv.Child != nil &&
		!ValuesStructurallyEqual(fv.Child, resultValue) {
		return nil, nil
	}
	child, err := newPullUpOutputQOVForSource(alias, resultValue)
	if err != nil {
		return nil, err
	}

	if isAdmittedFieldValue(fv) {
		return RebuildFieldValue(fv, child)
	}
	// Legacy-private package fixture path. It is not externally constructible
	// and cannot pass AsFieldValue admission.
	NoteFieldValueMint(fv.Field, fv.Resolved != nil)
	return &fieldValue{Field: fv.Field, Typ: fv.Typ, Child: child, Resolved: fv.Resolved}, nil
}

// newPullUpOutputQOV mints the output root introduced by pull-up. Most callers
// provide an ordinary candidate alias. MaxMatchMap deliberately pulls values
// into Quantifier.current(), however: that reserved correlation denotes the
// newly produced row and may only be minted inside an owner-scoped builder.
// PullUpValue is that builder, so it uses the same exact private current handle
// as ordinal-layout owners instead of routing current through the public QOV
// constructor (which correctly rejects it as a foreign source correlation).
func newPullUpOutputQOV(alias CorrelationIdentifier, typ Type) (QuantifiedObjectValue, error) {
	if alias.isCurrent() {
		return newCurrentQOVForLayout(typ)
	}
	return NewQuantifiedObjectValue(alias, typ)
}

// newPullUpOutputQOVForSource is newPullUpOutputQOV given the VALUE whose type
// the output carries, so a source that already holds an exact handle hands it
// over instead of being thawed and re-snapshotted.
//
// The round trip was pure waste and the single largest planner allocator on this
// branch: Type() builds a fresh ordinary graph out of the handle — deliberately,
// so no caller can mutate the identity a QOV depends on — and SnapshotExactType
// then walks that graph back to the interned node it started from. Interning is
// what makes the shortcut exactly equivalent rather than merely equivalent-looking:
// the long way round is now guaranteed to return the same object.
//
// The dropped half is the source layout, which the long way round also dropped:
// thaw does not restore .Legs, so snapshotQOVRecordLayout over a thawed graph
// always answered nil.
func newPullUpOutputQOVForSource(alias CorrelationIdentifier, source Value) (QuantifiedObjectValue, error) {
	exact, ok := exactTypeOfValue(source)
	if !ok {
		return newPullUpOutputQOV(alias, source.Type())
	}
	if exact.code == TypeCodeNull || exact.code == TypeCodeRelation {
		return nil, resolutionError(TypeMalformedCode, "qov.flowed", "QOV root must be an object or scalar exact type")
	}
	if alias.isCurrent() {
		return &quantifiedObjectValue{correlation: CurrentCorrelation(), flowed: exact}, nil
	}
	if alias.IsZero() {
		return nil, resolutionError(CorrelationZero, "qov.correlation", "correlation is zero")
	}
	return &quantifiedObjectValue{correlation: alias, flowed: exact}, nil
}

// exactTypeOfValue returns the exact handle a values-owned value already carries,
// without thawing it. Only the exact QOV carries one; every other value derives
// its type and must be asked for it.
func exactTypeOfValue(value Value) (*exactType, bool) {
	qov, isQOV := value.(*quantifiedObjectValue)
	if !isQOV || qov == nil || qov.flowed == nil {
		return nil, false
	}
	return qov.flowed, true
}

// PushDownValue rewrites v (which references the output of resultValue)
// to be expressed in terms of the inputs of resultValue. This is the
// inverse of PullUpValue.
//
// Examples (where resultValue = RecordConstructor(a=FV("x"), b=FV("y"))):
//
//   - v = FV("a") → FV("x")       // output field "a" maps to input "x"
//   - v = FV("b") → FV("y")       // output field "b" maps to input "y"
//   - v = QOV(alias) → resultValue // the whole output maps to the result
//
// Returns nil if the push-down fails.
func PushDownValue(v Value, resultValue Value, upperAlias CorrelationIdentifier) Value {
	if v == nil || resultValue == nil {
		return nil
	}

	// Case 1: v is a QuantifiedObjectValue referencing the upper alias
	// → replace with the entire resultValue.
	if qov, ok := v.(*quantifiedObjectValue); ok {
		if qov.correlation == upperAlias {
			return resultValue
		}
	}

	// Case 2: resultValue is a RecordConstructorValue and v is a
	// FieldValue → resolve the field to its input expression.
	if rc, ok := resultValue.(*RecordConstructorValue); ok {
		// This is the inverse of PullUpValue's nominal-source bridge above.
		// An ORDER BY key can be rooted at a selected scan's anonymous row even
		// though the retained result program names the same source row.  Match
		// against the exact QOV already present in the result program; never mint
		// or infer a target from the requested field alone.
		if !upperAlias.isCurrent() {
			normalized, err := TranslateLogicalSourceNameNormalizationInValue(
				v, upperAlias, rc)
			if err != nil {
				return nil
			}
			v = normalized
		}
		if fv, ok := v.(*fieldValue); ok {
			// A select/join sort key can already be expressed in one of the
			// constructor's input scopes (for example C.NAME#1 rooted at
			// QOV(C)). Match that exact value before interpreting its baked
			// ordinal as an OUTPUT ordinal. The latter is a different
			// coordinate system: #1 on C is leg-local, while rc.Fields[1] is
			// constructor-global. Treating them as interchangeable maps an
			// I.TITLE#2 request onto the third column of C in a joined result.
			for _, field := range rc.Fields {
				if semanticEqual(v, field.Value) {
					return field.Value
				}
			}

			// Interpreting the first ordinal as an OUTPUT slot is legal only
			// when the field is rooted in this exact constructor row.  The
			// ordinal by itself is not an identity: another projection can have
			// slot 0 too, and a same-width foreign row can put an unrelated field
			// there.  Validate both the frozen root layout and the upper alias
			// before crossing coordinate systems. Nested requests such as
			// UPDATE's OLD.ID follow the same rule; only the suffix changes row
			// domains after the root slot selects OLD.
			if isAdmittedFieldValue(fv) {
				constructorRoot, err := SnapshotExactType(rc.Type())
				if err != nil || !exactTypesEqual(fv.rootType, constructorRoot.(*exactType)) {
					return nil
				}
				root, rootOK := fv.Child.(*quantifiedObjectValue)
				if !rootOK || root == nil || root.correlation != upperAlias {
					return nil
				}
			}

			// A BAKED node resolves by ORDINAL — same rationale
			// as composeFieldOverConstructor: a name lookup would pick the FIRST
			// of two duplicate same-named output columns regardless of which the
			// ordinal denotes (the conflation hazard). Out-of-range = malformed;
			// decline rather than guess. For a nested path the first ordinal is in
			// the constructor's output domain and the suffix is in the selected
			// child's exact domain. Re-resolve that suffix atomically; copying the
			// original path would retain the wrong root type/domain.
			if fv.Resolved != nil {
				ordinals := fv.Resolved.Ordinals()
				if len(ordinals) == 0 {
					return nil
				}
				rootOrdinal := ordinals[0]
				if rootOrdinal < 0 || rootOrdinal >= len(rc.Fields) {
					return nil
				}
				selected := rc.Fields[rootOrdinal].Value
				if len(ordinals) == 1 {
					return selected
				}
				resolved, err := ResolveFieldOrdinals(selected, ordinals[1:])
				if err != nil {
					return nil
				}
				return resolved
			}
			return nil
		}
	}

	// Case 3: resultValue is a passthrough (QOV/ObjectValue) — a legacy flat
	// field stays flat, while an upper-anchored field is restored to this
	// passthrough source.
	if _, ok := resultValue.(*quantifiedObjectValue); ok {
		return pushDownThroughPassthrough(v, resultValue, upperAlias)
	}
	if _, ok := resultValue.(*ObjectValue); ok {
		return pushDownThroughPassthrough(v, resultValue, upperAlias)
	}

	return nil
}

// pushDownThroughPassthrough handles push-down through identity-like result
// values. Legacy flat fields stay flat. A field anchored on the upper alias is
// restored to the passthrough source; any other base declines rather than
// changing source identity or dropping a chained nested path.
func pushDownThroughPassthrough(
	v Value,
	resultValue Value,
	upperAlias CorrelationIdentifier,
) Value {
	fv, ok := v.(*fieldValue)
	if !ok || fv == nil || !nonNilPassthroughValue(resultValue) {
		return nil
	}
	if fv.Child == nil {
		NoteFieldValueMint(fv.Field, fv.Resolved != nil)
		return &fieldValue{Field: fv.Field, Typ: fv.Typ, Resolved: fv.Resolved}
	}
	qov, ok := fv.Child.(*quantifiedObjectValue)
	if !ok || qov == nil || qov.correlation != upperAlias {
		return nil
	}
	if isAdmittedFieldValue(fv) {
		rebuilt, err := RebuildFieldValue(fv, resultValue)
		if err != nil {
			return nil
		}
		return rebuilt
	}
	NoteFieldValueMint(fv.Field, fv.Resolved != nil)
	return &fieldValue{Field: fv.Field, Typ: fv.Typ, Child: resultValue, Resolved: fv.Resolved}
}

func nonNilPassthroughValue(value Value) bool {
	switch value := value.(type) {
	case *quantifiedObjectValue:
		return value != nil
	case *ObjectValue:
		return value != nil
	default:
		return false
	}
}

// PullUpValues translates a list of values through a result value,
// returning a map from original value to pulled-up value. Values that
// cannot be pulled up are omitted from the map.
//
// This is the batch form used by Ordering.PullUpThroughValue.
func PullUpValues(toBePulledUp []Value, resultValue Value, alias CorrelationIdentifier) (map[Value]Value, error) {
	result := make(map[Value]Value)
	for _, v := range toBePulledUp {
		pulled, err := PullUpValue(v, resultValue, alias)
		if err != nil {
			return nil, err
		}
		if pulled != nil {
			result[v] = pulled
		}
	}
	return result, nil
}

// PushDownValues translates a list of values through a result value,
// returning the pushed-down values in order. Values that cannot be
// pushed down are returned as nil entries.
func PushDownValues(toBePushedDown []Value, resultValue Value, upperAlias CorrelationIdentifier) []Value {
	result := make([]Value, len(toBePushedDown))
	for i, v := range toBePushedDown {
		result[i] = PushDownValue(v, resultValue, upperAlias)
	}
	return result
}

// semanticEqual checks if two values are structurally equivalent.
// Uses ValuesStructurallyEqual which recursively compares value trees
// by concrete type and field values, rather than the fragile
// ExplainValue string comparison which could theoretically produce
// false positives on structurally different values that happen to
// render identically.
func semanticEqual(a, b Value) bool {
	return ValuesStructurallyEqual(a, b)
}
