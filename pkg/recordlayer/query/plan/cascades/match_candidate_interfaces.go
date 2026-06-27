package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// ValueIndexLikeMatchCandidate is the interface for match candidates
// defined over value-based index-like data structures — secondary
// value indexes (ValueIndexScanMatchCandidate) and the primary scan
// (PrimaryScanMatchCandidate).
//
// Java's ValueIndexLikeMatchCandidate provides default implementations
// for computeMatchedOrderingParts and computeOrderingFromScanComparisons.
// In Go these are not embedded as defaults (Go has no default methods);
// they live in the OrderingPartsComputer interface and package-level
// functions respectively. This interface captures the structural
// contract — implementors carry a base type and column-level metadata
// beyond what the base MatchCandidate requires.
//
// Ports Java's
// `com.apple.foundationdb.record.query.plan.cascades.ValueIndexLikeMatchCandidate`.
type ValueIndexLikeMatchCandidate interface {
	MatchCandidate

	// GetBaseType returns the base record type for this candidate.
	// Ports Java's WithBaseQuantifierMatchCandidate.getBaseType().
	GetBaseType() values.Type

	// GetColumnSize returns the number of key columns in the index
	// (or primary key). Ports Java's MatchCandidate.getColumnSize().
	GetColumnSize() int

	// CreatesDuplicates reports whether the index can produce
	// duplicate entries per record (e.g., a fan-out/repeated-field
	// index). Ports Java's MatchCandidate.createsDuplicates().
	CreatesDuplicates() bool

	// HasAndOrderedByRecordTypeKey reports whether the index key
	// starts with the record type key, partitioning the index by
	// record type. Ports Java's
	// MatchCandidate.hasAndOrderedByRecordTypeKey().
	HasAndOrderedByRecordTypeKey() bool

	// GetSargableAliasesRequiredForBinding returns the set of
	// sargable parameter aliases that MUST be bound for this
	// candidate to be valid. For example, if the index starts with
	// a record type key, the first alias is required.
	// Ports Java's MatchCandidate.getSargableAliasesRequiredForBinding().
	GetSargableAliasesRequiredForBinding() []values.CorrelationIdentifier
}

// ScanWithFetchMatchCandidate is the interface for match candidates
// that support the covering-index-to-fetch pattern. When an index
// scan covers enough columns, the fetch can be deferred; when it
// doesn't, a FetchFromPartialRecordPlan wraps the scan.
//
// The key method is PushValueThroughFetch, which attempts to translate
// a single Value from the full-record domain (correlated to
// sourceAlias) to the index-entry domain (correlated to targetAlias).
// If translation succeeds, the value can be evaluated on the
// partial/covering record without a full fetch.
//
// Ports Java's
// `com.apple.foundationdb.record.query.plan.cascades.ScanWithFetchMatchCandidate`.
type ScanWithFetchMatchCandidate interface {
	MatchCandidate

	// PushValueThroughFetch attempts to translate a value from the
	// full-record domain (correlated to sourceAlias) to the
	// index-entry domain (correlated to targetAlias). Returns the
	// translated value and true on success; nil and false if the
	// value cannot be expressed using only the index entry's columns.
	//
	// Ports Java's ScanWithFetchMatchCandidate.pushValueThroughFetch.
	PushValueThroughFetch(
		value values.Value,
		sourceAlias values.CorrelationIdentifier,
		targetAlias values.CorrelationIdentifier,
	) (values.Value, bool)
}

// Compile-time interface assertions.
var (
	_ ValueIndexLikeMatchCandidate = (*ValueIndexScanMatchCandidate)(nil)
	_ ScanWithFetchMatchCandidate  = (*ValueIndexScanMatchCandidate)(nil)
)
