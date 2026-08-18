package plans

import (
	"fmt"
	"slices"
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
	// distinctProofIndexName names the secondary UNIQUE index whose uniqueness
	// licensed eliding a DISTINCT above this scan (distinct_proof_stamp.go).
	distinctProofIndexName string
	// keyComponentTypes is aligned with scanComparisons. It carries the
	// physical index-key width (not the RHS type), which is load-bearing for
	// FLOAT versus DOUBLE tuple encoding.
	keyComponentTypes []values.Type
	// primaryKeyComponentTypes is aligned with pkColumnNames. Index entries
	// append the trimmed primary key after the index key, so these types are
	// required before that suffix can support an ORDER BY claim. In particular,
	// an appended FLOAT/DOUBLE PK is not logically ordered in the presence of
	// arbitrary raw NaN encodings.
	primaryKeyComponentTypes []values.Type
	// physicalGroupingPrefixCount is aggregate-index metadata. For a permuted
	// MIN/MAX key it is g-p: only that many logical grouping columns occur
	// contiguously before the aggregate value in the physical BY_GROUP key.
	physicalGroupingPrefixCount int
	physicalGroupingPrefixKnown bool

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
	// Retained for coordinate-safe covering reconstruction even on fan-out
	// indexes. Fan-out ordering is suppressed independently by the stamped
	// createsDuplicates signal, because positions after an exploded key part
	// are not one globally sorted logical stream.
	pkColumnNames []string
	// unique reports whether the index is declared UNIQUE. A full equality
	// bind then has a bounded physical-key multiplicity only when component
	// types and values prove it: normally one, 2^k for k signed-zero float
	// components, and unknown for NULL under NULLS-DISTINCT semantics.
	unique bool
	// valueColumnNames are the covering-only columns of a KeyWithValue root —
	// stored in the FDB VALUE, past the split point. They extend the covering
	// surface (the executor reads them from the entry VALUE tuple) but are not
	// key columns: they never order the scan and never bound its range.
	valueColumnNames []string
	// orderingKeyNamesKnown/orderingKeyNamesSafe state whether columnNames are
	// semantic bare-field ordering keys. Function indexes retain leaf names for
	// row layout, but CARDINALITY(TAGS) must not advertise ordering on TAGS.
	// WithIndexMetadata establishes the ordinary bare-field default; candidate
	// stamping can explicitly mark it unsafe. The state is tri-valued so later
	// metadata-preserving copies cannot accidentally turn an unsafe plan safe.
	orderingKeyNamesKnown bool
	orderingKeyNamesSafe  bool
	// createsDuplicates and distinctRecordsKnown carry the match candidate's
	// fan-out signal onto the plan for the DistinctRecords property. Java's
	// DistinctRecordsProperty.visitIndexPlan returns !matchCandidate.createsDuplicates()
	// (empty candidate → false) — a fan-out index (repeated/collection field)
	// creates duplicates; a scalar-field index does NOT, regardless of
	// uniqueness. distinctRecordsKnown is false until the signal is stamped
	// (WithDistinctRecordsSignal), mirroring Java's empty-candidate default.
	createsDuplicates    bool
	distinctRecordsKnown bool
	// commonPrimaryKeyValues is the index's common primary key translated to
	// STRUCTURE-encoding Values (RFC-189 B3 / Java PrimaryKeyProperty.visitIndexPlan
	// via ScalarTranslationVisitor): record-type-key prefixes and nesting are
	// encoded so PK identity compares by structure, not bare column names. Feeds
	// PrimaryKeyProperty (computePrimaryKey's index arm) so ImplementDistinctUnionRule
	// dedups two index legs only when they carry the SAME structural PK — never
	// conflating Field("ID") with Concat(RecordTypeKey(), Field("ID")). nil = the
	// property abstains (no dedup); populated REGARDLESS of fan-out (index entries
	// always carry the PK). pkColumnNames is also retained for coordinate-safe
	// coverage on fan-out scans; the duplicate signal independently suppresses
	// its use as an ordering suffix.
	commonPrimaryKeyValues []values.Value
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
) (*RecordQueryIndexPlan, error) {
	base, err := newPlanExprBaseForType("RecordQueryIndexPlan", flowedType)
	if err != nil {
		return nil, err
	}
	comps := make([]*predicates.ComparisonRange, len(scanComparisons))
	copy(comps, scanComparisons)
	return &RecordQueryIndexPlan{
		PlanExprBase:      base,
		indexName:         indexName,
		scanComparisons:   comps,
		keyComponentTypes: normalizeKeyComponentTypes(nil, len(comps)),
		recordTypes:       dedupSortedStrings(recordTypes),
		flowedType:        base.resultValue.Type(),
		reverse:           reverse,
		resultValue:       base.resultValue,
	}, nil
}

// GetResultValue returns the index scan's STABLE per-instance result value — the
// single correlation identity a bare index scan carries as its own memo
// expression (RFC-184 W2). Falls back to PlanExprBase (a fresh QOV per call) for
// struct-literal test plans that bypass the constructor (resultValue is nil).
func (p *RecordQueryIndexPlan) GetResultValue() values.Value {
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
	cp := *p
	cp.scanComparisons = copied
	cp.keyComponentTypes = normalizeKeyComponentTypes(p.keyComponentTypes, len(comps))
	return &cp
}

