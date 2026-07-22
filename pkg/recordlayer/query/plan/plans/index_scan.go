package plans

import (
	"fmt"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryIndexPlan is an index scan over a secondary index —
// reads index entries whose key prefix satisfies the scan comparisons,
// then fetches the corresponding records. Mirrors Java's
// `RecordQueryIndexPlan`.
//
// Seed surface:
//   - IndexName: name of the index being scanned.
//   - ScanComparisons: ordered list of ComparisonRanges (one per
//     index key column, left-to-right). The prefix defines the FDB
//     key range: equality ranges become exact prefix bytes, the first
//     inequality becomes range bounds, and the rest are empty (full
//     scan for those suffix columns).
//   - RecordTypes: which record types the index covers.
//   - Reverse: scan direction.
//   - FlowedType: rich Type of the row stream.
//
// The index scan is a LEAF in the plan tree — it reads directly from
// FDB (the index subspace). A follow-up fetch step may be needed if
// the index is non-covering; that lands as a separate plan node
// (RecordQueryFetchFromPartialRecordPlan in Java) when covering-index
// rules port.
type RecordQueryIndexPlan struct {
	PlanExprBase
	indexName       string
	scanComparisons []*predicates.ComparisonRange
	recordTypes     []string
	flowedType      values.Type
	reverse         bool
	strictlySorted  bool
	covering        bool
	coveringColumns []string

	// columnNames is the index's key column list, in index-key order. It
	// drives the ordering the scan provides: the non-equality-bound suffix
	// of these columns is sorted.
	columnNames []string
	// pkColumnNames is the record type's primary-key column list, used to
	// extend the ordering past the index key: a value index's entries are
	// (index key, primary key), so the scan's output order covers the
	// trimmed PK suffix too. Mirrors Java's
	// ValueIndexExpansionVisitor.fullKey(index, primaryKey) — the ordering in
	// ValueIndexLikeMatchCandidate.computeOrderingFromScanComparisons is
	// derived over the FULL key (index root + trimPrimaryKey'd PK), which is
	// what lets an equality-prefixed scan (status = ?) satisfy ORDER BY pk.
	// Empty for fan-out (createsDuplicates) indexes, where positions past the
	// fan-out are not sort-ordered (Java breaks the sorted-suffix loop at a
	// duplicating key part).
	pkColumnNames []string
	// unique reports whether the index is declared UNIQUE — an all-equality
	// scan over a unique index's full key yields at most one row.
	unique bool
	// createsDuplicates and distinctRecordsKnown carry the match candidate's
	// fan-out signal onto the plan for the DistinctRecords property. Java's
	// DistinctRecordsProperty.visitIndexPlan returns !matchCandidate.createsDuplicates()
	// (empty candidate → false) — a fan-out index (repeated/collection field)
	// creates duplicates; a scalar-field index does NOT, regardless of
	// uniqueness. distinctRecordsKnown is false until the signal is stamped
	// (WithDistinctRecordsSignal), mirroring Java's empty-candidate default.
	createsDuplicates    bool
	distinctRecordsKnown bool
	// resultValue is the stable per-instance QuantifiedObjectValue standing for
	// the rows this leaf emits — minted once at construction, returned by
	// GetResultValue, EXCLUDED from Equals/Hash (its correlation id is unique per
	// instance). A bare leaf that stands as its own Cascades expression must
	// present a consistent row identity across repeated interrogations, the role
	// physicalIndexScanWrapper's fresh-per-call GetResultValue could not (RFC-184
	// W2). Carried through every With* struct-copy. nil for struct-literal test
	// plans that bypass the constructor — GetResultValue falls back to PlanExprBase.
	resultValue values.Value
}

// NewRecordQueryIndexPlan constructs an index scan plan.
func NewRecordQueryIndexPlan(
	indexName string,
	scanComparisons []*predicates.ComparisonRange,
	recordTypes []string,
	flowedType values.Type,
	reverse bool,
) *RecordQueryIndexPlan {
	if flowedType == nil {
		flowedType = values.UnknownType
	}
	comps := make([]*predicates.ComparisonRange, len(scanComparisons))
	copy(comps, scanComparisons)
	return &RecordQueryIndexPlan{
		indexName:       indexName,
		scanComparisons: comps,
		recordTypes:     dedupSortedStrings(recordTypes),
		flowedType:      flowedType,
		reverse:         reverse,
		resultValue:     values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier()),
	}
}

