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

	// rootKeyExpression is the record-layer key-expression AST when metadata
	// exposes it. Fan-out expansion needs the tree topology (Then/Nesting and
	// the exact FAN_OUT node); the historical flat columnNames slice cannot
	// distinguish a scalar field from a repeated field or preserve a shared
	// fan-out parent. It is always stored as a defensive clone.
	rootKeyExpression *gen.KeyExpression

	traversalOnce sync.Once
	traversal     *Traversal
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
		indexName:              indexName,
		recordTypes:            types,
		columnNames:            cols,
		columnFunctions:        fns,
		pkColumnNames:          pkCols,
		sargableAliases:        aliases,
		flowedType:             flowedType,
		unique:                 unique,
		createsDuplicates:      createsDuplicatesSignal != nil && *createsDuplicatesSignal,
		createsDuplicatesKnown: createsDuplicatesSignal != nil,
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
	if i < len(c.columnFunctions) && c.columnFunctions[i] == FunctionKindCardinality {
		return values.NewCardinalityValue(fv)
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
		// ordering part carries the SAME Value the query's sort key does.
		// Flat FieldValue (no child) keeps parity with the historical
		// behaviour for plain columns.
		colValue := c.ColumnValue(idx, nil)

		sortOrder := MatchedSortOrderAscending
		if isReverse {
			sortOrder = MatchedSortOrderDescending
		}

		parts = append(parts, NewMatchedOrderingPart(paramID, colValue, cr, sortOrder))
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
	// (Index.trimPrimaryKey), matching fullKey construction.
	if !c.CreatesDuplicates() && len(parts) == len(c.columnNames) {
		for _, col := range plans.TrimmedPKSuffix(c.columnNames, c.pkColumnNames) {
			colValue := values.NewFieldValue(nil, col, values.UnknownType)
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
	for _, alias := range c.sargableAliases {
		cr, ok := bindings[alias]
		if !ok || cr == nil || cr.IsEmpty() {
			return prefix
		}
		switch cr.GetRangeType() {
		case predicates.ComparisonRangeEquality:
			prefix[alias] = cr
			// A zero-valued FLOAT/DOUBLE equality TERMINATES the prefix, even
			// though more indexed columns could be consumed.
			//
			// IEEE says -0.0 == +0.0 and this engine's `=` follows IEEE, but
			// FDB tuple encoding preserves the sign bit, so the two zeros are
			// distinct adjacent index keys and the probe must span BOTH. The
			// executor widens a zero bound that terminates the scan range.
			// Consuming a trailing column defeats that: with index (v, w) and
			// `v = 0 AND w = 5` the union of (-0.0,5) and (+0.0,5) is not a
			// contiguous interval — the span between them also admits
			// (-0.0, w>5) and (+0.0, w<5) — so no single TupleRange can express
			// it, and the unwidened probe silently missed the -0.0 row.
			//
			// Stopping here makes the zero equality terminal, so the executor
			// widens it across both keys, and leaves `w = 5` to be applied as a
			// RESIDUAL filter. The scan is bounded to the two zero groups and
			// the residual drops the in-between keys, which is exactly the
			// wanted pair — one scan, no multi-range machinery.
			//
			// Only a compile-time-constant zero is detectable here. A
			// correlated comparand that happens to be zero at runtime keeps the
			// full prefix; de-sargging every correlated composite join to cover
			// it would trade a rare wrong row for a broad performance cliff.
			if isZeroFloatEqualityRange(cr) {
				return prefix
			}
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
			return false
		}
	}
	if c.rootKeyExpression == nil {
		// Legacy flat metadata: the explicit false signal and supported
		// function tags are the caller's semantic authority.
		return true
	}
	descriptors, ok := keyExpressionFlatColumnDescriptors(
		c.rootKeyExpression,
	)
	if !ok || len(descriptors) != len(c.columnNames) {
		return false
	}
	for i, descriptor := range descriptors {
		function := ""
		if i < len(c.columnFunctions) {
			function = c.columnFunctions[i]
		}
		if !strings.EqualFold(descriptor.name, c.columnNames[i]) ||
			descriptor.function != function {
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
	for _, col := range c.pkColumnNames {
		upperColumn := strings.ToUpper(col)
		if _, blocked := blockedColumns[upperColumn]; !blocked {
			coveredColumns[upperColumn] = struct{}{}
		}
	}

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
			// baked leaf / resolved ordinal — the post-ordinalization shape) OR the
			// source QuantifiedObjectValue directly. ANY other child — a FieldValue
			// chain, or a composite like a RecordConstructorValue — is not the indexed
			// Value and must decline (else its accessor structure is silently dropped).
			if v.Child != nil {
				if _, isBareSource := v.Child.(*values.QuantifiedObjectValue); !isBareSource {
					return nil, false
				}
			}
			if _, covered := coveredColumns[strings.ToUpper(v.Field)]; covered {
				// PRESERVE the baked ordinal when rebasing the correlation to the
				// index-scan target — but ONLY for a SINGLE-ACCESSOR path
				// (Resolved.Single). The fetch's inner presents its partial record
				// in the record's LOGICAL slot layout (descriptor-shaped, non-covered
				// fields nil — the covering-scan row-shaping in executor.go /
				// flat_map_cursor.go). A pushed-below-fetch predicate references
				// COVERED COLUMNS in that record domain, whose frontier IS the record
				// descriptor — the SAME layout as the target — so a single-column
				// ordinal reads the same slot (an index-LAYOUT covering row is
				// separately guarded to refuse a baked ordinal rather than misread).
				//
				// The single-accessor gate is load-bearing: a FUSED multi-accessor
				// path (composeFieldOverField collapses `t.addr.city` into ONE node —
				// Child=QOV, Resolved=[ADDR,CITY], Field="CITY") passes the
				// bare-source allowlist above, but its Root() ordinal is ADDR's;
				// baking it single-accessor would drop the descent and, if a covered
				// column shares the leaf name, silently read the WRONG slot (wrong
				// rows). Resolved.Single() admits only the flat covered-column shape
				// (both the source-relative and the frontier-pinned single-accessor
				// forms the join/merge machinery produces for `r.c`); a multi-accessor
				// path falls through to the lazy branch (correct-or-loud).
				//
				// Dropping the ordinal for the flat case WAS the bug: a pushed
				// predicate later evaluated as a residual (a join key on an indexed
				// column that lost the index bound to a competing IS NULL) hit the
				// executor's no-name-fallback path and failed LOUD (ordinal -1).
				if v.Resolved != nil {
					if acc, single := v.Resolved.Single(); single {
						return values.NewCorrelatedFieldValueWithResolvedOrdinal(
							values.NewQuantifiedObjectValue(targetAlias),
							v.Field, acc.Ordinal, v.Typ,
						), true
					}
				}
				return values.NewFieldValue(
					values.NewQuantifiedObjectValue(targetAlias),
					v.Field, v.Typ,
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

// isZeroFloatEqualityRange reports whether cr is an equality against a
// compile-time-constant zero FLOAT/DOUBLE. Such an equality must terminate a
// scan prefix so the executor can widen it across both signed zeros — see the
// call site for why consuming a trailing column makes that inexpressible.
//
// A non-constant (correlated or parameterised) comparand returns false: its
// value is unknown at plan time, and de-sargging on the possibility of a
// runtime zero would cost every correlated composite join.
func isZeroFloatEqualityRange(cr *predicates.ComparisonRange) bool {
	if cr == nil || !cr.IsEquality() {
		return false
	}
	cmp := cr.GetEqualityComparison()
	if cmp == nil || cmp.Operand == nil {
		return false
	}
	// Deliberately NOT gated on the declared type. The executor decides to
	// widen from the runtime VALUE (isZeroFloatBound), so gating here on a
	// declared FLOAT/DOUBLE would let an untyped literal that evaluates to 0.0
	// widen at execution while match-time reasoning never noticed -- the
	// property and the behaviour would disagree, which is the whole defect
	// class this guards. Match on the value's Go type, exactly as the
	// executor does.
	// Constness checked BEFORE evaluating: evaluating an arbitrary operand with
	// a nil context treats unbound parameters as NULL, so `v = COALESCE(?, 0.0)`
	// folds to zero and is misreported as a constant zero even when the runtime
	// binding is nonzero -- which would drop the trailing column from EVERY
	// composite probe of that shape.
	if !values.IsConstantValue(cmp.Operand) {
		return false
	}
	v, ok := values.EvaluateConstant(cmp.Operand)
	if !ok || v == nil {
		return false
	}
	switch n := v.(type) {
	case float64:
		return n == 0
	case float32:
		return n == 0
	}
	return false
}
