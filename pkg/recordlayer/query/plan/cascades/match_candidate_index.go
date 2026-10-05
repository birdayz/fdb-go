package cascades

import (
	"strings"
	"sync"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"google.golang.org/protobuf/proto"
)

// ValueIndexScanMatchCandidate represents a secondary index as a
// match candidate. Each index key column has a corresponding sargable
// alias; predicate matching binds comparisons to these aliases to
// determine which prefix of the index key can be used for a scan.
//
// Ports the consumed surface of Java's `ValueIndexScanMatchCandidate`:
// the candidate expression Traversal (built lazily), key-column names +
// sargable aliases, per-column function bridges (CARDINALITY), and
// duplicate-creation tracking. Java additionally materializes
// index-value Values and ordering alias lists on the candidate;
// Go derives ordering from the column list at match-adjustment time
// (matched_ordering_part.go) instead.
type ValueIndexScanMatchCandidate struct {
	indexName       string
	recordTypes     []string
	columnNames     []string
	pkColumnNames   []string
	sargableAliases []values.CorrelationIdentifier
	flowedType      values.Type
	unique          bool
	// keyComponentTypes is aligned with columnNames and comes from the
	// descriptor plus expanded key expression, not from query comparands.
	keyComponentTypes []values.Type
	// primaryKeyComponentTypes is aligned with pkColumnNames and controls
	// whether the appended index-entry PK suffix is a sound ordering source.
	primaryKeyComponentTypes []values.Type

	// recordTypeRowTypes are the per-record-type row layouts the index serves,
	// one per entry of recordTypes — the descriptor-shaped positional types
	// covering-column resolution needs (RFC-197 item 1). flowedType is the
	// SINGLE-type answer and degrades to UnknownType for a multi-type index,
	// which has no one layout; this slice keeps each type's layout separately
	// so a covering push can still be proven per type. Empty when the def
	// supplies none, in which case flowedType is the only layout known.
	recordTypeRowTypes []values.Type

	// columnFunctions is parallel to columnNames: columnFunctions[i] names
	// the function wrapping the i-th key column's field, or "" if the column
	// is a plain field. The only non-empty entry today is
	// FunctionKindCardinality ("cardinality"), the bridge for a CARDINALITY()
	// index — the candidate's i-th column Value is then
	// CardinalityValue(FieldValue(col)) instead of a bare FieldValue. This is
	// the Go analog of Java's index match candidate carrying the column's
	// Value (CardinalityFunctionKeyExpression.toValue() on the candidate side),
	// so predicate and sort matching compare the SAME Value the query side
	// builds. When nil, every column is a plain field (the common case).
	columnFunctions []string

	// createsDuplicates is true when the index's root expression can
	// produce multiple entries per record (fan-out / repeated-field
	// indexes). Ports Java's index.getRootExpression().createsDuplicates().
	// When the optional signal is missing or stale, a structured FAN_OUT root
	// remains authoritative through CreatesDuplicates.
	// predicateProto is the stored sparse-index predicate
	// (RecordMetaDataProto.Index.predicate) when the index is filtered;
	// nil for a full index. See WithPredicateProto.
	predicateProto *gen.Predicate

	// opaqueFilter records that this index filters records through a predicate
	// with no proto representation (Go's Index.SetPredicate closure). Sparseness
	// and its serialized form are different questions; this carries the FACT so
	// consumers stop inferring it from the representation. See WithOpaqueFilter.
	opaqueFilter bool

	createsDuplicates bool
	// createsDuplicatesKnown reports whether the fan-out status was supplied
	// by the IndexDef. When false (no signal), the DistinctRecords property
	// abstains (distinct=false) rather than assuming non-fan-out — otherwise a
	// fan-out index whose def omits the signal would over-report distinct and
	// let an enclosing DISTINCT be wrongly elided. Mirrors Java's
	// empty-match-candidate default in DistinctRecordsProperty.
	createsDuplicatesKnown bool

	// commonPrimaryKeyValues is the index's common primary key translated to
	// structure-encoding Values (RFC-189 B3), threaded from the IndexDef and
	// stamped onto the RecordQueryIndexPlan for PrimaryKeyProperty. Populated
	// regardless of fan-out (index entries always carry the PK); nil when the
	// def supplies no structural PK (the property then abstains).
	commonPrimaryKeyValues []values.Value

	// valueColumnNames are the covering-only columns of a KeyWithValue root —
	// the inner key's columns past the split point, stored in the FDB VALUE.
	// They are never sargable and never order the scan (the physical entry key
	// is key columns + primary key); their sole role is covering translation.
	// Java's candidate carries them as indexValueValues, populated by the
	// expansion's valueValues list (ValueIndexExpansionVisitor.java:109-121).
	// Nil for a non-covering root.
	valueColumnNames []string

	// rootKeyExpression is the record-layer key-expression AST when metadata
	// exposes it. Fan-out expansion needs the tree topology (Then/Nesting and
	// the exact FAN_OUT node); the historical flat columnNames slice cannot
	// distinguish a scalar field from a repeated field or preserve a shared
	// fan-out parent. It is always stored as a defensive clone.
	rootKeyExpression *gen.KeyExpression

	columnsOnce sync.Once
	columns     *valueIndexExpansion
	expansion   *valueIndexExpansion

	// pkEntryOrdinals is IndexDefWithPrimaryKeyEntryOrdinals, aligned with
	// pkColumnNames; nil when the def does not state it.
	pkEntryOrdinals []int

	logicalRecordOnce sync.Once
	logicalRecord     *indexEntryToLogicalRecord
}

// indexEntryToLogicalRecord is ScanWithFetchMatchCandidate's
// IndexEntryToLogicalRecord: the reader that builds the queried record from an
// entry, and the record's fields it covers.
type indexEntryToLogicalRecord struct {
	reader        *values.RecordConstructorValue
	logicalFields []values.FieldValue
}

// WithRecordTypeRowTypes attaches the per-record-type row layouts to a freshly
// constructed candidate (RFC-197 item 1). Call before the translate function is
// built. A nil/empty slice leaves the candidate with flowedType as its only
// layout — the single-record-type case, and fail-closed for a multi-type index.
func (c *ValueIndexScanMatchCandidate) WithRecordTypeRowTypes(rowTypes []values.Type) *ValueIndexScanMatchCandidate {
	c.recordTypeRowTypes = append([]values.Type(nil), rowTypes...)
	return c
}

// WithKeyComponentTypes attaches authoritative physical index-key types to a
// freshly constructed candidate.
func (c *ValueIndexScanMatchCandidate) WithKeyComponentTypes(types []values.Type) *ValueIndexScanMatchCandidate {
	c.keyComponentTypes = normalizePhysicalKeyTypes(types, len(c.columnNames))
	return c
}

// GetKeyComponentTypes returns physical types aligned with index columns.
func (c *ValueIndexScanMatchCandidate) GetKeyComponentTypes() []values.Type {
	return append([]values.Type(nil), c.keyComponentTypes...)
}

// WithPrimaryKeyComponentTypes attaches authoritative physical types aligned
// with GetPKColumnNames to a freshly constructed candidate.
func (c *ValueIndexScanMatchCandidate) WithPrimaryKeyComponentTypes(types []values.Type) *ValueIndexScanMatchCandidate {
	c.primaryKeyComponentTypes = normalizePhysicalKeyTypes(types, len(c.pkColumnNames))
	return c
}

// WithPrimaryKeyEntryOrdinals attaches the entry KEY positions of the primary
// key columns to a freshly constructed candidate.
func (c *ValueIndexScanMatchCandidate) WithPrimaryKeyEntryOrdinals(ordinals []int) *ValueIndexScanMatchCandidate {
	if len(ordinals) == len(c.pkColumnNames) {
		c.pkEntryOrdinals = append([]int(nil), ordinals...)
	}
	return c
}

// indexEntryToLogicalRecord is the candidate's logical record, or nil when an
// entry cannot be read into one.
func (c *ValueIndexScanMatchCandidate) indexEntryToLogicalRecord() *indexEntryToLogicalRecord {
	c.logicalRecordOnce.Do(func() { c.logicalRecord = c.computeIndexEntryToLogicalRecord() })
	return c.logicalRecord
}

