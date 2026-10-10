// Portions derived from FoundationDB Record Layer (RankScanBounds.java,
// RecordCoreArgumentException.java, KeyStoreSerializationKeyManager.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2025 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2026 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"fmt"
	"strings"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// RankScanBounds selects score or rank bounds and optionally returns each entry's
// absolute rank in its value. Matches Java's RankScanBounds.
type RankScanBounds struct {
	ScanType           IndexScanType
	RankRange          TupleRange
	IncludeRankAsValue bool
}

// RecordCoreArgumentError reports an invalid argument with its context.
// Matches Java's RecordCoreArgumentException; the fields are the log keys the
// raising sites attach (SCAN_TYPE, INDEX_NAME, SUBSPACE_KEY).
type RecordCoreArgumentError struct {
	Message   string
	ScanType  IndexScanType
	IndexName string
	// SubspaceKey is the offending subspace key of a subspace-key refusal
	// (Index.normalizeSubspaceKey, FormerIndex's constructor;
	// LogMessageKeys.SUBSPACE_KEY), and HasSubspaceKey says the site attached
	// one: the offending key is often nil, so nil cannot mean "not attached".
	SubspaceKey    any
	HasSubspaceKey bool
	// Plan is the refused plan's class name (LogMessageKeys.PLAN).
	Plan string
	// ClientKnobName, ClientKnobValue and Expected are a refused client knob's
	// log keys (CLIENT_KNOB_NAME, CLIENT_KNOB_VALUE, EXPECTED).
	ClientKnobName  string
	ClientKnobValue string
	Expected        string
	// Cause is the exception Java's constructor chains (the key store's
	// IOException / GeneralSecurityException in KeyStoreSerializationKeyManager).
	Cause error
}

// Unwrap exposes the chained cause to errors.Is / errors.As.
func (e *RecordCoreArgumentError) Unwrap() error { return e.Cause }

// Error renders the message and, in parentheses, only the fields its site set.
func (e *RecordCoreArgumentError) Error() string {
	var fields []string
	if e.ScanType != "" {
		fields = append(fields, "scanType="+string(e.ScanType))
	}
	if e.IndexName != "" {
		fields = append(fields, fmt.Sprintf("index=%q", e.IndexName))
	}
	if e.HasSubspaceKey {
		fields = append(fields, fmt.Sprintf("subspace_key=%v", e.SubspaceKey))
	}
	if e.Plan != "" {
		fields = append(fields, "plan="+e.Plan)
	}
	if e.ClientKnobName != "" {
		fields = append(fields, fmt.Sprintf("client_knob_name=%q", e.ClientKnobName))
	}
	if e.Expected != "" {
		fields = append(fields, fmt.Sprintf("client_knob_value=%q", e.ClientKnobValue), "expected="+e.Expected)
	}
	msg := e.Message
	if len(fields) > 0 {
		msg += " (" + strings.Join(fields, ", ") + ")"
	}
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

func (*RecordCoreArgumentError) JavaRecordCoreException() {}

// NewRankScanBounds accepts only BY_VALUE and BY_RANK, even without rank values.
func NewRankScanBounds(scanType IndexScanType, rankRange TupleRange, includeRankAsValue bool) (RankScanBounds, error) {
	bounds := RankScanBounds{ScanType: scanType, RankRange: rankRange, IncludeRankAsValue: includeRankAsValue}
	return bounds, bounds.validate()
}

func (b RankScanBounds) validate() error {
	if b.ScanType != IndexScanByValue && b.ScanType != IndexScanByRank {
		return &RecordCoreArgumentError{Message: "a rank index can only be scanned by value or by rank", ScanType: b.ScanType}
	}
	return nil
}

// ScanRankIndex scans a RANK index, optionally enriching values with tuple(rank).
// Rank lookups remain serializable even when the underlying scan is snapshot.
func (store *FDBRecordStore) ScanRankIndex(index *Index, bounds RankScanBounds, continuation []byte, props ScanProperties) RecordCursor[*IndexEntry] {
	if err := bounds.validate(); err != nil {
		return &errorCursor[*IndexEntry]{err: err}
	}
	if index == nil {
		return &errorCursor[*IndexEntry]{err: &RecordCoreArgumentError{Message: "rank scan requires an index", ScanType: bounds.ScanType}}
	}
	state, err := store.readIndexState(index.Name)
	if err != nil {
		return &errorCursor[*IndexEntry]{err: err}
	}
	if !state.IsScannable() {
		return &errorCursor[*IndexEntry]{err: &IndexNotReadableError{IndexName: index.Name, CurrentState: state}}
	}
	maintainer, err := store.getIndexMaintainer(index)
	if err != nil {
		return &errorCursor[*IndexEntry]{err: err}
	}
	rank, ok := maintainer.(*rankIndexMaintainer)
	if !ok {
		return &errorCursor[*IndexEntry]{err: &RecordCoreArgumentError{Message: "rank scan requires a RANK index", ScanType: bounds.ScanType, IndexName: index.Name}}
	}
	var cursor RecordCursor[*IndexEntry]
	if bounds.ScanType == IndexScanByRank {
		cursor = rank.ScanByRank(bounds.RankRange, continuation, props)
	} else {
		cursor = rank.Scan(bounds.RankRange, continuation, props)
	}
	if bounds.IncludeRankAsValue {
		cursor = MapErrCursor(cursor, rank.entryWithRank)
	}
	return instrumentCursor(store.context.Timer(), EventScanIndex, cursor)
}

func (m *rankIndexMaintainer) entryWithRank(entry *IndexEntry) (*IndexEntry, error) {
	// The stored entry key may append PK components or omit overlapping PK
	// components. Only the declared group+score columns belong in the lookup.
	columns := m.index.RootExpression.ColumnSize()
	if len(entry.Key) < columns {
		return nil, &RecordCoreArgumentError{Message: "rank entry has fewer columns than the index expression", IndexName: m.index.Name}
	}
	rank, err := m.RankForScore(entry.Key[:columns], true)
	if err != nil {
		return nil, err
	}
	enriched := *entry
	enriched.Value = tuple.Tuple{nil}
	if rank != nil {
		enriched.Value[0] = *rank
	}
	return &enriched, nil
}
