// Portions derived from FoundationDB Record Layer (QuantifierValues.java,
// FieldValue.java, ComposeFieldValueOverFieldValueRule.java, Value.java),
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2026 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package ddl

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
)

// quantifierValues is Java's QuantifierValues (QuantifierValues.java) over the
// graph Go's translator builds for an index definition: it resolves a value
// written in terms of the graph's quantifiers down to the base record,
// composing field accesses through record constructors and through other
// field accesses as Java's simplification does (ComposeFieldValueOverRecord-
// ConstructorRule, ComposeFieldValueOverFieldValueRule).
//
// Java keeps one alias -> value map for the whole plan, which its unique
// aliases allow. Go's translator names quantifiers by table alias, reuses a
// name at every level of a chain, and lets a level above a join name its own
// quantifier after one leg while its values read the merged row's legs
// (`Project[T.COL2, E]` over a quantifier named E). So a quantified value is
// resolved in SCOPE (resolveAlias): the design's 3.3 implementation notes,
// item 2, state the rule and the shapes it was measured on.
//
// A quantifier over an explode stands for the collection being unnested, not
// for the element (Java's unnestedCollectionValue): the collection's field
// path with its last accessor MARKED, one marker per explode, so two unnests
// of one array stay distinct and the renderer knows the array is reached
// through an unnest. A resolved field access is a pathColumn, whose steps carry
// the markers: the planner's FieldPath cannot hold them, nor a step into an
// unnested array's element.
type quantifierValues struct {
	explodes map[*expressions.ExplodeExpression]int
}

func newQuantifierValues() *quantifierValues {
	return &quantifierValues{explodes: map[*expressions.ExplodeExpression]int{}}
}

// scope is an expression whose quantifiers a value may name, and the scope it
// sits in (for a correlation to an enclosing leg).
type scope struct {
	expr   expressions.RelationalExpression
	parent *scope
}

func (s *scope) child(expr expressions.RelationalExpression) *scope {
	return &scope{expr: expr, parent: s}
}

// member returns the expression a translated Reference holds. The index
// definition's graph is the translator's, before any planning, so every
// Reference holds exactly one.
func member(ref *expressions.Reference) (expressions.RelationalExpression, error) {
	if ref == nil {
		return nil, internalError("a quantifier ranges over no reference")
	}
	members := ref.Members()
	if len(members) != 1 {
		return nil, internalError("a translated reference holds %d members, want 1", len(members))
	}
	return members[0], nil
}

// passesRowThrough reports whether e's result is its single quantifier's row
// (a sort, a filter): a value above it reads the row below it.
func passesRowThrough(e expressions.RelationalExpression) (expressions.Quantifier, bool) {
	qs := e.GetQuantifiers()
	if len(qs) != 1 {
		return expressions.Quantifier{}, false
	}
	qov, ok := values.AsQuantifiedObjectValue(e.GetResultValue())
	if !ok || qov.Correlation() != qs[0].GetAlias() {
		return expressions.Quantifier{}, false
	}
	return qs[0], true
}

// resolve rewrites v, a value of the expression sc names, over the base
// record.
func (qv *quantifierValues) resolve(v values.Value, sc *scope) (values.Value, error) {
	if v == nil {
		return nil, nil
	}
	if qov, ok := values.AsQuantifiedObjectValue(v); ok {
		return qv.resolveAlias(qov.Correlation(), sc)
	}
	if fv, ok := values.AsFieldValue(v); ok {
		child, err := qv.resolve(fv.ChildValue(), sc)
		if err != nil {
			return nil, err
		}
		return qv.access(child, fv.Path().Ordinals())
	}
	switch x := v.(type) {
	case *values.QueriedValue:
		// The scanned record: every resolved path is rooted at one quantified
		// value of its type, so two paths compare by their accessors alone.
		base, err := values.NewQuantifiedObjectValue(baseCorrelation, x.Type())
		if err != nil {
			return nil, internalError("the scanned record has no exact type: %v", err)
		}
		return base, nil
	case *values.ConstantValue:
		return v, nil
	case *values.RecordConstructorValue:
		fields := make([]values.RecordConstructorField, len(x.Fields))
		for i, f := range x.Fields {
			resolved, err := qv.resolve(f.Value, sc)
			if err != nil {
				return nil, err
			}
			fields[i] = values.RecordConstructorField{Name: f.Name, Value: resolved}
		}
		return values.NewRawRecordConstructorValue(fields...), nil
	case *values.DerivedValue:
		// A group by's row carries each aggregate as a typed DerivedValue over
		// the aggregate; Java's row holds the aggregate itself.
		if len(x.ChildrenList) == 1 {
			switch x.ChildrenList[0].(type) {
			case *values.AggregateValue, *values.IndexOnlyAggregateValue:
				return qv.resolve(x.ChildrenList[0], sc)
			}
		}
	}
	children := v.Children()
	if len(children) == 0 {
		return v, nil
	}
	resolved := make([]values.Value, len(children))
	for i, c := range children {
		r, err := qv.resolve(c, sc)
		if err != nil {
			return nil, err
		}
		resolved[i] = r
	}
	rebuilt, err := values.WithChildrenChecked(v, resolved)
	if err != nil {
		return nil, internalError("cannot rebuild %s over resolved children: %v", v.Name(), err)
	}
	return rebuilt, nil
}

