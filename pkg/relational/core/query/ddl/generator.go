// Package ddl holds the materialized-view index generator — the Go port of
// Java's MaterializedViewIndexGenerator (RFC-202). It is the single producer
// of index metadata from `CREATE INDEX … AS SELECT` (and, via the ON-source
// front end, `CREATE INDEX … ON t(cols)`) declarations: it consumes the
// logical plan the ordinary query front end built for the index's SELECT and
// emits the index's root key expression, type, and options.
//
// Java reference (tag 4.12.11.0):
// fdb-relational-core/.../recordlayer/query/ddl/MaterializedViewIndexGenerator.java
package ddl

import (
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	querycore "fdb.dev/pkg/relational/core/query"
	"fdb.dev/pkg/relational/core/query/logical"
)

// GeneratedIndex is the generator's output: everything the metadata builder
// needs to materialise the index. Mirrors the RecordLayerIndex.Builder state
// Java's generator fills in (name/table/type/unique are the caller's; the
// generator owns root expression, index type and options).
type GeneratedIndex struct {
	// TableName is the record type the index is over — Java's
	// getRecordTypeName() (MaterializedViewIndexGenerator.java:791-801).
	TableName string
	// Root is the index's root key expression.
	Root recordlayer.KeyExpression
	// IndexType is a recordlayer.IndexType* constant ("" never happens; the
	// value arm emits VALUE or VERSION, the aggregate arm its own type).
	IndexType string
	// Options carries generator-owned options (PERMUTED_SIZE for permuted
	// min/max). May be nil.
	Options map[string]string
	// Predicate is the sparse-index predicate proto for a WHERE-carrying
	// definition (getTopLevelPredicate → IndexPredicate.fromQueryPredicate,
	// MaterializedViewIndexGenerator.java:169-172). Nil for a full index.
	Predicate *gen.Predicate
}

// Options carries the caller-side switches Java threads into
// MaterializedViewIndexGenerator.from (DdlVisitor.java:214-216).
type Options struct {
	// UseLegacyExtremumEver is Java's useLegacyBasedExtremumEver: `WITH
	// ATTRIBUTES LEGACY_EXTREMUM_EVER` selects the LONG-based extremum
	// maintainer (MIN_EVER_LONG / MAX_EVER_LONG) instead of the tuple-based
	// one (MaterializedViewIndexGenerator.java:449-465).
	UseLegacyExtremumEver bool
}

// Generate is the entry point: op is the logical plan of the index's SELECT
// as produced by the catalog-aware plan visitor (and its post-passes), md the
// metadata the SELECT was planned against.
//
// Ports MaterializedViewIndexGenerator.generate
// (MaterializedViewIndexGenerator.java:149-246): the value/version arm is
// generateValue, the aggregate arm (:204-244) generateAggregate.
func Generate(op logical.LogicalOperator, md *recordlayer.RecordMetaData, opts Options) (*GeneratedIndex, error) {
	// The md == nil text fallback of the plan visitor produces a plan with no
	// resolved values; an index generated from it would come from unresolved
	// names. Assert loudly (RFC-202 D4).
	if md == nil {
		return nil, api.NewError(api.ErrCodeInternalError,
			"index generator invoked without metadata — the catalog-less plan fallback must be unreachable here")
	}
	d, err := decompose(op)
	if err != nil {
		return nil, err
	}
	// Every rendered field name comes from the scanned record type's
	// DESCRIPTOR, by accessor ordinal — the storage name Java's
	// ResolvedAccessor carries. Go's semantic accessors present FOLDED
	// display names (the runtime positional namespace), which corrupt a
	// quoted-DDL column ("col1" → COL1) if rendered into metadata.
	res := storageNames{}
	if rt := md.RecordTypes()[d.scan.Table]; rt != nil {
		res.root = rt.Descriptor
	}
	// The predicate arm runs before the value/aggregate split, as in Java
	// (getTopLevelPredicate at :169-172 precedes collectResultValues at
	// :174): a WHERE makes the index sparse regardless of which arm builds
	// its key expression.
	pred, err := generatePredicate(d, res)
	if err != nil {
		return nil, err
	}
	var gi *GeneratedIndex
	if d.aggregate != nil {
		gi, err = generateAggregate(d, opts, res)
	} else {
		gi, err = generateValue(d, md, res)
	}
	if err != nil {
		return nil, err
	}
	gi.Predicate = pred
	return gi, nil
}

// decomposed is the Go analogue of Java's topologically-sorted expression
// list (MaterializedViewIndexGenerator.java:161-166): the plan's operators by
// role, plus the validity checks of checkValidity (:611-637) that are
// expressible over this plan shape.
type decomposed struct {
	project   *logical.LogicalProject // nil for SELECT * (no final projection)
	sort      *logical.LogicalSort
	aggregate *logical.LogicalAggregate
	filter    *logical.LogicalFilter
	scan      *logical.LogicalScan
}

