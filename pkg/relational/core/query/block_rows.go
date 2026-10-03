package query

import (
	"fmt"
	"slices"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// foldBlock builds the block select for proj. A WHERE filter under the list
// contributes its quantifier and predicates; a join select under that (or
// directly under the list) contributes its quantifiers and predicates when the
// list and WHERE read its sources. A WHERE never merges into an outer join's ON
// predicates: it stays above that join's select. A block that ranges over its
// input reads the sources below it through the input's row (readThroughRow).
// scopeBodies holds the derived-table and CTE bodies of a translation. SQL
// scoping hides a body's sources from the blocks around it, so a read above a
// body never reads a source inside it: a same-named one is an outer row.
type scopeBodies map[expressions.RelationalExpression]bool

func (b scopeBodies) hides(ref *expressions.Reference) bool {
	if ref == nil || len(b) == 0 {
		return false
	}
	for _, member := range ref.AllMembers() {
		if b[member] {
			return true
		}
	}
	return false
}

func foldBlock(proj *expressions.LogicalProjectionExpression, bodies scopeBodies) (*expressions.SelectExpression, error) {
	qs := proj.GetQuantifiers()
	result := proj.GetResultValue()
	if len(qs) != 1 || qs[0].GetRangesOver() == nil {
		return expressions.NewSelectExpression(result, qs, nil)
	}
	from := qs[0]
	var where []predicates.QueryPredicate
	if filter, isFilter := from.GetRangesOver().Get().(*expressions.LogicalFilterExpression); isFilter && filter != nil {
		inner := filter.GetInner()
		if inner.GetAlias() != from.GetAlias() {
			return expressions.NewSelectExpression(result, qs, nil)
		}
		from, where = inner, filter.GetPredicates()
	}
	if box, isSelect := from.GetRangesOver().Get().(*expressions.SelectExpression); isSelect && box != nil {
		// Reads of sources nested in the box's own quantifiers (a box inside the
		// box) become reads of those quantifiers' rows first.
		r, w, ok, err := readBlockThrough(result, where, box.GetQuantifiers(), bodies)
		if err != nil {
			return nil, err
		}
		if ok && readsBoxSources(box, from, r, w, bodies) && (len(w) == 0 || box.ChildrenAsSet()) {
			return box.WithTranslatedValues(r, box.GetQuantifiers(), append(append([]predicates.QueryPredicate(nil), box.GetPredicates()...), w...))
		}
	}
	r, w, ok, err := readBlockThrough(result, where, []expressions.Quantifier{from}, bodies)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("the block reads a column its input row %s does not carry", from.GetAlias())
	}
	if buried, err := readsBelow(r, w, from, bodies); err != nil || buried {
		return nil, fmt.Errorf("the block reads a source below its input row %s", from.GetAlias())
	}
	if aliasedBelow(from) {
		// Go names the input after its rightmost source; once the block reads
		// the input's row, one alias would name both that row and the source.
		fresh := expressions.ForEachQuantifier(from.GetRangesOver())
		if r, w, err = rebaseBlock(r, w, from.GetAlias(), fresh.GetAlias()); err != nil {
			return nil, err
		}
		from = fresh
	}
	return expressions.NewSelectExpression(r, []expressions.Quantifier{from}, w)
}

// aliasedBelow reports whether q's alias also names a quantifier below q.
func aliasedBelow(q expressions.Quantifier) bool {
	seen := map[*expressions.Reference]bool{}
	var below func(ref *expressions.Reference) bool
	below = func(ref *expressions.Reference) bool {
		if ref == nil || seen[ref] {
			return false
		}
		seen[ref] = true
		for _, inner := range ref.Get().GetQuantifiers() {
			if inner.GetAlias() == q.GetAlias() || below(inner.GetRangesOver()) {
				return true
			}
		}
		return false
	}
	return below(q.GetRangesOver())
}

// rowsBelow maps every quantifier alias in the graphs qs range over to the
// rows flowed under it.
func rowsBelow(qs []expressions.Quantifier, bodies scopeBodies) map[values.CorrelationIdentifier][]values.Type {
	below := map[values.CorrelationIdentifier][]values.Type{}
	seen := map[*expressions.Reference]bool{}
	var walk func(ref *expressions.Reference)
	walk = func(ref *expressions.Reference) {
		if ref == nil || seen[ref] || bodies.hides(ref) {
			return
		}
		seen[ref] = true
		for _, q := range ref.Get().GetQuantifiers() {
			if row, err := q.RequireFlowedObjectValue(); err == nil {
				below[q.GetAlias()] = append(below[q.GetAlias()], row.FlowedType())
			}
			walk(q.GetRangesOver())
		}
	}
	for _, q := range qs {
		walk(q.GetRangesOver())
	}
	return below
}

