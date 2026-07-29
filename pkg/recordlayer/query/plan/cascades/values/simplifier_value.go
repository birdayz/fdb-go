package values

import "fmt"

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
// surrounding predicate to match against. (Java models this as its
// ValueSimplificationRuleSet; Go's equivalent is this fold.)
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
	if s := composeFieldOverConstructor(rebuilt); s != nil {
		return SimplifyValue(s)
	}
	if s := composeFieldOverField(rebuilt); s != nil {
		return SimplifyValue(s)
	}
	if s := simplifyCoalesce(rebuilt); s != rebuilt {
		return s
	}
	if isCoalesceValue(rebuilt) {
		return rebuilt
	}
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
		// `NULL :: NullableLong` vs `NULL :: TypeUnknown`, this carries
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
		if cv, ok := c.(*ConstantValue); ok {
			if folded := tryCastConstant(cv, x.Target); folded != nil {
				return folded
			}
		}
		if c == x.Child {
			return v
		}
		return NewCastValue(c, x.Target)
	case *PromoteValue:
		c := SimplifyValue(x.Child)
		if cv, ok := c.(*ConstantValue); ok {
			// Apply the promotion through Evaluate before re-tagging. Numeric
			// promotions align the carrier width (including direct LONG→FLOAT
			// rounding), while STRING→UUID reshapes the canonical string into a
			// neutral [16]byte. On an error, keep the Promote node so it surfaces
			// at execution, exactly as Java's PromoteValue does.
			if folded, err := (&PromoteValue{Child: cv, Target: x.Target}).Evaluate(nil); err == nil {
				return &ConstantValue{Value: folded, Typ: x.Target}
			}
			return NewPromoteValue(cv, x.Target)
		}
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

// composeFieldOverConstructor implements Java's ComposeFieldValueOverRecordConstructorRule:
// field(RecordConstructor(..., x as name, ...), "name") → x
func composeFieldOverConstructor(v Value) Value {
	fv, ok := v.(*FieldValue)
	if !ok || fv.Child == nil {
		return nil
	}
	rc, ok := fv.Child.(*RecordConstructorValue)
	if !ok {
		return nil
	}
	// A BAKED node composes by ORDINAL — Java's
	// ComposeFieldValueOverRecordConstructorRule.findColumn is
	// getColumns().get(fieldOrdinal). Composing by the display name would pick
	// the FIRST of two duplicate same-named columns regardless of which the
	// ordinal denotes (the conflation hazard). An out-of-range ordinal against
	// the node's OWN child RC is always a tree inconsistency — a planner bug,
	// loud like Java's IndexOutOfBounds (the fail-loud re-stamp treatment),
	// never a silent decline that rides the broken node into the plan.
	if fv.Resolved != nil {
		acc, single := fv.Resolved.Single()
		if !single {
			// A fused multi-accessor path over its own RC: the ROOT ordinal
			// selects the column; the remaining steps would need re-anchoring
			// over the column's value tree. Decline
			// the fold — the node stays as-is, no wrong answer possible.
			return nil
		}
		o := acc.Ordinal
		if o < 0 || o >= len(rc.Fields) {
			panic(fmt.Sprintf("baked FieldValue %s#%d composed over its own %d-column RecordConstructor — tree inconsistent with the bake (planner bug; Java throws IndexOutOfBounds)", fv.Field, o, len(rc.Fields)))
		}
		return rc.Fields[o].Value
	}
	// A LAZY node has no ordinal, so it has nothing to select a member with, and
	// it DECLINES. Java's rule has no name arm to port: findColumn is
	// `getColumns().get(fieldOrdinal)` and nothing else
	// (ComposeFieldValueOverRecordConstructorRule.java:99-102), because Java's
	// FieldValue is resolved at construction and a nameless lazy carrier is a
	// shape it cannot express.
	//
	// The arm this replaces matched a member by `field.Name == fv.Field` and was
	// correct only because of a duplicate-name guard sitting under it — a guard
	// whose existence is the argument against the key it guards. Both are gone
	// together (RFC-197 item 3); a declined fold leaves the node as it stands,
	// which is never a wrong answer.
	//
	// Measured before removing, not argued: the arm MATCHED zero times across the
	// explaindiff corpus, the whole //pkg/relational/sqldriver FDB suite and every
	// conformance harness — a panic wired into the match point is never reached.
	return nil
}

// composeFieldOverField implements Java's ComposeFieldValueOverFieldValueRule:
// field(field(v, path1), path2) is a nested field access. In Go's single-step
// model this doesn't apply directly (FieldValue has one Field, not a path).
// But when Child is another FieldValue accessing the same base, we can flatten.
func tryCastConstant(cv *ConstantValue, target Type) (out *ConstantValue) {
	cast := NewCastValue(cv, target)
	result, err := cast.Evaluate(nil)
	if err != nil {
		// Cast/arith/type-mismatch errors mean "not foldable" — decline.
		// The runtime typed-error family now arrives as a return value, so
		// the prior recover that caught those panics is dead and removed.
		return nil
	}
	if result != nil {
		return &ConstantValue{Value: result, Typ: target}
	}
	return nil
}