// decompose peels the plan Project → Sort → Aggregate → Filter → Scan.
//
// Anything else — joins, set operations, CTEs, derived tables, unnests —
// fails with Java's "expected to find exactly one type filter operator"
// (getRecordTypeName, MaterializedViewIndexGenerator.java:797): in Java those
// shapes plan to zero or multiple LogicalTypeFilterExpressions over the
// multiple quantifiers, and the record-type resolution rejects them before
// anything else runs (:151 runs before checkValidity at :166).
// IndexTest.java:500-520 and :718-725 pin the message for the join shapes.
func decompose(op logical.LogicalOperator) (*decomposed, error) {
	d := &decomposed{}
	cur := op
	if p, ok := cur.(*logical.LogicalProject); ok {
		d.project = p
		cur = p.Input
	}
	if s, ok := cur.(*logical.LogicalSort); ok {
		d.sort = s
		cur = s.Input
	}
	if a, ok := cur.(*logical.LogicalAggregate); ok {
		d.aggregate = a
		cur = a.Input
	}
	if f, ok := cur.(*logical.LogicalFilter); ok {
		d.filter = f
		cur = f.Input
	}
	scan, ok := cur.(*logical.LogicalScan)
	if !ok {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"Unsupported query, expected to find exactly one type filter operator")
	}
	d.scan = scan
	return d, nil
}

// storageNames resolves a resolved-accessor path step to the DESCRIPTOR's
// field name — the STORAGE name. Java's ResolvedAccessor carries the
// Type.Record.Field itself, so getFieldPathNames yields storage names and the
// generated key expression's field() names always match the descriptor
// (toFieldKeyExpression, MaterializedViewIndexGenerator.java:810-827). Go's
// semantic accessors instead present the FOLDED display name (the runtime
// positional namespace — rlcatalog's recordTypeTable presents folded
// identifiers by design), so rendering acc.Field into stored metadata
// corrupts a quoted-DDL column ("col1" → COL1) into a name the descriptor
// does not carry and metadata validation rejects. The accessor's ORDINAL is
// its identity (Java's element equality is ordinal-only,
// FieldValue.java:675-689) and indexes the declared column order, which IS
// descriptor field order for the base-table scan — the only source shape
// decompose admits.
type storageNames struct {
	root protoreflect.MessageDescriptor
}

// fieldName returns the storage name of path[depth], descending nested
// message descriptors along path[0:depth]. Falls back to the accessor's own
// (folded) name when the ordinal lies outside the descriptor — the
// __ROW_VERSION pseudo-slot sits one past the descriptor's fields and its
// name is identical in both namespaces.
type fieldAccessor struct {
	ordinal int
	name    string
	typ     values.Type
}

func fieldAccessors(field values.FieldValue) ([]fieldAccessor, bool) {
	if field == nil || field.Path() == nil || field.Path().Len() == 0 {
		return nil, false
	}
	path := make([]fieldAccessor, field.Path().Len())
	for i := range path {
		accessor, ok := field.Path().Accessor(i)
		if !ok || accessor.Ordinal() < 0 {
			return nil, false
		}
		name, _ := accessor.DisplayName()
		path[i] = fieldAccessor{ordinal: accessor.Ordinal(), name: name, typ: accessor.FieldType()}
	}
	return path, true
}

func (s storageNames) fieldName(path []fieldAccessor, depth int) string {
	desc := s.root
	for i := 0; i < depth && desc != nil; i++ {
		if path[i].ordinal < 0 || path[i].ordinal >= desc.Fields().Len() {
			desc = nil
			break
		}
		desc = desc.Fields().Get(path[i].ordinal).Message()
	}
	if desc == nil || path[depth].ordinal < 0 || path[depth].ordinal >= desc.Fields().Len() {
		return path[depth].name
	}
	return string(desc.Fields().Get(path[depth].ordinal).Name())
}

// sortOrderFunction maps a sort key's direction to the ordering-function
// wrapper name, or "" for plain ascending.
//
// Java: OrderByExpression.toSortOrder (OrderByExpression.java:112-118)
// produces the RequestedSortOrder, and getOrderByValues
// (MaterializedViewIndexGenerator.java:347-363) maps it:
//
//	ASCENDING              (asc,  nulls first) → no wrapper
//	ASCENDING_NULLS_LAST   (asc,  nulls last)  → order_asc_nulls_last
//	DESCENDING             (desc, nulls last)  → order_desc_nulls_last
//	DESCENDING_NULLS_FIRST (desc, nulls first) → order_desc_nulls_first
//
// The plan visitor already applied SQL's defaults (ASC → nulls first,
// DESC → nulls last) to SortKey.NullsFirst, matching ParseHelpers.isDescending
// / isNullsLast (ParseHelpers.java:167-179).
func sortOrderFunction(k logical.SortKey) string {
	if k.Dir == logical.SortDesc {
		if k.NullsFirst {
			return recordlayer.OrderFuncDescNullsFirst
		}
		return recordlayer.OrderFuncDescNullsLast
	}
	if !k.NullsFirst {
		return recordlayer.OrderFuncAscNullsLast
	}
	return ""
}