// readsBelow reports whether result or where reads a source below from other
// than from's own row (which may share a source's name).
func readsBelow(result values.Value, where []predicates.QueryPredicate, from expressions.Quantifier, bodies scopeBodies) (bool, error) {
	own, err := from.RequireFlowedObjectValue()
	if err != nil {
		return false, err
	}
	below := rowsBelow([]expressions.Quantifier{from}, bodies)
	found := false
	visit := func(n values.Value) bool {
		qov, isQOV := values.AsQuantifiedObjectValue(n)
		if !isQOV {
			return true
		}
		if _, buried := sourceRowOf(qov, below[qov.Correlation()]); buried {
			_, ownRow := sourceRowOf(qov, []values.Type{own.FlowedType()})
			found = !(qov.Correlation() == from.GetAlias() && ownRow)
		}
		return !found
	}
	values.WalkValue(result, visit)
	for _, p := range where {
		predicates.TransformEmbeddedValues(p, func(v values.Value) values.Value {
			values.WalkValue(v, visit)
			return v
		})
	}
	return found, nil
}

func rebaseBlock(
	result values.Value,
	where []predicates.QueryPredicate,
	source, target values.CorrelationIdentifier,
) (values.Value, []predicates.QueryPredicate, error) {
	aliases, err := values.NewAliasMap([]values.AliasPair{{Source: source, Target: target}})
	if err != nil {
		return nil, nil, err
	}
	if result, err = values.RebaseValueChecked(result, aliases); err != nil {
		return nil, nil, err
	}
	rebased := make([]predicates.QueryPredicate, len(where))
	for i, p := range where {
		if rebased[i], err = predicates.RebasePredicateChecked(p, aliases); err != nil {
			return nil, nil, err
		}
	}
	return result, rebased, nil
}

// readBlockThrough reads the block's result and WHERE through each of qs.
func readBlockThrough(
	result values.Value,
	where []predicates.QueryPredicate,
	qs []expressions.Quantifier,
	bodies scopeBodies,
) (values.Value, []predicates.QueryPredicate, bool, error) {
	for _, q := range qs {
		var ok bool
		var err error
		if result, ok, err = readThroughRow(result, q, bodies); err != nil || !ok {
			return nil, nil, false, err
		}
		rewritten := make([]predicates.QueryPredicate, len(where))
		for i, p := range where {
			var failed error
			allRead := true
			rewritten[i] = predicates.TransformEmbeddedValues(p, func(v values.Value) values.Value {
				read, ok, err := readThroughRow(v, q, bodies)
				if err != nil {
					failed = err
				}
				allRead = allRead && ok
				if err != nil || !ok {
					return v
				}
				return read
			})
			if failed != nil || !allRead {
				return nil, nil, false, failed
			}
		}
		where = rewritten
	}
	return result, where, true, nil
}

// readsBoxSources reports whether the block's result and WHERE read the box's
// FROM sources, each with a type the box itself reads that source with. A read
// of from's own row (the box's result) is not a source read; such a block stays
// a select over from for SelectMergeRule.
func readsBoxSources(box *expressions.SelectExpression, from expressions.Quantifier, result values.Value, where []predicates.QueryPredicate, bodies scopeBodies) bool {
	sources, ok := sourceTypes(box)
	if !ok {
		return false
	}
	buried := rowsBelow(box.GetQuantifiers(), bodies)
	readsSource := false
	foldable := true
	visit := func(n values.Value) bool {
		qov, isQOV := values.AsQuantifiedObjectValue(n)
		if !isQOV {
			return true
		}
		types, isSource := sources[qov.Correlation()]
		switch {
		case isSource && readsAs(qov, types):
			readsSource = true
		case isSource || qov.Correlation() == from.GetAlias() || len(buried[qov.Correlation()]) > 0:
			foldable = false
		}
		return foldable
	}
	values.WalkValue(result, visit)
	for _, p := range where {
		predicates.TransformEmbeddedValues(p, func(v values.Value) values.Value {
			values.WalkValue(v, visit)
			return v
		})
	}
	return foldable && readsSource
}

