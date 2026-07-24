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
func PullUpValue(v Value, resultValue Value, alias CorrelationIdentifier) Value {
	if v == nil || resultValue == nil {
		return nil
	}

	// Case 1: v semantically equals the entire result value.
	if semanticEqual(v, resultValue) {
		return &QuantifiedObjectValue{Correlation: alias, Typ: resultValue.Type()}
	}

	// Case 2: resultValue is a RecordConstructorValue — check whether
	// v matches one of its fields' values.
	if rc, ok := resultValue.(*RecordConstructorValue); ok {
		return pullUpThroughRecordConstructor(v, rc, alias)
	}

	// Case 3: resultValue is a QuantifiedObjectValue or ObjectValue —
	// a passthrough. If v is a FieldValue, field access passes
	// through with its base rebound to the output alias.
	if _, ok := resultValue.(*QuantifiedObjectValue); ok {
		return pullUpThroughPassthrough(v, resultValue, alias)
	}
	if _, ok := resultValue.(*ObjectValue); ok {
		return pullUpThroughPassthrough(v, resultValue, alias)
	}

	return nil
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
// gated: only a baked input (bakedness must survive pull-up) or a dup-named
// RC (only ordinal seeds build them — projection RCs suffix duplicates)
// bakes; a lazy input over a clean-named RC emits a lazy node.
func pullUpThroughRecordConstructor(v Value, rc *RecordConstructorValue, alias CorrelationIdentifier) Value {
	inBaked, inPinned := false, false
	if fv, ok := v.(*FieldValue); ok && fv.Resolved != nil {
		inBaked = true
		inPinned = fv.Resolved.FrontierPinned
	}
	for i, field := range rc.Fields {
		_, fieldOwnedByAlias := GetCorrelatedToOfValue(field.Value)[alias]
		if semanticEqual(v, field.Value) ||
			(fieldOwnedByAlias && CanBridgeOrderingValueRoots(v, field.Value)) {
			out := &FieldValue{
				Field: field.Name,
				Typ:   field.Value.Type(),
				Child: &QuantifiedObjectValue{
					Correlation: alias,
					Typ:         rc.Type(),
				},
			}
			if inBaked || rcHasDuplicateNames(rc) {
				// The frontier-contract bit INHERITS from the input: a pinned
				// seed ref pulled through the join's RC still reads a
				// positional row (the gated join builds them), so the loud
				// guard must survive the pull-up. A dup-name disambiguation
				// bake over a LAZY input establishes no frontier contract —
				// unpinned.
				out.Resolved = NewFieldPathOfSingle(field.Name, i, inPinned)
			}
			return out
		}
	}
	return nil
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
) Value {
	fv, ok := v.(*FieldValue)
	if !ok || fv == nil || !nonNilPassthroughValue(resultValue) {
		return nil
	}
	// A correlated field can pass through only the value it is rooted on.
	// Re-anchoring a field over a different QOV/ObjectValue silently changes
	// which source owns the column. This check also declines the legacy chained
	// FieldValue representation: copying only its outer node would drop the
	// inner path. Canonical fused paths are supported because their Child is
	// the passthrough root and their complete path lives in Resolved.
	if fv.Child != nil &&
		!ValuesStructurallyEqual(fv.Child, resultValue) {
		return nil
	}

	// Preserve the baked-ordinal marker through the copy: the passthrough is an
	// identity result value (same record flows), so the baked position stays
	// valid; dropping it would silently degrade a BAKED node to lazy (the
	// conflation hazard).
	return &FieldValue{
		Field:    fv.Field,
		Typ:      fv.Typ,
		Child:    &QuantifiedObjectValue{Correlation: alias, Typ: resultValue.Type()},
		Resolved: fv.Resolved,
	}
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
	if qov, ok := v.(*QuantifiedObjectValue); ok {
		if qov.Correlation == upperAlias {
			return resultValue
		}
	}

	// Case 2: resultValue is a RecordConstructorValue and v is a
	// FieldValue → resolve the field to its input expression.
	if rc, ok := resultValue.(*RecordConstructorValue); ok {
		if fv, ok := v.(*FieldValue); ok {
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

			// A BAKED node resolves by ORDINAL — same rationale
			// as composeFieldOverConstructor: a name lookup would pick the FIRST
			// of two duplicate same-named output columns regardless of which the
			// ordinal denotes (the conflation hazard). Out-of-range = malformed;
			// decline rather than guess. A MULTI-accessor path declines too:
			// the root ordinal selects the column but the remaining steps would
			// need re-anchoring over it; nil is the
			// generic can't-push-down answer.
			if fv.Resolved != nil {
				if acc, single := fv.Resolved.Single(); single {
					if o := acc.Ordinal; o >= 0 && o < len(rc.Fields) {
						return rc.Fields[o].Value
					}
				}
				return nil
			}
			// LAZY push-down: name-based, but DECLINE on an ambiguous name —
			// same rationale as composeFieldOverConstructor's lazy arm (a
			// dup-named RC has no defensible first match).
			var match Value
			for _, field := range rc.Fields {
				if field.Name == fv.Field {
					if match != nil {
						return nil
					}
					match = field.Value
				}
			}
			if match != nil {
				return match
			}
			return nil // field not found in constructor
		}
	}

	// Case 3: resultValue is a passthrough (QOV/ObjectValue) — a legacy flat
	// field stays flat, while an upper-anchored field is restored to this
	// passthrough source.
	if _, ok := resultValue.(*QuantifiedObjectValue); ok {
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
	fv, ok := v.(*FieldValue)
	if !ok || fv == nil || !nonNilPassthroughValue(resultValue) {
		return nil
	}
	if fv.Child == nil {
		// Preserve the baked-ordinal marker — see pullUpThroughPassthrough.
		return &FieldValue{
			Field:    fv.Field,
			Typ:      fv.Typ,
			Resolved: fv.Resolved,
		}
	}
	qov, ok := fv.Child.(*QuantifiedObjectValue)
	if !ok || qov == nil || qov.Correlation != upperAlias {
		return nil
	}
	return &FieldValue{
		Field:    fv.Field,
		Typ:      fv.Typ,
		Child:    resultValue,
		Resolved: fv.Resolved,
	}
}

func nonNilPassthroughValue(value Value) bool {
	switch value := value.(type) {
	case *QuantifiedObjectValue:
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
func PullUpValues(toBePulledUp []Value, resultValue Value, alias CorrelationIdentifier) map[Value]Value {
	result := make(map[Value]Value)
	for _, v := range toBePulledUp {
		if pulled := PullUpValue(v, resultValue, alias); pulled != nil {
			result[v] = pulled
		}
	}
	return result
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