// orderByValues resolves the sort keys to their projected values and the
// per-value ordering-function map.
//
// Ports getOrderByValues (MaterializedViewIndexGenerator.java:339-385) plus
// the ORDER-BY ⊆ projection rule of LogicalOperator.generateSelect
// (LogicalOperator.java:389-390): Java takes the set difference between the
// sort's expression values and the select's output values, wraps a non-empty
// remainder in an outer select, and DdlVisitor.java:217 rejects that shape as
// INVALID_COLUMN_REFERENCE. Go tests membership directly over value identity
// (RFC-202 D3 — never the plan's shape; see
// embedded/index_ddl_order_by_shape_test.go for why position carries no
// information).
//
// The returned values are the PROJECTION's value objects (not the sort key's
// own copies) so that identity-keyed ordering functions and the reorder step
// agree on object identity. Java needs no such canonicalisation because its
// rebased sort values compare equal to the output values by construction; Go
// compares structurally and then canonicalises to the projection instance.
//
// SELECT * is expanded to exact source FieldValues before this helper is
// called. Passing that expansion as projected is load-bearing: it
// canonicalizes an ORDER BY field to the very same Value instance and lets
// reorderValues remove the field from the covering tail. Treating star as a
// nil projection prepended the sort-key copy and then retained the expanded
// copy, producing a false "multiple disconnected references" rejection (and
// could hide the array-field semantic rejection behind it).
func orderByValues(sort *logical.LogicalSort, projected []values.Value) ([]values.Value, map[values.Value]string, error) {
	if sort == nil {
		return nil, map[values.Value]string{}, nil
	}
	// Java's orderingFunctions is an IdentityHashMap (RFC-202 D12): Go's map
	// over the Value interface compares pointers for pointer-typed values,
	// which is the same identity semantics.
	fns := make(map[values.Value]string, len(sort.Keys))
	out := make([]values.Value, 0, len(sort.Keys))
	for _, k := range sort.Keys {
		var v values.Value
		switch {
		case k.Pos > 0:
			// Positional ORDER BY <n> is an output ordinal by SQL definition;
			// it resolves to the projection slot directly.
			if projected == nil || k.Pos > len(projected) {
				return nil, nil, api.NewErrorf(api.ErrCodeInvalidColumnReference,
					"ORDER BY position %d is not in the select list", k.Pos)
			}
			v = projected[k.Pos-1]
		case k.Value == nil:
			// A nil Value slot means the expression walker declined the
			// key's shape. Silently reading "unknown" as "projected" is how
			// the wrong index gets built — hard error (RFC-202 D3).
			return nil, nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
				"Unsupported index definition, cannot resolve ORDER BY expression %q", k.Expr)
		default:
			v = k.Value
			if projected != nil {
				found := false
				for _, pv := range projected {
					if pv != nil && projectedValueMatches(v, pv) {
						v = pv // canonicalise to the projection's instance
						found = true
						break
					}
				}
				if !found {
					// Java: DdlVisitor.java:217, pinned by IndexTest.java:710-716.
					return nil, nil, api.NewError(api.ErrCodeInvalidColumnReference,
						"Cannot create index and order by an expression that is not present in the projection list")
				}
			}
		}
		if fn := sortOrderFunction(k); fn != "" {
			fns[v] = fn
		}
		out = append(out, v)
	}
	return out, fns, nil
}

// projectedValueMatches is the single-source DDL equivalent of Java's
// alias-aware Value equality. Most values compare structurally. A resolved
// field also admits the same exact accessor path and leaf type when the two
// producer-local QOV roots differ: SELECT * expansion owns the scan-shaped
// QOV while ORDER BY owns a rebased logical-plan QOV. The decomposer has
// already restricted this generator to one source, so ignoring that
// producer-local root cannot conflate two join legs.
func projectedValueMatches(ordering, projected values.Value) bool {
	if values.ValuesStructurallyEqual(ordering, projected) {
		return true
	}
	orderingField, orderingOK := values.AsFieldValue(ordering)
	projectedField, projectedOK := values.AsFieldValue(projected)
	if !orderingOK || !projectedOK || orderingField.Path() == nil || projectedField.Path() == nil {
		return false
	}
	orderingOrdinals := orderingField.Path().Ordinals()
	projectedOrdinals := projectedField.Path().Ordinals()
	if len(orderingOrdinals) != len(projectedOrdinals) {
		return false
	}
	for i := range orderingOrdinals {
		if orderingOrdinals[i] != projectedOrdinals[i] {
			return false
		}
	}
	orderingType, projectedType := orderingField.ResultType(), projectedField.ResultType()
	return orderingType != nil && projectedType != nil && orderingType.Equals(projectedType)
}