// computeIndexEntryToLogicalRecord is
// ScanWithFetchMatchCandidate.computeIndexEntryToLogicalRecord over the entry's
// KEY (index columns, then the trimmed primary key) and VALUE columns. Java
// also refuses a builder missing a proto-required field or covering a repeated
// one; Go rows carry no required-ness, and no array is ever extracted.
func (c *ValueIndexScanMatchCandidate) computeIndexEntryToLogicalRecord() *indexEntryToLogicalRecord {
	expansion := c.indexExpansion()
	if expansion == nil || len(c.recordTypes) != 1 {
		return nil
	}
	baseType, ok := c.flowedType.(*values.RecordType)
	if !ok {
		return nil
	}
	alias := expansion.base.Correlation()
	covered := &values.IndexEntryRecordReader{}
	var logical []values.FieldValue
	add := func(v values.Value, source values.TupleSource, ordinal int) bool {
		field, reader, ok := values.ExtractFromIndexEntry(v, alias, source, []int{ordinal})
		if !ok {
			return true
		}
		if !covered.CoverField(field, reader) {
			return false
		}
		logical = append(logical, field)
		return true
	}
	for i, keyValue := range expansion.keyValues {
		if !add(keyValue, values.TupleSourceKey, i) {
			return nil
		}
	}
	for j, column := range c.pkColumnNames {
		if j >= len(c.pkEntryOrdinals) || c.pkEntryOrdinals[j] < 0 {
			continue
		}
		field, err := resolveKeyFieldPath(expansion.base, []string{column})
		if err != nil {
			return nil
		}
		if !add(field, values.TupleSourceKey, c.pkEntryOrdinals[j]) {
			return nil
		}
	}
	for i, valueValue := range expansion.valueValues {
		if !add(valueValue, values.TupleSourceValue, i) {
			return nil
		}
	}
	return &indexEntryToLogicalRecord{reader: covered.ToRecordValue(baseType), logicalFields: logical}
}

// GetPrimaryKeyComponentTypes returns types aligned with pkColumnNames.
func (c *ValueIndexScanMatchCandidate) GetPrimaryKeyComponentTypes() []values.Type {
	return append([]values.Type(nil), c.primaryKeyComponentTypes...)
}

// rowLayouts returns every row layout this candidate's records can have: the
// per-record-type layouts when the def supplied them, else the single flowed
// type.
func (c *ValueIndexScanMatchCandidate) rowLayouts() []values.Type {
	if len(c.recordTypeRowTypes) > 0 {
		return c.recordTypeRowTypes
	}
	return []values.Type{c.flowedType}
}

// orderingKeyLayout returns the ONE row layout this candidate's ordering keys
// may be domained in, or nil.
//
// The layout is the RECORD's row layout, never the per-index entry layout, and
// that is what makes the intersection merge work: a primary-key intersection's
// premise is exactly one common record type, and its comparison keys exist to
// be compared ACROSS legs. Per-index domains would hand two legs two different
// tokens for the same primary-key column, and the merged ordering would be
// empty. It also sits consistently beside the *RecordTypeValue discriminators
// that share the same comparison-key list — those are record-level too.
//
// Fails closed on more than one layout rather than picking one: a multi-record
// -type index has no single row whose ordinals mean anything, and choosing
// layouts[0] would stamp one record type's ordinals onto another's rows.
func (c *ValueIndexScanMatchCandidate) orderingKeyLayout() *values.RecordType {
	layouts := c.rowLayouts()
	if len(layouts) != 1 {
		return nil
	}
	rt, isRecord := layouts[0].(*values.RecordType)
	if !isRecord {
		return nil
	}
	return rt
}

// bakeOrderingColumn resolves a METADATA column name (an index key column, a
// primary-key column) to a domained ordinal in the candidate's record row
// layout, so the ordering key it mints carries an identity instead of a display
// name. A name that does not resolve returns nil: unresolved FieldValues are no
// longer Values, and an unaddressable ordering key must decline the claim.
//
// Resolution is UNIQUE-match, matching the runtime authority this key is
// verified against (bakedIntersectionKeys): first-matching a name against a
// duplicate-named layout would silently bake some other column's slot. A key
// column is baked by its whole field path (bakeOrderingPath), so a nested leaf
// never stands for a top-level field of the same name.
func (c *ValueIndexScanMatchCandidate) bakeOrderingColumn(name string) values.Value {
	return bakeOrderingColumnIn(c.orderingKeyLayout(), name)
}

// bakeOrderingPath is bakeOrderingColumn over a key column's field path, each
// step resolved by its unique name in the record it descends.
func (c *ValueIndexScanMatchCandidate) bakeOrderingPath(path []string) values.Value {
	layout := c.orderingKeyLayout()
	if len(path) == 1 {
		return bakeOrderingColumnIn(layout, path[0])
	}
	if layout == nil || len(path) == 0 {
		return nil
	}
	root, ok := orderingKeyCarrier(layout)
	if !ok {
		return nil
	}
	return resolveUpperFieldPath(root, path)
}

// keyColumnPath is key column i's field path (nil for a column that reads no
// field path, such as an exploded element).
func (c *ValueIndexScanMatchCandidate) keyColumnPath(i int) []string {
	described := c.indexColumns()
	if described == nil || i < 0 || i >= len(described.keyColumns) {
		return nil
	}
	return described.keyColumns[i].path
}

// nestedKeyColumnPath is keyColumnPath for a nested leaf only.
func (c *ValueIndexScanMatchCandidate) nestedKeyColumnPath(i int) []string {
	if path := c.keyColumnPath(i); len(path) > 1 {
		return path
	}
	return nil
}

// columnPaths returns the nested field path of every key and value column,
// nil for a top-level one, for the plan's column metadata.
func (c *ValueIndexScanMatchCandidate) columnPaths() (keyPaths, valuePaths [][]string) {
	described := c.indexColumns()
	if described == nil {
		return nil, nil
	}
	nested := func(columns []keyExpansionColumn) [][]string {
		var out [][]string
		for i, column := range columns {
			if len(column.path) > 1 {
				if out == nil {
					out = make([][]string, len(columns))
				}
				out[i] = column.path
			}
		}
		return out
	}
	return nested(described.keyColumns), nested(described.valueColumns)
}

// trimmableKeyColumnNames are the key columns a primary-key column of the same
// name is trimmed against: top-level fields only, as Java's trimPrimaryKey
// compares key expressions, and CARDINALITY(x) or S.X is not field X.
func (c *ValueIndexScanMatchCandidate) trimmableKeyColumnNames() []string {
	names := make([]string, 0, len(c.columnNames))
	for i, name := range c.columnNames {
		if i < len(c.columnFunctions) && c.columnFunctions[i] == FunctionKindCardinality {
			continue
		}
		if c.nestedKeyColumnPath(i) != nil {
			continue
		}
		names = append(names, name)
	}
	return names
}

// keyColumnCanExtendOrderingClaim is values.ColumnCanExtendOrderingClaim for
// key column i, answered for a nested leaf by the type its path reads.
func (c *ValueIndexScanMatchCandidate) keyColumnCanExtendOrderingClaim(i int) bool {
	path := c.nestedKeyColumnPath(i)
	if path == nil {
		return values.ColumnCanExtendOrderingClaim(c.orderingKeyLayout(), c.columnNames[i])
	}
	if i < len(c.keyComponentTypes) && c.keyComponentTypes[i] != nil &&
		c.keyComponentTypes[i].Code() != values.TypeCodeUnknown {
		return !values.TypeTerminatesOrderingClaim(c.keyComponentTypes[i])
	}
	leaf := c.bakeOrderingPath(path)
	return leaf != nil && !values.TypeTerminatesOrderingClaim(leaf.Type())
}

// floatQuestionColumnNames is columnNames with a nested leaf's name blanked:
// it is no top-level column, so a name-keyed float question must not find one
// (a blank name could be a float).
func (c *ValueIndexScanMatchCandidate) floatQuestionColumnNames() []string {
	names := append([]string(nil), c.columnNames...)
	for i := range names {
		if c.nestedKeyColumnPath(i) != nil {
			names[i] = ""
		}
	}
	return names
}