// sourceTypes maps each quantifier of sel to the types sel reads it with: its
// flowed row, and the null-supplied row of an outer join's result.
func sourceTypes(sel *expressions.SelectExpression) (map[values.CorrelationIdentifier][]values.Type, bool) {
	sources := make(map[values.CorrelationIdentifier][]values.Type, len(sel.GetQuantifiers()))
	for _, q := range sel.GetQuantifiers() {
		qov, err := q.RequireFlowedObjectValue()
		if err != nil {
			return nil, false
		}
		sources[q.GetAlias()] = []values.Type{qov.FlowedType()}
	}
	values.WalkValue(sel.GetResultValue(), func(n values.Value) bool {
		if qov, isQOV := values.AsQuantifiedObjectValue(n); isQOV {
			if types, isSource := sources[qov.Correlation()]; isSource {
				sources[qov.Correlation()] = append(types, qov.FlowedType())
			}
		}
		return true
	})
	return sources, true
}

// readRowOf is inner's row as its select reads it: the null-supplied row when
// the select is an outer join that null-extends inner, else the flowed row.
func readRowOf(inner expressions.Quantifier, types []values.Type) (values.QuantifiedObjectValue, error) {
	row, err := inner.RequireFlowedObjectValue()
	if err != nil || len(types) < 2 {
		return row, err
	}
	for _, typ := range types[1:] {
		if !values.FlowedTypeEquals(row, typ) {
			return values.NewQuantifiedObjectValue(inner.GetAlias(), typ)
		}
	}
	return row, nil
}

func readsAs(qov values.QuantifiedObjectValue, types []values.Type) bool {
	return slices.ContainsFunc(types, func(typ values.Type) bool { return values.FlowedTypeEquals(qov, typ) })
}

// sourceRowOf is the type among types that qov reads: the type itself, or the
// one an outer join above null-supplies (the same row, made nullable).
func sourceRowOf(qov values.QuantifiedObjectValue, types []values.Type) (values.Type, bool) {
	for _, typ := range types {
		if values.FlowedTypeEquals(qov, typ) {
			return typ, true
		}
	}
	read := qov.FlowedType()
	if read == nil || !read.IsNullable() {
		return nil, false
	}
	for _, typ := range types {
		if values.WithNullability(typ, true).Equals(read) {
			return typ, true
		}
	}
	return nil, false
}

// readThroughRow re-expresses v's reads of the sources below q as reads of q's
// row, as Java's rewireQov reads a FROM operator through its quantifier: each
// maximal subtree that a select below q publishes as a result column becomes
// that column of q's row. Row-preserving operators (sort, filter, limit,
// distinct, unique) pass the row through. ok=false when such a read is not a column of
// the row.
func readThroughRow(v values.Value, q expressions.Quantifier, bodies scopeBodies) (values.Value, bool, error) {
	row, err := q.RequireFlowedObjectValue()
	if err != nil {
		return nil, false, err
	}
	return readThroughRowShadowed(v, q, row, nil, bodies)
}

// readThroughRowShadowed is readThroughRow below binders that shadow sources:
// a read matching a shadowing binder's alias and row reads that binder, not a
// same-named source further down. row is q's row as the reader reads it (the
// null-supplied row below an outer join).
func readThroughRowShadowed(
	v values.Value,
	q expressions.Quantifier,
	row values.QuantifiedObjectValue,
	shadowed map[values.CorrelationIdentifier][]values.Type,
	bodies scopeBodies,
) (values.Value, bool, error) {
	ref := q.GetRangesOver()
	if v == nil || ref == nil || q.Kind() == expressions.QuantifierExistential || bodies.hides(ref) {
		return v, true, nil
	}
	switch e := ref.Get().(type) {
	case *expressions.SelectExpression:
		return readThroughSelect(v, q, row, e, shadowed, bodies)
	case *expressions.LogicalSortExpression, *expressions.LogicalFilterExpression,
		*expressions.LogicalLimitExpression, *expressions.LogicalDistinctExpression,
		*expressions.LogicalUniqueExpression:
		inner := e.GetQuantifiers()
		if len(inner) != 1 {
			return v, true, nil
		}
		if rv, isQOV := values.AsQuantifiedObjectValue(e.GetResultValue()); !isQOV || rv.Correlation() != inner[0].GetAlias() {
			return v, true, nil
		}
		innerRow, err := inner[0].RequireFlowedObjectValue()
		if err != nil {
			return nil, false, err
		}
		read, ok, err := readThroughRowShadowed(v, inner[0], innerRow, shadowed, bodies)
		if err != nil || !ok || inner[0].GetAlias() == q.GetAlias() {
			return read, ok, err
		}
		rebased, err := rebaseRowReads(read, innerRow, q.GetAlias())
		return rebased, err == nil, err
	case *expressions.GroupByExpression:
		// A grouped row publishes its grouping columns: a read of the input
		// is a read of the column grouping it, as Java pulls an ORDER BY up
		// through the aggregate's result value.
		inner := e.GetInner()
		innerRow, err := inner.RequireFlowedObjectValue()
		if err != nil {
			return nil, false, err
		}
		read, ok, err := readThroughRowShadowed(v, inner, innerRow, shadowed, bodies)
		if err != nil || !ok {
			return read, ok, err
		}
		// Go names an input after its rightmost source, so the input's alias
		// can also name a row above it; the type tells them apart.
		readsInput := func(n values.Value) bool {
			found := false
			values.WalkValue(n, func(c values.Value) bool {
				if qov, isQOV := values.AsQuantifiedObjectValue(c); isQOV && qov.Correlation() == inner.GetAlias() {
					_, found = sourceRowOf(qov, []values.Type{innerRow.FlowedType()})
				}
				return !found
			})
			return found
		}
		if !readsInput(read) {
			return read, true, nil
		}
		columns, isRecord := e.GetResultValue().(*values.RecordConstructorValue)
		if !isRecord {
			return read, false, nil
		}
		// The grouped row can share the input's name and type, so whether a
		// read is settled is decided on the input's reads, not re-checked.
		return readGroupedColumns(read, readsInput, func(n values.Value) (values.Value, error) {
			return readSourceColumn(n, columns, row)
		})
	}
	return v, true, nil
}