// reorderValues puts the ORDER BY values first, in ORDER BY order, then the
// remaining projection values in projection order. The ORDER BY fixes the key
// order; the projection order only fixes the tail.
//
// Ports reorderValues (MaterializedViewIndexGenerator.java:387-394); the
// SELECT-order-vs-ORDER-BY-order direction is pinned by IndexTest.java:673-681
// (`SELECT a1, a2 … ORDER BY a2, a1` → concat(A2, A1)).
func reorderValues(vals, orderBy []values.Value) ([]values.Value, error) {
	if len(vals) < len(orderBy) {
		return nil, api.NewError(api.ErrCodeInternalError,
			"index generator: more ORDER BY values than projected values")
	}
	if len(orderBy) == 0 {
		return vals, nil
	}
	out := make([]values.Value, 0, len(vals))
	out = append(out, orderBy...)
	for _, v := range vals {
		inOrderBy := false
		for _, ov := range orderBy {
			if values.ValuesStructurallyEqual(v, ov) {
				inOrderBy = true
				break
			}
		}
		if !inOrderBy {
			out = append(out, v)
		}
	}
	return out, nil
}

// generateValue is the value/version arm
// (MaterializedViewIndexGenerator.java:187-203).
func generateValue(d *decomposed, md *recordlayer.RecordMetaData, res storageNames) (*GeneratedIndex, error) {
	fieldValues, err := projectedValues(d, md)
	if err != nil {
		return nil, err
	}
	// projectedValues expands SELECT * to the exact output list, so both star
	// and explicit projections use one canonical membership/identity path.
	orderBy, orderingFns, err := orderByValues(d.sort, fieldValues)
	if err != nil {
		return nil, err
	}
	return buildValueIndex(d.scan.Table, fieldValues, orderBy, orderingFns, res)
}

// buildValueIndex is the shared tail of the value arm
// (MaterializedViewIndexGenerator.java:190-203), reached from the plain
// projection (generateValue) and from an aggregate-free GROUP BY plan, whose
// grouping values ARE the projection in Java's collectResultValues terms.
func buildValueIndex(table string, fieldValues, orderBy []values.Value, orderingFns map[values.Value]string, res storageNames) (*GeneratedIndex, error) {
	// Version partition (MaterializedViewIndexGenerator.java:183-189): the
	// projected FieldValues whose result type IS the __ROW_VERSION
	// pseudo-field's type (Type.primitiveType(VERSION, true)). Java filters
	// by TYPE equality alone — an aliased projection of the pseudo-field
	// still counts — and the version values STAY inside fieldValues so
	// reorder and the covering split treat them as ordinary key columns; only
	// the COUNT decides the index type (≤1, and VERSION instead of VALUE).
	versionCount := 0
	for _, v := range fieldValues {
		if fv, ok := values.AsFieldValue(v); ok &&
			fv.ResultType() != nil && fv.ResultType().Code() == values.TypeCodeVersion && fv.ResultType().IsNullable() {
			versionCount++
		}
	}
	if versionCount > 1 {
		// Java's exact message (MaterializedViewIndexGenerator.java:185).
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"Cannot have index with more than one version column")
	}
	// Java :190-192: a multi-column value index must have a top-level ORDER BY
	// (the key order would otherwise be unspecified). IndexTest.java:694-700.
	if len(fieldValues) > 1 && len(orderBy) == 0 {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"Unsupported index definition, value indexes must have an order by clause at the top level")
	}
	reordered, err := reorderValues(fieldValues, orderBy)
	if err != nil {
		return nil, err
	}
	expr, err := generateKeyExpression(reordered, orderingFns, res)
	if err != nil {
		return nil, err
	}
	// Split point (Java :195-198): the ORDER BY length; -1 (no covering
	// split) when the ORDER BY is empty — both DDL forms pass
	// generateKeyValueExpressionWithEmptyKey = false (DdlVisitor.java:218,
	// OnSourceIndexGenerator.java:227), the vector index being the only
	// caller that flips it.
	splitPoint := len(orderBy)
	if len(orderBy) == 0 {
		splitPoint = -1
	}
	root := expr
	if splitPoint != -1 && splitPoint < len(fieldValues) {
		// Covering index (Java :199-200): keyWithValue(expr, splitPoint).
		root = recordlayer.KeyWithValue(expr, splitPoint)
	}
	// Java :189: IndexTypes.VERSION when the (aggregate-free) projection
	// carries a version column, VALUE otherwise.
	indexType := recordlayer.IndexTypeValue
	if versionCount > 0 {
		indexType = recordlayer.IndexTypeVersion
	}
	return &GeneratedIndex{
		TableName: table,
		Root:      root,
		IndexType: indexType,
	}, nil
}