// bakeOrderingColumnIn is bakeOrderingColumn's body, shared with the
// primary-scan candidate: both mint their matched ordering parts from a
// metadata column-name list against the one record row layout the scan flows,
// and both feed the same set-operation merge, so they must agree on the domain
// token or the merge collapses.
func bakeOrderingColumnIn(layout *values.RecordType, name string) values.Value {
	if layout == nil {
		return nil
	}
	ordinal, unique := uniqueUpperFieldIndex(layout, name)
	if !unique {
		return nil
	}
	root, ok := orderingKeyCarrier(layout)
	if !ok {
		return nil
	}
	resolved, err := values.ResolveFieldOrdinals(root, []int{ordinal})
	if err != nil {
		return nil
	}
	return resolved
}

// resolvedColumnsInRow converts metadata column labels into immutable,
// QOV-rooted ordinal accesses against the one authoritative row layout. It is
// intentionally all-or-nothing: a missing or duplicate label makes the whole
// key unavailable instead of returning a partially typed key whose remaining
// coordinates could be mistaken for a complete primary key.
func resolvedColumnsInRow(layout values.Type, columns []string) []values.Value {
	if len(columns) == 0 {
		return nil
	}
	record, ok := layout.(*values.RecordType)
	if !ok || record == nil {
		return nil
	}
	root, ok := orderingKeyCarrier(record)
	if !ok {
		return nil
	}
	result := make([]values.Value, len(columns))
	for i, column := range columns {
		ordinal, unique := uniqueUpperFieldIndex(record, column)
		if !unique {
			return nil
		}
		resolved, err := values.ResolveFieldOrdinals(root, []int{ordinal})
		if err != nil {
			return nil
		}
		result[i] = resolved
	}
	return result
}

// orderingKeyCarrier mints the stable owner-current phase root for metadata
// ordering keys over a record row. Independent scan candidates over the same
// exact row type must state the same root: their primary-key suffixes are
// compared across legs when an intersection ordering is merged. A fresh unique
// correlation per candidate makes the same (domain, ordinal) look like two
// different columns and silently deletes the merge ordering.
//
// The values-owned layout factory is the only legal way to mint this tagged
// current root. The identity tile states the ordinary scan representation; no
// source windows are needed because these keys address the emitted row itself.
func orderingKeyCarrier(record *values.RecordType) (values.QuantifiedObjectValue, bool) {
	if record == nil {
		return nil, false
	}
	var tiles []values.OrdinalTileSpec
	if width := len(record.Fields); width > 0 {
		tiles = []values.OrdinalTileSpec{{Start: 0, Width: width, Kind: values.OrdinalTileFlat}}
	}
	layout, err := values.NewOrdinalLayoutForCarrierType(record, tiles, nil)
	if err != nil {
		return nil, false
	}
	return layout.Carrier(), true
}

// orderingColumnValue is ColumnValue for the ordering-key mint: the same Value
// shape, with the plain-field case carrying its resolved identity.
//
// A CARDINALITY column is no column of the row layout: it is the function over
// its argument baked like any key column, so it is minted on the same carrier
// root and compares as a whole Value against the request's.
func (c *ValueIndexScanMatchCandidate) orderingColumnValue(i int) values.Value {
	if i < len(c.columnFunctions) && c.columnFunctions[i] != "" {
		if _, isOrder := OrderFunctionDirection(c.columnFunctions[i]); !isOrder {
			if c.columnFunctions[i] != FunctionKindCardinality {
				return c.keyValueOverOrderingCarrier(i)
			}
			argument := c.bakeOrderingPath(c.keyColumnPath(i))
			if argument == nil {
				return nil
			}
			return values.NewCardinalityValue(argument)
		}
		// An order-wrapped column ORDERS BY its underlying field — the
		// wrapper only changes the physical encoding and the direction,
		// which columnMatchedSortOrder carries separately. Java derives the
		// same split by simplifying ToOrderedBytesValue(fv) to (fv,
		// direction) when minting ordering parts
		// (OrderingValueComputationRuleSet's ToOrderedBytes rule).
	}
	path := c.keyColumnPath(i)
	if len(path) == 0 {
		return nil
	}
	return c.bakeOrderingPath(path)
}

// keyValueOverOrderingCarrier is key column i's Value as the expansion built
// it (FunctionKeyExpression.toValue), re-rooted on the ordering carrier the
// baked columns use.
func (c *ValueIndexScanMatchCandidate) keyValueOverOrderingCarrier(i int) values.Value {
	expansion := c.indexExpansion()
	layout := c.orderingKeyLayout()
	if expansion == nil || layout == nil || i < 0 || i >= len(expansion.keyValues) ||
		expansion.keyValues[i] == nil {
		return nil
	}
	root, ok := orderingKeyCarrier(layout)
	if !ok {
		return nil
	}
	translation := values.NewTranslationMapBuilder().
		When(expansion.base.Correlation()).
		Then(func(_ values.CorrelationIdentifier, _ values.Value) values.Value { return root }).
		Build()
	translated, err := values.TranslateCorrelationsChecked(expansion.keyValues[i], translation)
	if err != nil {
		return nil
	}
	return translated
}

// WithValueColumns attaches the covering-only (FDB VALUE part) column names of
// a KeyWithValue-rooted index to a freshly constructed candidate. Names are
// upper-cased for the SQL-convention case-insensitive covering match, exactly
// like columnNames. Nil/empty clears the value part.
func (c *ValueIndexScanMatchCandidate) WithValueColumns(names []string) *ValueIndexScanMatchCandidate {
	if len(names) == 0 {
		c.valueColumnNames = nil
		return c
	}
	upper := make([]string, len(names))
	for i, n := range names {
		upper[i] = strings.ToUpper(n)
	}
	c.valueColumnNames = upper
	return c
}

// GetValueColumnNames returns the covering-only (FDB VALUE part) column names,
// or nil for a non-covering root.
func (c *ValueIndexScanMatchCandidate) GetValueColumnNames() []string {
	return c.valueColumnNames
}

// WithCommonPrimaryKey sets the index's structural common primary key on the
// (freshly constructed) candidate and returns it (RFC-189 B3).
func (c *ValueIndexScanMatchCandidate) WithCommonPrimaryKey(pk []values.Value) *ValueIndexScanMatchCandidate {
	c.commonPrimaryKeyValues = pk
	return c
}

// WithRootKeyExpression attaches a caller-owned copy of the record-layer key
// expression to a freshly constructed candidate. A nil root clears the
// optional structure. Call this before GetTraversal; traversal construction is
// deliberately stable under sync.Once.
func (c *ValueIndexScanMatchCandidate) WithRootKeyExpression(root *gen.KeyExpression) *ValueIndexScanMatchCandidate {
	if root == nil {
		c.rootKeyExpression = nil
		return c
	}
	c.rootKeyExpression = proto.Clone(root).(*gen.KeyExpression)
	return c
}

// WithPredicateProto marks the candidate as a SPARSE (filtered) index by
// attaching the stored index predicate (RecordMetaDataProto.Index.predicate).
// The expansion converts it into a candidate-side QueryPredicate
// (ValueIndexExpansionVisitor.java:138-162), so a query matches the candidate
// only when the matcher can account for the predicate — never as if the index
// held every record.
//
// This is where a predicate ENTERS the candidate machinery, and therefore where
// a predicate that provably filters NOTHING is resolved to no predicate at all.
// Sparseness is read back as `predicateProto != nil` in four separate places —
// the fan-out expansion arm, AbstractDataAccessRule's root-match restriction,
// candidatePreservesBaseRecordCardinality, and the flat expansion — and only
// the last converts the predicate before deciding. Classifying here is what
// keeps the other three from treating a COMPLETE index as filtered, which for
// the fan-out arm meant `WHERE TRUE` produced no candidate at all. Java gets
// the same effect structurally: the planner only ever sees an index predicate
// through IndexPredicate.toPredicate, whose folding constructors have already
// collapsed a tautology to the constant (IndexPredicate.java:344-345).
//
// Only a PROVED tautology is dropped; anything unprovable stays sparse.
func (c *ValueIndexScanMatchCandidate) WithPredicateProto(pred *gen.Predicate) *ValueIndexScanMatchCandidate {
	if pred == nil {
		c.predicateProto = nil
		return c
	}
	normalized := NormalizeIndexPredicateProto(pred)
	if constantPredicateArmIsTrue(normalized) {
		c.predicateProto = nil
		return c
	}
	c.predicateProto = normalized
	return c
}