// baseCorrelation names the scanned record every resolved path is rooted at.
var baseCorrelation = values.NamedCorrelationIdentifier("__INDEX_BASE")

// resolveAlias is what the quantified value named alias stands for in sc.
func (qv *quantifierValues) resolveAlias(alias values.CorrelationIdentifier, sc *scope) (values.Value, error) {
	for s := sc; s != nil; s = s.parent {
		if v, ok, err := qv.aliasIn(alias, s); ok || err != nil {
			return v, err
		}
	}
	return nil, internalError("the index definition reads %s, which no enclosing expression binds", alias)
}

// aliasIn answers alias in one scope, or reports it unbound there.
func (qv *quantifierValues) aliasIn(alias values.CorrelationIdentifier, sc *scope) (values.Value, bool, error) {
	if sel, ok := sc.expr.(*expressions.SelectExpression); ok {
		for _, q := range sel.GetQuantifiers() {
			if q.GetAlias() == alias {
				v, err := qv.leg(q, sc)
				return v, true, err
			}
		}
		return nil, false, nil
	}
	qs := sc.expr.GetQuantifiers()
	if len(qs) != 1 {
		return nil, false, nil
	}
	// A value of a single-input expression reads its input row. Every alias on
	// the way down through expressions passing that row on labels the same
	// row; a leg of the join producing it is named by its own alias.
	labels := qs[0].GetAlias() == alias
	cur := sc
	producer, err := member(qs[0].GetRangesOver())
	if err != nil {
		return nil, false, err
	}
	for {
		cur = cur.child(producer)
		inner, passes := passesRowThrough(producer)
		if !passes {
			break
		}
		labels = labels || inner.GetAlias() == alias
		if producer, err = member(inner.GetRangesOver()); err != nil {
			return nil, false, err
		}
	}
	if sel, ok := producer.(*expressions.SelectExpression); ok {
		// Above a select, a name reads a LEG WINDOW of its merged row: the
		// layout authority the executor agrees with, flattened across nested
		// joins, so a leg's alias names that leg even where the translator
		// named the quantifier over a nested join after its rightmost leg.
		if v, found, err := qv.windowed(alias, sel, cur); found || err != nil {
			return v, true, err
		}
		for _, q := range sel.GetQuantifiers() {
			if q.GetAlias() == alias {
				v, err := qv.leg(q, cur)
				return v, true, err
			}
		}
	}
	if !labels {
		return nil, false, nil
	}
	v, err := qv.row(producer, cur)
	return v, true, err
}

// windowed is what alias names in the merged row sel's result carries, read
// through values.OrdinalSeedLegWindowsAcceptingNested: a nested leg's one slot,
// a scalar element's one column, or a record of a flat run's columns. Not found
// when the row is not a seed the authority recognizes or alias is not a leg of
// it.
func (qv *quantifierValues) windowed(alias values.CorrelationIdentifier, sel *expressions.SelectExpression, selScope *scope) (values.Value, bool, error) {
	rc, ok := sel.GetResultValue().(*values.RecordConstructorValue)
	if !ok {
		return nil, false, nil
	}
	windows, _ := values.OrdinalSeedLegWindowsAcceptingNested(rc)
	w, ok := windows[alias]
	if !ok || w.Typ == nil {
		return nil, false, nil
	}
	switch w.Kind {
	case values.LegKindNested:
		if w.Offset < 0 || w.Offset >= len(rc.Fields) {
			return nil, true, internalError("leg %s's nested slot %d lies outside a %d-column row", alias, w.Offset, len(rc.Fields))
		}
		v, err := qv.resolve(rc.Fields[w.Offset].Value, selScope)
		return v, true, err
	case values.LegKindFlatRun:
		width := len(w.Typ.Fields)
		if w.Offset < 0 || width == 0 || w.Offset+width > len(rc.Fields) {
			return nil, true, internalError("leg %s's window [%d, +%d) lies outside a %d-column row", alias, w.Offset, width, len(rc.Fields))
		}
		if width == 1 && isUnnestLeg(alias, sel) {
			// An unnest's element leg: the one column is the element itself,
			// not a record of it.
			v, err := qv.resolve(rc.Fields[w.Offset].Value, selScope)
			return v, true, err
		}
		fields := make([]values.RecordConstructorField, width)
		for i := range fields {
			v, err := qv.resolve(rc.Fields[w.Offset+i].Value, selScope)
			if err != nil {
				return nil, true, err
			}
			fields[i] = values.RecordConstructorField{Name: w.Typ.Fields[i].Name, Value: v}
		}
		return values.NewRawRecordConstructorValue(fields...), true, nil
	default:
		return nil, false, nil
	}
}