// projectedValues returns the index's projected value list.
//
// Java flattens the top operator's result value with Values.deconstructRecord
// and dereferences each through the quantifier map (:174 → :330-336). Go's
// final LogicalProject already carries the flattened, source-resolved value
// list in ProjectedValues; a nil slot means the walker declined that
// expression's shape, which is a hard error, not a silent skip.
//
// SELECT * has no final projection (the plan is the bare source, optionally
// under a sort); Java has expanded the star into every column by this point,
// so Go expands it here from the record type's fields in declaration order.
func projectedValues(d *decomposed, md *recordlayer.RecordMetaData) ([]values.Value, error) {
	if d.project == nil {
		return starValues(d.scan, md)
	}
	out := make([]values.Value, 0, len(d.project.ProjectedValues))
	for i, pv := range d.project.ProjectedValues {
		if pv == nil {
			name := ""
			if i < len(d.project.Projections) {
				name = d.project.Projections[i]
			}
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
				"Unsupported index definition, cannot map select element %q to a key expression", name)
		}
		out = append(out, pv)
	}
	if len(out) == 0 {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"Unsupported index definition, empty projection")
	}
	return out, nil
}

// starValues expands `SELECT *` into one baked FieldValue per column of the
// scanned record type, in declaration order — the state Java's plan is
// already in when the generator runs (the star expands during planning).
func starValues(scan *logical.LogicalScan, md *recordlayer.RecordMetaData) ([]values.Value, error) {
	rt := md.RecordTypes()[scan.Table]
	if rt == nil {
		return nil, api.NewErrorf(api.ErrCodeUndefinedTable,
			"Unknown table %q", scan.Table)
	}
	fields := rt.Descriptor.Fields()
	rowFields := make([]values.Field, fields.Len())
	for i := 0; i < fields.Len(); i++ {
		field := fields.Get(i)
		rowFields[i] = values.Field{
			Name:      strings.ToUpper(string(field.Name())),
			Ordinal:   i,
			FieldType: querycore.TargetTypeForFD(field),
		}
	}
	corrName := scan.Binding
	if corrName == "" {
		corrName = scan.Alias
	}
	if corrName == "" {
		corrName = scan.Table
	}
	qov, err := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier(corrName),
		&values.RecordType{RecordName: string(rt.Descriptor.Name()), Fields: rowFields},
	)
	if err != nil {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"Unsupported index definition, cannot type star expansion: %v", err)
	}
	out := make([]values.Value, 0, fields.Len())
	for i := 0; i < fields.Len(); i++ {
		fv, resolveErr := values.ResolveFieldOrdinals(qov, []int{i})
		if resolveErr != nil {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
				"Unsupported index definition, cannot resolve star column %d: %v", i, resolveErr)
		}
		out = append(out, fv)
	}
	return out, nil
}

// generateKeyExpression is the expression builder
// (MaterializedViewIndexGenerator.java:497-524): a run of consecutive
// FieldValues is compressed into one trie (shared prefixes become one
// nesting), every other value becomes its own component, and the components
// are concatenated.
func generateKeyExpression(vals []values.Value, orderingFns map[values.Value]string, res storageNames) (recordlayer.KeyExpression, error) {
	if len(vals) == 0 {
		return recordlayer.EmptyKey(), nil
	}
	if len(vals) == 1 {
		return leafKeyExpression(vals[0], orderingFns, res)
	}
	var trieNodes []*fieldTrieNode
	var components []recordlayer.KeyExpression
	i := 0
	for i < len(vals) {
		_, isField := values.AsFieldValue(vals[i])
		if !isField {
			expr, err := leafKeyExpression(vals[i], orderingFns, res)
			if err != nil {
				return nil, err
			}
			components = append(components, expr)
			i++
			continue
		}
		node, next, err := computeTrieForValues(vals, i)
		if err != nil {
			return nil, err
		}
		if err := node.validateNoOverlaps(trieNodes); err != nil {
			return nil, err
		}
		trieNodes = append(trieNodes, node)
		expr, err := trieKeyExpression(node, orderingFns, res, nil)
		if err != nil {
			return nil, err
		}
		components = append(components, expr)
		i = next
	}
	if len(components) == 1 {
		return components[0], nil
	}
	return concatFlat(components), nil
}

// concatFlat concatenates components with Java's ThenKeyExpression
// constructor semantics: a component that is itself a Then contributes its
// CHILDREN, not a nested node (ThenKeyExpression.java:264-270 flattens in
// `add`). Go's recordlayer.Concat stores children verbatim, so without this a
// trie run followed by a non-field component would serialize as a nested
// Then proto — a byte shape Java can never produce for the same DDL.
func concatFlat(components []recordlayer.KeyExpression) recordlayer.KeyExpression {
	flat := make([]recordlayer.KeyExpression, 0, len(components))
	for _, c := range components {
		if then, ok := c.(*recordlayer.CompositeKeyExpression); ok {
			flat = append(flat, then.SubKeyExpressions()...)
			continue
		}
		flat = append(flat, c)
	}
	return recordlayer.Concat(flat...)
}