// composeFieldOverField is Java's ComposeFieldValueOverFieldValueRule
// (ComposeFieldValueOverFieldValueRule.java:57-69): field(field(x, p1), p2) →
// field(x, p1.withSuffix(p2)) — chained FieldValue nodes fuse into ONE node
// carrying the whole path (Java's canonical form; chained nodes are not).
//
// GATED TO FULLY-BAKED CHAINS: firing on lazy chains would rewrite every
// nested-access chain corpus-wide —
// memo identity, Explain renderings, every rule matching chained FieldValues.
// A lazy chain keeps its shape; only baked-over-baked chains fuse.
// Java's inverse (ExpandFusedFieldValueRule) is deliberately NOT ported:
// Java never co-resides the two (Compose in DefaultValueSimplificationRuleSet,
// Expand only in MaxMatchMapSimplification), and compose-only cannot loop.
func composeFieldOverField(v Value) Value {
	outer, ok := v.(*FieldValue)
	if !ok || outer.Child == nil || outer.Resolved == nil {
		return nil
	}
	inner, ok := outer.Child.(*FieldValue)
	if !ok || inner.Resolved == nil || inner.Child == nil {
		return nil
	}
	fused := inner.Resolved.WithSuffix(outer.Resolved)
	return &FieldValue{
		// Display = the LAST step's name (Java getLastFieldName); the fused
		// node reads exactly what the chain read, so Typ is the OUTER's.
		Field:    fused.Last().Field,
		Typ:      outer.Typ,
		Child:    inner.Child,
		Resolved: fused,
	}
}

func isCoalesceValue(v Value) bool {
	sf, ok := v.(*ScalarFunctionValue)
	return ok && sf.FuncName == "COALESCE"
}

// simplifyCoalesce implements Java's EvaluateConstantCoalesceRule:
//   - COALESCE(NULL, ..., NULL, <non-null-constant>, ...) → <non-null-constant>
//   - COALESCE(x, NULL, y, NULL) → COALESCE(x, y)  (remove nulls after first non-constant)
//   - COALESCE(NULL, ..., NULL) → NULL
//
// Returns v unchanged when v is not a COALESCE or no simplification applies.
func simplifyCoalesce(v Value) Value {
	sf, ok := v.(*ScalarFunctionValue)
	if !ok || sf.FuncName != "COALESCE" {
		return v
	}

	var newArgs []Value
	yieldsNew := false
	removeRedundantNulls := false
	seenOnlyConstantsSoFar := true
	onlyNulls := true

	for _, child := range sf.Args {
		if cannotFoldCoalesce(child) {
			onlyNulls = false
			removeRedundantNulls = true
			seenOnlyConstantsSoFar = false
		} else if _, isNull := child.(*NullValue); isNull {
			if removeRedundantNulls {
				yieldsNew = true
				continue
			}
		} else {
			onlyNulls = false
			if seenOnlyConstantsSoFar {
				// Java inserts PromoteValue children before building
				// COALESCE, so returning the winning constant preserves the
				// common result carrier. Go records the common type on the
				// ScalarFunctionValue instead; apply its carrier conversion
				// here before this rule removes the function wrapper.
				if constant, isConstant := child.(*ConstantValue); isConstant {
					return &ConstantValue{
						Value: coerceNumericResult(constant.Value, sf.Type()),
						Typ:   sf.Typ,
					}
				}
				return child
			}
		}
		newArgs = append(newArgs, child)
	}

	if onlyNulls {
		return &NullValue{Typ: sf.Typ}
	}
	if !yieldsNew {
		return v
	}
	if len(newArgs) == 1 {
		return newArgs[0]
	}
	return &ScalarFunctionValue{FuncName: sf.FuncName, Args: newArgs, Typ: sf.Typ}
}

// cannotFoldCoalesce mirrors Java's EvaluateConstantCoalesceRule.cannotFold:
// a value CAN be folded if it's NullValue, or a non-nullable constant
// (LiteralValue with isNotNullable). In Go terms: NullValue, ConstantValue
// with non-nil payload, or BooleanValue with non-nil *bool.
func cannotFoldCoalesce(v Value) bool {
	if _, isNull := v.(*NullValue); isNull {
		return false
	}
	if c, isConst := v.(*ConstantValue); isConst && c.Value != nil {
		return false
	}
	if bv, isBool := v.(*BooleanValue); isBool && bv.Value != nil {
		return false
	}
	return true
}

// ValueSimplifyContext carries context for context-aware value simplification.
// Matches Java's AbstractRuleCall fields: constantAliases + isRoot.
type ValueSimplifyContext struct {
	ConstantAliases map[CorrelationIdentifier]struct{}
	IsRoot          bool
}

