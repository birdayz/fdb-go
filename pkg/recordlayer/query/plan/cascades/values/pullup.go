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
//   - v = FV("x") → FV("a")       // input field "x" becomes output field "a"
//   - v = FV("y") → FV("b")       // input field "y" becomes output field "b"
//   - v = resultValue → QOV(alias) // the whole result maps to the output alias
//
// For non-RecordConstructor result values (e.g. a QuantifiedObjectValue
// passthrough), v is matched directly:
//
//   - v = resultValue → QOV(alias)
//   - v = FV("x"), resultValue = QOV(q) → FV("x") // field access passes through
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
	// through unchanged (different field, same base).
	if _, ok := resultValue.(*QuantifiedObjectValue); ok {
		return pullUpThroughPassthrough(v, alias)
	}
	if _, ok := resultValue.(*ObjectValue); ok {
		return pullUpThroughPassthrough(v, alias)
	}

	return nil
}

// pullUpThroughRecordConstructor handles the case where the result
// value is a record constructor with named fields.
//
// For each field in the constructor, check if v equals that field's
// value. If so, v can be accessed as the output field name.
//
// RFC-173 Slice 2: the emitted reference is re-framed to the RC's OUTPUT
// column i, so when the ordinal matters it is BAKED — a lazy name node over a
// duplicate-named RC output would later resolve to the FIRST same-named column
// regardless of which column matched (§5 conflation hazard). Baking is gated
// to keep the stage dark: only a baked input (bakedness must survive pull-up)
// or a dup-named RC (unconstructible under the name model — only ordinal
// seeds build them) bakes; a lazy input over a clean-named RC emits the lazy
// node it always did.
func pullUpThroughRecordConstructor(v Value, rc *RecordConstructorValue, alias CorrelationIdentifier) Value {
	inBaked, inPinned := false, false
	if fv, ok := v.(*FieldValue); ok && fv.Resolved != nil {
		inBaked = true
		inPinned = fv.Resolved.FrontierPinned
	}
	for i, field := range rc.Fields {
		if semanticEqual(v, field.Value) {
			out := &FieldValue{Field: field.Name, Typ: field.Value.Type()}
			if inBaked || rcHasDuplicateNames(rc) {
				// The frontier-contract bit INHERITS from the input: a pinned
				// seed ref pulled through the join's RC still reads a
				// positional row (the gated join births them), so the loud
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

// rcHasDuplicateNames reports whether two RC columns share a name — the §5
// duplicate-name shape, constructible only by ordinal seeds (RFC-173 S2+).
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
// result value (QOV, ObjectValue). Field accesses pass through
// unchanged.
//
// Limitation: the pulled-up value is a bare FieldValue without anchoring
// it to the alias. In multi-source contexts (e.g. joins), Java would
// produce a FieldAccessValue(QuantifiedObjectValue(alias), field) to
// disambiguate which source the field comes from. Go doesn't have
// FieldAccessValue yet. In practice this is safe because the call sites
// (ordering pullup) use this in single-source contexts where there's no
// ambiguity. If multi-source passthrough pull-up is needed, either add
// FieldAccessValue or prefix the field with the alias ("alias.field").
func pullUpThroughPassthrough(v Value, alias CorrelationIdentifier) Value {
	if fv, ok := v.(*FieldValue); ok {
		// Preserve the RFC-173 baked-ordinal marker through the copy: the
		// passthrough is an identity result value (same record flows), so the
		// baked position stays valid; dropping it would silently degrade a
		// BAKED node to lazy (§5 conflation hazard).
		return &FieldValue{Field: fv.Field, Typ: fv.Typ, Resolved: fv.Resolved}
	}
	return nil
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
			// RFC-173 Slice 2: a BAKED node resolves by ORDINAL — same rationale
			// as composeFieldOverConstructor: a name lookup would pick the FIRST
			// of two duplicate same-named output columns regardless of which the
			// ordinal denotes (§5 conflation hazard). Out-of-range = malformed;
			// decline rather than guess. A MULTI-accessor path declines too:
			// the root ordinal selects the column but the remaining steps would
			// need re-anchoring over it — S3-W2 territory, and nil is the
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
			// dup-named RC has no defensible first match; review W2 checklist).
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

	// Case 3: resultValue is a passthrough (QOV/ObjectValue) — field
	// accesses pass through unchanged.
	if _, ok := resultValue.(*QuantifiedObjectValue); ok {
		return pushDownThroughPassthrough(v)
	}
	if _, ok := resultValue.(*ObjectValue); ok {
		return pushDownThroughPassthrough(v)
	}

	return nil
}

// pushDownThroughPassthrough handles push-down through identity-like
// result values. Field accesses pass through unchanged.
func pushDownThroughPassthrough(v Value) Value {
	if fv, ok := v.(*FieldValue); ok {
		// Preserve the RFC-173 baked-ordinal marker — see pullUpThroughPassthrough.
		return &FieldValue{Field: fv.Field, Typ: fv.Typ, Resolved: fv.Resolved}
	}
	return nil
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
