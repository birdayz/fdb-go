// Portions derived from FoundationDB Record Layer (MessageHelpers.java,
// QueryResult.java, IndexOrphanBehavior.java, RecordCursor.java,
// and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package executor

import (
	"bytes"
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"fdb.dev/internal/recordlayer/indexfetch"
	"fdb.dev/internal/recordlayer/indexpredicate"
	"fdb.dev/internal/recordlayer/query/plan/cascades/values"
	"fdb.dev/internal/recordlayer/query/plan/plans"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

// executeScan stamps each minted row with the layout the scan's output boundary
// will hold it to. Its rows therefore assert that layout from birth, which is only
// true where that boundary actually runs — see executeScanUnstamped for the one
// caller where it does not.
func executeScan(
	ctx context.Context,
	p *plans.RecordQueryScanPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	return executeScanWithRowLayout(ctx, p, store, evalCtx, continuation, props, mintedRowLayout(p))
}

// executeScanUnstamped is executeScan for a consumer that will read the scan's
// rows BEFORE the scan's layout is true of them, and whose own boundary therefore
// replaces it: a TypeFilter fused over a primary scan on an INTERMINGLED store.
//
// That caller already bypasses the scan's output boundary, and for exactly this
// reason: a PK range there can legitimately surface another record descriptor, and
// the scan's single-type layout would reject it before the load-bearing predicate
// discards it. Stamping is the same assertion made one step earlier, so it has to
// be withheld in the same place — otherwise a foreign row carries a layout that
// lies about it through a stage whose boundary is skipped by design.
func executeScanUnstamped(
	ctx context.Context,
	p *plans.RecordQueryScanPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	return executeScanWithRowLayout(ctx, p, store, evalCtx, continuation, props, nil)
}

func executeScanWithRowLayout(
	_ context.Context,
	p *plans.RecordQueryScanPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
	rowLayout values.OrdinalLayout,
) (recordlayer.RecordCursor[QueryResult], error) {
	scanProps := recordlayer.NewScanProperties(props).WithReverse(p.IsReverse())

	// If the plan carries scan comparisons (PK predicates pushed down
	// by the Cascades planner), convert them to an FDB tuple range and
	// scan only that range. Mirrors Java's RecordQueryScanPlan.executePlan()
	// which calls comparisons.toTupleRange() → store.scanRecords(range).
	if comps := p.GetScanComparisons(); len(comps) > 0 {
		fingerprintSalt, err := primaryScanRangeFingerprintSalt(p)
		if err != nil {
			return nil, fmt.Errorf("executor: building PK scan execution identity: %w", err)
		}
		spec, err := bindScanComparisonsToRangeSetWithNaNBlocks(
			comps,
			p.GetKeyComponentTypes(),
			scanBindContext(evalCtx),
			p.IsReverse(),
			fingerprintSalt,
		)
		if err != nil {
			return nil, fmt.Errorf("executor: building scan ranges for PK comparisons: %w", err)
		}

		// When the PK uses RecordTypeKey() as its first component, FDB
		// keys are prefixed with the record type discriminator. Prepend
		// it so the scan range matches the actual key structure; Prepend
		// turns an open endpoint into the inclusive prefix, which keeps
		// the scan inside this record type's keys.
		types := p.GetRecordTypes()
		var recordTypePrefix tuple.Tuple
		if len(types) == 1 {
			md := store.GetMetaData()
			rt := md.GetRecordType(types[0])
			if rt != nil && rt.PrimaryKey != nil && recordlayer.KeyExpressionHasRecordTypePrefix(rt.PrimaryKey) {
				recordTypePrefix = tuple.Tuple{rt.GetRecordTypeKey()}
			}
		}

		stored, err := newScanRangeSetCursor(
			spec,
			continuation,
			scanProps,
			func(
				tupleRange recordlayer.TupleRange,
				innerContinuation []byte,
				childProps recordlayer.ScanProperties,
			) (recordlayer.RecordCursor[*recordlayer.FDBStoredRecord[proto.Message]], error) {
				if len(recordTypePrefix) > 0 {
					tupleRange = tupleRange.Prepend(recordTypePrefix)
				}

				// The range's own endpoints: the cursor applies the continuation
				// after deriving the prefix length from them, as Java's does.
				return store.ScanRecordsInRange(
					tupleRange.Low,
					tupleRange.High,
					tupleRange.LowEndpoint,
					tupleRange.HighEndpoint,
					innerContinuation,
					childProps,
				), nil
			},
		)
		if err != nil {
			return nil, err
		}
		// A record's primary key carries the record-type prefix, if any,
		// before the components the comparisons constrain.
		stored = filterScanKeys(spec, stored,
			func(r *recordlayer.FDBStoredRecord[proto.Message]) tuple.Tuple { return r.PrimaryKey },
			len(recordTypePrefix))
		mapped := recordlayer.MapCursor(stored, storedRecordToQueryResult(rowLayout))
		return applySkipLimit(mapped, props.Skip, props.ReturnedRowLimit), nil
	}

	types := p.GetRecordTypes()
	var inner recordlayer.RecordCursor[*recordlayer.FDBStoredRecord[proto.Message]]
	if len(types) == 1 {
		inner = store.ScanRecordsByType(types[0], continuation, scanProps)
	} else {
		inner = store.ScanRecords(continuation, scanProps)
	}

	return recordlayer.MapCursor(inner, storedRecordToQueryResult(rowLayout)), nil
}

// openIndexEntryCursor opens the physical index-entry cursor for an index scan:
// metadata lookup, readability gate, range-set binding under the caller's
// execution-identity salt, and the per-range scan factory. Shared by the
// non-covering scan and by the covering scan, which differ ONLY in how they
// shape the entries they receive — never in which entries they receive. That
// sharing is what makes the covering path provably a consumer of the same
// forked range set (RFC-208 signed-zero alternatives) as the fetching path.
//
// The salt is a PARAMETER rather than derived here because the two plan types
// have different execution identities: a covering plan builds its rows from the
// entry, so its continuation must never be interchangeable with a fetching
// scan's over the same range.
func openIndexEntryCursor(
	p *plans.RecordQueryIndexPlan,
	fingerprintSalt string,
	compatibleFingerprintSalts []string,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[*recordlayer.IndexEntry], int, error) {
	scan, err := bindIndexScan(p, fingerprintSalt, compatibleFingerprintSalts, store, evalCtx, props)
	if err != nil {
		return nil, 0, err
	}
	maintainer, err := store.GetIndexMaintainer(scan.index)
	if err != nil {
		return nil, 0, fmt.Errorf("executor: getting index maintainer for %q: %w", p.GetIndexName(), err)
	}
	pushSkip := props.Skip > 0 && indexScanCanPushSkip(scan.index.Type, scan.rangeSet)
	outerSkip := props.Skip
	if pushSkip {
		outerSkip = 0
	}
	cursor, err := openIndexScanRanges(scan, continuation,
		func(
			scanRange recordlayer.TupleRange,
			innerContinuation []byte,
			childProperties recordlayer.ScanProperties,
		) (recordlayer.RecordCursor[*recordlayer.IndexEntry], error) {
			if pushSkip {
				// One unfiltered range is Java's index cursor shape: skip before
				// charging or fetching, without widening the returned-row budget.
				childProperties.ExecuteProperties.Skip = props.Skip
				childProperties.ExecuteProperties.ReturnedRowLimit = props.ReturnedRowLimit
			}
			// Instrumented here because this path never touches FDBRecordStore.ScanIndex:
			// the per-range factory needs the maintainer directly, so the store method
			// that would otherwise count the scan is bypassed entirely.
			return recordlayer.InstrumentIndexScanCursor(store.Context().Timer(),
				maintainer.Scan(scanRange, innerContinuation, childProperties)), nil
		},
		func(e *recordlayer.IndexEntry) *recordlayer.IndexEntry { return e })
	return cursor, outerSkip, err
}

// indexScanCanPushSkip recognizes a single ordinary KV scan with no residual
// key filter. Other cursor families and combined ranges retain logical skipping.
// Remote-fetch leaves never take Skip: ScanIndexRemoteFetch would run a skipped
// scan as SCAN_AND_FETCH, so remoteFetchIndexRecords keeps one outer Skip.
func indexScanCanPushSkip(indexType string, spec scanRangeSetSpec) bool {
	switch indexType {
	case recordlayer.IndexTypeValue, recordlayer.IndexTypeVersion, recordlayer.IndexTypeRank,
		recordlayer.IndexTypeTimeWindowLeaderboard, recordlayer.IndexTypePermutedMin, recordlayer.IndexTypePermutedMax:
	default:
		return false
	}
	if spec.empty || spec.keyFilter != nil {
		return false
	}
	for _, count := range spec.alternativeCounts {
		if count != 1 {
			return false
		}
	}
	return true
}

// boundIndexScan is an index scan's metadata, readability gate and range set,
// which every fetch method and the covering scan share.
type boundIndexScan struct {
	plan      *plans.RecordQueryIndexPlan
	index     *recordlayer.Index
	rangeSet  scanRangeSetSpec
	scanProps recordlayer.ScanProperties
}

func bindIndexScan(
	p *plans.RecordQueryIndexPlan,
	fingerprintSalt string,
	compatibleFingerprintSalts []string,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	props recordlayer.ExecuteProperties,
) (boundIndexScan, error) {
	idx := store.GetMetaData().GetIndex(p.GetIndexName())
	if idx == nil {
		return boundIndexScan{}, fmt.Errorf("executor: index %q not found in metadata", p.GetIndexName())
	}
	if err := requireReadableQueryIndexWithProof(store, idx, p.GetMatchedIndexPredicate()); err != nil {
		return boundIndexScan{}, err
	}
	rangeSet, err := bindScanComparisonsToRangeSetWithNaNBlocks(
		p.GetScanComparisons(),
		p.GetKeyComponentTypes(),
		scanBindContext(evalCtx),
		p.IsReverse(),
		fingerprintSalt,
		compatibleFingerprintSalts...,
	)
	if err != nil {
		return boundIndexScan{}, fmt.Errorf("executor: building scan ranges for %q: %w", p.GetIndexName(), err)
	}
	return boundIndexScan{
		plan:      p,
		index:     idx,
		rangeSet:  rangeSet,
		scanProps: recordlayer.NewScanProperties(props).WithReverse(p.IsReverse()),
	}, nil
}

// openIndexScanRanges opens one cursor over the scan's physical ranges, each
// leaf opened by open, admitting only results whose index entry passes the
// range set's key filter.
func openIndexScanRanges[T any](
	scan boundIndexScan,
	continuation []byte,
	open scanRangeLeafFactory[T],
	entry func(T) *recordlayer.IndexEntry,
) (recordlayer.RecordCursor[T], error) {
	cursor, err := newScanRangeSetCursor(scan.rangeSet, continuation, scan.scanProps, open)
	if err != nil {
		return nil, fmt.Errorf("executor: opening scan ranges for %q: %w", scan.plan.GetIndexName(), err)
	}
	// An index entry's key begins with the indexed values, aligned with the
	// scan comparisons.
	return filterScanKeys(scan.rangeSet, cursor, func(v T) tuple.Tuple { return entry(v).Key }, 0), nil
}

// executeIndexScan runs a NON-covering index scan: every entry is resolved to
// its base record by primary key, with the plan's index fetch method. Mirrors
// Java's RecordQueryIndexPlan.executePlan. Coveringness is not a flag consulted
// here — it is RecordQueryCoveringIndexPlan, a distinct plan type with its own
// executor, exactly as in Java.
func executeIndexScan(
	_ context.Context,
	p *plans.RecordQueryIndexPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	fingerprintSalt, err := indexScanRangeFingerprintSalt(p, recordlayer.IndexScanByValue)
	if err != nil {
		return nil, fmt.Errorf("executor: building scan execution identity for %q: %w", p.GetIndexName(), err)
	}
	legacyFingerprintSalt, err := legacyIndexScanRangeFingerprintSaltFields(
		p, recordlayer.IndexScanByValue, false, nil)
	if err != nil {
		return nil, fmt.Errorf("executor: building legacy scan execution identity for %q: %w", p.GetIndexName(), err)
	}
	salts := []string{legacyFingerprintSalt}
	if props.ScanState == nil {
		// A remote scan and its fallback charge one budget, as Java's share ExecuteState.
		props.ScanState = recordlayer.NewScanLimiterState()
	}
	scanAndFetch := func(continuation []byte) (recordlayer.RecordCursor[QueryResult], error) {
		indexCursor, outerSkip, err := openIndexEntryCursor(p, fingerprintSalt, salts, store, evalCtx, continuation, props)
		if err != nil {
			return nil, err
		}
		return applySkipLimit(newIndexFetchCursor(indexCursor, store, props.ScanState),
			outerSkip, props.ReturnedRowLimit), nil
	}
	switch effectiveIndexFetchMethod(p.GetIndexFetchMethod(), store.Context().APIVersion()) {
	case indexfetch.ScanAndFetch:
		return scanAndFetch(continuation)
	case indexfetch.UseRemoteFetch:
		return remoteFetchIndexRecords(p, fingerprintSalt, salts, store, evalCtx, continuation, props)
	case indexfetch.UseRemoteFetchWithFallback:
		// Opening fails only where scanning and fetching would too (binding,
		// readability, the continuation); remote fetch's own failures, Java's
		// construction-time ones included, come from the leaves into Fallback.
		remote, err := remoteFetchIndexRecords(p, fingerprintSalt, salts, store, evalCtx, continuation, props)
		if err != nil {
			return nil, err
		}
		// Java's fallBackContinueFrom: resume after the last row returned, with
		// the same skip and row limit, so both apply again from there.
		return recordlayer.Fallback(remote, func(last *recordlayer.RecordCursorResult[QueryResult]) recordlayer.RecordCursor[QueryResult] {
			resume := continuation
			if last != nil {
				var err error
				if resume, err = last.GetContinuation().ToBytes(); err != nil {
					return recordlayer.NewErrorCursor[QueryResult](err)
				}
			}
			cursor, err := scanAndFetch(resume)
			if err != nil {
				return recordlayer.NewErrorCursor[QueryResult](err)
			}
			return cursor
		}), nil
	default:
		return nil, &recordlayer.RecordCoreError{Message: "Unknown useIndexPrefetch option", IndexName: p.GetIndexName()}
	}
}

// effectiveIndexFetchMethod is the method a plan runs with: below API version
// 7.1 Java's RecordQueryIndexPlan.executePlan (:259-271) scans and fetches
// instead, checked at execution because the version can change after planning.
func effectiveIndexFetchMethod(method indexfetch.Method, apiVersion int) indexfetch.Method {
	if method != indexfetch.ScanAndFetch && !recordlayer.SupportsRemoteFetch(apiVersion) {
		return indexfetch.ScanAndFetch
	}
	return method
}

// remoteFetchIndexRecords is Java's RecordQueryIndexPlan.executeUsingRemoteFetch
// over each of the scan's physical ranges: the storage server returns every
// entry's record, and an orphan entry fails the scan (IndexOrphanBehavior.ERROR).
func remoteFetchIndexRecords(
	p *plans.RecordQueryIndexPlan,
	fingerprintSalt string,
	compatibleFingerprintSalts []string,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	scan, err := bindIndexScan(p, fingerprintSalt, compatibleFingerprintSalts, store, evalCtx, props)
	if err != nil {
		return nil, err
	}
	commonPrimaryKeyLength := store.GetCommonPrimaryKeyLength(scan.index)
	records, err := openIndexScanRanges(scan, continuation,
		func(
			scanRange recordlayer.TupleRange,
			innerContinuation []byte,
			childProperties recordlayer.ScanProperties,
		) (recordlayer.RecordCursor[*recordlayer.FDBIndexedRecord], error) {
			return store.ScanIndexRemoteFetch(scan.index, recordlayer.IndexScanByValue, scanRange,
				commonPrimaryKeyLength, innerContinuation, childProperties, recordlayer.IndexOrphanError), nil
		},
		func(r *recordlayer.FDBIndexedRecord) *recordlayer.IndexEntry { return r.IndexEntry })
	if err != nil {
		return nil, err
	}
	rows := recordlayer.MapCursor(records, func(r *recordlayer.FDBIndexedRecord) QueryResult {
		return FromStoredRecord(r.Record)
	})
	return applySkipLimit(rows, props.Skip, props.ReturnedRowLimit), nil
}

// executeCoveringIndexScan runs a covering index scan: the row is rebuilt from
// the index ENTRY, with no base-record read. Mirrors Java's
// RecordQueryCoveringIndexPlan, which scans its inner index plan's entries and
// maps each through IndexKeyValueToPartialRecord.
func executeCoveringIndexScan(
	_ context.Context,
	p *plans.RecordQueryCoveringIndexPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	fingerprintSalt, err := coveringIndexScanRangeFingerprintSalt(p, recordlayer.IndexScanByValue)
	if err != nil {
		return nil, fmt.Errorf("executor: building scan execution identity for %q: %w", p.GetIndexName(), err)
	}
	legacyFingerprintSalt, err := legacyIndexScanRangeFingerprintSaltFields(
		p.GetIndexPlan(), recordlayer.IndexScanByValue, true, p.GetCoveringColumns())
	if err != nil {
		return nil, fmt.Errorf("executor: building legacy scan execution identity for %q: %w", p.GetIndexName(), err)
	}
	indexCursor, outerSkip, err := openIndexEntryCursor(
		p.GetIndexPlan(), fingerprintSalt, []string{legacyFingerprintSalt},
		store, evalCtx, continuation, props)
	if err != nil {
		return nil, err
	}

	// The entry reader is built for the plan's single queried record type
	// (ScanWithFetchMatchCandidate.computeIndexEntryToLogicalRecord), whose
	// metadata-aware row shape it fills.
	var desc protoreflect.MessageDescriptor
	var logicalType *values.RecordType
	if rts := p.GetRecordTypes(); len(rts) == 1 {
		if rt := store.GetMetaData().GetRecordType(rts[0]); rt != nil && rt.Descriptor != nil {
			desc = rt.Descriptor
			logicalType = PositionalTypeForRecordLayout(rt.Descriptor, store.GetMetaData().IsStoreRecordVersions())
		}
	}
	reader, err := newCoveringEntryReader(p.GetIndexPlan().GetEntryReader(), desc, logicalType, mintedRowLayout(p))
	if err != nil {
		_ = indexCursor.Close()
		return nil, fmt.Errorf("executor: covering scan of %q: %w", p.GetIndexName(), err)
	}
	return applySkipLimit(&coveringIndexCursor{
		inner:  indexCursor,
		reader: reader,
		binder: entryBinder{outer: evalCtx},
	}, outerSkip, props.ReturnedRowLimit), nil
}

// requireReadableQueryIndex is the executor backstop for plans built manually
// or raced against mutable metadata/store state. Low-level record-store index
// APIs retain Java's READABLE_UNIQUE_PENDING scannability and sparse-index
// scanning for online-index tooling, but a query plan may have relied on
// uniqueness, cardinality, ordering, or complete coverage even when the leaf
// itself looks like an ordinary scan. Therefore query-plan execution requires
// both the one state Java admits to planning (strictly READABLE) and a complete
// index, or a matched predicate proof for the current filtered index.
//
// "Complete" is HasFilteringPredicate, not HasPredicate: a stored predicate
// that is a PROVED tautology rejects no record, so the index holds an entry for
// every row and nothing is missing to reject. Planning already reasons that way
// (a tautological candidate predicate is never attached —
// ValueIndexExpansionVisitor.java:141), so a backstop keyed on the mere
// presence of a predicate field would kill, at execution, plans the planner is
// entitled to build and no query can route around.
func requireReadableQueryIndex(store *recordlayer.FDBRecordStore, idx *recordlayer.Index) error {
	return requireReadableQueryIndexWithProof(store, idx, nil)
}

func requireReadableQueryIndexWithProof(store *recordlayer.FDBRecordStore, idx *recordlayer.Index, proof []byte) error {
	state, err := store.ReadIndexState(idx.Name)
	if err != nil {
		return err
	}
	if state == recordlayer.IndexStateReadable {
		if idx.HasFilteringPredicate() {
			current := idx.GetPredicateProto()
			if len(proof) == 0 || current == nil {
				return &FilteredIndexPlanError{IndexName: idx.Name}
			}
			encoded, err := (proto.MarshalOptions{Deterministic: true}).Marshal(indexpredicate.Normalize(current))
			if err != nil || !bytes.Equal(encoded, proof) {
				return &FilteredIndexPlanError{IndexName: idx.Name}
			}
		}
		return nil
	}
	return &recordlayer.IndexNotReadableError{
		IndexName:    idx.Name,
		CurrentState: state,
	}
}
