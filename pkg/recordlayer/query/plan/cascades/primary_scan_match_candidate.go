package cascades

import (
	"strings"
	"sync"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// WithPrimaryKeyMatchCandidate is a MatchCandidate that uses a primary key
// to identify a record. Ports Java's
// `com.apple.foundationdb.record.query.plan.cascades.WithPrimaryKeyMatchCandidate`.
type WithPrimaryKeyMatchCandidate interface {
	MatchCandidate
	// GetPrimaryKeyValues returns the primary key values for this
	// candidate, or nil if the primary key cannot be computed.
	// Ports Java's getPrimaryKeyValuesMaybe() — nil corresponds to
	// Optional.empty().
	GetPrimaryKeyValues() []values.Value
}

// WithBaseQuantifierMatchCandidate is a MatchCandidate defined using a
// base quantifier (i.e., not a true join index). Ports Java's
// `com.apple.foundationdb.record.query.plan.cascades.WithBaseQuantifierMatchCandidate`.
type WithBaseQuantifierMatchCandidate interface {
	MatchCandidate
	// GetBaseType returns the base record type for this candidate.
	GetBaseType() values.Type
}

// PrimaryScanMatchCandidate represents a match candidate backed by the
// primary key scan. The primary scan is always unique (the PK uniquely
// identifies every record).
//
// In Java, PrimaryScanMatchCandidate implements MatchCandidate,
// ValueIndexLikeMatchCandidate, and WithPrimaryKeyMatchCandidate.
// The Go port implements MatchCandidate, WithPrimaryKeyMatchCandidate,
// WithBaseQuantifierMatchCandidate, and OrderingPartsComputer — the last of
// these standing in for the ValueIndexLikeMatchCandidate default that gives a
// Java primary scan its key order.
//
// Key distinction from ValueIndexScanMatchCandidate: the primary scan
// has separate "available" vs "queried" record types. Available types
// are all types in the store; queried types are the subset the query
// needs. Whether a TypeFilterPlan wraps the scan depends on
// HasAndOrderedByRecordTypeKey(), not on whether available and queried
// differ — see ToScanPlan.
type PrimaryScanMatchCandidate struct {
	// parameters are the sargable aliases — one per primary key column.
	parameters []values.CorrelationIdentifier

	// traversal is the Traversal of the primary scan graph.
	traversal     *Traversal
	traversalOnce sync.Once

	// availableRecordTypes are all record types in the store.
	availableRecordTypes []string

	// queriedRecordTypes are the record types the query actually needs.
	queriedRecordTypes []string

	// primaryKeyColumns are the column names of the primary key.
	primaryKeyColumns []string
	// keyComponentTypes is authoritative physical primary-key metadata aligned
	// with primaryKeyColumns. It is separate from comparison operand types.
	keyComponentTypes []values.Type

	// hasRecordTypeKeyPrefix reports whether the FULL primary key (the one
	// backing this candidate, not just primaryKeyColumns — whose structural
	// projection intentionally excludes the separately supplied type-key
	// coordinate) starts with a RecordTypeKeyExpression. Mirrors Java's
	// PrimaryScanMatchCandidate.hasAndOrderedByRecordTypeKey(), computed
	// once by the caller from RecordType.PrimaryKeyHasRecordTypePrefix()
	// (or RecordMetaData.PrimaryKeyHasRecordTypePrefix() for the
	// store-wide case) and carried here rather than re-derived, since the
	// candidate does not keep the raw KeyExpression.
	//
	// When true, every row this scan can physically produce already
	// belongs to the queried type — the FDB key range is prefixed by the
	// record-type ordinal by construction (see executeScan) — so
	// ToScanPlan does not need a TypeFilterPlan on top. When false, the
	// scan's key range is not type-discriminated (an intermingled store or
	// a store using a primary key without the type-key prefix) and rows of
	// other available types can genuinely appear, so the wrap is required.
	hasRecordTypeKeyPrefix bool

	// baseType is the record type flowing through the scan.
	baseType values.Type

	// primaryKeyValues is computed lazily — the PK columns as Value objects.
	primaryKeyValues     []values.Value
	primaryKeyValuesOnce sync.Once
}

// NewPrimaryScanMatchCandidate constructs a primary-scan match candidate.
//
//   - traversal: the Traversal of this candidate's expression tree (may
//     be nil if not yet computed; GetTraversal will build it lazily).
//   - parameters: sargable aliases, one per PK column.
//   - availableRecordTypes: all record types in the store.
//   - queriedRecordTypes: the subset of types the query needs.
//   - primaryKeyColumns: the PK column names in order.
//   - hasRecordTypeKeyPrefix: whether the primary key backing this
//     candidate starts with a RecordTypeKeyExpression — see the field
//     doc on hasRecordTypeKeyPrefix. Callers derive this from
//     RecordType.PrimaryKeyHasRecordTypePrefix() /
//     RecordMetaData.PrimaryKeyHasRecordTypePrefix().
//   - baseType: the record type of the scan output.
func NewPrimaryScanMatchCandidate(
	traversal *Traversal,
	parameters []values.CorrelationIdentifier,
	availableRecordTypes []string,
	queriedRecordTypes []string,
	primaryKeyColumns []string,
	hasRecordTypeKeyPrefix bool,
	baseType values.Type,
) *PrimaryScanMatchCandidate {
	params := make([]values.CorrelationIdentifier, len(parameters))
	copy(params, parameters)
	avail := make([]string, len(availableRecordTypes))
	copy(avail, availableRecordTypes)
	queried := make([]string, len(queriedRecordTypes))
	copy(queried, queriedRecordTypes)
	pkCols := make([]string, len(primaryKeyColumns))
	copy(pkCols, primaryKeyColumns)

	return &PrimaryScanMatchCandidate{
		parameters:             params,
		traversal:              traversal,
		availableRecordTypes:   avail,
		queriedRecordTypes:     queried,
		primaryKeyColumns:      pkCols,
		keyComponentTypes:      physicalTypesFromFlatRow(baseType, pkCols, nil),
		hasRecordTypeKeyPrefix: hasRecordTypeKeyPrefix,
		baseType:               baseType,
	}
}

// WithKeyComponentTypes attaches authoritative physical primary-key types to
// a freshly constructed candidate.
func (c *PrimaryScanMatchCandidate) WithKeyComponentTypes(types []values.Type) *PrimaryScanMatchCandidate {
	c.keyComponentTypes = normalizePhysicalKeyTypes(types, len(c.primaryKeyColumns))
	return c
}

// GetKeyComponentTypes returns physical types aligned with the candidate's
// primary-key columns.
func (c *PrimaryScanMatchCandidate) GetKeyComponentTypes() []values.Type {
	return append([]values.Type(nil), c.keyComponentTypes...)
}

// HasAndOrderedByRecordTypeKey reports whether the primary key starts with
// the record type key, partitioning the scan by record type. Mirrors Java's
// PrimaryScanMatchCandidate.hasAndOrderedByRecordTypeKey().
func (c *PrimaryScanMatchCandidate) HasAndOrderedByRecordTypeKey() bool {
	return c.hasRecordTypeKeyPrefix
}

// CandidateName returns "primary(type1,type2,...)" using the available
// record types. Mirrors Java's PrimaryScanMatchCandidate.getName().
func (c *PrimaryScanMatchCandidate) CandidateName() string {
	return "primary(" + strings.Join(c.availableRecordTypes, ",") + ")"
}

// GetTraversal returns the Traversal of this candidate's expression
// tree, built lazily on first access via ExpandValueIndex. The
// traversal is stable once computed (sync.Once).
func (c *PrimaryScanMatchCandidate) GetTraversal() *Traversal {
	c.traversalOnce.Do(func() {
		if c.traversal == nil {
			c.traversal = ExpandValueIndex(c)
		}
	})
	return c.traversal
}

// GetColumnNames returns the primary key column names.
func (c *PrimaryScanMatchCandidate) GetColumnNames() []string {
	return c.primaryKeyColumns
}

// GetSargableAliases returns the ordered parameter list (one per PK
// column).
func (c *PrimaryScanMatchCandidate) GetSargableAliases() []values.CorrelationIdentifier {
	return c.parameters
}

// GetRecordTypes returns the queried record type names. This matches
// Go's MatchCandidate.GetRecordTypes() contract and Java's
// getQueriedRecordTypeNames().
func (c *PrimaryScanMatchCandidate) GetRecordTypes() []string {
	return c.queriedRecordTypes
}

// GetAvailableRecordTypes returns all record types available in the
// store. Mirrors Java's PrimaryScanMatchCandidate.getAvailableRecordTypes().
func (c *PrimaryScanMatchCandidate) GetAvailableRecordTypes() []string {
	return c.availableRecordTypes
}

// IsUnique returns true — the primary key is always unique.
func (c *PrimaryScanMatchCandidate) IsUnique() bool { return true }

// GetBaseType returns the base record type. Implements
// WithBaseQuantifierMatchCandidate.
func (c *PrimaryScanMatchCandidate) GetBaseType() values.Type {
	return c.baseType
}

// GetPrimaryKeyValues returns the primary key as a list of Values.
// Lazily computed from primaryKeyColumns. Returns nil if there are no
// PK columns. Implements WithPrimaryKeyMatchCandidate.
func (c *PrimaryScanMatchCandidate) GetPrimaryKeyValues() []values.Value {
	c.primaryKeyValuesOnce.Do(func() {
		c.primaryKeyValues = resolvedColumnsInRow(c.baseType, c.primaryKeyColumns)
	})
	return append([]values.Value(nil), c.primaryKeyValues...)
}

// orderingKeyLayout returns the ONE row layout this scan's ordering keys may be
// domained in — the record row the scan flows — or nil when that is not a record
// type. It is the primary-scan half of orderingKeyLayoutProvider, so an
// intersection partition can domain its comparison keys against a leg that is a
// primary scan exactly as it does against an index leg.
func (c *PrimaryScanMatchCandidate) orderingKeyLayout() *values.RecordType {
	rt, isRecord := c.baseType.(*values.RecordType)
	if !isRecord {
		return nil
	}
	return rt
}

// bakeOrderingColumn resolves a primary-key column name to a domained ordinal
// in the record row layout this scan flows. Same authority and same
// unique-match rule as the index candidate's — see bakeOrderingColumnIn.
func (c *PrimaryScanMatchCandidate) bakeOrderingColumn(name string) values.Value {
	rt := c.orderingKeyLayout()
	if rt == nil {
		return nil
	}
	return bakeOrderingColumnIn(rt, name)
}

// ComputeMatchedOrderingParts computes one ordering part per primary-key
// column the sort requests, in key order, carrying whatever comparison range
// the match bound for that column.
//
// Java's PrimaryScanMatchCandidate inherits this from
// ValueIndexLikeMatchCandidate, so a primary-scan match reports its key order
// exactly like an index-scan match. Without it a primary-scan match reports NO
// ordering, so it can never satisfy a requested ordering and never gets a
// reverse variant — `WHERE pk = ? ORDER BY pk2 DESC` fell back to an in-memory
// sort over the forward scan, and no descending IN-union over a primary scan
// could be enumerated at all.
//
// A primary key is a flat list of scalar columns (no fan-out), so the
// duplicate-producing branch of Java's shared implementation has nothing to
// act on here, and there is no trimmed-PK suffix to continue into: the primary
// key IS the full key.
func (c *PrimaryScanMatchCandidate) ComputeMatchedOrderingParts(
	matchInfo MatchInfo,
	sortParameterIDs []values.CorrelationIdentifier,
	isReverse bool,
) []*MatchedOrderingPart {
	var bindings map[values.CorrelationIdentifier]*predicates.ComparisonRange
	if matchInfo != nil {
		if regularInfo := matchInfo.GetRegularMatchInfo(); regularInfo != nil {
			bindings = regularInfo.GetParameterBindingMap()
		}
	}

	sortOrder := MatchedSortOrderAscending
	if isReverse {
		sortOrder = MatchedSortOrderDescending
	}

	var parts []*MatchedOrderingPart
	for _, paramID := range sortParameterIDs {
		idx := -1
		for i, alias := range c.parameters {
			if alias == paramID {
				idx = i
				break
			}
		}
		// A parameter outside the key, or past the columns this candidate
		// knows about, ends the usable ordering prefix — the positions after
		// it are not contiguous and would claim an order the records do not
		// have.
		if idx < 0 || idx >= len(c.primaryKeyColumns) {
			break
		}
		// The same termination the index candidate applies: a FLOAT/DOUBLE
		// primary-key column's physical key order is not its logical order
		// (NaN packs into two disjoint blocks while the comparator ranks all
		// NaN equal and greatest), so it cannot extend the claim and neither
		// can any column after it.
		if !values.ColumnCanExtendOrderingClaim(c.orderingKeyLayout(), c.primaryKeyColumns[idx]) {
			break
		}
		// Childless FieldValue, matching the index candidate — resolved
		// against the record row layout the scan flows, so the key states a
		// column identity rather than a display name. A primary scan can be an
		// intersection leg beside an index scan, and both legs' comparison keys
		// meet in one merged ordering: they must be domained in the SAME record
		// layout or the merge hands them different tokens and collapses.
		colValue := c.bakeOrderingColumn(c.primaryKeyColumns[idx])
		if colValue == nil {
			break
		}
		parts = append(parts, NewMatchedOrderingPart(
			paramID, colValue, bindings[paramID], sortOrder))
	}
	return parts
}

// ComputeBoundParameterPrefixMap walks the sargable aliases in order
// and collects the longest prefix satisfying index scan discipline:
// N equalities + optional trailing inequality.
//
// Identical to ValueIndexScanMatchCandidate.ComputeBoundParameterPrefixMap
// — mirrors Java's default MatchCandidate.computeBoundParameterPrefixMap.
func (c *PrimaryScanMatchCandidate) ComputeBoundParameterPrefixMap(
	bindings map[values.CorrelationIdentifier]*predicates.ComparisonRange,
) map[values.CorrelationIdentifier]*predicates.ComparisonRange {
	prefix := make(map[values.CorrelationIdentifier]*predicates.ComparisonRange)
	for i, alias := range c.parameters {
		cr, ok := bindings[alias]
		if !ok || cr == nil || cr.IsEmpty() {
			return prefix
		}
		if !candidatePhysicalKeyTypeKnown(c.keyComponentTypes, i) {
			// Without an authoritative tuple-wire domain, the comparison remains
			// residual. Its declared/runtime type cannot safely stand in for key
			// metadata shared across record types.
			return prefix
		}
		if candidateRangeHasUnsupportedPhysicalStartsWith(cr, c.keyComponentTypes, i) {
			// STARTS_WITH is a tuple PREFIX_STRING range only for an
			// authoritative STRING key. Leave every other physical type as a
			// residual predicate rather than claiming scan compensation.
			return prefix
		}
		if candidateRangeHasKnownConstantNaN(cr, c.keyComponentTypes, i) {
			// Leave every visible NaN comparison as residual compensation. A
			// single FDB tuple payload is neither exact equality nor an exact
			// ordered endpoint for the comparator's canonical NaN value.
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

// ToScanPlan converts the matched prefix into a physical plan. When the
// primary key is NOT ordered by the record-type key
// (HasAndOrderedByRecordTypeKey() is false), the scan's key range is not
// type-discriminated and can genuinely produce rows of any available type,
// so the scan is wrapped in a TypeFilterPlan restricting it to the queried
// types.
//
// Mirrors Java's PrimaryScanMatchCandidate.toEquivalentPlan(): extracts
// ComparisonRanges from the prefix map in PK column order and embeds
// them as ScanComparisons in the RecordQueryScanPlan, then wraps in a
// TypeFilterPlan unless hasAndOrderedByRecordTypeKey() — a scan whose key
// range is already prefixed by the record-type ordinal cannot yield any
// other type, so a filter on top would be pure overhead (and, worse for
// costing, an unearned selectivity discount; see TypeFilterCost).
func (c *PrimaryScanMatchCandidate) ToScanPlan(
	prefixMap map[values.CorrelationIdentifier]*predicates.ComparisonRange,
	reverse bool,
) plans.RecordQueryPlan {
	// Build the base scan over available record types.
	flowedType := c.baseType
	if flowedType == nil {
		flowedType = values.UnknownType
	}

	// Use queriedRecordTypes (not availableRecordTypes) so that executeScan
	// can correctly prepend the RecordTypeKey prefix for point lookups.
	// The TypeFilterPlan wrapper (added below when types differ) handles
	// the broader filtering; the scan itself targets the specific type.
	scanPlan, err := plans.NewRecordQueryScanPlan(c.queriedRecordTypes, flowedType, reverse)
	if err != nil {
		return nil
	}

	// Attach primary key values if available.
	if pkVals := c.GetPrimaryKeyValues(); len(pkVals) > 0 {
		scanPlan = scanPlan.WithPrimaryKey(pkVals)
	}
	// Physical key types govern both bound probes and the ordering of an
	// entirely unbound primary scan. Stamp them independently of whether a
	// comparison prefix exists; otherwise an unbound LONG PK degrades to
	// Unknown and loses its natural ordering.
	scanPlan = scanPlan.WithKeyComponentTypes(c.keyComponentTypes)

	// Extract scan comparisons from the prefix map in PK column order.
	// Mirrors Java's toScanComparisons(comparisonRanges).
	if len(prefixMap) > 0 {
		comps := make([]*predicates.ComparisonRange, 0, len(c.parameters))
		for _, alias := range c.parameters {
			cr, ok := prefixMap[alias]
			if !ok || cr == nil {
				break
			}
			comps = append(comps, cr)
		}
		if len(comps) > 0 {
			scanPlan = scanPlan.WithScanComparisons(comps)
		}
	}

	// A record-type-key-prefixed primary key already scopes the physical
	// scan to the queried type (see executeScan) — no filter needed.
	// Mirrors Java's `if (hasAndOrderedByRecordTypeKey()) { return scanPlan; }`.
	if c.hasRecordTypeKeyPrefix {
		return scanPlan
	}

	// Wrap in a TypeFilterPlan to restrict to queried types.
	filtered, err := plans.NewRecordQueryTypeFilterPlan(c.queriedRecordTypes, scanPlan)
	if err != nil {
		return nil
	}
	return filtered
}

// String returns a human-readable label for debugging.
// Mirrors Java's PrimaryScanMatchCandidate.toString().
func (c *PrimaryScanMatchCandidate) String() string {
	return "primary[" + strings.Join(c.queriedRecordTypes, ",") + "]"
}

var (
	_ MatchCandidate                   = (*PrimaryScanMatchCandidate)(nil)
	_ OrderingPartsComputer            = (*PrimaryScanMatchCandidate)(nil)
	_ WithPrimaryKeyMatchCandidate     = (*PrimaryScanMatchCandidate)(nil)
	_ WithBaseQuantifierMatchCandidate = (*PrimaryScanMatchCandidate)(nil)
)
