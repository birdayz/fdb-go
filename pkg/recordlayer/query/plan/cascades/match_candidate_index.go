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

	traversalOnce sync.Once
	traversal     *Traversal
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
// name. A name that does not resolve stays LAZY — an unaddressable ordering key
// costs an elision, an ordinal against the wrong layout reads as authoritative
// and addresses another row.
//
// Resolution is UNIQUE-match, matching the runtime authority this key is
// verified against (bakedIntersectionKeys). It must not be first-match: the
// candidate's column list drops nesting parents (metadataIndexDef
// .IndexColumnNames), so the name reaching here is a bare leaf, and
// first-matching a leaf against a duplicate-named layout would silently bake
// some other column's slot. A nested leaf whose name collides with a top-level
// field is a stronger version of the same hazard and is fenced one level up:
// NewPlanContextFromIndexDefs refuses a root key expression containing a
// non-fan-out nested leaf outright, so no such column ever reaches a candidate.
func (c *ValueIndexScanMatchCandidate) bakeOrderingColumn(name string) values.Value {
	return bakeOrderingColumnIn(c.orderingKeyLayout(), name)
}

// bakeOrderingColumnIn is bakeOrderingColumn's body, shared with the
// primary-scan candidate: both mint their matched ordering parts from a
// metadata column-name list against the one record row layout the scan flows,
// and both feed the same set-operation merge, so they must agree on the domain
// token or the merge collapses.
func bakeOrderingColumnIn(layout *values.RecordType, name string) values.Value {
	lazy := values.NewFieldValue(nil, name, values.UnknownType)
	if layout == nil {
		return lazy
	}
	domain := values.OrdinalDomainOfType(layout)
	if !domain.IsKnown() {
		return lazy
	}
	ordinal, unique := uniqueUpperFieldIndex(layout, name)
	if !unique {
		return lazy
	}
	return values.NewFieldValueWithResolvedOrdinalInDomain(
		name, ordinal, values.UnknownType, domain)
}

// orderingColumnValue is ColumnValue for the ordering-key mint: the same Value
// shape, with the plain-field case carrying its resolved identity.
//
// A function-keyed column is deliberately left alone. CARDINALITY(x) is not a
// column of the row layout, so it is matched as a whole Value rather than by
// column identity, and baking the wrapped child would make an otherwise
// structurally equal pair unequal.
func (c *ValueIndexScanMatchCandidate) orderingColumnValue(i int) values.Value {
	if i < len(c.columnFunctions) && c.columnFunctions[i] != "" {
		if _, isOrder := OrderFunctionDirection(c.columnFunctions[i]); isOrder {
			// An order-wrapped column ORDERS BY its underlying field — the
			// wrapper only changes the physical encoding and the direction,
			// which columnMatchedSortOrder carries separately. Java derives the
			// same split by simplifying ToOrderedBytesValue(fv) to (fv,
			// direction) when minting ordering parts
			// (OrderingValueComputationRuleSet's ToOrderedBytes rule).
			return c.bakeOrderingColumn(c.columnNames[i])
		}
		return c.ColumnValue(i, nil)
	}
	return c.bakeOrderingColumn(c.columnNames[i])
}