// fieldTrieNode is the Go form of FieldValueTrieNode
// (FieldValueTrieNode.java): a compressed trie over resolved accessor paths.
// value is non-nil at a leaf that terminates a projected FieldValue.
type fieldTrieNode struct {
	value    values.FieldValue
	children []trieChild // insertion-ordered (Java's ImmutableMap preserves it)
}

type trieChild struct {
	accessor fieldAccessor
	node     *fieldTrieNode
}

// accessorKeyEqual is trie-key equality. Java keys the children map on
// ResolvedAccessor, whose equals is ORDINAL-only (FieldValue.java:675-689) —
// with the explode-counter refinement of AnnotatedAccessor
// (MaterializedViewIndexGenerator.java:683-718) keeping two unnests of the
// same array distinct. Go's ResolvedAccessor carries no explode marker yet
// (the unnest shapes do not reach this generator — decompose rejects them),
// so ordinal equality is exact here.
func accessorKeyEqual(a, b fieldAccessor) bool {
	return a.ordinal == b.ordinal
}

// computeTrieForValues consumes the maximal run of FieldValues starting at
// start whose paths extend the empty prefix, building the trie. Returns the
// node and the index one past the consumed run.
// Ports FieldValueTrieNode.computeTrieForValues (FieldValueTrieNode.java:201-238).
func computeTrieForValues(vals []values.Value, start int) (*fieldTrieNode, int, error) {
	node, next, err := computeTrieAtDepth(vals, start, 0)
	if err != nil {
		return nil, 0, err
	}
	return node, next, nil
}

func computeTrieAtDepth(vals []values.Value, start, depth int) (*fieldTrieNode, int, error) {
	node := &fieldTrieNode{}
	i := start
	for i < len(vals) {
		fv, ok := values.AsFieldValue(vals[i])
		if !ok {
			break
		}
		path, ok := fieldAccessors(fv)
		if !ok {
			break
		}
		if len(path) == depth {
			// The path terminates exactly at this prefix.
			if depth == 0 {
				break // a zero-length path cannot occur (FieldPath is non-empty)
			}
			return &fieldTrieNode{value: fv}, i + 1, nil
		}
		if !prefixMatches(vals, start, i, depth) {
			break
		}
		acc := path[depth]
		// A duplicate child key = the same nested field path referenced twice
		// non-adjacently under one parent (FieldValueTrieNode.java:249-253).
		for _, c := range node.children {
			if accessorKeyEqual(c.accessor, acc) {
				return nil, 0, overlapError()
			}
		}
		child, next, err := computeTrieAtDepth(vals, i, depth+1)
		if err != nil {
			return nil, 0, err
		}
		node.children = append(node.children, trieChild{accessor: acc, node: child})
		i = next
	}
	return node, i, nil
}

// prefixMatches reports whether vals[i]'s path agrees with vals[start]'s path
// on the first depth accessors — the "prefix.isPrefixOf(fieldPath)" walk of
// Java's iterator version, expressed over the slice.
func prefixMatches(vals []values.Value, start, i, depth int) bool {
	baseField, ok := values.AsFieldValue(vals[start])
	if !ok {
		return false
	}
	base, ok := fieldAccessors(baseField)
	if !ok {
		return false
	}
	cur, ok := values.AsFieldValue(vals[i])
	if !ok {
		return false
	}
	path, ok := fieldAccessors(cur)
	if !ok {
		return false
	}
	if len(path) < depth || len(base) < depth {
		return false
	}
	for k := 0; k < depth; k++ {
		if !accessorKeyEqual(base[k], path[k]) {
			return false
		}
	}
	return true
}

func overlapError() error {
	return api.NewError(api.ErrCodeUnsupportedOperation,
		"Index with multiple disconnected references to the same column are not supported")
}

// validateNoOverlaps rejects a new trie whose root-level accessors collide
// with an earlier trie's — the same parent reached twice non-adjacently
// (FieldValueTrieNode.validateNoOverlaps, FieldValueTrieNode.java:136-153;
// IndexTest.java:745-752, :763-771 pin the message).
func (n *fieldTrieNode) validateNoOverlaps(others []*fieldTrieNode) error {
	if len(n.children) == 0 {
		return nil
	}
	for _, other := range others {
		for _, oc := range other.children {
			for _, c := range n.children {
				if accessorKeyEqual(c.accessor, oc.accessor) {
					return overlapError()
				}
			}
		}
	}
	return nil
}