// WithOpaqueFilter marks the candidate as filtering records through a predicate
// that has NO proto representation, so nothing can be attached to the candidate
// graph and no matcher can account for it.
//
// It is deliberately a SEPARATE flag from predicateProto rather than a synthetic
// predicate. A synthesized stand-in would have to be some concrete proto, and
// every such proto is either provably tautological (which normalizes away, back
// to "complete") or claims a filter shape the index does not actually have,
// which the matcher would then try to compensate. The honest statement is "a
// filter exists and its content is unknowable", and that is a different fact
// from any predicate.
//
// The only sound reading of an unknowable filter is the pessimistic one, so this
// flag is one-way: it can be set, never cleared, and unlike WithPredicateProto
// there is no tautology escape — a Go closure cannot be proved to reject
// nothing.
func (c *ValueIndexScanMatchCandidate) WithOpaqueFilter() *ValueIndexScanMatchCandidate {
	c.opaqueFilter = true
	return c
}

// HasOpaqueFilter reports whether this candidate filters records through a
// predicate the planner cannot inspect. Such an index is never COMPLETE: it
// cannot stand in for the base table, and its UNIQUE declaration constrains only
// the records its filter admitted.
func (c *ValueIndexScanMatchCandidate) HasOpaqueFilter() bool {
	return c != nil && c.opaqueFilter
}

// GetPredicateProto returns a defensive copy of the sparse-index predicate, or
// nil for a full index.
func (c *ValueIndexScanMatchCandidate) GetPredicateProto() *gen.Predicate {
	if c.predicateProto == nil {
		return nil
	}
	return proto.Clone(c.predicateProto).(*gen.Predicate)
}

// GetRootKeyExpression returns a defensive copy of the record-layer
// key-expression AST, or nil when the candidate was built from flat metadata.
func (c *ValueIndexScanMatchCandidate) GetRootKeyExpression() *gen.KeyExpression {
	if c.rootKeyExpression == nil {
		return nil
	}
	return proto.Clone(c.rootKeyExpression).(*gen.KeyExpression)
}

// GetCommonPrimaryKeyValues returns the index's structural common primary key
// (RFC-189 B3), or nil when the def supplied none.
func (c *ValueIndexScanMatchCandidate) GetCommonPrimaryKeyValues() []values.Value {
	return c.commonPrimaryKeyValues
}

// FunctionKindCardinality is the columnFunctions entry marking a key column as
// CARDINALITY(field). Matches the "cardinality" function-key name on the
// record-layer side so the bridge is name-stable across the two layers.
const FunctionKindCardinality = "cardinality"

// Order-function column tags — the four OrderFunctionKeyExpression names
// (OrderFunctionKeyExpressionFactory.java:44-48: "order_" + Direction
// lowercased). A key column carrying one stores the TupleOrdering encoding of
// its field, so its candidate-side Value is ToOrderedBytesValue(field,
// direction) (OrderFunctionKeyExpression.toValue,
// OrderFunctionKeyExpression.java:99-103) and its matched ordering direction
// is the function's, not the tuple-natural ascending.
const (
	FunctionKindOrderAscNullsFirst  = "order_asc_nulls_first"
	FunctionKindOrderAscNullsLast   = "order_asc_nulls_last"
	FunctionKindOrderDescNullsFirst = "order_desc_nulls_first"
	FunctionKindOrderDescNullsLast  = "order_desc_nulls_last"
)

// OrderFunctionDirection maps an order-function name to its TupleOrdering
// direction; ok=false for any other function name.
//
// EXACT match, not case-folded. Java never classifies by name at all — it holds
// the direction as a field on OrderFunctionKeyExpression and dispatches on the
// type — and it registers the four builders under names already lowercased
// (OrderFunctionKeyExpressionFactory: `FUNCTION_NAME_PREFIX +
// direction.name().toLowerCase(Locale.ROOT)`), so no upper-case spelling can
// name a built-in order function in either engine.
//
// Folding case here was not a harmless convenience. RegisterFunction is public
// API over a CASE-SENSITIVE map (key_expression.go), so an application may
// register its own evaluator under `ORDER_ASC_NULLS_FIRST`; the record layer
// would then encode that column with the application's function while the
// planner classified it as the built-in tuple-order encoding, deriving ordered
// ranges — or eliminating a sort — from bytes nothing ever wrote in that order.
// Exact match keeps this consistent with isOrderFunctionName
// (order_function_key_expression.go), which never folded.
func OrderFunctionDirection(name string) (values.OrderedBytesDirection, bool) {
	switch name {
	case FunctionKindOrderAscNullsFirst:
		return values.OrderedBytesAscNullsFirst, true
	case FunctionKindOrderAscNullsLast:
		return values.OrderedBytesAscNullsLast, true
	case FunctionKindOrderDescNullsFirst:
		return values.OrderedBytesDescNullsFirst, true
	case FunctionKindOrderDescNullsLast:
		return values.OrderedBytesDescNullsLast, true
	default:
		return 0, false
	}
}

// matchedSortOrderForDirection maps a TupleOrdering direction to the matched
// sort order of a FORWARD scan over entries encoded in that direction — the
// entry bytes ARE the direction's ordering, so a forward scan yields it
// directly and a reverse scan yields its polar opposite (Java: TupleOrdering
// .Direction ↔ OrderingPart sort-order mapping in OrderingValueComputationRuleSet).
func matchedSortOrderForDirection(dir values.OrderedBytesDirection) MatchedSortOrder {
	switch dir {
	case values.OrderedBytesAscNullsLast:
		return MatchedSortOrderAscendingNullsLast
	case values.OrderedBytesDescNullsFirst:
		return MatchedSortOrderDescendingNullsFirst
	case values.OrderedBytesDescNullsLast:
		return MatchedSortOrderDescending
	default:
		return MatchedSortOrderAscending
	}
}

// flipMatchedSortOrder is the polar opposite (reverse scan) of a matched sort
// order: ASC_NULLS_FIRST ↔ DESC_NULLS_LAST, ASC_NULLS_LAST ↔ DESC_NULLS_FIRST
// — byte-order reversal flips direction AND null placement together.
func flipMatchedSortOrder(s MatchedSortOrder) MatchedSortOrder {
	switch s {
	case MatchedSortOrderAscending:
		return MatchedSortOrderDescending
	case MatchedSortOrderDescending:
		return MatchedSortOrderAscending
	case MatchedSortOrderAscendingNullsLast:
		return MatchedSortOrderDescendingNullsFirst
	default:
		return MatchedSortOrderAscendingNullsLast
	}
}

// columnMatchedSortOrder returns the matched sort order of column i for a scan
// in the given direction: tuple-natural ascending for a plain column, the
// order function's direction for an order-wrapped one.
func (c *ValueIndexScanMatchCandidate) columnMatchedSortOrder(i int, isReverse bool) MatchedSortOrder {
	order := MatchedSortOrderAscending
	if i < len(c.columnFunctions) {
		if dir, isOrder := OrderFunctionDirection(c.columnFunctions[i]); isOrder {
			order = matchedSortOrderForDirection(dir)
		}
	}
	if isReverse {
		return flipMatchedSortOrder(order)
	}
	return order
}

// NewValueIndexScanMatchCandidate constructs a match candidate for a
// secondary index. columnNames and sargableAliases must be parallel
// slices in index key column order (left-to-right): columnNames[i] is
// the field name for the i-th key column, sargableAliases[i] is the
// correlation identifier used for predicate binding. This compatibility
// constructor has no duplicate/cardinality metadata, so the candidate
// deliberately remains UNKNOWN and cannot plan until a root key expression
// with FAN_OUT structure is attached. Metadata-backed scalar callers should
// use NewValueIndexScanMatchCandidateWithFunctions with an explicit false
// createsDuplicatesSignal.
func NewValueIndexScanMatchCandidate(
	indexName string,
	recordTypes []string,
	columnNames []string,
	sargableAliases []values.CorrelationIdentifier,
	flowedType values.Type,
	unique bool,
	pkColumnNames []string,
) *ValueIndexScanMatchCandidate {
	return NewValueIndexScanMatchCandidateWithFunctions(
		indexName, recordTypes, columnNames, nil, sargableAliases,
		flowedType, unique, pkColumnNames, nil,
	)
}