// WithKeyComponentTypes returns a copy with authoritative physical index-key
// types. The vector may include unbound key suffix components needed for
// ordering congruence; it is never truncated to the comparison prefix.
func (p *RecordQueryIndexPlan) WithKeyComponentTypes(types []values.Type) *RecordQueryIndexPlan {
	cp := *p
	cp.keyComponentTypes = normalizeKeyComponentTypes(types, len(p.scanComparisons))
	return &cp
}

// GetKeyComponentTypes returns physical types aligned with index-key
// coordinates; the vector can be longer than GetScanComparisons.
func (p *RecordQueryIndexPlan) GetKeyComponentTypes() []values.Type {
	return append([]values.Type(nil), p.keyComponentTypes...)
}

// WithPrimaryKeyComponentTypes returns a copy carrying authoritative physical
// types aligned with GetPKColumnNames. These types affect ordering only; index
// scan bounds remain aligned with GetKeyComponentTypes.
func (p *RecordQueryIndexPlan) WithPrimaryKeyComponentTypes(types []values.Type) *RecordQueryIndexPlan {
	cp := *p
	cp.primaryKeyComponentTypes = normalizePrimaryKeyComponentTypes(types, len(p.pkColumnNames))
	return &cp
}

// GetPrimaryKeyComponentTypes returns physical types aligned with the
// untrimmed primary-key column metadata.
func (p *RecordQueryIndexPlan) GetPrimaryKeyComponentTypes() []values.Type {
	return append([]values.Type(nil), p.primaryKeyComponentTypes...)
}

// WithPhysicalGroupingPrefixCount marks the contiguous logical grouping-key
// prefix of a BY_GROUP aggregate index. It is carried on the underlying index
// plan so the aggregate wrapper can preserve candidate metadata without a
// second planner-side reconstruction.
func (p *RecordQueryIndexPlan) WithPhysicalGroupingPrefixCount(count int) *RecordQueryIndexPlan {
	if count < 0 {
		count = 0
	}
	cp := *p
	cp.physicalGroupingPrefixCount = count
	cp.physicalGroupingPrefixKnown = true
	return &cp
}

func (p *RecordQueryIndexPlan) physicalGroupingPrefix() (int, bool) {
	return p.physicalGroupingPrefixCount, p.physicalGroupingPrefixKnown
}

// WithCommonPrimaryKey returns a copy carrying the index's structural common
// primary key (RFC-189 B3). Shallow copy (cp := *p) so every other field
// auto-carries.
func (p *RecordQueryIndexPlan) WithCommonPrimaryKey(pk []values.Value) *RecordQueryIndexPlan {
	cp := *p
	if pk != nil {
		copied := make([]values.Value, len(pk))
		copy(copied, pk)
		cp.commonPrimaryKeyValues = copied
	} else {
		cp.commonPrimaryKeyValues = nil
	}
	return &cp
}

// GetCommonPrimaryKeyValues returns the index's structural common primary key
// (RFC-189 B3), or nil when unknown/abstaining.
func (p *RecordQueryIndexPlan) GetCommonPrimaryKeyValues() []values.Value {
	return p.commonPrimaryKeyValues
}

// GetColumnNames returns the index's key column names, in index-key order.
func (p *RecordQueryIndexPlan) GetColumnNames() []string { return p.columnNames }

// GetValueColumnNames returns the covering-only (FDB VALUE part) columns of a
// KeyWithValue-rooted index, or nil.
func (p *RecordQueryIndexPlan) GetValueColumnNames() []string { return p.valueColumnNames }

// WithValueColumnNames returns a shallow copy carrying the covering-only
// (FDB VALUE part) column names. Like the other index metadata, this is a
// function of the index the plan names and stays out of the structural key.
func (p *RecordQueryIndexPlan) WithValueColumnNames(names []string) *RecordQueryIndexPlan {
	cp := *p
	cp.valueColumnNames = make([]string, len(names))
	copy(cp.valueColumnNames, names)
	return &cp
}

// AllCoveredEntryColumns returns the entry's column names in ENTRY layout
// order — key columns, then the KeyWithValue VALUE part — the list the
// covering executor aligns positionally against (index key values ++ entry
// value tuple).
func (p *RecordQueryIndexPlan) AllCoveredEntryColumns() []string {
	if len(p.valueColumnNames) == 0 {
		return p.columnNames
	}
	out := make([]string, 0, len(p.columnNames)+len(p.valueColumnNames))
	out = append(out, p.columnNames...)
	out = append(out, p.valueColumnNames...)
	return out
}

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
// full-equality physical-multiplicity arm of the cost model.
//
// The names and UNIQUE bit remain derived metadata of the named index. The
// physical key-type vectors are different: they govern probe multiplicity and
// ordering congruence, so structuralKey folds them into plan identity. If a
// caller replaces the PK name coordinate system, any old parallel type proof
// is cleared to Unknown rather than applied to a different name by position.
func (p *RecordQueryIndexPlan) WithIndexMetadata(columnNames, pkColumnNames []string, unique bool) *RecordQueryIndexPlan {
	cp := *p
	cp.columnNames = append([]string(nil), columnNames...)
	cp.pkColumnNames = append([]string(nil), pkColumnNames...)
	if slices.Equal(p.pkColumnNames, pkColumnNames) {
		cp.primaryKeyComponentTypes = normalizePrimaryKeyComponentTypes(
			p.primaryKeyComponentTypes, len(pkColumnNames),
		)
	} else {
		cp.primaryKeyComponentTypes = normalizePrimaryKeyComponentTypes(nil, len(pkColumnNames))
	}
	cp.unique = unique
	if !cp.orderingKeyNamesKnown {
		cp.orderingKeyNamesKnown = true
		cp.orderingKeyNamesSafe = true
	}
	return &cp
}