// trieKeyExpression renders a trie node
// (MaterializedViewIndexGenerator.java:584-609): each child becomes a field
// expression, nested when it has children of its own; the ordering wrapper is
// applied at the LEAF (:598-600), never at the root.
func trieKeyExpression(n *fieldTrieNode, orderingFns map[values.Value]string, res storageNames, prefix []fieldAccessor) (recordlayer.KeyExpression, error) {
	if len(n.children) == 0 {
		return nil, api.NewError(api.ErrCodeInternalError, "index generator: empty trie node")
	}
	parts := make([]recordlayer.KeyExpression, 0, len(n.children))
	for _, c := range n.children {
		childPath := make([]fieldAccessor, 0, len(prefix)+1)
		childPath = append(append(childPath, prefix...), c.accessor)
		name := res.fieldName(childPath, len(childPath)-1)
		leaf, err := fieldLeafExpression(c.accessor, name, c.node.value)
		if err != nil {
			return nil, err
		}
		if len(c.node.children) > 0 {
			childExpr, err := trieKeyExpression(c.node, orderingFns, res, childPath)
			if err != nil {
				return nil, err
			}
			parts = append(parts, recordlayer.Nest(name, childExpr))
			continue
		}
		if c.node.value != nil {
			if fn, ok := orderingFns[values.Value(c.node.value)]; ok {
				parts = append(parts, recordlayer.FunctionExpr(fn, leaf))
				continue
			}
		}
		parts = append(parts, leaf)
	}
	if len(parts) == 1 {
		return parts[0], nil
	}
	return recordlayer.Concat(parts...), nil
}

// fieldLeafExpression builds the field expression for a terminal accessor.
// Ports toFieldKeyExpression (MaterializedViewIndexGenerator.java:810-827)
// for the FanOut path: an array field reached here was NOT unnested and is
// rejected; a non-array field is FanType.None.
//
// name is the accessor's STORAGE name (storageNames.fieldName); acc.Field is
// only consulted for the __ROW_VERSION pseudo-field check, whose name is
// identical in both namespaces. leafValue (nil for an intermediate rendered
// as a nesting parent) supplies the field's type — Go's ResolvedAccessor
// carries no type, the FieldValue's leaf Typ does.
func fieldLeafExpression(acc fieldAccessor, name string, leafValue values.FieldValue) (recordlayer.KeyExpression, error) {
	// The __ROW_VERSION pseudo-field renders as the version key expression —
	// name AND type must both match (Java's toFieldKeyExpression,
	// MaterializedViewIndexGenerator.java:821-823).
	if leafValue != nil && values.IsRowVersionPseudoField(acc.name, leafValue.ResultType()) {
		return recordlayer.VersionKey(), nil
	}
	if leafValue != nil && leafValue.ResultType() != nil && leafValue.ResultType().Code() == values.TypeCodeArray {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"Unsupported index definition, cannot create index on array field '%s' without unnesting",
			name)
	}
	return recordlayer.Field(name), nil
}

// leafKeyExpression builds the key expression for one non-trie value,
// applying the ordering wrapper (toKeyExpression(value, orderingFunctions),
// MaterializedViewIndexGenerator.java:527-534, over :551-582).
func leafKeyExpression(v values.Value, orderingFns map[values.Value]string, res storageNames) (recordlayer.KeyExpression, error) {
	expr, err := valueKeyExpression(v, res)
	if err != nil {
		return nil, err
	}
	if fn, ok := orderingFns[v]; ok {
		return recordlayer.FunctionExpr(fn, expr), nil
	}
	return expr, nil
}

// valueKeyExpression is toKeyExpression(Value)
// (MaterializedViewIndexGenerator.java:551-582).
func valueKeyExpression(v values.Value, res storageNames) (recordlayer.KeyExpression, error) {
	if field, ok := values.AsFieldValue(v); ok {
		return fieldPathExpression(field, recordlayer.FanTypeNone, res)
	}
	switch val := v.(type) {
	case *values.CardinalityValue:
		// CARDINALITY consumes the materialised array: the field is accessed
		// with Concatenate, not FanOut (:555-566).
		child, ok := values.AsFieldValue(val.Child)
		if !ok {
			return nil, api.NewError(api.ErrCodeUnsupportedOperation,
				"CARDINALITY() must be applied to a `field()` in an index key expression.")
		}
		arg, err := fieldPathExpression(child, recordlayer.FanTypeConcatenate, res)
		if err != nil {
			return nil, err
		}
		return recordlayer.CardinalityExpr(arg), nil
	case *values.ArithmeticValue:
		left, err := valueKeyExpression(val.Left, res)
		if err != nil {
			return nil, err
		}
		right, err := valueKeyExpression(val.Right, res)
		if err != nil {
			return nil, err
		}
		name, err := arithmeticFunctionName(val.Op)
		if err != nil {
			return nil, err
		}
		return recordlayer.FunctionExpr(name, recordlayer.Concat(left, right)), nil
	case *values.ScalarFunctionValue:
		// The bit operators parse as ScalarFunctionValues in Go; Java models
		// them as ArithmeticValues and lowercases the operator name
		// (:567-575; IndexTest.java pins bitand/bitor/bitxor).
		name, ok := bitFunctionName(val.FuncName)
		if !ok {
			return nil, unableToConstruct()
		}
		args := make([]recordlayer.KeyExpression, 0, len(val.Args))
		for _, a := range val.Args {
			expr, err := valueKeyExpression(a, res)
			if err != nil {
				return nil, err
			}
			args = append(args, expr)
		}
		return recordlayer.FunctionExpr(name, argumentExpression(args)), nil
	case *values.ConstantValue:
		// LiteralValue → Key.Expressions.value(literal) (:576-577).
		return recordlayer.Literal(val.Value), nil
	default:
		return nil, unableToConstruct()
	}
}