// NewValueIndexScanMatchCandidateWithFunctions is NewValueIndexScanMatchCandidate
// plus a parallel columnFunctions slice (see the struct field). Pass nil
// columnFunctions for an all-plain-field index. A non-empty columnFunctions[i]
// (FunctionKindCardinality) makes the i-th column's match Value
// CardinalityValue(FieldValue(col)) so a CARDINALITY() predicate/sort binds.
func NewValueIndexScanMatchCandidateWithFunctions(
	indexName string,
	recordTypes []string,
	columnNames []string,
	columnFunctions []string,
	sargableAliases []values.CorrelationIdentifier,
	flowedType values.Type,
	unique bool,
	pkColumnNames []string,
	createsDuplicatesSignal *bool,
) *ValueIndexScanMatchCandidate {
	aliases := make([]values.CorrelationIdentifier, len(sargableAliases))
	copy(aliases, sargableAliases)
	types := make([]string, len(recordTypes))
	copy(types, recordTypes)
	cols := make([]string, len(columnNames))
	copy(cols, columnNames)
	pkCols := make([]string, len(pkColumnNames))
	copy(pkCols, pkColumnNames)
	var fns []string
	if columnFunctions != nil {
		fns = make([]string, len(columnFunctions))
		copy(fns, columnFunctions)
	}
	return &ValueIndexScanMatchCandidate{
		indexName:         indexName,
		recordTypes:       types,
		columnNames:       cols,
		columnFunctions:   fns,
		pkColumnNames:     pkCols,
		sargableAliases:   aliases,
		flowedType:        flowedType,
		unique:            unique,
		keyComponentTypes: physicalTypesFromFlatRow(flowedType, cols, fns),
		// A flat row type proves the carrier of a visible field, but it does
		// not prove that the field is the next physical PK coordinate. The PK
		// can contain an unrepresented RecordTypeKey, literal, version, nested,
		// or function component before it. Only the metadata adapter can prove
		// that topology, through WithPrimaryKeyComponentTypes.
		primaryKeyComponentTypes: normalizePhysicalKeyTypes(nil, len(pkCols)),
		createsDuplicates:        createsDuplicatesSignal != nil && *createsDuplicatesSignal,
		createsDuplicatesKnown:   createsDuplicatesSignal != nil,
	}
}

// ColumnValue returns the match Value for the i-th index key column over the
// given base (the QuantifiedObjectValue of the index's record source). For a
// plain field this is FieldValue(base, col); for a CARDINALITY()-keyed column
// it is CardinalityValue(FieldValue(base, col)). This is the single source of
// truth the predicate-placeholder expansion AND the ordered-index-scan sort
// matching both consult, so a CARDINALITY() query value binds to the index by
// Value-tree equality (Java: the match candidate carries the column's Value).
func (c *ValueIndexScanMatchCandidate) ColumnValue(i int, base values.Value) values.Value {
	described := c.indexColumns()
	if described == nil || i < 0 || i >= len(described.keyColumns) {
		return nil
	}
	column := described.keyColumns[i]
	if len(column.path) == 0 {
		return nil
	}
	if base == nil {
		layout := c.orderingKeyLayout()
		if layout == nil {
			return nil
		}
		var err error
		base, err = values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier(), layout)
		if err != nil {
			return nil
		}
	}
	qov, ok := values.AsQuantifiedObjectValue(base)
	if !ok {
		return nil
	}
	fv := resolveUpperFieldPath(qov, column.path)
	if fv == nil {
		return nil
	}
	if column.function != "" {
		if column.function == FunctionKindCardinality {
			return values.NewCardinalityValue(fv)
		}
		if dir, isOrder := OrderFunctionDirection(column.function); isOrder {
			// The column's stored bytes are the TupleOrdering encoding, so the
			// candidate-side Value is ToOrderedBytesValue(field, direction)
			// (OrderFunctionKeyExpression.toValue). A query comparison on the
			// BARE field does not structurally equal this Value, so it never
			// binds the placeholder — the order-wrapped column is not sargable
			// through the flat bridge, exactly Java's placeholder behaviour
			// absent a comparand-conversion simplification.
			return values.NewToOrderedBytesValue(fv, dir)
		}
	}
	return fv
}

// duplicateProducingColumns returns, per index key column, Java's
// normalizedKeyExpression.createsDuplicates(): whether the expansion registered
// it below a FAN_OUT. If metadata says the index duplicates but no expansion
// classifies its columns, every position is conservatively marked.
func (c *ValueIndexScanMatchCandidate) duplicateProducingColumns() []bool {
	result := make([]bool, len(c.columnNames))
	if described := c.indexColumns(); described != nil &&
		len(described.keyDuplicates) == len(result) {
		hasDuplicate := false
		for _, duplicate := range described.keyDuplicates {
			hasDuplicate = hasDuplicate || duplicate
		}
		if hasDuplicate || !c.createsDuplicates {
			return append(result[:0], described.keyDuplicates...)
		}
	}
	if c.CreatesDuplicates() {
		for i := range result {
			result[i] = true
		}
	}
	return result
}

// CandidateName returns the index name.
func (c *ValueIndexScanMatchCandidate) CandidateName() string { return c.indexName }

// GetTraversal returns the Traversal of this candidate's expression
// tree, built lazily on first access by the key-expression expansion and stable
// once computed (sync.Once). Ports Java's
// ValueIndexScanMatchCandidate.getTraversal().
func (c *ValueIndexScanMatchCandidate) GetTraversal() *Traversal {
	if expansion := c.indexExpansion(); expansion != nil {
		return expansion.traversal
	}
	return nil
}

// effectiveRootKeyExpression is the stored root, or the root the flat column
// metadata spells when the candidate carries none.
func (c *ValueIndexScanMatchCandidate) effectiveRootKeyExpression() *gen.KeyExpression {
	if c.rootKeyExpression != nil {
		return c.rootKeyExpression
	}
	return flatColumnsRootKeyExpression(c.columnNames, c.columnFunctions)
}

// indexColumns is the candidate's admission: its key expression's columns, as
// the expansion visitor registers them, when the metadata names exactly those
// columns; nil when the index is not a candidate.
//
// Without FAN_OUT structure only an affirmative createsDuplicates=false signal
// admits the candidate: UNKNOWN metadata may hide a fan-out index, and a known
// duplicate-producing index without a FAN_OUT AST has no Explode with which to
// repair cardinality.
func (c *ValueIndexScanMatchCandidate) indexColumns() *valueIndexExpansion {
	if c == nil {
		return nil
	}
	c.columnsOnce.Do(func() {
		fanOut := keyExpressionContainsFanOut(c.rootKeyExpression)
		if !fanOut && (!c.createsDuplicatesKnown || c.createsDuplicates) {
			return
		}
		described, err := describeValueIndexRoot(c, c.effectiveRootKeyExpression())
		if err != nil || !c.columnsDescribe(described) {
			return
		}
		if fanOut {
			for _, function := range c.columnFunctions {
				if function != "" {
					return
				}
			}
		}
		// With a base type, the index is a candidate only if it expands: a
		// function key's Value depends on its arguments' types.
		if _, typed := candidateBaseType(c); typed {
			expansion, err := expandValueIndexRoot(c, c.effectiveRootKeyExpression(), c.predicateProto)
			if err != nil {
				return
			}
			c.expansion = expansion
		}
		c.columns = described
	})
	return c.columns
}

// indexExpansion is the candidate graph of an admitted candidate, or nil when
// it has none (no exact base type).
func (c *ValueIndexScanMatchCandidate) indexExpansion() *valueIndexExpansion {
	if c.indexColumns() == nil {
		return nil
	}
	return c.expansion
}

// columnsDescribe reports whether the flat column metadata names exactly the
// columns the expansion registered, so the name-keyed surfaces and the
// candidate graph speak of the same entry.
func (c *ValueIndexScanMatchCandidate) columnsDescribe(expansion *valueIndexExpansion) bool {
	if len(expansion.keyColumns) != len(c.columnNames) ||
		len(expansion.valueColumns) != len(c.valueColumnNames) {
		return false
	}
	for i, column := range expansion.keyColumns {
		function := ""
		if i < len(c.columnFunctions) {
			function = c.columnFunctions[i]
		}
		if !strings.EqualFold(column.name, c.columnNames[i]) || column.function != function {
			return false
		}
	}
	for i, column := range expansion.valueColumns {
		if column.function != "" || !strings.EqualFold(column.name, c.valueColumnNames[i]) {
			return false
		}
	}
	return true
}

// GetColumnNames returns the ordered column-name list (one per index
// key column, parallel to GetSargableAliases).
func (c *ValueIndexScanMatchCandidate) GetColumnNames() []string { return c.columnNames }