// coveredOrdinalSets resolves the covered-column names against every row
// layout the index serves — see buildCoveredOrdinalSets.
func (c *ValueIndexScanMatchCandidate) coveredOrdinalSets(coveredColumns map[string]struct{}) []coveredOrdinalSet {
	return buildCoveredOrdinalSets(c.rowLayouts(), coveredColumns)
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
func OrderFunctionDirection(name string) (values.OrderedBytesDirection, bool) {
	switch strings.ToLower(name) {
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
	fv := values.NewFieldValue(base, c.columnNames[i], values.UnknownType)
	if i < len(c.columnFunctions) {
		if c.columnFunctions[i] == FunctionKindCardinality {
			return values.NewCardinalityValue(fv)
		}
		if dir, isOrder := OrderFunctionDirection(c.columnFunctions[i]); isOrder {
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

// duplicateProducingColumns returns the per-index-column analogue of Java's
// normalizedKeyExpression.createsDuplicates(). A direct FAN_OUT field marks
// its one position; every child position underneath a fan-out nesting parent
// is duplicate-producing. If metadata says the index duplicates but does not
// provide a classifiable key tree, every position is conservatively marked.
func (c *ValueIndexScanMatchCandidate) duplicateProducingColumns() []bool {
	result := make([]bool, len(c.columnNames))
	if c.rootKeyExpression != nil {
		if classified, ok := classifyDuplicateProducingColumns(
			c.rootKeyExpression,
			false,
		); ok && len(classified) == len(result) {
			hasDuplicate := false
			for _, duplicate := range classified {
				hasDuplicate = hasDuplicate || duplicate
			}
			if hasDuplicate || !c.createsDuplicates {
				return classified
			}
		}
	}
	if c.CreatesDuplicates() {
		for i := range result {
			result[i] = true
		}
	}
	return result
}

func classifyDuplicateProducingColumns(
	expression *gen.KeyExpression,
	inheritedFanOut bool,
) ([]bool, bool) {
	if expression == nil || keyExpressionShapeCount(expression) != 1 {
		return nil, false
	}
	switch {
	case expression.Field != nil:
		if expression.Field.FanType == nil {
			return nil, false
		}
		return []bool{
			inheritedFanOut ||
				expression.Field.GetFanType() == gen.Field_FAN_OUT,
		}, true

	case expression.Then != nil:
		var result []bool
		for _, child := range expression.Then.GetChild() {
			childColumns, ok := classifyDuplicateProducingColumns(
				child,
				inheritedFanOut,
			)
			if !ok {
				return nil, false
			}
			result = append(result, childColumns...)
		}
		return result, true

	case expression.Nesting != nil:
		nesting := expression.Nesting
		if nesting.Parent == nil || nesting.Parent.FanType == nil ||
			nesting.Child == nil {
			return nil, false
		}
		switch nesting.Parent.GetFanType() {
		case gen.Field_SCALAR:
			return classifyDuplicateProducingColumns(
				nesting.Child,
				inheritedFanOut,
			)
		case gen.Field_FAN_OUT:
			return classifyDuplicateProducingColumns(nesting.Child, true)
		default:
			return nil, false
		}

	default:
		return nil, false
	}
}

// CandidateName returns the index name.
func (c *ValueIndexScanMatchCandidate) CandidateName() string { return c.indexName }

// GetTraversal returns the Traversal of this candidate's expression
// tree, built lazily on first access via ExpandValueIndex. The
// traversal is stable once computed (sync.Once). Ports Java's
// ValueIndexScanMatchCandidate.getTraversal().
func (c *ValueIndexScanMatchCandidate) GetTraversal() *Traversal {
	c.traversalOnce.Do(func() {
		c.traversal = ExpandValueIndex(c)
	})
	return c.traversal
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
		// ordinary column. Ask the plan-side authorities instead, the same ones
		// equalityPrefixLen consults, so the two derivations cannot classify a
		// column differently.
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
		canExtend := values.ColumnCanExtendOrderingClaim(
			c.orderingKeyLayout(), c.columnNames[idx],
		)
		claimsOwnOrder := plans.EqualityBoundCoordinateClaimsOwnOrder(cr) || canExtend
		carriesTheSuffix := plans.EqualityPinsSinglePhysicalKey(cr) || canExtend
		if !claimsOwnOrder {
			break
		}

		colValue := c.orderingColumnValue(idx)

		// A plain column orders tuple-naturally; an order-wrapped column
		// orders in its function's direction (forward scan), flipped whole —
		// direction and null placement together — under a reverse scan.
		sortOrder := c.columnMatchedSortOrder(idx, isReverse)

		parts = append(parts, NewMatchedOrderingPart(paramID, colValue, cr, sortOrder))

		// Emitted, but nothing may claim order THROUGH it. Breaking AFTER the
		// append rather than before is the whole point of the split: the
		// coordinate keeps its own claim. The PK suffix, however, must still be
		// refused — the equality spans two physical blocks, so a later column
		// restarts at the boundary and is not ordered across them (do not call
		// this a "tie class": for signed zeros the two comparators disagree, so
		// the admitted rows are two sort values, not one) — and breaking alone
		// does not refuse it: the
		// gate below counts emitted parts against the key length, and this
		// coordinate has now been counted. Record the refusal explicitly.
		if !carriesTheSuffix {
			suffixCarried = false
			break
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
	// have. PK columns already in the index key are trimmed
	// (Index.trimPrimaryKey), matching fullKey construction. (3) The last
	// emitted coordinate must carry order through itself; a full part count is
	// not sufficient, since a coordinate that claims only its OWN order can sit
	// at the end of the key.
	if !c.CreatesDuplicates() && len(parts) == len(c.columnNames) && suffixCarried {
		for _, col := range plans.TrimmedPKSuffix(c.columnNames, c.pkColumnNames) {
			// The suffix terminates on the same rule as the key columns: a
			// FLOAT/DOUBLE PK column cannot extend the claim.
			if !values.ColumnCanExtendOrderingClaim(c.orderingKeyLayout(), col) {
				break
			}
			colValue := c.bakeOrderingColumn(col)
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
		// A raw NaN endpoint is incomplete for equality and ordered predicates:
		// logical comparison canonicalizes every payload, while FDB tuple order
		// preserves distinct NaN regions. Keep the predicate as compensation.
		if candidateRangeHasKnownConstantNaN(cr, c.keyComponentTypes, i) {
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
	indexPlan := plans.NewRecordQueryIndexPlan(
		c.indexName,
		comps,
		c.recordTypes,
		c.flowedType,
		reverse,
	)
	indexPlan = stampIndexMetadata(c, indexPlan)

	// Build the TranslateValueFunction for this index: translates
	// FieldValues whose field name matches a covered index column.
	translateFn := c.buildTranslateValueFunction()

	return plans.NewRecordQueryFetchFromPartialRecordPlan(
		indexPlan,
		translateFn,
		c.flowedType,
		plans.FetchIndexRecordsPrimaryKey,
	)
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
// candidate's flat scan, coverage, ordering, and PK-suffix surfaces.
//
// A structural FAN_OUT root is sufficient because ExpandValueIndex must build
// and successfully match through an Explode graph before a scan can exist.
// Without FAN_OUT structure, only an affirmative createsDuplicates=false
// signal permits the historical flat compatibility path. UNKNOWN metadata may
// hide a sparse/multiplying fan-out index, and createsDuplicates=true without
// a FAN_OUT AST lacks the topology needed for cardinality compensation.
// Scalar nested leaves are rejected regardless: their full accessor path is
// not represented by the flat candidate column bridge.
func (c *ValueIndexScanMatchCandidate) metadataSufficientForPlanning() bool {
	if c == nil ||
		keyExpressionContainsNonFanOutNestedLeaf(c.rootKeyExpression) {
		return false
	}
	if keyExpressionContainsFanOut(c.rootKeyExpression) {
		// The structural fan-out expansion models Field/Then/Nesting, not
		// function-tagged columns. Any such combination is inconsistent
		// metadata and must decline.
		for _, function := range c.columnFunctions {
			if function != "" {
				return false
			}
		}
		return true
	}
	if !c.createsDuplicatesKnown || c.createsDuplicates {
		return false
	}
	for _, function := range c.columnFunctions {
		if function != "" && function != FunctionKindCardinality {
			if _, isOrder := OrderFunctionDirection(function); !isOrder {
				return false
			}
		}
	}
	if c.rootKeyExpression == nil {
		// Legacy flat metadata: the explicit false signal and supported
		// function tags are the caller's semantic authority.
		return true
	}
	keyDescriptors, valueDescriptors, ok := keyExpressionKeyValueColumnDescriptors(
		c.rootKeyExpression,
	)
	if !ok || len(keyDescriptors) != len(c.columnNames) ||
		len(valueDescriptors) != len(c.valueColumnNames) {
		return false
	}
	for i, descriptor := range keyDescriptors {
		function := ""
		if i < len(c.columnFunctions) {
			function = c.columnFunctions[i]
		}
		if !strings.EqualFold(descriptor.name, c.columnNames[i]) ||
			descriptor.function != function {
			return false
		}
	}
	// Value-part columns are covering-only: a function-valued value column
	// (e.g. a CARDINALITY in the VALUE part) cannot be translated by covered
	// name, so the whole candidate declines fail-closed rather than serve a
	// mis-translated covering row.
	for i, descriptor := range valueDescriptors {
		if descriptor.function != "" ||
			!strings.EqualFold(descriptor.name, c.valueColumnNames[i]) {
			return false
		}
	}
	return true
}

// canProduceScanPlan additionally proves that a structural fan-out root was
// successfully expanded. This keeps the individual planning/coverage/ordering
// methods fail-closed even for a prebuilt candidate whose FAN_OUT appears under
// an unsupported key-expression wrapper or disagrees with its column metadata.
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

// buildTranslateValueFunction creates a TranslateValueFunction that
// can translate values from the full-record domain to the index-entry
// domain. A FieldValue is translatable if its field name matches one
// of the non-fan-out index columns or primary-key columns (case-insensitive).
// An exploded element cannot cover the original repeated field.
//
// Ports the conceptual equivalent of Java's
// ScanWithFetchMatchCandidate.createTranslateValueFunction.
func (c *ValueIndexScanMatchCandidate) buildTranslateValueFunction() plans.TranslateValueFunction {
	if !c.canProduceScanPlan() {
		return func(
			values.Value,
			values.CorrelationIdentifier,
			values.CorrelationIdentifier,
		) (values.Value, bool) {
			return nil, false
		}
	}
	hasFunctionKey := false
	for _, function := range c.columnFunctions {
		if function != "" {
			hasFunctionKey = true
			break
		}
	}
	duplicateProducingColumns := c.duplicateProducingColumns()
	blockedColumns := make(map[string]struct{})
	for i, col := range c.columnNames {
		functionKey := i < len(c.columnFunctions) &&
			c.columnFunctions[i] != ""
		if duplicateProducingColumns[i] || functionKey {
			blockedColumns[strings.ToUpper(col)] = struct{}{}
		}
	}

	coveredColumns := make(map[string]struct{}, len(c.columnNames)+len(c.pkColumnNames))
	// A function-key covering row retains every key position for tuple/logical
	// row alignment, but those positions are semantic expressions rather than
	// their raw leaf fields. Until physical plans carry semantic key Values,
	// expose none of the index-key fields through Fetch translation. The PK
	// suffix remains independently safe and index-resident (the real FDB
	// CARDINALITY test pins SELECT ID as covering); function-name collisions
	// are blocked below.
	if !hasFunctionKey {
		for i, col := range c.columnNames {
			if duplicateProducingColumns[i] {
				continue
			}
			coveredColumns[strings.ToUpper(col)] = struct{}{}
		}
	}
	// The VALUE part of a KeyWithValue root is stored RAW in the FDB value
	// (only key columns can carry an order-function encoding, and the
	// admission check declines a function-valued value column), so covering
	// translation of these columns is safe even when a key column is
	// function-wrapped. This is the whole point of the covering split
	// (KeyWithValueExpression: "additional query-relevant columns can be read
	// without fetching the record").
	for _, col := range c.valueColumnNames {
		coveredColumns[strings.ToUpper(col)] = struct{}{}
	}
	for _, col := range c.pkColumnNames {
		upperColumn := strings.ToUpper(col)
		if _, blocked := blockedColumns[upperColumn]; !blocked {
			coveredColumns[upperColumn] = struct{}{}
		}
	}
	coveredSets := c.coveredOrdinalSets(coveredColumns)

	return func(value values.Value, sourceAlias, targetAlias values.CorrelationIdentifier) (values.Value, bool) {
		switch v := value.(type) {
		case *values.FieldValue:
			// Only a TOP-LEVEL bare column translates by covered-column name;
			// decline a CHAINED accessor (v.Child is itself a FieldValue). Java's
			// pushValueThroughFetch (ScanWithFetchMatchCandidate.java ~51-76) matches
			// the WHOLE value tree — accessor chain included — against a provided
			// index Value via semanticEquals, so a chained accessor (e.g. ADDR.CITY,
			// FieldValue{CITY, Child: FieldValue{ADDR, …}}) whose LEAF name happens to
			// collide with a covered top-level column is NOT the same indexed Value;
			// translating it here to a flat read of the index entry's CITY column
			// would return wrong rows/values. A BARE column reference — v.Child a
			// source QuantifiedObjectValue, OR a baked leaf (Child nil / a resolved
			// ordinal, the post-ordinalization shape) — is the covering case and
			// still translates by covered name. Only a FieldValue-over-FieldValue is
			// a genuine chain; declining it (rather than dropping v.Child) lets the
			// recursive-decomposition path handle a deeper match or report not-pushable.
			// ALLOWLIST (not a chained-only blocklist): a bare column is Child==nil (a
			// baked leaf / resolved ordinal — the post-ordinalization shape) OR
			// THIS SOURCE's QuantifiedObjectValue directly. ANY other child — a
			// FieldValue chain, a composite like a RecordConstructorValue, or
			// ANOTHER QUANTIFIER's object value — is not the indexed Value and must
			// decline (else its accessor structure is silently dropped, or a
			// foreign row's column is read as this row's).
			//
			// The correlation is checked, not assumed: it is the FIRST element of
			// column identity (RFC-197 — identity is (correlation, domain,
			// ordinal path)), and it is the ONLY element that can separate two
			// quantifiers over the SAME TABLE. Their layouts are identical by
			// construction, so their domain tokens are equal and the ordinal check
			// below passes for both; a self-join's `t2.city` would be rebased onto
			// targetAlias and read the index entry belonging to `t1` — wrong row,
			// silently, and the pushability oracle would additionally report the
			// foreign column "available below the fetch" to the covering rules.
			//
			// Java refuses the same rebase structurally: its equivalence map
			// equates ONLY sourceAlias with the candidate's baseAlias
			// (ScanWithFetchMatchCandidate.java:60), so a value over any other
			// quantifier can never semanticEquals a provided index value and never
			// reaches the rebase arm (:66-68). Java then falls through to "return
			// the value unchanged, pushable" (:71) because a value not correlated
			// to sourceAlias survives its final filter (:75). Go declines instead:
			// Go's own-row columns do NOT reliably carry sourceAlias — a column of
			// the fetched row can be correlated to the TABLE alias while
			// sourceAlias is the filter's QUANTIFIER alias (the two-namespace
			// defect documented at rule_push_filter_through_fetch.go:273-279), so
			// "not sourceAlias ⇒ foreign row ⇒ pass through" would readmit exactly
			// the wrong-rows hole that comment closed. Until the namespaces are
			// one, the covered-column set stays the only authority and an
			// unprovable correlation declines. A declined push is recoverable.
			if v.Child != nil {
				qov, isBareSource := v.Child.(*values.QuantifiedObjectValue)
				if !isBareSource || qov.Correlation != sourceAlias {
					return nil, false
				}
			}
			// A CHILDLESS baked reference has no correlation to check, and needs
			// none: it reads the row currently being evaluated, and there is no way
			// to express "the other quantifier's column" without a child to name
			// that quantifier. So within the record-descriptor frontier the
			// (domain, ordinal) pair below fully determines the column, and the
			// correlation element is supplied by the rule's own quantifier context
			// — this fetch's row is what the predicate above it is evaluated
			// against (the shape rule_push_filter_through_fetch.go:262-267
			// describes: after ordinalization a bare column is Child == nil with an
			// EMPTY correlation set, which is why that rule routes every accessor
			// through the covered-column set instead of asking correlation).
			//
			// The covering question is asked and answered in ORDINALS, in a
			// stated DOMAIN (RFC-197 item 1). The index definition's column
			// names were resolved against each record type's descriptor when
			// this function was built; here the reference must prove its own
			// ordinal indexes THAT layout.
			//
			// The fetch's inner presents its partial record in the record's
			// LOGICAL slot layout (descriptor-shaped, non-covered fields nil —
			// the covering-scan row-shaping in executor.go /
			// flat_map_cursor.go). So the pushed reference's frontier IS the
			// record descriptor, the same layout as the target, and an ordinal
			// proven to index it reads the same slot on both sides. That
			// proof used to be the comment you are reading; it is now the
			// predicate OrdinalIn checks, which is why BOTH the source-relative
			// and the frontier-pinned single-accessor forms are admitted (each
			// states the layout it indexes) and why everything else declines:
			//
			//   - a FUSED multi-accessor path (composeFieldOverField collapses
			//     `t.addr.city` into ONE node — Child=QOV, Resolved=[ADDR,CITY],
			//     Field="CITY") has an ADDR-relative root ordinal, and its leaf
			//     name colliding with a covered column is exactly the trap;
			//   - a pinned reference into an assembled MERGED row (a join box's
			//     leg window) carries a leg-relative ordinal that is not a
			//     record-descriptor ordinal at all — the same integer meaning a
			//     different column, the failure mode a name comparison cannot
			//     even express;
			//   - a LAZY reference has no ordinal, and the display name it
			//     still carries is not a fallback.
			//
			// Each of those declines the push. A declined push is a filter that
			// stays above the fetch — recoverable; a wrong ordinal is wrong
			// rows.
			//
			// Preserving the ordinal for the admitted case is load-bearing in
			// the other direction too: a pushed predicate later evaluated as a
			// residual (a join key on an indexed column that lost the index
			// bound to a competing IS NULL) hits the executor's
			// no-name-fallback path and fails LOUD if it arrives lazy.
			if ord, domain, covered := pushCoveredOrdinal(coveredSets, v); covered {
				return values.NewCorrelatedFieldValueWithResolvedOrdinalInDomain(
					values.NewQuantifiedObjectValue(targetAlias),
					v.Field, ord, v.Typ, domain,
				), true
			}
			return nil, false
		case *values.QuantifiedObjectValue:
			if v.Correlation == sourceAlias {
				// The index entry provides individual covered fields, not the
				// complete logical record. Java's pushability gate accepts a
				// source-correlated FieldValue or record constructor, but never
				// a whole QuantifiedObjectValue; accepting it would let SELECT
				// * eliminate the Fetch and return a partial index row.
				return nil, false
			}
			return value, true
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