// readGroupedColumns replaces each read of an aggregate's input in v with the
// grouping column carrying it; ok is false when a read of the input names no
// grouping column.
func readGroupedColumns(
	v values.Value,
	readsInput func(values.Value) bool,
	column func(values.Value) (values.Value, error),
) (values.Value, bool, error) {
	if !readsInput(v) {
		return v, true, nil
	}
	c, err := column(v)
	if err != nil || c != nil {
		return c, err == nil, err
	}
	children := v.Children()
	if len(children) == 0 {
		return v, false, nil
	}
	rebuilt := make([]values.Value, len(children))
	changed := false
	for i, child := range children {
		read, ok, err := readGroupedColumns(child, readsInput, column)
		if err != nil || !ok {
			return read, ok, err
		}
		rebuilt[i] = read
		changed = changed || read != child
	}
	if !changed {
		return v, true, nil
	}
	read, err := values.WithChildrenChecked(v, rebuilt)
	return read, err == nil, err
}

// rebaseRowReads renames v's reads of source's row to target. A read of the
// same alias with another row type is a read of a different binder that
// shares the name (Go names an input after its rightmost source), so it stays.
func rebaseRowReads(v values.Value, source values.QuantifiedObjectValue, target values.CorrelationIdentifier) (values.Value, error) {
	m := values.NewTranslationMapBuilder().When(source.Correlation()).Then(
		func(_ values.CorrelationIdentifier, leaf values.Value) values.Value {
			qov, isQOV := values.AsQuantifiedObjectValue(leaf)
			if !isQOV {
				return leaf
			}
			if _, reads := sourceRowOf(qov, []values.Type{source.FlowedType()}); !reads {
				return leaf
			}
			read, err := values.NewQuantifiedObjectValue(target, qov.FlowedType())
			if err != nil {
				return nil
			}
			return read
		},
	).Build()
	return values.TranslateCorrelationsChecked(v, m)
}