// GetResultValue returns the index scan's STABLE per-instance result value — the
// single correlation identity a bare index scan carries as its own memo
// expression (RFC-184 W2). Falls back to PlanExprBase (a fresh QOV per call) for
// struct-literal test plans that bypass the constructor (resultValue is nil).
func (p *RecordQueryIndexPlan) GetResultValue() values.Value {
	if p.resultValue == nil {
		return p.PlanExprBase.GetResultValue()
	}
	return p.resultValue
}

// GetIndexName returns the index name.
func (p *RecordQueryIndexPlan) GetIndexName() string { return p.indexName }

// GetScanComparisons returns the per-column comparison ranges.
func (p *RecordQueryIndexPlan) GetScanComparisons() []*predicates.ComparisonRange {
	return p.scanComparisons
}

// WithScanComparisons returns a copy of the plan with new per-column comparison
// ranges, preserving every other field (covering/coveringColumns/strictlySorted/
// reverse/flowedType/recordTypes). Used by the RFC-153 buried-merge correlation
// rebase to rewrite a SARG comparand without losing the index's covering metadata.
func (p *RecordQueryIndexPlan) WithScanComparisons(comps []*predicates.ComparisonRange) *RecordQueryIndexPlan {
	copied := make([]*predicates.ComparisonRange, len(comps))
	copy(copied, comps)
	return &RecordQueryIndexPlan{
		indexName:            p.indexName,
		scanComparisons:      copied,
		recordTypes:          p.recordTypes,
		flowedType:           p.flowedType,
		reverse:              p.reverse,
		strictlySorted:       p.strictlySorted,
		covering:             p.covering,
		coveringColumns:      p.coveringColumns,
		columnNames:          p.columnNames,
		pkColumnNames:        p.pkColumnNames,
		unique:               p.unique,
		createsDuplicates:    p.createsDuplicates,
		distinctRecordsKnown: p.distinctRecordsKnown,
		resultValue:          p.resultValue,
	}
}

// GetColumnNames returns the index's key column names, in index-key order.
func (p *RecordQueryIndexPlan) GetColumnNames() []string { return p.columnNames }

// GetPKColumnNames returns the record type's primary-key column names.
func (p *RecordQueryIndexPlan) GetPKColumnNames() []string { return p.pkColumnNames }

// IsUnique reports whether the scanned index is declared UNIQUE.
func (p *RecordQueryIndexPlan) IsUnique() bool { return p.unique }

// WithDistinctRecordsSignal stamps the match candidate's fan-out signal onto a
// shallow copy (Java DistinctRecordsProperty). Call it when building an index
// plan from a candidate that exposes createsDuplicates(); it marks the signal
// known so the DistinctRecords property no longer falls back to the
// empty-candidate default.
func (p *RecordQueryIndexPlan) WithDistinctRecordsSignal(createsDuplicates bool) *RecordQueryIndexPlan {
	cp := *p
	cp.createsDuplicates = createsDuplicates
	cp.distinctRecordsKnown = true
	return &cp
}

// ProducesDistinctRecords ports Java DistinctRecordsProperty.visitIndexPlan:
// an index scan produces distinct records iff its match candidate did NOT
// create duplicates. Until the signal is stamped (no candidate) it returns
// false — Java's empty-candidate default. Independent of UNIQUE: a non-unique
// scalar index does not create duplicates and so IS distinct.
func (p *RecordQueryIndexPlan) ProducesDistinctRecords() bool {
	return p.distinctRecordsKnown && !p.createsDuplicates
}

// WithIndexMetadata returns a shallow copy carrying the index's key columns,
// the record type's primary-key columns, and the UNIQUE flag. These describe
// the INDEX, not the scan: they are inputs to ordering derivation and to the
// point-lookup arm of the cost model.
//
// They are deliberately NOT part of HashCodeWithoutChildren /
// EqualsPlanWithoutChildren. Node identity for an index scan is (index name,
// scan comparisons, record types, reverse, strictlySorted, covering) — two
// scans of the same index with the same comparisons ARE the same memo node,
// and the metadata is a function of the index they both name, so folding it
// into the hash could only ever split identical nodes apart and shift memo
// dedup without changing what the nodes mean.
func (p *RecordQueryIndexPlan) WithIndexMetadata(columnNames, pkColumnNames []string, unique bool) *RecordQueryIndexPlan {
	cp := *p
	cp.columnNames = columnNames
	cp.pkColumnNames = pkColumnNames
	cp.unique = unique
	return &cp
}

// GetRecordTypes returns the covered record types.
func (p *RecordQueryIndexPlan) GetRecordTypes() []string { return p.recordTypes }

// GetFlowedType returns the rich row Type.
func (p *RecordQueryIndexPlan) GetFlowedType() values.Type { return p.flowedType }