// GetPKColumnNames returns the primary-key column names of the record
// type the index ranges over, in PK order. Consumers append the
// trimPrimaryKey'd remainder after the index key columns to model the
// FULL entry key (Java: ValueIndexExpansionVisitor.fullKey) — index
// entries are (index key, primary key), so the scan's sort order and
// its matched ordering parts extend into the PK suffix. The PK is NOT
// part of the sargable surface (see unmatchedFieldsForIndex in
// planning_cost_model.go): unlike Java, Go's candidate never folds the
// PK into sargableAliases, and that invariant must hold.
func (c *ValueIndexScanMatchCandidate) GetPKColumnNames() []string {
	if !c.canProduceScanPlan() {
		return nil
	}
	return c.pkColumnNames
}

// GetSargableAliases returns the ordered parameter list (one per
// index key column).
func (c *ValueIndexScanMatchCandidate) GetSargableAliases() []values.CorrelationIdentifier {
	return c.sargableAliases
}

// GetRecordTypes returns which record types this index covers.
func (c *ValueIndexScanMatchCandidate) GetRecordTypes() []string { return c.recordTypes }

// IsUnique reports whether the index enforces uniqueness.
func (c *ValueIndexScanMatchCandidate) IsUnique() bool { return c.unique }

// ComputeMatchedOrderingParts computes ordering parts for each index
// column, using bound comparison ranges from the match info. Ports
// Java's ValueIndexLikeMatchCandidate.computeMatchedOrderingParts.
func (c *ValueIndexScanMatchCandidate) ComputeMatchedOrderingParts(
	matchInfo MatchInfo,
	sortParameterIDs []values.CorrelationIdentifier,
	isReverse bool,
) []*MatchedOrderingPart {
	if !c.canProduceScanPlan() {
		return nil
	}
	regularInfo := matchInfo.GetRegularMatchInfo()
	bindings := regularInfo.GetParameterBindingMap()
	duplicateProducingColumns := c.duplicateProducingColumns()

	var parts []*MatchedOrderingPart
	// Whether every coordinate emitted below also carries order THROUGH itself.
	// One that does not (a signed-zero-widened float equality) is still emitted
	// — it claims its own order — but it disqualifies the PK suffix, and the
	// "did the loop consume the whole key" count cannot see that: when such a
	// coordinate is the LAST index column, the count is full even though the
	// claim stops there.
	suffixCarried := true
	// The per-coordinate "could this hold a signed zero" question, answered by
	// the physical key type first and the layout second — the plan side's
	// splitKeyOrder asks the same function, which is what keeps the two
	// derivations classifying every coordinate alike.
	columnCouldBeFloat := plans.IndexColumnCouldBeFloat(c.keyComponentTypes, c.orderingKeyLayout(), c.floatQuestionColumnNames())
	for _, paramID := range sortParameterIDs {
		idx := -1
		for i, alias := range c.sargableAliases {
			if alias == paramID {
				idx = i
				break
			}
		}
		if idx < 0 || idx >= len(c.columnNames) {
			break
		}

		cr := bindings[paramID]
		// Java does not expose a duplicate-producing normalized key as an
		// ordering Value. An equality bound fixes the exploded element and
		// lets ordering continue at the next position; an unbound/range-bound
		// fan-out position terminates the usable ordering prefix.
		if duplicateProducingColumns[idx] {
			if cr != nil &&
				cr.GetRangeType() == predicates.ComparisonRangeEquality {
				continue
			}
			break
		}

		// Use the candidate's column Value (FieldValue, or
		// CardinalityValue(FieldValue) for a function-keyed column) so the
		// ordering part carries the SAME Value the query's sort key does —
		// resolved against the record row layout it indexes, so the key states
		// a column identity rather than a display name.
		// An ordering claim terminates at a coordinate whose physical key order
		// is not its logical order. A FLOAT/DOUBLE column is such a coordinate:
		// NaN payloads pack into two disjoint blocks (negative NaN before -Inf,
		// positive NaN after +Inf) while the comparator ranks every NaN equal
		// and greatest. Stop before emitting it — and, because the loop stops,
		// the PK-suffix continuation below is skipped too, which is required:
		// all NaNs are ONE logical tie class split across two physical ranges,
		// so no later column is ordered within that tie.
		//
		// A coordinate PINNED to one physical key is exempt, float or not: it
		// is FIXED, not sorted, so it claims no order and the columns after it
		// stay claimable. The duplicate-producing arm above does NOT cover this
		// — it is gated on duplicateProducingColumns[idx] and never sees an
		// ordinary column. Ask the plan-side authorities instead — the same
		// EqualityBoundCoordinateClaimsOwnOrder / EqualityPinsSinglePhysicalKey
		// OnColumn pair splitKeyOrder consults, fed the same per-coordinate
		// type answer (IndexColumnCouldBeFloat) — so the two derivations
		// classify every coordinate alike: a widened equality claims its own
		// order on both sides (SORTED there), a pinned equality is FIXED on both
		// sides even after a widened one, and nothing after a widened
		// coordinate is claimed sorted on either.
		//
		// TWO questions, deliberately asked separately, because a signed-zero
		// float equality answers them differently and answering both with one
		// predicate is what previously cost this coordinate its own claim:
		//
		//   claimsOwnOrder  — may THIS column be emitted as an ordering part?
		//   carriesTheSuffix — may LATER columns claim order THROUGH it?
		//
		// A zero-valued float equality spans BOTH signed zeros. It therefore
		// pins no single key and cannot carry the suffix (a later column
		// restarts at the block boundary), yet the executor's range set opens
		// the two zero blocks in KEY ORDER — reversed wholesale under a reverse
		// scan. So it claims its own order while terminating the suffix.
		//
		// That enumeration order is the WHOLE ground. Do not reason instead that
		// the admitted rows share one logical value and so satisfy any ORDER BY:
		// the predicate comparator and the sort comparator disagree on signed
		// zeros by design (see plans.EqualityBoundCoordinateClaimsOwnOrder), so
		// the claim is directional and a DESC request is not free.
		canExtend := c.keyColumnCanExtendOrderingClaim(idx)
		claimsOwnOrder := plans.EqualityBoundCoordinateClaimsOwnOrder(cr) || canExtend
		// canExtend ("this column is not a float") still short-circuits: a
		// non-float coordinate has no signed zero, so it carries the suffix
		// whether it is equality-bound or merely sorted.
		//
		// A FLOAT coordinate must prove it pins, and the operand alone cannot
		// prove it. An IN-list binding arrives UNKNOWN-typed, which the
		// operand-only predicate reads as "not a float" and pins on — so the
		// per-binding leg advertised a PK order the runtime signed-zero widening
		// does not deliver. Ask the column-aware authority, the same one the
		// plans-side derivation asks per coordinate (splitKeyOrder's pins), so
		// neither half can classify this coordinate differently from the other.
		pinsOneKey := plans.EqualityPinsSinglePhysicalKeyOnColumn(cr, columnCouldBeFloat(idx))
		carriesTheSuffix := canExtend || pinsOneKey
		// Past a coordinate that does not carry the suffix, only a coordinate
		// PINNED to one physical key may still be emitted: every admitted row
		// carries its one value within every block of the widened coordinate,
		// so it is FIXED everywhere in the stream (`d = 0.0 AND b = 1` binds B
		// FIXED; the plan-side rich form says the same). A coordinate that is
		// merely sorted, or itself widened, restarts at each block boundary and
		// is not ordered across the stream, so it ends the claim here — and the
		// PK suffix stays refused (suffixCarried) whatever follows.
		if !suffixCarried && !pinsOneKey {
			break
		}
		if !claimsOwnOrder {
			break
		}

		colValue := c.orderingColumnValue(idx)
		if colValue == nil {
			break
		}

		// A plain column orders tuple-naturally; an order-wrapped column
		// orders in its function's direction (forward scan), flipped whole —
		// direction and null placement together — under a reverse scan.
		sortOrder := c.columnMatchedSortOrder(idx, isReverse)

		parts = append(parts, NewMatchedOrderingPart(paramID, colValue, cr, sortOrder))

		// Emitted, but nothing may claim order THROUGH it. Recording the
		// refusal AFTER the append rather than breaking before it is the whole
		// point of the split: the coordinate keeps its own claim. The PK
		// suffix, however, must be refused — the equality spans two physical
		// blocks, so a later sorted column restarts at the boundary and is not
		// ordered across them (do not call this a "tie class": for signed zeros
		// the two comparators disagree, so the admitted rows are two sort
		// values, not one) — and the loop continuing for pinned coordinates
		// does not refuse it either: the gate below counts emitted parts
		// against the key length, and a fully pinned tail fills that count.
		if !carriesTheSuffix {
			suffixCarried = false
		}
	}

	// Continue the matched ordering into the trimmed primary-key suffix.
	// Index entries are (index key, primary key), so after the index key
	// columns the entry order continues through the PK remainder. Java
	// gets this for free: its expansion (ValueIndexExpansionVisitor)
	// registers PK placeholders, so sortParameterIds spans the FULL key
	// (getFullKeyExpression) and the loop above emits PK parts natively.
	// Go's candidate keeps the PK out of the sargable surface (see
	// GetPKColumnNames), so the PK parts are synthesized here instead:
	// unbound (empty comparison range), directional by isReverse — which
	// is what lets SatisfiesRequestedOrdering resolve an ORDER BY on the
	// PK against an equality-prefixed index scan and pick the scan
	// direction, exactly like Java's satisfiesRequestedOrdering over
	// full-key ordering parts.
	//
	// Gates: (1) Go conservatively synthesizes no suffix for a fan-out
	// candidate. Java can skip an equality-bound duplicate-producing position
	// and continue through later full-key aliases, including the PK; Go keeps
	// PK aliases outside sortParameterIDs and cannot prove that continuation
	// positionally here. This is a conservative Go limitation, not exact Java
	// parity. (2) The suffix only continues a FULLY emitted index key —
	// if the loop above stopped early the positions would not be
	// contiguous and the suffix would claim an order the entries don't
	// have. TrimmedPKSuffix drops PK columns already named in the index key,
	// which is a claim about ORDERING and not about the stored entry: for a
	// single-type index Index.TrimPrimaryKey drops those same columns from the
	// bytes, but a universal index is never assigned
	// primaryKeyComponentPositions, so its entries repeat the column. The suffix
	// is right either way, because a column already fixed by an earlier position
	// contributes nothing to sort order — ordering by (indexKey…, a, b) equals
	// ordering by (indexKey…, b). An earlier revision justified this by
	// asserting the entry was trimmed, which held only for the single-type case.
	//
	// A genuinely MULTI-TYPE index does not reach this reasoning at all:
	// orderingKeyLayout fails closed when rowLayouts returns more than one, so
	// no ordering parts are emitted for it. The untrimmed case that reaches here
	// is a universal index over a single record type. (3) The last
	// emitted coordinate must carry order through itself; a full part count is
	// not sufficient, since a coordinate that claims only its OWN order can sit
	// at the end of the key.
	if !c.CreatesDuplicates() && len(parts) == len(c.columnNames) && suffixCarried {
		for _, col := range plans.TrimmedPKSuffix(c.trimmableKeyColumnNames(), c.pkColumnNames) {
			// The suffix terminates on the same rule as the key columns: a
			// FLOAT/DOUBLE PK column cannot extend the claim.
			if !values.ColumnCanExtendOrderingClaim(c.orderingKeyLayout(), col) {
				break
			}
			colValue := c.bakeOrderingColumn(col)
			if colValue == nil {
				break
			}
			sortOrder := MatchedSortOrderAscending
			if isReverse {
				sortOrder = MatchedSortOrderDescending
			}
			// PK parts have no sargable alias by design; the parameter id
			// is only informational on MatchedOrderingPart (no production
			// consumer reads it), so a fresh identifier is used.
			parts = append(parts, NewMatchedOrderingPart(
				values.UniqueCorrelationIdentifier(), colValue, nil, sortOrder))
		}
	}
	return parts
}