// SimplifyValueWithContext applies context-aware simplification rules that
// SimplifyValue cannot handle. Ports Java's EliminateArithmeticValueWithConstantRule,
// FoldConstantRule, and LiftConstructorRule.
//
// Call SimplifyValue first (context-free), then SimplifyValueWithContext
// on the result with the appropriate context.
func SimplifyValueWithContext(v Value, ctx ValueSimplifyContext) Value {
	if v == nil {
		return nil
	}
	rebuilt := simplifyChildrenWithContext(v, ctx)
	if s := eliminateArithmeticWithConstant(rebuilt, ctx); s != nil {
		return SimplifyValueWithContext(s, ctx)
	}
	if ctx.IsRoot {
		if s := liftConstructor(rebuilt); s != nil {
			return SimplifyValueWithContext(s, ctx)
		}
	}
	if s := foldConstant(rebuilt, ctx); s != nil {
		return s
	}
	return rebuilt
}

func simplifyChildrenWithContext(v Value, ctx ValueSimplifyContext) Value {
	childCtx := ValueSimplifyContext{
		ConstantAliases: ctx.ConstantAliases,
		IsRoot:          false,
	}
	switch x := v.(type) {
	case *ArithmeticValue:
		l := SimplifyValueWithContext(x.Left, childCtx)
		r := SimplifyValueWithContext(x.Right, childCtx)
		if l == x.Left && r == x.Right {
			return v
		}
		return &ArithmeticValue{Op: x.Op, Left: l, Right: r}
	case *RecordConstructorValue:
		anyChanged := false
		newFields := make([]RecordConstructorField, len(x.Fields))
		for i, f := range x.Fields {
			n := SimplifyValueWithContext(f.Value, childCtx)
			if n != f.Value {
				anyChanged = true
			}
			newFields[i] = RecordConstructorField{Name: f.Name, Value: n}
		}
		if !anyChanged {
			return v
		}
		return &RecordConstructorValue{Fields: newFields}
	}
	return v
}

// eliminateArithmeticWithConstant implements Java's EliminateArithmeticValueWithConstantRule.
// For ADD/SUB where one operand's correlations are all constant, drop the constant
// operand (the result is order-equivalent to the non-constant operand).
func eliminateArithmeticWithConstant(v Value, ctx ValueSimplifyContext) Value {
	av, ok := v.(*ArithmeticValue)
	if !ok {
		return nil
	}
	if av.Op != OpAdd && av.Op != OpSub {
		return nil
	}
	allCorrelated := GetCorrelatedToOfValue(av)
	if containsAll(ctx.ConstantAliases, allCorrelated) {
		return nil
	}
	leftCorr := GetCorrelatedToOfValue(av.Left)
	if containsAll(ctx.ConstantAliases, leftCorr) {
		return av.Right
	}
	rightCorr := GetCorrelatedToOfValue(av.Right)
	if containsAll(ctx.ConstantAliases, rightCorr) {
		return av.Left
	}
	return nil
}

// foldConstant implements Java's FoldConstantRule.
// When all correlations of a value are constant, wrap in ConstantValue.
func foldConstant(v Value, ctx ValueSimplifyContext) Value {
	if _, ok := v.(*ConstantValue); ok {
		return nil
	}
	corr := GetCorrelatedToOfValue(v)
	if !containsAll(ctx.ConstantAliases, corr) {
		return nil
	}
	newChildren := make([]Value, 0)
	for _, child := range v.Children() {
		if cv, ok := child.(*ConstantValue); ok {
			if inner, iok := cv.Value.(Value); iok {
				newChildren = append(newChildren, inner)
				continue
			}
		}
		newChildren = append(newChildren, child)
	}
	rebuilt := WithChildren(v, newChildren)
	if rebuilt == nil {
		return nil
	}
	return &ConstantValue{Value: rebuilt, Typ: v.Type()}
}

// liftConstructor implements Java's LiftConstructorRule.
// Flattens nested RecordConstructorValue: RC(a, RC(b, c), d) → RC(a, b, c, d).
// Only fires at root (isRoot=true).
func liftConstructor(v Value) Value {
	outer, ok := v.(*RecordConstructorValue)
	if !ok {
		return nil
	}
	hasInnerRC := false
	for _, f := range outer.Fields {
		if _, isRC := f.Value.(*RecordConstructorValue); isRC {
			hasInnerRC = true
			break
		}
	}
	if !hasInnerRC {
		return nil
	}
	var lifted []RecordConstructorField
	for _, f := range outer.Fields {
		if inner, isRC := f.Value.(*RecordConstructorValue); isRC {
			for _, innerField := range inner.Fields {
				lifted = append(lifted, innerField)
			}
		} else {
			lifted = append(lifted, f)
		}
	}
	return &RecordConstructorValue{Fields: lifted}
}

func containsAll(set map[CorrelationIdentifier]struct{}, subset map[CorrelationIdentifier]struct{}) bool {
	for k := range subset {
		if _, ok := set[k]; !ok {
			return false
		}
	}
	return true
}
