// Portions derived from FoundationDB Record Layer (IndexSpec.java,
// IndexableAggregateValue.java, StreamableAggregateValue.java,
// IndexPredicates.java, and others),
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2026 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package ddl

import (
	"fmt"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
)

// indexSpec is Java's IndexSpec (IndexSpec.java): what the graph of an
// index-defining query is making, collected bottom-up.
type indexSpec struct {
	scanCount  int
	recordType string // "" until a scan contributes it
	// predicate holds the conjuncts of the innermost filtering expression, raw:
	// Java normalizes them without resolving (IndexPredicates.normalize).
	predicate []predicates.QueryPredicate
	groupBy   *groupBySpec
	orderBy   *orderBySpec
	// projection is the index's columns, resolved down to the base record.
	projection []values.Value
}

type groupBySpec struct {
	expr  *expressions.GroupByExpression
	scope *scope
}

// orderBySpec is Java's IndexSpec.OrderBy: the ORDER BY columns resolved down
// to the base record, and the ordering function of each keyed by identity.
type orderBySpec struct {
	values    []values.Value
	functions map[values.Value]string
}

func unsupported(format string, args ...any) error {
	return api.NewErrorf(api.ErrCodeUnsupportedOperation, format, args...)
}

const typeFilterMessage = "Unsupported query, expected to find exactly one type filter operator"

// merge combines what the children of one expression found (IndexSpec.merge,
// :255-270): the record type first, so a join trips the type-filter message
// before the join assertion, then one of each predicate, group by and sort.
func merge(children []*indexSpec) (*indexSpec, error) {
	merged := &indexSpec{}
	for _, c := range children {
		if merged.recordType != "" && c.recordType != "" {
			return nil, unsupported(typeFilterMessage)
		}
		if merged.scanCount != 0 && c.scanCount != 0 {
			return nil, unsupported("Unsupported index definition, join indexes are not supported")
		}
		if merged.predicate != nil && c.predicate != nil {
			return nil, unsupported("Unsupported index definition, more than one predicate found")
		}
		if merged.groupBy != nil && c.groupBy != nil {
			return nil, unsupported("Unsupported index definition, more than one group by expression found")
		}
		if merged.orderBy != nil && c.orderBy != nil {
			return nil, unsupported("Unsupported index definition, more than one sort expression found")
		}
		merged.scanCount += c.scanCount
		if c.recordType != "" {
			merged.recordType = c.recordType
		}
		if c.predicate != nil {
			merged.predicate = c.predicate
		}
		if c.groupBy != nil {
			merged.groupBy = c.groupBy
		}
		if c.orderBy != nil {
			merged.orderBy = c.orderBy
		}
	}
	return merged, nil
}

// specCollector is IndexSpec's Visitor over Go's expression classes; the
// design's 3.3 table maps each Java arm to its Go expression.
type specCollector struct {
	qv *quantifierValues
}

func (c *specCollector) visit(e expressions.RelationalExpression, sc *scope) (*indexSpec, error) {
	children := make([]*indexSpec, 0, len(e.GetQuantifiers()))
	for _, q := range e.GetQuantifiers() {
		if q.Kind() == expressions.QuantifierExistential {
			// The target plans an index query with literal processing disabled
			// (DdlVisitor.java:266), and then ExpressionVisitor.visitExists-
			// ExpressionAtom does not add the EXISTS subquery's existential
			// operator to the plan fragment (ExpressionVisitor.java:661-665):
			// its graph holds the EXISTS predicate and no quantifier for the
			// subquery. So nothing under an existential is collected, and the
			// predicate is what the definition is refused for (measured:
			// top_exists, exists_uncorrelated, exists_correlated_literal).
			continue
		}
		producer, err := member(q.GetRangesOver())
		if err != nil {
			return nil, err
		}
		child, err := c.visit(producer, sc.child(producer))
		if err != nil {
			return nil, err
		}
		children = append(children, child)
	}
	switch x := e.(type) {
	case *expressions.ExplodeExpression:
		// An explode yields elements rather than records: merge only.
		return merge(children)
	case *expressions.FullUnorderedScanExpression:
		// Java's table access is TypeFilter(Scan); Go's translator emits the
		// scan alone, over the one table, so it contributes both the scan and
		// the record type (the design's 3.3 implementation notes, item 1).
		spec, err := c.evaluate(e, children)
		if err != nil {
			return nil, err
		}
		spec.scanCount++
		return spec, withRecordType(spec, x.GetRecordTypes())
	case *expressions.LogicalTypeFilterExpression:
		spec, err := c.evaluate(e, children)
		if err != nil {
			return nil, err
		}
		return spec, withRecordType(spec, x.GetRecordTypes())
	case *expressions.GroupByExpression:
		if len(x.GetAggregates()) > 1 {
			return nil, unsupported("Unsupported index definition, found group by expression with more than one aggregation")
		}
		spec, err := c.evaluate(e, children)
		if err != nil {
			return nil, err
		}
		if spec.groupBy != nil {
			return nil, unsupported("Unsupported index definition, multiple group by expressions found")
		}
		spec.groupBy = &groupBySpec{expr: x, scope: sc}
		return spec, nil
	case *expressions.SelectExpression:
		return c.filtering(e, children, x.GetPredicates())
	case *expressions.LogicalFilterExpression:
		// Java's select owns both halves; Go's translator splits it, the
		// filter holding the predicates and the projection above it the
		// result. The result check is the projection's, so a filter over an
		// explode (a row of elements) is not refused as a non-record result.
		merged, err := merge(children)
		if err != nil {
			return nil, err
		}
		return c.owningPredicates(merged, x.GetPredicates())
	case *expressions.LogicalSortExpression:
		spec, err := c.evaluate(e, children)
		if err != nil {
			return nil, err
		}
		if spec.orderBy != nil {
			return nil, unsupported("Unsupported index definition, more than one sort expression found")
		}
		if spec.orderBy, err = c.orderByOf(x, sc); err != nil {
			return nil, err
		}
		return spec, nil
	default:
		return c.evaluate(e, children)
	}
}