// isUnnestLeg reports whether alias names an unnest leg of sel: the deepest
// quantifier of that name, as the translator names a quantifier over a nested
// join after its rightmost leg, ranges over an explode.
func isUnnestLeg(alias values.CorrelationIdentifier, sel *expressions.SelectExpression) bool {
	for _, q := range sel.GetQuantifiers() {
		if q.GetAlias() != alias {
			continue
		}
		producer, err := member(q.GetRangesOver())
		for err == nil {
			inner, passes := passesRowThrough(producer)
			if !passes {
				break
			}
			producer, err = member(inner.GetRangesOver())
		}
		if err != nil {
			return false
		}
		switch p := producer.(type) {
		case *expressions.ExplodeExpression:
			return true
		case *expressions.SelectExpression:
			return isUnnestLeg(alias, p)
		}
		return false
	}
	return false
}

// leg is what a select's quantifier stands for: an explode's collection, or
// the row its producer computes.
func (qv *quantifierValues) leg(q expressions.Quantifier, selectScope *scope) (values.Value, error) {
	producer, err := member(q.GetRangesOver())
	if err != nil {
		return nil, err
	}
	return qv.row(producer, selectScope.child(producer))
}

// row is the row producer computes, resolved: a scan's queried record, an
// explode's marked collection, any other expression's result value.
func (qv *quantifierValues) row(producer expressions.RelationalExpression, sc *scope) (values.Value, error) {
	if explode, ok := producer.(*expressions.ExplodeExpression); ok {
		return qv.unnestedCollection(explode, sc)
	}
	// A sort or a filter computes its input's row, whatever its quantifier is
	// named.
	if inner, passes := passesRowThrough(producer); passes {
		below, err := member(inner.GetRangesOver())
		if err != nil {
			return nil, err
		}
		return qv.row(below, sc.child(below))
	}
	return qv.resolve(producer.GetResultValue(), sc)
}

// unnestedCollection is Java's unnestedCollectionValue: the collection an
// explode unnests, its field path's last accessor marked with a number of its
// own. A collection that is not a field access stands for itself, unmarked.
func (qv *quantifierValues) unnestedCollection(explode *expressions.ExplodeExpression, sc *scope) (values.Value, error) {
	collection, err := qv.resolve(explode.GetCollectionValue(), sc)
	if err != nil {
		return nil, err
	}
	path, ok := collection.(*pathColumn)
	if !ok {
		return collection, nil
	}
	marker, seen := qv.explodes[explode]
	if !seen {
		marker = len(qv.explodes) + 1
		qv.explodes[explode] = marker
	}
	marked := path.clone()
	marked.steps[len(marked.steps)-1].marker = marker
	return marked, nil
}

// access applies the ordinal path ords to base: through a record constructor
// it takes the field, over a resolved path it extends it, over the scanned
// record it starts one.
func (qv *quantifierValues) access(base values.Value, ords []int) (values.Value, error) {
	for len(ords) > 0 {
		rcv, ok := base.(*values.RecordConstructorValue)
		if !ok {
			break
		}
		if ords[0] < 0 || ords[0] >= len(rcv.Fields) {
			return nil, internalError("ordinal %d reads past a %d-column row", ords[0], len(rcv.Fields))
		}
		base, ords = rcv.Fields[ords[0]].Value, ords[1:]
	}
	if len(ords) == 0 {
		return base, nil
	}
	var path *pathColumn
	switch b := base.(type) {
	case *pathColumn:
		path = b.clone()
	default:
		if _, ok := values.AsQuantifiedObjectValue(base); !ok {
			return nil, internalError("a field path %v over %s, which is not a record", ords, base.Name())
		}
		path = &pathColumn{root: base, typ: base.Type()}
	}
	for _, ord := range ords {
		if err := path.step(ord); err != nil {
			return nil, err
		}
	}
	return path, nil
}

