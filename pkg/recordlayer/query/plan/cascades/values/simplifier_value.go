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
	fv, ok := v.(*fieldValue)
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
	outer, ok := v.(*fieldValue)
	if !ok || outer.Child == nil || outer.Resolved == nil {
		return nil
	}
	inner, ok := outer.Child.(*fieldValue)
	if !ok || inner.Resolved == nil || inner.Child == nil {
		return nil
	}
	fused := inner.Resolved.WithSuffix(outer.Resolved)
	return &fieldValue{
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
				replacement, replaceable := coalesceReplacementFor(child, sf)
				if !replaceable {
					return v
				}
				return replacement
			}
		}
		newArgs = append(newArgs, child)
	}

	if onlyNulls {
		// A NULL result needs no carrier: ScalarFunctionValue.Evaluate
		// returns before coerceNumericResult when the result is nil, so a
		// NullValue reproduces the removed node exactly.
		return &NullValue{Typ: sf.Typ}
	}
	if !yieldsNew {
		return v
	}
	if len(newArgs) == 1 {
		// The degenerate exit removes the node just as the winning-constant
		// exit above does, and owes the same debt. It is the exit where a
		// RUNTIME value survives: len(newArgs) == 1 together with yieldsNew
		// implies the survivor is a cannotFoldCoalesce child (a null skipped
		// here requires removeRedundantNulls, which only a non-constant child
		// sets, and that child was appended), so anything to be restored has
		// to be a node in the tree rather than arithmetic on a literal.
		replacement, replaceable := coalesceReplacementFor(newArgs[0], sf)
		if !replaceable {
			return v
		}
		return replacement
	}
	return &ScalarFunctionValue{FuncName: sf.FuncName, Args: newArgs, Typ: sf.Typ}
}

// coalesceReplacementFor returns the value that may stand in for a COALESCE
// node, or replaceable=false to decline the simplification and leave the node
// alone.
//
// A COALESCE node owes its parent TWO things, and dropping the node drops both
// unless they are put back:
//
//   - its declared TYPE, which parents dispatch on. CastValue.Evaluate reads
//     `c.Child.Type()` to pick a cast rule, and Java's cast table admits
//     INT->BOOLEAN while rejecting LONG->BOOLEAN. So
//     `CAST(COALESCE(int_column, CAST(NULL AS BIGINT)) AS BOOLEAN)` is LONG at
//     the cast and correctly refuses; reduced to the bare INT column it started
//     answering, which is Go accepting a cast Java rejects.
//   - its result CARRIER. ScalarFunctionValue.Evaluate post-processes with
//     coerceNumericResult(result, declaredType), so a DOUBLE-declared COALESCE
//     hands back a float64. `COALESCE(long_column, CAST(NULL AS DOUBLE))`
//     reduced to the bare column handed back an int64 — the same number on a
//     carrier the declared type says it does not have.
//
// Java owes neither, and the reason is where the conversion lives: Java wraps
// every VariadicFunctionValue child in a PromoteValue to the common type at
// construction, so EvaluateConstantCoalesceRule yielding a child yields the
// promotion with it. Go records the common type on the function node instead.
//
// PromoteValue pays both debts at once, and not merely by resembling the right
// node: its Type() IS the target, and for every non-UUID target its Evaluate IS
// coerceNumericResult(child, target) — the same function on the same argument
// that the removed node applied.
//
// The UUID target is where that identity stops holding, and it is REACHABLE
// rather than theoretical: MaximumType(STRING, UUID) is UUID, so
// `COALESCE(string_column, uuid_column)` declares UUID over a STRING survivor,
// and there PromoteValue parses the string into a neutral [16]byte where
// coerceNumericResult passes it through. Substituting a different conversion is
// not this rule's call to make, so it declines and the COALESCE stands — which
// costs a simplification and can never cost an answer.
//
// A CONSTANT winner takes the conversion statically instead: folding it now is
// strictly better than leaving a Promote over a literal for a later pass, and
// it carries the declared type on the folded literal, so it owes nothing
// further.
func coalesceReplacementFor(winner Value, sf *ScalarFunctionValue) (Value, bool) {
	declared := sf.Type()
	if constant, isConstant := winner.(*ConstantValue); isConstant {
		return &ConstantValue{
			Value: coerceNumericResult(constant.Value, declared),
			Typ:   sf.Typ,
		}, true
	}
	if sameDeclaredType(winner.Type(), declared) && !carrierConvertingType(declared) {
		// Nothing to restore: the survivor already presents the node's type,
		// and the node's own post-processing was the identity.
		return winner, true
	}
	if IsUuid(declared) {
		return nil, false
	}
	return NewPromoteValue(winner, declared), true
}

// sameDeclaredType compares two types on every axis EXCEPT nullability.
//
// Nullability is excluded because ScalarFunctionValue.Type() forces it on
// (`WithNullability(s.Typ, true)`) whatever the arguments were, so a NOT NULL
// survivor under a nullable-forced declaration differs on that axis alone and
// on no other. Wrapping for that difference would add a node whose only effect
// is to re-assert a nullability the parent already treats as nullable.
func sameDeclaredType(a, b Type) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return WithNullability(a, true).Equals(WithNullability(b, true))
}

// carrierConvertingType reports whether coerceNumericResult is anything other
// than the identity for this type — that is, whether a value reaching it can
// come out on a different Go carrier than it went in on.
//
// It is a statement ABOUT coerceNumericResult, so it is stated here beside it
// and pinned by TestCarrierConvertingTypeMatchesCoerceNumericResult, which
// drives every TypeCode through both and fails if the two ever disagree. The
// alternative — a caller restating "DOUBLE and FLOAT" inline — is a copy that
// nothing makes track the switch it is copying.
func carrierConvertingType(t Type) bool {
	if t == nil {
		return false
	}
	switch t.Code() {
	case TypeCodeDouble, TypeCodeFloat:
		return true
	default:
		return false
	}
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
