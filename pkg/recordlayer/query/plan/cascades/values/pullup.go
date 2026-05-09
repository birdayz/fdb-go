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
func pullUpThroughRecordConstructor(v Value, rc *RecordConstructorValue, alias CorrelationIdentifier) Value {
	for _, field := range rc.Fields {
		if semanticEqual(v, field.Value) {
			return &FieldValue{Field: field.Name, Typ: field.Value.Type()}
		}
	}
	return nil
}

// pullUpThroughPassthrough handles pull-up through an identity-like
// result value (QOV, ObjectValue). Field accesses pass through
// unchanged.
func pullUpThroughPassthrough(v Value, alias CorrelationIdentifier) Value {
	if fv, ok := v.(*FieldValue); ok {
		return &FieldValue{Field: fv.Field, Typ: fv.Typ}
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
			for _, field := range rc.Fields {
				if field.Name == fv.Field {
					return field.Value
				}
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
		return &FieldValue{Field: fv.Field, Typ: fv.Typ}
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

// semanticEqual checks if two values are semantically equivalent,
// using ExplainValue comparison (the same approach used by
// valuesEqual in rich_ordering.go).
func semanticEqual(a, b Value) bool {
	if a == b {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return ExplainValue(a) == ExplainValue(b)
}
