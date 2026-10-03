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
	"math"

	"google.golang.org/protobuf/reflect/protoreflect"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
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
// Ports MaterializedViewIndexGenerator.generate (Java 4.14.2.0,
// MaterializedViewIndexGenerator.java:95-117): the query is translated into
// the graph Go's query path plans (Java plans the index query into its
// RelationalExpression graph, DdlVisitor.java:266), the graph's top is checked
// as DdlVisitor.java:274 checks it, IndexSpec is collected over it with the
// quantifiers resolved by QuantifierValues, checked for validity, and turned
// into the key expression, the index type and options, and the predicate.
func Generate(op logical.LogicalOperator, md *recordlayer.RecordMetaData, opts Options) (*GeneratedIndex, error) {
	// The md == nil text fallback of the plan visitor produces a plan with no
	// resolved values; an index generated from it would come from unresolved
	// names. Assert loudly (RFC-202 D4).
	if md == nil {
		return nil, api.NewError(api.ErrCodeInternalError,
			"index generator invoked without metadata — the catalog-less plan fallback must be unreachable here")
	}
	ref, _, err := querycore.TranslateIndexDefinitionToCascades(op, md)
	if err != nil {
		return nil, err
	}
	if ref == nil {
		return nil, unsupported("Unsupported index definition, the query does not translate")
	}
	root, err := member(ref)
	if err != nil {
		return nil, err
	}
	qv := newQuantifierValues()
	c := &specCollector{qv: qv}
	rootScope := &scope{expr: root}

	// The root's result is the projection (a star projects the whole row).
	resultRow, err := qv.row(root, rootScope)
	if err != nil {
		return nil, err
	}
	result, err := qv.deconstruct(resultRow)
	if err != nil {
		return nil, err
	}
	if err := rejectSubquerySorts(root); err != nil {
		return nil, err
	}
	if err := checkTop(c, root, rootScope, result); err != nil {
		return nil, err
	}

	spec, err := c.visit(root, rootScope)
	if err != nil {
		return nil, err
	}
	if spec.projection, err = c.projectionOf(result, spec.groupBy); err != nil {
		return nil, err
	}
	if err := c.checkValidity(spec); err != nil {
		return nil, err
	}

	// Every rendered field name comes from the scanned record type's
	// DESCRIPTOR, by accessor ordinal — the storage name Java's
	// ResolvedAccessor carries. GetRecordType: spec.recordType is the SQL
	// identifier the translator's scan carries, the map is keyed by the STORED
	// protobuf name, and an escaped name misses; a miss leaves res.root nil and
	// the index would be built from folded display names.
	res := storageNames{}
	if rt := md.GetRecordType(spec.recordType); rt != nil {
		res.root = rt.Descriptor
	}
	var gi *GeneratedIndex
	if aggregateOf(spec.projection) != nil {
		gi, err = generateAggregate(c, spec, opts, res)
	} else {
		gi, err = generateValue(c, spec, res)
	}
	if err != nil {
		return nil, err
	}
	// The predicate is serialized after the key expression is built, as the
	// target orders it (generate(), :101-104).
	if gi.Predicate, err = generatePredicate(spec, res); err != nil {
		return nil, err
	}
	return gi, nil
}

// topSort is the ORDER BY of the definition's own select: the root, or the
// sort under the root's projection. Nil without an ORDER BY.
func topSort(root expressions.RelationalExpression) *expressions.LogicalSortExpression {
	if sort, ok := root.(*expressions.LogicalSortExpression); ok {
		return sort
	}
	if _, ok := root.(*expressions.LogicalProjectionExpression); ok {
		if producer, err := member(root.GetQuantifiers()[0].GetRangesOver()); err == nil {
			if sort, ok := producer.(*expressions.LogicalSortExpression); ok {
				return sort
			}
		}
	}
	return nil
}

