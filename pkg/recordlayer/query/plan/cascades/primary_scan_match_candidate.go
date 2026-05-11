package cascades

import (
	"strings"
	"sync"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
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
// and WithBaseQuantifierMatchCandidate.
//
// Key distinction from ValueIndexScanMatchCandidate: the primary scan
// has separate "available" vs "queried" record types. Available types
// are all types in the store; queried types are the subset the query
// needs. When they differ, a TypeFilterPlan wraps the scan.
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
//   - baseType: the record type of the scan output.
func NewPrimaryScanMatchCandidate(
	traversal *Traversal,
	parameters []values.CorrelationIdentifier,
	availableRecordTypes []string,
	queriedRecordTypes []string,
	primaryKeyColumns []string,
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
		parameters:           params,
		traversal:            traversal,
		availableRecordTypes: avail,
		queriedRecordTypes:   queried,
		primaryKeyColumns:    pkCols,
		baseType:             baseType,
	}
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
		if len(c.primaryKeyColumns) == 0 {
			return
		}
		pkVals := make([]values.Value, len(c.primaryKeyColumns))
		for i, col := range c.primaryKeyColumns {
			pkVals[i] = &values.FieldValue{Field: col, Typ: values.UnknownType}
		}
		c.primaryKeyValues = pkVals
	})
	return c.primaryKeyValues
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
	for _, alias := range c.parameters {
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

// ToScanPlan converts the matched prefix into a physical plan. If the
// queried record types are a strict subset of the available types (and
// the PK does not begin with a record-type discriminator), the scan is
// wrapped in a TypeFilterPlan.
//
// Mirrors Java's PrimaryScanMatchCandidate.toEquivalentPlan(), adapted
// to Go's simplified plan constructors.
func (c *PrimaryScanMatchCandidate) ToScanPlan(
	prefixMap map[values.CorrelationIdentifier]*predicates.ComparisonRange,
	reverse bool,
) plans.RecordQueryPlan {
	// Build the base scan over available record types.
	flowedType := c.baseType
	if flowedType == nil {
		flowedType = values.UnknownType
	}

	scanPlan := plans.NewRecordQueryScanPlan(c.availableRecordTypes, flowedType, reverse)

	// Attach primary key values if available.
	if pkVals := c.GetPrimaryKeyValues(); len(pkVals) > 0 {
		scanPlan = scanPlan.WithPrimaryKey(pkVals)
	}

	// If available == queried, no filter needed.
	if stringSlicesEqual(c.availableRecordTypes, c.queriedRecordTypes) {
		return scanPlan
	}

	// Wrap in a TypeFilterPlan to restrict to queried types.
	return plans.NewRecordQueryTypeFilterPlan(c.queriedRecordTypes, scanPlan)
}

// String returns a human-readable label for debugging.
// Mirrors Java's PrimaryScanMatchCandidate.toString().
func (c *PrimaryScanMatchCandidate) String() string {
	return "primary[" + strings.Join(c.queriedRecordTypes, ",") + "]"
}

// stringSlicesEqual reports whether two string slices have the same
// elements (order-sensitive).
func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var (
	_ MatchCandidate                   = (*PrimaryScanMatchCandidate)(nil)
	_ WithPrimaryKeyMatchCandidate     = (*PrimaryScanMatchCandidate)(nil)
	_ WithBaseQuantifierMatchCandidate = (*PrimaryScanMatchCandidate)(nil)
)