// WithOrderingKeyNamesUnavailable returns a copy that retains the physical
// column names for row layout/costing but forbids plan-level ordering synthesis
// from them. Use for expression-key indexes whose semantic ordering Values are
// not carried on RecordQueryIndexPlan.
func (p *RecordQueryIndexPlan) WithOrderingKeyNamesUnavailable() *RecordQueryIndexPlan {
	cp := *p
	cp.orderingKeyNamesKnown = true
	cp.orderingKeyNamesSafe = false
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
// mirroring Java's full scanParameters equality), the reverse / strictlySorted
// flags, the record-type set, and the flowed type (equals-only). Coveringness is
// NOT folded here and cannot be: it is a distinct plan type wrapping this one,
// whose own structuralKey folds this key as a sub-key. The
// stable per-instance resultValue is excluded (RFC-184 W2). The same key drives
// EqualsPlanWithoutChildren and HashCodeWithoutChildren.
func (p *RecordQueryIndexPlan) structuralKey() *structuralKey {
	return newStructuralKey().
		Str(p.indexName).
		ScanComps(p.scanComparisons).
		Types(p.keyComponentTypes).
		Types(p.primaryKeyComponentTypes).
		Bool(p.physicalGroupingPrefixKnown).
		Int(p.physicalGroupingPrefixCount).
		Bool(p.reverse).
		Bool(p.strictlySorted).
		Strs(p.recordTypes).
		Type(p.flowedType).
		Str(p.distinctProofIndexName)
}

func (p *RecordQueryIndexPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryIndexPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

func (p *RecordQueryIndexPlan) HashCodeWithoutChildren() uint64 {
	if hash, ok := p.cachedStructuralHash(p); ok {
		return hash
	}
	hash := p.structuralKey().Hash("indexplan|")
	p.storeStructuralHash(p, hash)
	return hash
}

// Explain renders a one-line label. A bare index scan is never covering — a
// covering scan is a distinct plan type wrapping this one, and it renders
// through explainWithCovering.
func (p *RecordQueryIndexPlan) Explain() string { return p.explainScan(false) }

// explainWithCovering renders this scan's label carrying the COVERING marker.
// It exists so RecordQueryCoveringIndexPlan can render its inner WITHOUT
// string surgery on an already-rendered label: splicing at the last "]" both
// assumes a bracket that a future label change could move and double-stamps
// whenever the inner already carries the marker. Passing the flag down to the
// one builder that knows the layout makes both impossible.
func (p *RecordQueryIndexPlan) explainWithCovering() string { return p.explainScan(true) }

func (p *RecordQueryIndexPlan) explainScan(covering bool) string {
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
	if covering {
		b.WriteString(" COVERING")
	}
	if p.reverse {
		b.WriteString(") REVERSE")
	} else {
		b.WriteString(")")
	}
	b.WriteString(explainDistinctProofSuffix(p.distinctProofIndexName))
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
func (p *RecordQueryIndexPlan) WithQuantifiers(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if err := validateQuantifierArity("RecordQueryIndexPlan", len(qs), 0); err != nil {
		return nil, err
	}
	return p, nil
}

// GetCorrelatedToWithoutChildren reports the correlations reached through this
// scan's comparison operands, mirroring physicalIndexScanWrapper.
func (p *RecordQueryIndexPlan) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return scanComparisonCorrelations(p.GetScanComparisons())
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryIndexPlan) GetRecordQueryPlan() RecordQueryPlan { return p }

// GetDistinctProofIndexName implements DistinctProofStamped. An ORDER-BY'd
// `SELECT DISTINCT <unique column>` elides its DISTINCT over an INDEX scan, so
// the index plan is a carrier too — and the index it SCANS need not be the index
// that PROVED the elision, which is why the stamp is its own field rather than
// being read back off indexName.
func (p *RecordQueryIndexPlan) GetDistinctProofIndexName() string {
	return p.distinctProofIndexName
}

// WithDistinctProofIndexName implements DistinctProofStampable.
func (p *RecordQueryIndexPlan) WithDistinctProofIndexName(indexName string) RecordQueryPlan {
	cp := *p
	cp.distinctProofIndexName = indexName
	return &cp
}

var _ DistinctProofStampable = (*RecordQueryIndexPlan)(nil)