// rejectSubquerySorts is the target's front-end refusal of an ORDER BY below
// the top level (QueryVisitor.java:948-949, ExpressionVisitor.java:205-206,
// !isTopLevel()), which runs while the definition is planned, before any check
// on the plan — in a derived table and in an EXISTS subquery alike. ORDER BY in
// a subquery is an approved Go extension for QUERIES (RFC-082); an index
// definition is not a query, so the extension does not reach it.
func rejectSubquerySorts(root expressions.RelationalExpression) error {
	top := topSort(root)
	var walk func(e expressions.RelationalExpression) error
	walk = func(e expressions.RelationalExpression) error {
		if sort, ok := e.(*expressions.LogicalSortExpression); ok && sort != top {
			return unsupported("order by is not supported in subquery")
		}
		for _, q := range e.GetQuantifiers() {
			producer, err := member(q.GetRangesOver())
			if err != nil {
				return err
			}
			if err := walk(producer); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(root)
}

// checkTop is DdlVisitor.java:274's assertion, made before IndexSpec runs as
// the target makes it: the query's top is a sort over the select, which fails
// when an ORDER BY key is not among the projected columns (LogicalOperator.
// generateSelect wraps the sort in one more select then). Go's translator
// emits Project(Sort(…)) for an ORDER BY; a key not among the projection's
// resolved columns is the target's INVALID_COLUMN_REFERENCE.
func checkTop(c *specCollector, root expressions.RelationalExpression, rootScope *scope, result []values.Value) error {
	e, sc := root, rootScope
	if _, ok := e.(*expressions.LogicalProjectionExpression); ok {
		producer, err := member(e.GetQuantifiers()[0].GetRangesOver())
		if err != nil {
			return err
		}
		e, sc = producer, sc.child(producer)
	}
	sort, ok := e.(*expressions.LogicalSortExpression)
	if !ok {
		return nil
	}
	order, err := c.orderByOf(sort, sc)
	if err != nil {
		return err
	}
	for _, v := range order.values {
		found := false
		for _, r := range result {
			if c.qv.equalValues(v, r) {
				found = true
				break
			}
		}
		if !found {
			return api.NewError(api.ErrCodeInvalidColumnReference,
				"Cannot create index and order by an expression that is not present in the projection list")
		}
	}
	return nil
}

// storageNames resolves a resolved-accessor path step to the DESCRIPTOR's
// field name — the STORAGE name Java's ResolvedAccessor carries, so the
// generated key expression's field() names always match the descriptor. Go's
// semantic accessors present the FOLDED display name, which corrupts a
// quoted-DDL column ("col1" → COL1) if rendered into metadata. The accessor's
// ORDINAL indexes the declared column order, which IS descriptor field order.
type storageNames struct {
	root protoreflect.MessageDescriptor
}

// fieldName returns the storage name of path[depth], descending nested
// message descriptors along path[0:depth] — through a nullable array's wrapper
// message to its element, as the key expression is written over the logical
// type and wrapped afterwards (NullableArrayUtils.wrapArray). Falls back to
// the accessor's own (folded) name when the ordinal lies outside the
// descriptor — the __ROW_VERSION pseudo-slot sits one past the descriptor's
// fields and its name is identical in both namespaces.
func (s storageNames) fieldName(path []fieldAccessor, depth int) string {
	desc := s.root
	for i := 0; i < depth && desc != nil; i++ {
		if path[i].ordinal < 0 || path[i].ordinal >= desc.Fields().Len() {
			desc = nil
			break
		}
		desc = elementMessage(desc.Fields().Get(path[i].ordinal).Message())
	}
	if desc == nil || path[depth].ordinal < 0 || path[depth].ordinal >= desc.Fields().Len() {
		return path[depth].name
	}
	return string(desc.Fields().Get(path[depth].ordinal).Name())
}

// elementMessage is md itself, or the element message of a nullable array's
// wrapper (`message M { repeated E values = 1; }`, Java's
// NullableArrayTypeUtils).
func elementMessage(md protoreflect.MessageDescriptor) protoreflect.MessageDescriptor {
	if md == nil || md.Fields().Len() != 1 {
		return md
	}
	fd := md.Fields().Get(0)
	if fd.Cardinality() != protoreflect.Repeated || fd.IsMap() || string(fd.Name()) != "values" {
		return md
	}
	if inner := fd.Message(); inner != nil {
		return inner
	}
	return md
}

// reorderValues puts the ORDER BY values first, in ORDER BY order, then the
// remaining projection values in projection order (MaterializedViewIndex-
// Generator.reorderValues: `keyValues.contains(value)`, Java equality).
func reorderValues(qv *quantifierValues, vals, orderBy []values.Value) ([]values.Value, error) {
	if len(vals) < len(orderBy) {
		return nil, api.NewError(api.ErrCodeInternalError,
			"index generator: more ORDER BY values than projected values")
	}
	if len(orderBy) == 0 {
		return vals, nil
	}
	out := append(make([]values.Value, 0, len(vals)), orderBy...)
	for _, v := range vals {
		inOrderBy := false
		for _, ov := range orderBy {
			if qv.equalValues(ov, v) {
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

// generateValue is the value/version arm (generate(), :108-110 with
// translateToKeyExpression and splitKeyFromValue): the ORDER BY columns lead
// the key, the rest of the projection follows as the index's value when there
// is an ORDER BY, and a version column makes it a VERSION index.
func generateValue(c *specCollector, spec *indexSpec, res storageNames) (*GeneratedIndex, error) {
	fieldValues := fieldValuesOf(spec.projection)
	var orderBy []values.Value
	var orderingFns map[values.Value]string
	if spec.orderBy != nil {
		orderBy, orderingFns = spec.orderBy.values, spec.orderBy.functions
	}
	reordered, err := reorderValues(c.qv, fieldValues, orderBy)
	if err != nil {
		return nil, err
	}
	expr, err := generateKeyExpression(reordered, orderingFns, res)
	if err != nil {
		return nil, err
	}
	// Split point: the ORDER BY length; no split without an ORDER BY (both
	// DDL forms keep emptyKeyAllowed false, the vector index being the only
	// caller that flips it).
	root := expr
	if len(orderBy) > 0 && len(orderBy) < len(fieldValues) {
		root = recordlayer.KeyWithValue(expr, len(orderBy))
	}
	indexType := recordlayer.IndexTypeValue
	for _, v := range fieldValues {
		if isVersionColumn(v) {
			indexType = recordlayer.IndexTypeVersion
		}
	}
	return &GeneratedIndex{TableName: spec.recordType, Root: root, IndexType: indexType}, nil
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
		_, isField := vals[i].(*pathColumn)
		if !isField {
			expr, err := leafKeyExpression(vals[i], orderingFns, res)
			if err != nil {
				return nil, err
			}
			components = append(components, expr)
			i++
			continue
		}
		node, next, err := computeTrieForValues(vals, i, res)
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
	return recordlayer.Concat(components...), nil
}

// fieldTrieNode is the Go form of FieldValueTrieNode
// (FieldValueTrieNode.java): a compressed trie over resolved accessor paths.
// value is non-nil at a leaf that terminates a projected FieldValue.
type fieldTrieNode struct {
	value    *pathColumn
	children []trieChild // insertion-ordered (Java's ImmutableMap preserves it)
}

type trieChild struct {
	accessor fieldAccessor
	node     *fieldTrieNode
}

// accessorKeyEqual is trie-KEY equality. Java keys a trie node's children in a
// map by ResolvedAccessor: a plain accessor hashes by ordinal and an
// AnnotatedAccessor by ordinal and marker, so as map keys a plain and an
// annotated accessor of one column never meet, and two unnests of one array
// (two markers) are two children (QuantifierValues.java, AnnotatedAccessor).
// A path's prefix test is Java's asymmetric equals instead (fieldAccessor.equal,
// prefixMatches).
func accessorKeyEqual(a, b fieldAccessor) bool {
	return a.ordinal == b.ordinal && a.marker == b.marker
}

// computeTrieForValues consumes the maximal run of FieldValues starting at
// start whose paths extend the empty prefix, building the trie. Returns the
// node and the index one past the consumed run.
// Ports FieldValueTrieNode.computeTrieForValues (FieldValueTrieNode.java:201-238).
func computeTrieForValues(vals []values.Value, start int, res storageNames) (*fieldTrieNode, int, error) {
	node, next, err := computeTrieAtDepth(vals, start, 0, res)
	if err != nil {
		return nil, 0, err
	}
	return node, next, nil
}

func computeTrieAtDepth(vals []values.Value, start, depth int, res storageNames) (*fieldTrieNode, int, error) {
	node := &fieldTrieNode{}
	i := start
	for i < len(vals) {
		fv, ok := vals[i].(*pathColumn)
		if !ok {
			break
		}
		path := fv.steps
		// Java tests the WHOLE path against the prefix — equals, then
		// isPrefixOf (FieldValueTrieNode.java:212-218) — so the prefix check
		// comes first. Testing only the path's LENGTH first took a top-level
		// column that follows a nested one (`s.x, ts`: ts has length 1 under
		// prefix [S]) for the end of the S subtree, dropping S's children and
		// rendering field(S) where Java renders concat(field(S).nest(X), TS).
		if !prefixMatches(vals, start, i, depth, res) {
			break
		}
		if len(path) == depth {
			// The path equals the prefix: it terminates here.
			if depth == 0 {
				break // a zero-length path cannot occur (FieldPath is non-empty)
			}
			return &fieldTrieNode{value: fv}, i + 1, nil
		}
		acc := path[depth]
		// A duplicate child key = the same nested field path referenced twice
		// non-adjacently under one parent (FieldValueTrieNode.java:249-253).
		for _, c := range node.children {
			if accessorKeyEqual(c.accessor, acc) {
				return nil, 0, overlapError()
			}
		}
		child, next, err := computeTrieAtDepth(vals, i, depth+1, res)
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
//
// The prefix is the receiver of the comparison, as Java's FieldPath.isPrefixOf
// compares with the PREFIX side's equals: a plain prefix accessor admits a
// marked one, a marked prefix accessor only the same marker.
func prefixMatches(vals []values.Value, start, i, depth int, res storageNames) bool {
	baseField, ok := vals[start].(*pathColumn)
	if !ok {
		return false
	}
	cur, ok := vals[i].(*pathColumn)
	if !ok {
		return false
	}
	base, path := baseField.steps, cur.steps
	if len(path) < depth || len(base) < depth {
		return false
	}
	for k := 0; k < depth; k++ {
		if !base[k].equal(path[k]) {
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
		if len(c.node.children) > 0 {
			// A nesting parent (ValueToKeyExpressionVisitor.trieToKeyExpression):
			// the step renders as a field, FanOut when an unnest reached it, and
			// nests the rest.
			fanType, err := accessorFanType(c.accessor, name, recordlayer.FanTypeFanOut)
			if err != nil {
				return nil, err
			}
			childExpr, err := trieKeyExpression(c.node, orderingFns, res, childPath)
			if err != nil {
				return nil, err
			}
			if fanType == recordlayer.FanTypeFanOut {
				parts = append(parts, recordlayer.NestFanOut(name, childExpr))
			} else {
				parts = append(parts, recordlayer.Nest(name, childExpr))
			}
			continue
		}
		leaf, err := fieldLeafExpression(c.accessor, name, c.node.value)
		if err != nil {
			return nil, err
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

// accessorFanType is fieldAccessorToKeyExpression's fan type
// (ValueToKeyExpressionVisitor): a non-array field is None; an array is
// indexable only through an unnest, which marks its accessor, or materialized
// whole (fanTypeForArray Concatenate, under CARDINALITY) — any other array
// reference is the target's 0A000.
func accessorFanType(acc fieldAccessor, name string, fanTypeForArray recordlayer.FanType) (recordlayer.FanType, error) {
	if acc.typ == nil || acc.typ.Code() != values.TypeCodeArray {
		return recordlayer.FanTypeNone, nil
	}
	if acc.marker == 0 && fanTypeForArray != recordlayer.FanTypeConcatenate {
		return 0, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"Unsupported index definition, cannot create index on array field '%s' without unnesting", name)
	}
	return fanTypeForArray, nil
}

// fieldLeafExpression builds the field expression for a terminal accessor
// (fieldAccessorToKeyExpression with FanOut): the __ROW_VERSION pseudo-field is
// the version key expression, an unnested array a FanOut field, any other
// array refused, anything else a scalar field.
//
// name is the accessor's STORAGE name (storageNames.fieldName); acc.name is
// only consulted for the __ROW_VERSION pseudo-field check, whose name is
// identical in both namespaces. leafValue supplies the field's type where the
// accessor carries none.
func fieldLeafExpression(acc fieldAccessor, name string, leafValue *pathColumn) (recordlayer.KeyExpression, error) {
	// The __ROW_VERSION pseudo-field renders as the version key expression —
	// name AND type must both match (ValueToKeyExpressionVisitor.isRowVersion).
	if leafValue != nil && values.IsRowVersionPseudoField(acc.name, leafValue.typ) {
		return recordlayer.VersionKey(), nil
	}
	fanType, err := accessorFanType(acc, name, recordlayer.FanTypeFanOut)
	if err != nil {
		return nil, err
	}
	if fanType == recordlayer.FanTypeFanOut {
		return recordlayer.FanOut(name), nil
	}
	return recordlayer.Field(name), nil
}

// isVersionColumn reports whether v is the __ROW_VERSION pseudo-column, by the
// type Java's Projection.versionValues filters on.
func isVersionColumn(v values.Value) bool {
	p, ok := v.(*pathColumn)
	return ok && p.typ != nil && p.typ.Code() == values.TypeCodeVersion && p.typ.IsNullable()
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
	if field, ok := v.(*pathColumn); ok {
		return fieldPathExpression(field, recordlayer.FanTypeFanOut, res)
	}
	switch val := v.(type) {
	case *values.CardinalityValue:
		// CARDINALITY consumes the materialised array: the field is accessed
		// with Concatenate, not FanOut (:555-566).
		child, ok := val.Child.(*pathColumn)
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
		if _, err := encapsulateLane(name, []values.Value{val.Left, val.Right}); err != nil {
			return nil, err
		}
		return recordlayer.FunctionExpr(name, recordlayer.Concat(left, right)), nil
	case *values.ConstantValue:
		// LiteralValue → Key.Expressions.value(literal) (:576-577).
		carrier, err := literalKeyCarrier(val)
		if err != nil {
			return nil, err
		}
		return recordlayer.Literal(carrier), nil
	default:
		return nil, unableToConstruct()
	}
}

// literalKeyCarrier returns the Go value whose Value proto matches the one Java
// stores for the same literal. Java's LiteralValue holds the boxed Java type of
// the literal's static type, and Key.Expressions.value(Integer) serialises as
// int_value while value(Long) serialises as long_value. Go's query runtime keeps
// every integer literal on an int64 carrier and every floating one on float64,
// whatever its static width, so the carrier has to be narrowed to the static
// type at this boundary. Without the narrowing Go stored long_value 10000 for
// the entry size bitmap_bucket_offset injects, and Java then could not plan any
// query over that table: its encapsulation of the stored (LONG, LONG) function
// failed where Java's own (LONG, INT) form succeeds.
//
// An INT-typed value outside the int32 range cannot be narrowed without changing
// the stored bytes, and the SQL literal typing never produces one (a literal is
// INT only when it fits, ParseHelpers.java:96-98), so reaching it is a typing
// defect upstream: it is refused instead of wrapping silently into the index
// definition. A FLOAT-typed float64 carries a value already rounded to float32
// (FLOAT literals are parsed as float32), so the float narrowing is exact.
//
// The carrier is decided by the STATIC type, never by the Go kind the value
// happens to arrive in: an INT- or LONG-typed constant held in any Go integer
// kind (a platform int, an int32, an unsigned kind) is carried as int32 or int64
// respectively, so no Go kind can reach the wire as the other width.
//
// Every (static type, Go kind) pair is either one of the pairs below or refused:
// an unrecognised pair (a FLOAT held in an integer kind, a STRING held in
// anything but a string) would otherwise reach the wire with whatever carrier
// the Go kind happens to have, the class of defect this function exists to end.
//
// A NULL literal is carried as nil whatever its type. A non-NULL value with no
// static type is refused: the type is what decides the carrier.
func literalKeyCarrier(c *values.ConstantValue) (any, error) {
	if c.Value == nil {
		return nil, nil
	}
	if c.Typ == nil {
		return nil, noLiteralKeyCarrier(c)
	}
	switch c.Typ.Code() {
	case values.TypeCodeInt, values.TypeCodeLong:
		n, ok, err := literalInteger(c.Value)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, noLiteralKeyCarrier(c)
		}
		if c.Typ.Code() == values.TypeCodeLong {
			return n, nil
		}
		if n < math.MinInt32 || n > math.MaxInt32 {
			return nil, api.NewErrorf(api.ErrCodeInternalError,
				"INT literal %d in an index definition does not fit in 32 bits", n)
		}
		return int32(n), nil
	case values.TypeCodeFloat:
		switch v := c.Value.(type) {
		case float32:
			return v, nil
		case float64:
			return float32(v), nil
		}
	case values.TypeCodeDouble:
		switch v := c.Value.(type) {
		case float64:
			return v, nil
		case float32:
			return float64(v), nil
		}
	case values.TypeCodeString:
		if v, ok := c.Value.(string); ok {
			return v, nil
		}
	case values.TypeCodeBoolean:
		if v, ok := c.Value.(bool); ok {
			return v, nil
		}
	case values.TypeCodeBytes:
		if v, ok := c.Value.([]byte); ok {
			return v, nil
		}
	}
	return nil, noLiteralKeyCarrier(c)
}

// noLiteralKeyCarrier is the refusal of a literal whose static type and Go kind
// are not a pair literalKeyCarrier knows how to carry.
func noLiteralKeyCarrier(c *values.ConstantValue) error {
	typ := "no static type"
	if c.Typ != nil {
		typ = "type " + c.Typ.String()
	}
	return api.NewErrorf(api.ErrCodeInternalError,
		"literal %v (%T) of %s in an index definition has no key carrier", c.Value, c.Value, typ)
}

// literalInteger reads an integer constant held in any Go integer kind as an
// int64. ok is false for a non-integer value, which the caller refuses;
// an unsigned value beyond int64 cannot be an INT or LONG literal and is refused.
func literalInteger(v any) (n int64, ok bool, err error) {
	switch x := v.(type) {
	case int64:
		return x, true, nil
	case int:
		return int64(x), true, nil
	case int32:
		return int64(x), true, nil
	case int16:
		return int64(x), true, nil
	case int8:
		return int64(x), true, nil
	case uint8:
		return int64(x), true, nil
	case uint16:
		return int64(x), true, nil
	case uint32:
		return int64(x), true, nil
	case uint:
		if uint64(x) > math.MaxInt64 {
			return 0, false, api.NewErrorf(api.ErrCodeInternalError,
				"integer literal %d in an index definition does not fit in 64 bits", x)
		}
		return int64(x), true, nil
	case uint64:
		if x > math.MaxInt64 {
			return 0, false, api.NewErrorf(api.ErrCodeInternalError,
				"integer literal %d in an index definition does not fit in 64 bits", x)
		}
		return int64(x), true, nil
	}
	return 0, false, nil
}

// encapsulateLane is ArithmeticValue.encapsulate's check as the target runs it
// at the index clause (ArithmeticValue.java:213-231), where it builds the
// index's Values: each operand's type must be primitive (a SemanticException
// otherwise), and the operator must have a lane over the two types (a
// VerifyException otherwise), both XX000 with the target's message. Operands
// are typed as Java types them (arithmeticOperandType), and an operand is
// checked before the call that holds it, as Java encapsulates bottom-up, since
// valueKeyExpression recurses into the operands first. Without this, Go's DDL
// stored a key the target can never plan (RFC-257 WS-J section 3.2).
func encapsulateLane(function string, operands []values.Value) (values.ArithmeticLane, error) {
	if len(operands) != 2 {
		return values.ArithmeticLane{}, api.NewErrorf(api.ErrCodeInternalError,
			"arithmetic operator %s takes two operands, not %d", function, len(operands))
	}
	left, right := arithmeticOperandType(operands[0]), arithmeticOperandType(operands[1])
	lane, err := values.EncapsulateArithmeticLane(function, left, right)
	if err != nil {
		return values.ArithmeticLane{}, api.NewError(api.ErrCodeInternalError, err.Error())
	}
	return lane, nil
}

// arithmeticOperandType is the type code of the Value Java builds for v as an
// arithmetic operand: a nested arithmetic, bit or bitmap operator by its lane's
// result (the lane it was constructed with, or, when its operands' types were
// unknown then, the lane they resolve to now), anything else by its Type.
func arithmeticOperandType(v values.Value) values.TypeCode {
	if val, ok := v.(*values.ArithmeticValue); ok {
		if lane, ok := val.Lane(); ok {
			return lane.Result
		}
		if name, err := arithmeticFunctionName(val.Op); err == nil {
			if lane, err := encapsulateLane(name, []values.Value{val.Left, val.Right}); err == nil {
				return lane.Result
			}
		}
		return values.TypeCodeUnknown
	}
	return v.Type().Code()
}

func unableToConstruct() error {
	return api.NewError(api.ErrCodeUnsupportedOperation, "unable to construct expression")
}

// arithmeticFunctionName is the op's key-expression function name, Java's
// lowercase logical operator name (ArithmeticValue.getLogicalOperator().name()
// .toLowerCase(), MaterializedViewIndexGenerator.java:574): add..mod, the bit
// operators (IndexTest.java pins bitand/bitor/bitxor) and the bitmap
// functions, whose second argument is the walker-injected INT entry size,
// stored as int_value as Java stores it (SemanticAnalyzer.java:1115).
func arithmeticFunctionName(op values.ArithmeticOp) (string, error) {
	if name := op.LogicalOperatorName(); name != "" {
		return name, nil
	}
	return "", unableToConstruct()
}

// fieldPathExpression renders a FieldValue's resolved accessor path as nested
// field expressions — ValueToKeyExpressionVisitor.fieldPathToKeyExpression:
// every step through fieldAccessorToKeyExpression with the same fan type for
// an array (FanOut, or Concatenate under CARDINALITY), an array step admitted
// only through an unnest's marker or as Concatenate.
func fieldPathExpression(fv *pathColumn, fanTypeForArray recordlayer.FanType, res storageNames) (recordlayer.KeyExpression, error) {
	accs := fv.steps
	if len(accs) == 0 {
		return nil, internalError("an empty field path")
	}
	// The __ROW_VERSION pseudo-field is always a single top-level accessor;
	// it renders as the version key expression (name AND type).
	if len(accs) == 1 && values.IsRowVersionPseudoField(accs[0].name, fv.typ) {
		return recordlayer.VersionKey(), nil
	}
	last := len(accs) - 1
	leafName := res.fieldName(accs, last)
	fanType, err := accessorFanType(accs[last], leafName, fanTypeForArray)
	if err != nil {
		return nil, err
	}
	var expr recordlayer.KeyExpression
	switch fanType {
	case recordlayer.FanTypeFanOut:
		expr = recordlayer.FanOut(leafName)
	case recordlayer.FanTypeConcatenate:
		expr = recordlayer.FieldConcatenate(leafName)
	default:
		expr = recordlayer.Field(leafName)
	}
	for i := last - 1; i >= 0; i-- {
		name := res.fieldName(accs, i)
		stepFan, err := accessorFanType(accs[i], name, fanTypeForArray)
		if err != nil {
			return nil, err
		}
		switch stepFan {
		case recordlayer.FanTypeFanOut:
			expr = recordlayer.NestFanOut(name, expr)
		case recordlayer.FanTypeNone:
			expr = recordlayer.Nest(name, expr)
		default:
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
				"Unsupported index definition, cannot nest through array field '%s' materialized whole", name)
		}
	}
	return expr, nil
}