// fieldAccessor is one resolved step of a field path the generator renders.
type fieldAccessor struct {
	ordinal int
	name    string
	typ     values.Type
	// marker is the unnest the step was reached through (Java's
	// AnnotatedAccessor), 0 for a plain step.
	marker int
}

// equal is Java's accessor equality, which is ASYMMETRIC: a plain
// ResolvedAccessor compares ordinals only and so equals an annotated one, while
// an AnnotatedAccessor equals only an annotated one with the same marker
// (QuantifierValues.java, AnnotatedAccessor.equals). The receiver is the
// left-hand side of Java's equals.
func (a fieldAccessor) equal(b fieldAccessor) bool {
	if a.ordinal != b.ordinal {
		return false
	}
	return a.marker == 0 || a.marker == b.marker
}

// pathColumn is a resolved field access: the scanned record and one step per
// accessor, a step into an unnested array's element included — Java's
// FieldValue over its FieldPath, whose accessors may be AnnotatedAccessors.
// It is a Value so it can sit where the query put the field (under an
// arithmetic, a CARDINALITY, an aggregate); it is never evaluated.
type pathColumn struct {
	root  values.Value
	steps []fieldAccessor
	typ   values.Type
}

func (p *pathColumn) Children() []values.Value { return []values.Value{p.root} }
func (p *pathColumn) Type() values.Type        { return p.typ }
func (p *pathColumn) Name() string             { return "field" }
func (p *pathColumn) Evaluate(any) (any, error) {
	return nil, internalError("an index definition's resolved field is not evaluated")
}

func (p *pathColumn) clone() *pathColumn {
	return &pathColumn{root: p.root, steps: append([]fieldAccessor(nil), p.steps...), typ: p.typ}
}

// step appends the field at ordinal of the path's current type: a record's
// field, or, over an array, a field of its element (an unnest's element).
func (p *pathColumn) step(ordinal int) error {
	typ := p.typ
	if arr, ok := typ.(*values.ArrayType); ok {
		typ = arr.ElementType
	}
	rt, ok := typ.(*values.RecordType)
	if !ok {
		return internalError("a field step %d into %v, which is not a record", ordinal, p.typ)
	}
	if ordinal < 0 || ordinal >= len(rt.Fields) {
		return internalError("a field step %d past a %d-field record", ordinal, len(rt.Fields))
	}
	f := rt.Fields[ordinal]
	p.steps = append(p.steps, fieldAccessor{ordinal: ordinal, name: f.Name, typ: f.FieldType})
	p.typ = f.FieldType
	return nil
}

// displayName is the last step's name, as Java's getLastFieldName.
func (p *pathColumn) displayName() string {
	if len(p.steps) == 0 {
		return ""
	}
	return p.steps[len(p.steps)-1].name
}

// equalValues is Java's Value.equals over two resolved values: paths compare
// their roots and their steps accessor by accessor with a's step as the
// receiver; every other value compares its kind and its children the same way,
// and a leaf structurally.
func (qv *quantifierValues) equalValues(a, b values.Value) bool {
	if a == nil || b == nil {
		return a == b
	}
	ap, aPath := a.(*pathColumn)
	bp, bPath := b.(*pathColumn)
	if aPath || bPath {
		if !aPath || !bPath || len(ap.steps) != len(bp.steps) {
			return false
		}
		for i := range ap.steps {
			if !ap.steps[i].equal(bp.steps[i]) {
				return false
			}
		}
		return values.ValuesStructurallyEqual(ap.root, bp.root)
	}
	ac, bc := a.Children(), b.Children()
	if len(ac) == 0 || len(bc) == 0 {
		return values.ValuesStructurallyEqual(a, b)
	}
	if fmt.Sprintf("%T", a) != fmt.Sprintf("%T", b) || len(ac) != len(bc) || a.Name() != b.Name() {
		return false
	}
	for i := range ac {
		if !qv.equalValues(ac[i], bc[i]) {
			return false
		}
	}
	// The children agree; the operator is what is left to compare.
	return values.ValuesStructurallyEqual(a, withChildrenOf(b, ac))
}

// withChildrenOf is b rebuilt over children, so that a structural comparison
// with a value whose children are those compares only the operators.
func withChildrenOf(b values.Value, children []values.Value) values.Value {
	rebuilt, err := values.WithChildrenChecked(b, children)
	if err != nil {
		return b
	}
	return rebuilt
}

func internalError(format string, args ...any) error {
	return api.NewErrorf(api.ErrCodeInternalError, "index generator: "+format, args...)
}