func unableToConstruct() error {
	return api.NewError(api.ErrCodeUnsupportedOperation, "unable to construct expression")
}

// argumentExpression is buildArgumentKeyExpression
// (MaterializedViewIndexGenerator.java:540-548).
func argumentExpression(args []recordlayer.KeyExpression) recordlayer.KeyExpression {
	switch len(args) {
	case 0:
		return recordlayer.EmptyKey()
	case 1:
		return args[0]
	default:
		return recordlayer.Concat(args...)
	}
}

// arithmeticFunctionName maps the arithmetic op to Java's lowercase logical
// operator name (ArithmeticValue.getLogicalOperator().name().toLowerCase(),
// MaterializedViewIndexGenerator.java:574).
func arithmeticFunctionName(op values.ArithmeticOp) (string, error) {
	switch op {
	case values.OpAdd:
		return "add", nil
	case values.OpSub:
		return "sub", nil
	case values.OpMul:
		return "mul", nil
	case values.OpDiv:
		return "div", nil
	case values.OpMod:
		return "mod", nil
	default:
		return "", unableToConstruct()
	}
}

// bitFunctionName maps Go's scalar bit-function spellings to Java's
// key-expression function names. Shift operators have no Java
// ArithmeticValue.LogicalOperator counterpart and are rejected. The bitmap
// bucketing functions are Java ArithmeticValues too
// (ArithmeticValue.java:374-375), so their key-expression form is the same
// function(name, concat(args)) lowering — with the walker-injected 10000
// entry-size literal as the second argument, exactly the proto Java stores.
func bitFunctionName(fn string) (string, bool) {
	switch fn {
	case "BITAND":
		return "bitand", true
	case "BITOR":
		return "bitor", true
	case "BITXOR":
		return "bitxor", true
	case "BITMAP_BUCKET_OFFSET":
		return "bitmap_bucket_offset", true
	case "BITMAP_BIT_POSITION":
		return "bitmap_bit_position", true
	default:
		return "", false
	}
}

// fieldPathExpression renders a FieldValue's resolved accessor path as nested
// field expressions — toKeyExpression(Iterator<ResolvedAccessor>, fanType)
// (MaterializedViewIndexGenerator.java:777-789): each intermediate accessor
// nests, the innermost carries the fan type when it is an array.
//
// fanTypeForArray applies only to an array LEAF; Go's accessors carry no
// per-step type, and an intermediate array step is unreachable (dotted access
// through an array requires an unnest, which decompose rejects).
func fieldPathExpression(fv values.FieldValue, fanTypeForArray recordlayer.FanType, res storageNames) (recordlayer.KeyExpression, error) {
	accs, ok := fieldAccessors(fv)
	if !ok {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"Unsupported index definition, cannot resolve column %q", fv.DisplayName())
	}
	// The __ROW_VERSION pseudo-field is always a single top-level accessor;
	// it renders as the version key expression (name AND type — Java's
	// toFieldKeyExpression, MaterializedViewIndexGenerator.java:821-823).
	if len(accs) == 1 && values.IsRowVersionPseudoField(accs[0].name, fv.ResultType()) {
		return recordlayer.VersionKey(), nil
	}
	isArray := fv.ResultType() != nil && fv.ResultType().Code() == values.TypeCodeArray
	var leaf recordlayer.KeyExpression
	leafName := res.fieldName(accs, len(accs)-1)
	switch {
	case !isArray:
		leaf = recordlayer.Field(leafName)
	case fanTypeForArray == recordlayer.FanTypeConcatenate:
		leaf = recordlayer.FieldConcatenate(leafName)
	default:
		// FanOut of a non-unnested array is invalid
		// (MaterializedViewIndexGenerator.java:814-819).
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"Unsupported index definition, cannot create index on array field '%s' without unnesting",
			leafName)
	}
	expr := leaf
	for i := len(accs) - 2; i >= 0; i-- {
		expr = recordlayer.Nest(res.fieldName(accs, i), expr)
	}
	return expr, nil
}
