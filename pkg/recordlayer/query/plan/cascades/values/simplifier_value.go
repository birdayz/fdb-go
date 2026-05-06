package values

// SimplifyValue is the standalone-Value counterpart to Simplify.
// Folds constant sub-trees in a Value (e.g. SELECT-list expressions
// or projection arguments that never reach a comparison and so never
// hit ComparisonConstantSimplifyRule).
//
// Two-phase per node, post-order:
//
//  1. Recurse into children — fold them first so partial folds work
//     (e.g. `name + (1+2)` becomes `name + 3` in one pass).
//  2. If the rebuilt node is fully constant per IsConstantValue, fold
//     to a literal Value via LiteralValue (preserves the original
//     Type so downstream type checks stay consistent).
//
// Returns the input unchanged when nothing folds — pointer-equality
// stable so callers can cheaply check for "did anything happen?".
//
// Why a free function rather than a CascadesRule: the rule framework
// targets QueryPredicate matchers; standalone Values have no
// surrounding predicate to match against. Phase 4.6 introduces a
// proper Value-rule infrastructure (ValueSimplificationRuleSet in
// Java) and SimplifyValue retires.
//
// Coverage: ArithmeticValue, CastValue, PromoteValue,
// ScalarFunctionValue, NotValue. Other composites
// (RecordConstructorValue, AggregateValue) are not folded —
// Aggregate inherently needs row context, RecordConstructor seldom
// appears in a fold-able position. Adding more shapes is mechanical
// when need arises (extend isFoldableComposite + simplifyChildren).
func SimplifyValue(v Value) Value {
	if v == nil {
		return nil
	}
	rebuilt := simplifyChildren(v)
	// Only fold composites that produce literal values when fully
	// constant. Leaves (ConstantValue / NullValue / BooleanValue /
	// FieldValue / …) have nothing to fold — re-wrapping them via
	// LiteralValue would just allocate a new equivalent node and
	// break the pointer-equality short-circuit callers depend on.
	// Composites outside this set (RecordConstructorValue,
	// AggregateValue) Evaluate to shapes LiteralValue can't faithfully
	// rewrap, so they pass through too.
	if !isFoldableComposite(rebuilt) {
		return rebuilt
	}
	if lit, ok := EvaluateConstant(rebuilt); ok {
		out := LiteralValue(lit)
		// Preserve the original Type — LiteralValue defaults to
		// TypeUnknown for non-bool / non-nil literals; we know the
		// arithmetic / cast result type from the source node, so
		// surface it on the folded ConstantValue / NullValue. Once
		// the Type hierarchy lands and rules start matching on
		// `NULL :: TypeInt` vs `NULL :: TypeUnknown`, this carries
		// the typed-null semantics through the fold path.
		switch o := out.(type) {
		case *ConstantValue:
			if o.Typ == TypeUnknown {
				o.Typ = v.Type()
			}
		case *NullValue:
			if o.Typ == TypeUnknown {
				o.Typ = v.Type()
			}
		}
		return out
	}
	return rebuilt
}

// isFoldableComposite is the whitelist of Value shapes SimplifyValue
// will attempt to collapse to a literal. Limited to composites whose
// Evaluate produces a Go-native scalar that LiteralValue can faithfully
// rewrap.
func isFoldableComposite(v Value) bool {
	switch v.(type) {
	case *ArithmeticValue, *CastValue, *PromoteValue, *ScalarFunctionValue, *NotValue,
		*AndOrValue, *ConditionSelectorValue, *PickValue, *EvaluatesToValue:
		return true
	}
	return false
}

// simplifyChildren rebuilds v with each child recursively simplified.
// Returns v unchanged (same pointer) when no child changed — keeps
// the SimplifyValue caller's pointer-equality short-circuit usable.
func simplifyChildren(v Value) Value {
	switch x := v.(type) {
	case *ArithmeticValue:
		l := SimplifyValue(x.Left)
		r := SimplifyValue(x.Right)
		if l == x.Left && r == x.Right {
			return v
		}
		return &ArithmeticValue{Op: x.Op, Left: l, Right: r}
	case *CastValue:
		c := SimplifyValue(x.Child)
		if c == x.Child {
			return v
		}
		return NewCastValue(c, x.Target)
	case *PromoteValue:
		c := SimplifyValue(x.Child)
		if c == x.Child {
			return v
		}
		return NewPromoteValue(c, x.Target)
	case *ScalarFunctionValue:
		anyChanged := false
		newArgs := make([]Value, len(x.Args))
		for i, a := range x.Args {
			n := SimplifyValue(a)
			if n != a {
				anyChanged = true
			}
			newArgs[i] = n
		}
		if !anyChanged {
			return v
		}
		return &ScalarFunctionValue{FuncName: x.FuncName, Args: newArgs, Typ: x.Typ}
	case *NotValue:
		c := SimplifyValue(x.Child)
		if c == x.Child {
			return v
		}
		return &NotValue{Child: c}
	case *AndOrValue:
		l := SimplifyValue(x.Left)
		r := SimplifyValue(x.Right)
		if l == x.Left && r == x.Right {
			return v
		}
		return NewAndOrValue(x.Op, l, r)
	case *ConditionSelectorValue:
		anyChanged := false
		newImpl := make([]Value, len(x.Implications))
		for i, impl := range x.Implications {
			n := SimplifyValue(impl)
			if n != impl {
				anyChanged = true
			}
			newImpl[i] = n
		}
		if !anyChanged {
			return v
		}
		return NewConditionSelectorValue(newImpl)
	case *EvaluatesToValue:
		c := SimplifyValue(x.Child)
		if c == x.Child {
			return v
		}
		return NewEvaluatesToValue(c, x.Eval)
	case *PickValue:
		anyChanged := false
		newSel := SimplifyValue(x.Selector)
		if newSel != x.Selector {
			anyChanged = true
		}
		newAlts := make([]Value, len(x.Alternatives))
		for i, a := range x.Alternatives {
			if a == nil {
				newAlts[i] = nil
				continue
			}
			n := SimplifyValue(a)
			if n != a {
				anyChanged = true
			}
			newAlts[i] = n
		}
		if !anyChanged {
			return v
		}
		return NewPickValue(newSel, newAlts, x.Typ)
	}
	return v
}