func withRecordType(spec *indexSpec, types []string) error {
	if len(types) != 1 {
		found := "nothing"
		if len(types) > 0 {
			found = strings.Join(types, ",")
		}
		return unsupported("Unsupported query, expected to find exactly one record type in type filter operator, however found %s", found)
	}
	if spec.recordType != "" {
		return unsupported(typeFilterMessage)
	}
	spec.recordType = types[0]
	return nil
}

// evaluate is evaluateAtExpression: the result check every expression but an
// explode passes, then the merge.
func (c *specCollector) evaluate(e expressions.RelationalExpression, children []*indexSpec) (*indexSpec, error) {
	if err := checkResultValue(e); err != nil {
		return nil, err
	}
	return merge(children)
}

// filtering is visitSelectExpression's predicate half (:387-402): the
// innermost filtering expression owns the predicate; one above a group by or
// above an owner must filter nothing.
func (c *specCollector) filtering(e expressions.RelationalExpression, children []*indexSpec, preds []predicates.QueryPredicate) (*indexSpec, error) {
	spec, err := c.evaluate(e, children)
	if err != nil {
		return nil, err
	}
	return c.owningPredicates(spec, preds)
}

func (c *specCollector) owningPredicates(spec *indexSpec, preds []predicates.QueryPredicate) (*indexSpec, error) {
	if spec.groupBy != nil || spec.predicate != nil {
		if len(preds) > 0 {
			if spec.groupBy != nil {
				return nil, unsupported("Unsupported index definition, found predicate in select-having")
			}
			return nil, unsupported("Unsupported index definition, found predicate in inner-select")
		}
		return spec, nil
	}
	if len(preds) > 0 {
		spec.predicate = append([]predicates.QueryPredicate(nil), preds...)
	}
	return spec, nil
}

// checkResultValue (:418-442): an expression's result is a record, every field
// of it one of Java's six value classes (the design's 3.3 value table).
func checkResultValue(e expressions.RelationalExpression) error {
	rv := e.GetResultValue()
	typ := rv.Type()
	if _, isRecord := typ.(*values.RecordType); !isRecord {
		return unsupported("Unsupported index definition, operator %s returns a non-record value", expressionClass(e))
	}
	rcv, ok := rv.(*values.RecordConstructorValue)
	if !ok {
		// Any other record-valued result deconstructs into field accesses.
		return nil
	}
	for _, f := range rcv.Fields {
		if !isSupportedResultValue(f.Value) {
			return unsupported("Unsupported index definition, not all fields can be mapped to key expression in %s", expressionClass(e))
		}
	}
	return nil
}

func isSupportedResultValue(v values.Value) bool {
	if _, ok := values.AsFieldValue(v); ok {
		return true
	}
	if _, ok := values.AsQuantifiedObjectValue(v); ok {
		return true
	}
	switch x := v.(type) {
	case *values.AggregateValue, *values.IndexOnlyAggregateValue, *values.ArithmeticValue,
		*values.ConstantValue, *values.CardinalityValue:
		return true
	case *values.DerivedValue:
		// A group by's typed aggregate carrier.
		if len(x.ChildrenList) == 1 {
			return isSupportedResultValue(x.ChildrenList[0])
		}
	}
	return false
}