// ComputeBoundParameterPrefixMap walks the sargable aliases in order
// and collects the longest prefix that satisfies index scan
// discipline:
//   - N equality-bound parameters (any number, including 0)
//   - followed by at most ONE inequality-bound parameter
//   - stops at the first unbound (empty) parameter or after the
//     first inequality
//
// Mirrors Java's default `MatchCandidate.computeBoundParameterPrefixMap`.
func (c *ValueIndexScanMatchCandidate) ComputeBoundParameterPrefixMap(
	bindings map[values.CorrelationIdentifier]*predicates.ComparisonRange,
) map[values.CorrelationIdentifier]*predicates.ComparisonRange {
	if !c.canProduceScanPlan() {
		return nil
	}
	prefix := make(map[values.CorrelationIdentifier]*predicates.ComparisonRange)
	for i, alias := range c.sargableAliases {
		cr, ok := bindings[alias]
		if !ok || cr == nil || cr.IsEmpty() {
			return prefix
		}
		// A shared index can report Unknown because its record types encode this
		// position with different tuple widths. Never guess from the RHS: leave
		// this and every following predicate as residual compensation.
		if !candidatePhysicalKeyTypeKnown(c.keyComponentTypes, i) {
			return prefix
		}
		if candidateRangeHasUnsupportedPhysicalStartsWith(cr, c.keyComponentTypes, i) {
			// PREFIX_STRING endpoints implement logical STARTS_WITH only for
			// an authoritative STRING key. Reconciliation will preserve this
			// comparison as a residual filter.
			return prefix
		}
		// A raw NaN endpoint is incomplete for ordered predicates: logical
		// comparison canonicalizes every payload, while FDB tuple order
		// preserves distinct NaN regions. Keep those as compensation. A NaN
		// EQUALITY binds as the terminal component: the executor reads both
		// NaN key blocks, every NaN the per-row `=` matches, and the
		// components after it stay residual (RFC-257 WS-E 5.3).
		if candidateRangeHasKnownConstantNaN(cr, c.keyComponentTypes, i) {
			if cr.GetRangeType() == predicates.ComparisonRangeEquality {
				prefix[alias] = cr
			}
			return prefix
		}
		switch cr.GetRangeType() {
		case predicates.ComparisonRangeEquality:
			prefix[alias] = cr
		case predicates.ComparisonRangeInequality:
			prefix[alias] = cr
			return prefix
		default:
			return prefix
		}
	}
	return prefix
}

// ToScanPlan converts the matched prefix into a physical plan. The
// plan is wrapped in a FetchFromPartialRecordPlan with a
// TranslateValueFunction that can translate FieldValues referencing
// covered index columns. This enables push-through rules (C-6) to
// push filters/maps below the fetch when they reference covered
// columns.
//
// Matches Java's ScanWithFetchMatchCandidate architecture where every
// index scan is wrapped in a Fetch that carries the translation
// function.
func (c *ValueIndexScanMatchCandidate) ToScanPlan(
	prefixMap map[values.CorrelationIdentifier]*predicates.ComparisonRange,
	reverse bool,
) plans.RecordQueryPlan {
	if !c.canProduceScanPlan() {
		return nil
	}
	comps := make([]*predicates.ComparisonRange, len(c.sargableAliases))
	for i, alias := range c.sargableAliases {
		if cr, ok := prefixMap[alias]; ok {
			comps[i] = cr
		} else {
			comps[i] = predicates.EmptyComparisonRange()
		}
	}
	indexPlan, err := plans.NewRecordQueryIndexPlan(
		c.indexName,
		comps,
		c.recordTypes,
		c.flowedType,
		reverse,
	)
	if err != nil {
		return nil
	}
	indexPlan = stampIndexMetadata(c, indexPlan)

	// ValueIndexScanMatchCandidate.toEquivalentPlan: a fetch over an entry read
	// into the logical record, or the bare index plan when it cannot be.
	logicalRecord := c.indexEntryToLogicalRecord()
	if logicalRecord == nil {
		return indexPlan
	}
	fetch, err := plans.NewRecordQueryFetchFromPartialRecordPlan(
		indexPlan.WithEntryReader(logicalRecord.reader),
		c.buildTranslateValueFunction(),
		c.flowedType,
		plans.FetchIndexRecordsPrimaryKey,
	)
	if err != nil {
		return nil
	}
	return fetch
}

// GetBaseType returns the base record type for this candidate.
// Implements ValueIndexLikeMatchCandidate.
func (c *ValueIndexScanMatchCandidate) GetBaseType() values.Type { return c.flowedType }