// IsReverse reports the scan direction.
func (p *RecordQueryIndexPlan) IsReverse() bool { return p.reverse }

// IsStrictlySorted reports whether the scan's ordering uniquely
// determines each record (no two adjacent records share the same key).
// Set by RemoveSortRule when DISTINCT covers all ordering keys or a
// unique index satisfies the full key set.
func (p *RecordQueryIndexPlan) IsStrictlySorted() bool { return p.strictlySorted }

// WithStrictlySorted returns a shallow copy with strictlySorted=true.
func (p *RecordQueryIndexPlan) WithStrictlySorted() *RecordQueryIndexPlan {
	cp := *p
	cp.strictlySorted = true
	return &cp
}

// IsCovering reports whether the index provides all columns needed by
// the query, eliminating the need to fetch the full record by PK.
func (p *RecordQueryIndexPlan) IsCovering() bool { return p.covering }

// GetCoveringColumns returns the index column names when covering.
func (p *RecordQueryIndexPlan) GetCoveringColumns() []string { return p.coveringColumns }

// WithCovering returns a shallow copy marked as a covering index scan.
func (p *RecordQueryIndexPlan) WithCovering(columns []string) *RecordQueryIndexPlan {
	cp := *p
	cp.covering = true
	cp.coveringColumns = make([]string, len(columns))
	copy(cp.coveringColumns, columns)
	return &cp
}

// GetResultType returns the row Type.
func (p *RecordQueryIndexPlan) GetResultType() values.Type { return p.flowedType }

// GetChildren returns nil — index scans are leaves.
func (p *RecordQueryIndexPlan) GetChildren() []RecordQueryPlan { return nil }

// EqualsWithoutChildren compares index name, scan comparisons (shape AND
// comparands), record types, and reverse flag. The comparands are load-bearing:
// an index scan is a memo LEAF, deduped by HashCodeWithoutChildren +
// EqualsWithoutChildren, so comparing only the range SHAPE would collapse
// IndexScan([= 5]) and IndexScan([= 7]) into one Reference and let extraction
// materialize the wrong-comparand scan. Mirrors Java's
// RecordQueryIndexPlan.equalsWithoutChildren, which compares
// Objects.equals(scanParameters, that.scanParameters) — full comparand equality.
// structuralKey folds the index scan's identity: index name, SARG comparison
// ranges (shape AND comparands — two different-comparand scans must stay in
// distinct References, else the memo materializes the wrong-comparand scan,
// mirroring Java's full scanParameters equality), the reverse / strictlySorted /
// covering flags, the record-type set, and the flowed type (equals-only). The
// stable per-instance resultValue is excluded (RFC-184 W2). The same key drives
// EqualsPlanWithoutChildren and HashCodeWithoutChildren.
func (p *RecordQueryIndexPlan) structuralKey() *structuralKey {
	return newStructuralKey().
		Str(p.indexName).
		ScanComps(p.scanComparisons).
		Bool(p.reverse).
		Bool(p.strictlySorted).
		Bool(p.covering).
		Strs(p.recordTypes).
		Type(p.flowedType)
}

func (p *RecordQueryIndexPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryIndexPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

func (p *RecordQueryIndexPlan) HashCodeWithoutChildren() uint64 {
	return p.structuralKey().Hash("indexplan|")
}

// Explain renders a one-line label.
func (p *RecordQueryIndexPlan) Explain() string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("IndexScan(%s, [", p.indexName))
	for i, cr := range p.scanComparisons {
		if i > 0 {
			b.WriteString(", ")
		}
		switch cr.GetRangeType() {
		case predicates.ComparisonRangeEmpty:
			b.WriteString("*")
		case predicates.ComparisonRangeEquality:
			b.WriteString("=")
		case predicates.ComparisonRangeInequality:
			b.WriteString("<>")
		}
	}
	b.WriteString("]")
	if p.covering {
		b.WriteString(" COVERING")
	}
	if p.reverse {
		b.WriteString(") REVERSE")
	} else {
		b.WriteString(")")
	}
	return b.String()
}

var (
	_ RecordQueryPlan                  = (*RecordQueryIndexPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryIndexPlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryIndexPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// WithQuantifiers returns this plan unchanged — it has no quantifiers to
// replace while children are raw pointers (RFC-183 P5 step 1).
func (p *RecordQueryIndexPlan) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return p
}

// GetCorrelatedToWithoutChildren reports the correlations reached through this
// scan's comparison operands, mirroring physicalIndexScanWrapper.
func (p *RecordQueryIndexPlan) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return scanComparisonCorrelations(p.GetScanComparisons())
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryIndexPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