// expressionClass names a Go expression's class in a message, as Java names
// its own (getClass().getSimpleName()).
func expressionClass(e expressions.RelationalExpression) string {
	return strings.TrimPrefix(fmt.Sprintf("%T", e), "*expressions.")
}

// orderByOf (:… orderByOf) flattens the ordering into one column per key
// component, resolved down to the base record; a record-valued part
// contributes each of its fields, all with that part's direction.
func (c *specCollector) orderByOf(sort *expressions.LogicalSortExpression, sc *scope) (*orderBySpec, error) {
	out := &orderBySpec{functions: map[values.Value]string{}}
	for _, key := range sort.GetSortKeys() {
		fn := orderingFunctionOf(key)
		part, err := c.qv.resolve(key.Value, sc)
		if err != nil {
			return nil, err
		}
		columns := []values.Value{part}
		if _, isRecord := part.Type().(*values.RecordType); isRecord {
			if columns, err = c.qv.deconstruct(part); err != nil {
				return nil, err
			}
		}
		for _, col := range columns {
			out.values = append(out.values, col)
			if fn != "" {
				out.functions[col] = fn
			}
		}
	}
	return out, nil
}

// orderingFunctionOf is the ordering function a column is wrapped in for its
// sort order, "" for plain ascending (nulls first): Java's
// orderingFunctionOf over OrderByExpression.toSortOrder, with SQL's defaults
// (ASC nulls first, DESC nulls last) applied where NULLS is not written.
func orderingFunctionOf(k expressions.SortKey) string {
	nullsFirst := !k.Reverse
	if k.NullsFirst != nil {
		nullsFirst = *k.NullsFirst
	}
	switch {
	case !k.Reverse && nullsFirst:
		return ""
	case !k.Reverse:
		return "order_asc_nulls_last"
	case nullsFirst:
		return "order_desc_nulls_first"
	default:
		return "order_desc_nulls_last"
	}
}

// deconstruct is Values.deconstructRecord over a resolved record value: a
// record constructor's fields, or one field access per field of a
// record-typed value.
func (qv *quantifierValues) deconstruct(v values.Value) ([]values.Value, error) {
	if rcv, ok := v.(*values.RecordConstructorValue); ok {
		out := make([]values.Value, len(rcv.Fields))
		for i, f := range rcv.Fields {
			out[i] = f.Value
		}
		return out, nil
	}
	rt, ok := v.Type().(*values.RecordType)
	if !ok {
		return []values.Value{v}, nil
	}
	out := make([]values.Value, len(rt.Fields))
	for i := range rt.Fields {
		col, err := qv.access(v, []int{i})
		if err != nil {
			return nil, err
		}
		out[i] = col
	}
	return out, nil
}

// isIndexableAggregate is Java's IndexableAggregateValue.
func isIndexableAggregate(v values.Value) bool {
	switch x := v.(type) {
	case *values.IndexOnlyAggregateValue:
		return true
	case *values.AggregateValue:
		return x.Op != values.AggAvg && x.Op != values.AggInvalid
	}
	return false
}

// isStreamableAggregate is Java's StreamableAggregateValue: the aggregates a
// query can compute, AVG among them.
func isStreamableAggregate(v values.Value) bool {
	_, ok := v.(*values.AggregateValue)
	return ok
}

// isTranslatableColumn (:… isTranslatableColumn).
func isTranslatableColumn(v values.Value) bool {
	if _, ok := v.(*pathColumn); ok {
		return true
	}
	switch v.(type) {
	case *values.ArithmeticValue, *values.CardinalityValue:
		return true
	}
	return isIndexableAggregate(v)
}

// projectionOf is ProjectionResolver.resolve (ProjectionResolver.java): the
// root's result columns resolved, and for a group by, checked against the
// grouping (or, for a lone aggregate, followed by it).
func (c *specCollector) projectionOf(result []values.Value, groupBy *groupBySpec) ([]values.Value, error) {
	if groupBy == nil {
		return result, nil
	}
	var grouping []values.Value
	for _, key := range groupBy.expr.GetGroupingKeys() {
		g, err := c.qv.resolve(key, groupBy.scope)
		if err != nil {
			return nil, err
		}
		grouping = append(grouping, g)
	}
	loneAggregate := len(result) == 1 && isIndexableAggregate(result[0])
	if !loneAggregate {
		if len(groupBy.expr.GetGroupingKeys()) == 0 {
			return nil, unsupported("Grouping values absent from aggregate result value")
		}
		next := 0
		for _, v := range result {
			if isIndexableAggregate(v) {
				continue
			}
			if next >= len(grouping) {
				return nil, unsupported("Aggregate result value contains values missing from the grouping expression")
			}
			if !c.qv.equalValues(v, grouping[next]) {
				return nil, unsupported("Aggregate result value does not align with grouping value")
			}
			next++
		}
		if next < len(grouping) {
			return nil, unsupported("Grouping value absent from aggregate result value")
		}
		return result, nil
	}
	return append(append([]values.Value(nil), result...), grouping...), nil
}