// GetColumnSize returns the number of key columns in the index.
// Implements ValueIndexLikeMatchCandidate.
func (c *ValueIndexScanMatchCandidate) GetColumnSize() int { return len(c.columnNames) }

// CreatesDuplicates reports whether the index may produce duplicate entries
// per record. A structured FAN_OUT root is authoritative even when an optional
// external signal is missing or incorrectly false. UNKNOWN is represented by
// the conservative true value because this bool-shaped compatibility contract
// has no abstain state; DistinctRecordsSignal retains the tri-state form.
// Implements ValueIndexLikeMatchCandidate.
func (c *ValueIndexScanMatchCandidate) CreatesDuplicates() bool {
	return keyExpressionContainsFanOut(c.rootKeyExpression) ||
		!c.createsDuplicatesKnown ||
		c.createsDuplicates
}

// DistinctRecordsSignal returns the fan-out signal for the DistinctRecords
// property. A structured FAN_OUT root establishes true even when the optional
// IndexDef signal is missing or stale. Without structural evidence, a missing
// signal remains nil (unknown, so the property safely abstains).
func (c *ValueIndexScanMatchCandidate) DistinctRecordsSignal() *bool {
	if keyExpressionContainsFanOut(c.rootKeyExpression) {
		v := true
		return &v
	}
	if !c.createsDuplicatesKnown {
		return nil
	}
	v := c.createsDuplicates
	return &v
}

// metadataSufficientForPlanning is the single admission authority for a value
// candidate's scan, coverage, ordering, and PK-suffix surfaces: the index's
// columns are the ones the metadata names (indexColumns).
func (c *ValueIndexScanMatchCandidate) metadataSufficientForPlanning() bool {
	return c.indexColumns() != nil
}

// canProduceScanPlan additionally proves that a structural fan-out root was
// successfully expanded, keeping the planning/coverage/ordering methods
// fail-closed for a fan-out candidate with no candidate graph.
func (c *ValueIndexScanMatchCandidate) canProduceScanPlan() bool {
	if !c.metadataSufficientForPlanning() {
		return false
	}
	if keyExpressionContainsFanOut(c.rootKeyExpression) {
		return c.GetTraversal() != nil
	}
	return true
}

// plainFieldColumnsForShortcut returns the candidate key only when it is
// semantically a sequence of bare top-level scalar fields. A handful of
// direct rules predate traversal matching and compare column names; they must
// not mistake CARDINALITY(TAGS), ADDR.CITY, or another expression key for a
// plain TAGS/CITY index merely because the metadata leaf name is the same.
//
// With no root AST, an explicit createsDuplicates=false signal plus empty
// columnFunctions is the legacy caller's authority that the supplied columns
// are flat scalar fields. Production metadata supplies the root as well.
func (c *ValueIndexScanMatchCandidate) plainFieldColumnsForShortcut() ([]string, bool) {
	if !c.canProduceScanPlan() {
		return nil, false
	}
	for _, function := range c.columnFunctions {
		if function != "" {
			return nil, false
		}
	}
	if c.rootKeyExpression != nil {
		rootNames, ok := keyExpressionTopLevelScalarFieldNames(
			c.rootKeyExpression,
		)
		if !ok || len(rootNames) != len(c.columnNames) {
			return nil, false
		}
		for i, rootName := range rootNames {
			if !strings.EqualFold(rootName, c.columnNames[i]) {
				return nil, false
			}
		}
	}
	return c.columnNames, true
}

// orderingColumns states what each key column orders the scan plan by: a
// plain field tuple-natural ascending, an order-wrapped field in the function's
// direction, a CARDINALITY column by the cardinality of its field. ok=false
// when the key carries another shape.
func (c *ValueIndexScanMatchCandidate) orderingColumns() ([]plans.IndexOrderingColumn, bool) {
	if !c.canProduceScanPlan() || c.rootKeyExpression == nil {
		return nil, false
	}
	described := c.indexColumns()
	columns := make([]plans.IndexOrderingColumn, len(described.keyColumns))
	for i, column := range described.keyColumns {
		if described.keyDuplicates[i] || (len(column.path) == 0 && column.function == "") {
			return nil, false
		}
		switch column.function {
		case "":
		case FunctionKindCardinality:
			columns[i].Cardinality = true
		default:
			if direction, isOrder := OrderFunctionDirection(column.function); isOrder {
				columns[i].Direction = direction
				continue
			}
			expansion := c.indexExpansion()
			if expansion == nil || expansion.keyValues[i] == nil {
				return nil, false
			}
			columns[i].Key = expansion.keyValues[i]
			columns[i].KeyRoot = expansion.base.Correlation()
		}
	}
	return columns, true
}

// HasAndOrderedByRecordTypeKey reports whether the index key starts
// with the record type key. For standard value indexes this is false;
// only indexes explicitly prefixed by recordType() return true.
// Implements ValueIndexLikeMatchCandidate.
func (c *ValueIndexScanMatchCandidate) HasAndOrderedByRecordTypeKey() bool { return false }

// GetSargableAliasesRequiredForBinding returns the set of sargable
// aliases that must be bound for the candidate to be valid. For
// standard value indexes, no aliases are required (the default).
// Implements ValueIndexLikeMatchCandidate.
func (c *ValueIndexScanMatchCandidate) GetSargableAliasesRequiredForBinding() []values.CorrelationIdentifier {
	return nil
}

// PushValueThroughFetch attempts to translate a value from the
// full-record domain to the index-entry domain. Returns the
// translated value and true on success; nil and false otherwise.
// Implements ScanWithFetchMatchCandidate.
func (c *ValueIndexScanMatchCandidate) PushValueThroughFetch(
	value values.Value,
	sourceAlias values.CorrelationIdentifier,
	targetAlias values.CorrelationIdentifier,
) (values.Value, bool) {
	if !c.canProduceScanPlan() {
		return nil, false
	}
	fn := c.buildTranslateValueFunction()
	return fn(value, sourceAlias, targetAlias)
}

// buildTranslateValueFunction is the Fetch's pushValueThroughFetch
// (ScanWithFetchMatchCandidate.pushValueThroughFetch over the logical record's
// fields): a field of the source is pushed when the logical record covers its
// exact path, a value not of the source passes unchanged, anything else stays
// above the fetch.
func (c *ValueIndexScanMatchCandidate) buildTranslateValueFunction() plans.TranslateValueFunction {
	logicalRecord := c.indexEntryToLogicalRecord()
	if !c.canProduceScanPlan() || logicalRecord == nil {
		return func(values.Value, values.CorrelationIdentifier, values.CorrelationIdentifier) (values.Value, bool) {
			return nil, false
		}
	}
	covered := make(map[string]struct{}, len(logicalRecord.logicalFields))
	for _, field := range logicalRecord.logicalFields {
		covered[ordinalPathKey(field.Path().Ordinals())] = struct{}{}
	}
	rowType := c.flowedType
	domain := values.OrdinalDomainOfType(rowType)
	return func(value values.Value, sourceAlias, targetAlias values.CorrelationIdentifier) (values.Value, bool) {
		if field, isField := values.AsFieldValue(value); isField {
			root, isSource := values.AsQuantifiedObjectValue(field.ChildValue())
			if !isSource || root.Correlation() != sourceAlias {
				return nil, false
			}
			// The path's ordinals must index the candidate's own layout.
			if field.Path().RootDomain() != domain {
				return nil, false
			}
			ordinals := field.Path().Ordinals()
			if _, ok := covered[ordinalPathKey(ordinals)]; !ok {
				return nil, false
			}
			target, err := values.NewQuantifiedObjectValue(targetAlias, rowType)
			if err != nil {
				return nil, false
			}
			translated, err := values.ResolveFieldOrdinals(target, ordinals)
			if err != nil {
				return nil, false
			}
			return translated, true
		}
		if object, isObject := values.AsQuantifiedObjectValue(value); isObject {
			if object.Correlation() == sourceAlias {
				// A partial index row cannot satisfy a whole-object read.
				return nil, false
			}
			return value, true
		}
		switch value.(type) {
		case *values.ConstantValue:
			return value, true
		default:
			return nil, false
		}
	}
}

var (
	_ MatchCandidate        = (*ValueIndexScanMatchCandidate)(nil)
	_ OrderingPartsComputer = (*ValueIndexScanMatchCandidate)(nil)
)

// Interface compliance also checked in match_candidate_interfaces.go.
