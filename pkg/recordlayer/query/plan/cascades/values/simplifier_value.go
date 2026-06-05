package values

import "strings"

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
	if s := composeFieldOverConstructor(rebuilt); s != nil {
		return SimplifyValue(s)
	}
	if s := composeFieldOverJoinMerge(rebuilt); s != nil {
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
			return &ConstantValue{Value: cv.Value, Typ: x.Target}
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

// composeFieldOverJoinMerge canonicalizes a BARE FieldValue over a BINARY
// JoinMergeAllValue into a FieldValue over the merge's INNER quantifier,
// i.e. field(join_merge_all[outer,inner], "f") → field(QOV(inner), "f"). This
// lets downstream reasoning (PartitionSelect predicate classification,
// ImplementNestedLoopJoinRule predicate embedding, index-candidate SARG
// matching) see a normal correlated FieldValue over a single quantifier instead
// of an opaque merge it cannot reason about (RFC-042).
//
// Soundness rests on a STRUCTURAL INVARIANT, not on the test suite: a bare
// FieldValue acquires a merge child ONLY via SelectMergeRule
// (rule_select_merge.go), which substitutes the captured merge for a
// QOV(parentAlias) whose alias is the merge's INNER quantifier — the merge is
// re-flowed under the inner side's own alias. So the only fields ever composed
// onto the merge are inner-side references; outer- and third-table columns
// resolve through their own QOV and reach the merge only as already-qualified
// `ALIAS.COL` keys (value_join_merge_all.go), never as a bare FieldValue over it.
// Hence the bare field is unambiguously the inner side and the rewrite is sound.
// The substituted child is a translator binary seed, which preserves leg order
// [outer, inner], so the inner is Aliases[1].
//
// Only the BINARY (2-alias) merge is composed: an N-ary re-enumeration merge has
// no single "inner" leg, and a bare field over it would be ambiguous — refuse and
// let Evaluate resolve through the merged row's qualified keys.
//
// A QUALIFIED field (one carrying a "." prefix) is likewise a shape the invariant
// says never reaches here. Rather than blindly assume inner — which would
// mis-resolve an outer/foreign column to nil (REVIEW.md #216) — refuse the
// rewrite and let JoinMergeAllValue.Evaluate resolve the qualified key. This is a
// fail-safe: it costs no real canonicalization (qualified fields never hit this
// path) while removing the silent-mis-resolution landmine if the merge's alias
// convention ever changes. TestFDB_JoinMerge_OuterColumn_* pins, E2E, that no
// outer-only column is ever dropped by this rule across multi-way joins.
//
// The deeper Java-aligned fix is to anchor fields to their source during pull-up
// (ordinal FieldValue over QOV(alias)) so the opaque-merge ambiguity never
// exists, retiring this rule — tracked as true-7.6 (ordinal substrate).
func composeFieldOverJoinMerge(v Value) Value {
	fv, ok := v.(*FieldValue)
	if !ok || fv.Child == nil {
		return nil
	}
	jm, ok := fv.Child.(*JoinMergeAllValue)
	// Only a translator SEED (Seed=true, always binary) — the exact set the
	// retired binary JoinMergeResultValue covered. A bare FieldValue acquires a
	// merge child ONLY via SelectMergeRule substituting a flattened translator
	// seed; re-enumeration merges (Seed=false) never reach here and must NOT be
	// rewritten — firing on them generates extra distinct sub-expressions that
	// inflate the memo (measured: the ≥4-way STAR exceeded the task budget when
	// this matched any binary merge instead of just seeds).
	if !ok || !jm.Seed || len(jm.Aliases) != 2 {
		return nil
	}
	// Qualified reference: not provably inner-side — leave the merge intact
	// (Evaluate resolves the qualified key correctly).
	if strings.Contains(fv.Field, ".") {
		return nil
	}
	return NewFieldValue(NewQuantifiedObjectValue(jm.Aliases[1]), fv.Field, fv.Typ)
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
	for _, field := range rc.Fields {
		if field.Name == fv.Field {
			return field.Value
		}
	}
	return nil
}

// composeFieldOverField implements Java's ComposeFieldValueOverFieldValueRule:
// field(field(v, path1), path2) is a nested field access. In Go's single-step
// model this doesn't apply directly (FieldValue has one Field, not a path).
// But when Child is another FieldValue accessing the same base, we can flatten.
func tryCastConstant(cv *ConstantValue, target Type) (out *ConstantValue) {
	defer func() {
		if r := recover(); r != nil {
			switch r.(type) {
			case *InvalidCastError, *ArithmeticOverflowError, *ScalarTypeMismatchError:
				out = nil
			default:
				panic(r)
			}
		}
	}()
	cast := NewCastValue(cv, target)
	result := cast.Evaluate(nil)
	if result != nil {
		return &ConstantValue{Value: result, Typ: target}
	}
	return nil
}

func composeFieldOverField(v Value) Value {
	outer, ok := v.(*FieldValue)
	if !ok || outer.Child == nil {
		return nil
	}
	_, ok = outer.Child.(*FieldValue)
	if !ok {
		return nil
	}
	// Go's FieldValue is single-step (one field name per node), so
	// field(field(x, "a"), "b") is already the canonical form for
	// nested access. Java has multi-step FieldPath; Go doesn't.
	// No simplification possible in Go's single-step model.
	return nil
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
		// Preserve the AnchoredJoin marker (RFC-077 7.6) — a simplified anchored
		// join result is still a join result; dropping it would un-hide the leg
		// QOVs from GetCorrelatedToOfValue.
		return &RecordConstructorValue{Fields: newFields, AnchoredJoin: x.AnchoredJoin}
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
	// Preserve the AnchoredJoin marker (RFC-077 7.6). liftConstructor only fires
	// on an RC with nested-RC fields, which an anchored join result never has (its
	// fields are FieldValues), so this is defensive — but it keeps the "preserved
	// through every reconstruction" invariant honest.
	return &RecordConstructorValue{Fields: lifted, AnchoredJoin: outer.AnchoredJoin}
}

func containsAll(set map[CorrelationIdentifier]struct{}, subset map[CorrelationIdentifier]struct{}) bool {
	for k := range subset {
		if _, ok := set[k]; !ok {
			return false
		}
	}
	return true
}