func readThroughSelect(
	v values.Value,
	q expressions.Quantifier,
	row values.QuantifiedObjectValue,
	sel *expressions.SelectExpression,
	shadowed map[values.CorrelationIdentifier][]values.Type,
	bodies scopeBodies,
) (values.Value, bool, error) {
	sources, ok := sourceTypes(sel)
	if !ok {
		return v, false, nil
	}
	for _, inner := range sel.GetQuantifiers() {
		// The select's other binders shadow same-named sources below inner.
		below := make(map[values.CorrelationIdentifier][]values.Type, len(shadowed)+len(sources))
		for alias, types := range shadowed {
			below[alias] = append(below[alias], types...)
		}
		for _, sibling := range sel.GetQuantifiers() {
			if sibling.GetAlias() != inner.GetAlias() {
				below[sibling.GetAlias()] = append(below[sibling.GetAlias()], sources[sibling.GetAlias()]...)
			}
		}
		innerRow, err := readRowOf(inner, sources[inner.GetAlias()])
		if err != nil {
			return nil, false, err
		}
		var ok bool
		if v, ok, err = readThroughRowShadowed(v, inner, innerRow, below, bodies); err != nil || !ok {
			return v, ok, err
		}
	}
	// A select passing one source through publishes that source's row as its
	// own; a read of it under q's alias is already a read of q's row.
	readsSource := func(n values.Value) bool {
		found := false
		values.WalkValue(n, func(c values.Value) bool {
			if qov, isQOV := values.AsQuantifiedObjectValue(c); isQOV {
				types, isSource := sources[qov.Correlation()]
				_, readsSourceRow := sourceRowOf(qov, types)
				ownRow := qov.Correlation() == q.GetAlias() && values.FlowedTypeEquals(qov, row.FlowedType())
				if isSource && readsSourceRow && !ownRow && !readsAs(qov, shadowed[qov.Correlation()]) {
					found = true
				}
			}
			return !found
		})
		return found
	}
	if !readsSource(v) {
		return v, true, nil
	}
	// A select passing one of its sources through publishes that source's row:
	// its reads are reads of q's row under q's alias.
	if passed, isQOV := values.AsQuantifiedObjectValue(sel.GetResultValue()); isQOV {
		if passed.Correlation() == q.GetAlias() {
			return v, !readsSource(v), nil
		}
		rebased, err := rebaseRowReads(v, passed, q.GetAlias())
		if err != nil {
			return nil, false, err
		}
		return rebased, !readsSource(rebased), nil
	}
	columns, isRecord := sel.GetResultValue().(*values.RecordConstructorValue)
	if !isRecord {
		return v, false, nil
	}
	var failed error
	read := values.Replace(v, func(n values.Value) values.Value {
		if failed != nil || !readsSource(n) {
			return n
		}
		column, err := readSourceColumn(n, columns, row)
		if err != nil {
			failed = err
			return n
		}
		if column != nil {
			return column
		}
		return n
	})
	if failed != nil {
		return nil, false, failed
	}
	return read, !readsSource(read), nil
}

// readSourceColumn is n read as a column of row. The column must read what n
// reads: when n reads a source null-supplied by an outer join above this
// select, row is the padded row and its column carries n's nullable type.
func readSourceColumn(
	n values.Value,
	columns *values.RecordConstructorValue,
	row values.QuantifiedObjectValue,
) (values.Value, error) {
	column, err := readColumn(n, columns, row)
	if err != nil || column == nil {
		return column, err
	}
	if !column.Type().Equals(n.Type()) {
		return nil, nil
	}
	return column, nil
}

// readColumn is n as a column of the row columns publishes: the column n is,
// or the column a field path of n starts with. The read is pinned to row, as
// its ordinal addresses that row and no source below it.
func readColumn(n values.Value, columns *values.RecordConstructorValue, row values.QuantifiedObjectValue) (values.Value, error) {
	if i := columnOf(n, columns); i >= 0 {
		return values.ResolveOrdinalSeedField(row, i)
	}
	field, isField := values.AsFieldValue(n)
	if !isField || field.Path() == nil {
		return nil, nil
	}
	ordinals := field.Path().Ordinals()
	for k := len(ordinals) - 1; k >= 1; k-- {
		prefix, err := values.ResolveFieldOrdinals(field.ChildValue(), ordinals[:k])
		if err != nil {
			return nil, nil
		}
		if i := columnOf(prefix, columns); i >= 0 {
			column, err := values.ResolveOrdinalSeedField(row, i)
			if err != nil {
				return nil, err
			}
			return values.ResolveFieldOrdinals(column, ordinals[k:])
		}
	}
	return nil, nil
}

func columnOf(n values.Value, columns *values.RecordConstructorValue) int {
	for i, f := range columns.Fields {
		if f.Value != nil && (values.SemanticEqualsUnderAliasMap(n, f.Value, values.EmptyAliasMap()) || sameFieldRead(n, f.Value)) {
			return i
		}
	}
	return -1
}

// sameFieldRead reports whether read and column read the same slot of the same
// source: one correlation, one ordinal path, and the read's row either the
// column's row or that row null-supplied by an outer join above it. A read
// through a block row is frontier-pinned where the column is not.
func sameFieldRead(read, column values.Value) bool {
	fr, okR := values.AsFieldValue(read)
	fc, okC := values.AsFieldValue(column)
	if !okR || !okC || fr.Path() == nil || fc.Path() == nil {
		return false
	}
	qr, okR := values.AsQuantifiedObjectValue(fr.ChildValue())
	qc, okC := values.AsQuantifiedObjectValue(fc.ChildValue())
	if !okR || !okC || qr.Correlation() != qc.Correlation() {
		return false
	}
	_, sameRow := sourceRowOf(qr, []values.Type{qc.FlowedType()})
	return sameRow && slices.Equal(fr.Path().Ordinals(), fc.Path().Ordinals())
}
