package cascades

import (
	"strings"
	"sync"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
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
	// Meaningful only when createsDuplicatesKnown is true; false otherwise
	// (the ordering-suffix logic treats unknown as non-fan-out, unchanged).
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

	traversalOnce sync.Once
	traversal     *Traversal
}

// WithCommonPrimaryKey sets the index's structural common primary key on the
// (freshly constructed) candidate and returns it (RFC-189 B3).
func (c *ValueIndexScanMatchCandidate) WithCommonPrimaryKey(pk []values.Value) *ValueIndexScanMatchCandidate {
	c.commonPrimaryKeyValues = pk
	return c
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
// correlation identifier used for predicate binding.
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
func (c *ValueIndexScanMatchCandidate) GetPKColumnNames() []string { return c.pkColumnNames }

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
	regularInfo := matchInfo.GetRegularMatchInfo()
	bindings := regularInfo.GetParameterBindingMap()

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
	// Gates: (1) fan-out indexes get no suffix — positions past a
	// duplicating key part are not sort-ordered (Java breaks its loop
	// there); (2) the suffix only continues a FULLY emitted index key —
	// if the loop above stopped early the positions would not be
	// contiguous and the suffix would claim an order the entries don't
	// have. PK columns already in the index key are trimmed
	// (Index.trimPrimaryKey), matching fullKey construction.
	if !c.createsDuplicates && len(parts) == len(c.columnNames) {
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
	prefix := make(map[values.CorrelationIdentifier]*predicates.ComparisonRange)
	for _, alias := range c.sargableAliases {
		cr, ok := bindings[alias]
		if !ok || cr == nil || cr.IsEmpty() {
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

// CreatesDuplicates reports whether the index can produce duplicate
// entries per record (fan-out / repeated-field indexes).
// Implements ValueIndexLikeMatchCandidate.
func (c *ValueIndexScanMatchCandidate) CreatesDuplicates() bool { return c.createsDuplicates }

// DistinctRecordsSignal returns the fan-out signal for the DistinctRecords
// property, or nil when the IndexDef supplied none (unknown → the property
// abstains to distinct=false, the safe under-report). A non-nil result carries
// the known createsDuplicates value.
func (c *ValueIndexScanMatchCandidate) DistinctRecordsSignal() *bool {
	if !c.createsDuplicatesKnown {
		return nil
	}
	v := c.createsDuplicates
	return &v
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
	fn := c.buildTranslateValueFunction()
	return fn(value, sourceAlias, targetAlias)
}

// buildTranslateValueFunction creates a TranslateValueFunction that
// can translate values from the full-record domain to the index-entry
// domain. A FieldValue is translatable if its field name matches one
// of the index's column names (case-insensitive).
//
// Ports the conceptual equivalent of Java's
// ScanWithFetchMatchCandidate.createTranslateValueFunction.
func (c *ValueIndexScanMatchCandidate) buildTranslateValueFunction() plans.TranslateValueFunction {
	coveredColumns := make(map[string]struct{}, len(c.columnNames)+len(c.pkColumnNames))
	for _, col := range c.columnNames {
		coveredColumns[strings.ToUpper(col)] = struct{}{}
	}
	for _, col := range c.pkColumnNames {
		coveredColumns[strings.ToUpper(col)] = struct{}{}
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
				return values.NewQuantifiedObjectValue(targetAlias), true
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