// aggregateOf is Projection.aggregate(): the one indexable aggregate, or nil.
func aggregateOf(projection []values.Value) values.Value {
	for _, v := range projection {
		if isIndexableAggregate(v) {
			return v
		}
	}
	return nil
}

// fieldValuesOf is Projection.fieldValues(): everything but the aggregate.
func fieldValuesOf(projection []values.Value) []values.Value {
	out := make([]values.Value, 0, len(projection))
	for _, v := range projection {
		if !isIndexableAggregate(v) {
			out = append(out, v)
		}
	}
	return out
}

// checkValidity (:163-205) rejects every definition the generator cannot turn
// into an index, bar the predicate (checked as it is serialized) and ordering
// by the aggregate (checked once the index type is known).
func (c *specCollector) checkValidity(spec *indexSpec) error {
	if spec.scanCount != 1 {
		return unsupported("Unsupported index definition, no iteration generator found")
	}
	if spec.recordType == "" {
		return unsupported(typeFilterMessage)
	}
	var nonIndexable, untranslatable []string
	for _, v := range spec.projection {
		if isStreamableAggregate(v) && !isIndexableAggregate(v) {
			nonIndexable = append(nonIndexable, renderColumn(v))
		}
	}
	if len(nonIndexable) > 0 {
		return unsupported("Unsupported aggregate index definition containing non-indexable aggregation (%s), consider using a value index on the aggregated column instead.",
			strings.Join(nonIndexable, ","))
	}
	for _, v := range spec.projection {
		if !isTranslatableColumn(v) {
			untranslatable = append(untranslatable, renderColumn(v))
		}
	}
	if len(untranslatable) > 0 {
		return unsupported("Unsupported index definition, cannot map %s to a key expression", strings.Join(untranslatable, ","))
	}
	versions := 0
	for _, v := range spec.projection {
		if isVersionColumn(v) {
			versions++
		}
	}
	if versions > 1 {
		return unsupported("Cannot have index with more than one version column")
	}
	if aggregateOf(spec.projection) == nil {
		for _, v := range spec.orderByValues() {
			if !isTranslatableColumn(v) || isIndexableAggregate(v) {
				return unsupported("Unsupported index definition, order by must be a subset of projection list")
			}
		}
		if len(fieldValuesOf(spec.projection)) > 1 && len(spec.orderByValues()) == 0 {
			return unsupported("Unsupported index definition, value indexes must have an order by clause at the top level")
		}
		return nil
	}
	_, err := c.aggregateOrderIndex(spec)
	return err
}

func (s *indexSpec) orderByValues() []values.Value {
	if s.orderBy == nil {
		return nil
	}
	return s.orderBy.values
}

// aggregateOrderIndex (:… aggregateOrderIndex): where the aggregate appears in
// the ordering, the rest of it the grouping columns in their key order;
// anything else is a covering aggregate index.
func (c *specCollector) aggregateOrderIndex(spec *indexSpec) (int, error) {
	order := spec.orderByValues()
	if len(order) == 0 {
		return -1, nil
	}
	aggregate := aggregateOf(spec.projection)
	fields := fieldValuesOf(spec.projection)
	next, index, inOrder := 0, -1, true
	for i, v := range order {
		switch {
		case c.qv.equalValues(v, aggregate):
			if index >= 0 {
				return 0, unsupported("Unsupported index definition, aggregate can appear only once in ordering clause")
			}
			index = i
		case next < len(fields):
			if !c.qv.equalValues(v, fields[next]) {
				inOrder = false
			}
			next++
		default:
			inOrder = false
		}
		if !inOrder {
			break
		}
	}
	if !inOrder || next < len(fields) {
		return 0, unsupported("Unsupported index definition, attempt to create a covering aggregate index")
	}
	return index, nil
}

// renderColumn renders a column in a message.
func renderColumn(v values.Value) string {
	if p, ok := v.(*pathColumn); ok {
		return p.displayName()
	}
	if av, ok := v.(*values.AggregateValue); ok {
		operand := ""
		if p, ok := av.Operand.(*pathColumn); ok {
			operand = p.displayName()
		}
		return strings.ToLower(av.Op.Symbol()) + "(" + operand + ")"
	}
	return values.ExplainValue(v)
}
