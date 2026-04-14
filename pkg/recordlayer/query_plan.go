package recordlayer

import (
	"context"
	"fmt"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// RecordQueryPlan is a node in a query execution plan tree.
// Plans are composable — each plan wraps a cursor from our existing
// cursor infrastructure. This is the execution engine only; the
// optimizer that creates plans is a separate component.
//
// Matches Java's RecordQueryPlan interface (simplified — no visitor,
// no cost model, no async).
type RecordQueryPlan interface {
	// Execute returns a cursor over the query results.
	Execute(store *FDBRecordStore, continuation []byte, props ScanProperties) RecordCursor[*FDBStoredRecord[proto.Message]]

	// Explain returns a human-readable description of the plan.
	Explain(indent int) string
}

// ScanPlan performs a full table scan over all records.
// Matches Java's RecordQueryScanPlan.
type ScanPlan struct {
	// RecordTypeName filters to a specific record type. Empty = all types.
	RecordTypeName string
}

func (p *ScanPlan) Execute(store *FDBRecordStore, continuation []byte, props ScanProperties) RecordCursor[*FDBStoredRecord[proto.Message]] {
	if p.RecordTypeName != "" {
		return store.ScanRecordsByType(p.RecordTypeName, continuation, props)
	}
	return store.ScanRecords(continuation, props)
}

func (p *ScanPlan) Explain(indent int) string {
	prefix := strings.Repeat("  ", indent)
	if p.RecordTypeName != "" {
		return fmt.Sprintf("%sScan(%s)", prefix, p.RecordTypeName)
	}
	return fmt.Sprintf("%sScan(*)", prefix)
}

// IndexPlan performs an index scan and fetches the full records.
// Matches Java's RecordQueryIndexPlan.
type IndexPlan struct {
	IndexName string
	Range     TupleRange
}

func (p *IndexPlan) Execute(store *FDBRecordStore, continuation []byte, props ScanProperties) RecordCursor[*FDBStoredRecord[proto.Message]] {
	// ScanIndexRecords returns FDBIndexedRecord — map to FDBStoredRecord
	indexedCursor := store.ScanIndexRecords(p.IndexName, p.Range, continuation, props)
	return MapCursor(indexedCursor, func(ir *FDBIndexedRecord) *FDBStoredRecord[proto.Message] {
		return ir.Record
	})
}

func (p *IndexPlan) Explain(indent int) string {
	prefix := strings.Repeat("  ", indent)
	rangeStr := "ALL"
	if p.Range.Low != nil || p.Range.High != nil {
		rangeStr = fmt.Sprintf("[%v, %v]", p.Range.Low, p.Range.High)
	}
	return fmt.Sprintf("%sIndex(%s, %s)", prefix, p.IndexName, rangeStr)
}

// FilterPlan wraps a child plan and applies a predicate to filter results.
// Matches Java's RecordQueryFilterPlan.
type FilterPlan struct {
	Child     RecordQueryPlan
	Predicate func(record *FDBStoredRecord[proto.Message]) bool
	// Description is a human-readable description of the predicate for Explain.
	Description string
}

func (p *FilterPlan) Execute(store *FDBRecordStore, continuation []byte, props ScanProperties) RecordCursor[*FDBStoredRecord[proto.Message]] {
	inner := p.Child.Execute(store, continuation, props)
	return &filterCursor[*FDBStoredRecord[proto.Message]]{inner: inner, predicate: p.Predicate}
}

func (p *FilterPlan) Explain(indent int) string {
	prefix := strings.Repeat("  ", indent)
	childExplain := p.Child.Explain(indent + 1)
	desc := p.Description
	if desc == "" {
		desc = "<predicate>"
	}
	return fmt.Sprintf("%sFilter(%s)\n%s", prefix, desc, childExplain)
}

// IndexScanPlan performs an index-only scan (no record fetch).
// Returns index entries as stored records with just the primary key.
// Useful for covering indexes where you only need the PK.
// Matches Java's RecordQueryCoveringIndexPlan (simplified).
type IndexScanPlan struct {
	IndexName string
	Range     TupleRange
}

func (p *IndexScanPlan) Execute(store *FDBRecordStore, continuation []byte, props ScanProperties) RecordCursor[*FDBStoredRecord[proto.Message]] {
	idx := store.metaData.GetIndex(p.IndexName)
	if idx == nil {
		return &emptyCursor[*FDBStoredRecord[proto.Message]]{}
	}
	maintainer, _ := store.getIndexMaintainer(idx)
	entryCursor := maintainer.Scan(p.Range, continuation, props)
	return MapCursor(entryCursor, func(entry *IndexEntry) *FDBStoredRecord[proto.Message] {
		pk := entry.PrimaryKey()
		return &FDBStoredRecord[proto.Message]{PrimaryKey: pk}
	})
}

func (p *IndexScanPlan) Explain(indent int) string {
	prefix := strings.Repeat("  ", indent)
	rangeStr := "ALL"
	if p.Range.Low != nil || p.Range.High != nil {
		rangeStr = fmt.Sprintf("[%v, %v]", p.Range.Low, p.Range.High)
	}
	return fmt.Sprintf("%sIndexScan(%s, %s) [covering]", prefix, p.IndexName, rangeStr)
}

// PrimaryKeyLookupPlan fetches a single record by primary key.
type PrimaryKeyLookupPlan struct {
	PrimaryKey tuple.Tuple
}

func (p *PrimaryKeyLookupPlan) Execute(store *FDBRecordStore, _ []byte, _ ScanProperties) RecordCursor[*FDBStoredRecord[proto.Message]] {
	record, err := store.LoadRecord(p.PrimaryKey)
	if err != nil || record == nil {
		return &emptyCursor[*FDBStoredRecord[proto.Message]]{}
	}
	// Single-element cursor
	return &singleRecordCursor{record: record}
}

func (p *PrimaryKeyLookupPlan) Explain(indent int) string {
	prefix := strings.Repeat("  ", indent)
	return fmt.Sprintf("%sLookup(pk=%v)", prefix, p.PrimaryKey)
}

// UnionPlan merges results from two child plans, deduplicating by primary key.
// Both children must produce results in the same order (forward or reverse).
// Matches Java's RecordQueryUnionPlan.
type UnionPlan struct {
	Left    RecordQueryPlan
	Right   RecordQueryPlan
	Reverse bool
}

func (p *UnionPlan) Execute(store *FDBRecordStore, continuation []byte, props ScanProperties) RecordCursor[*FDBStoredRecord[proto.Message]] {
	leftCursor := p.Left.Execute(store, continuation, props)
	rightCursor := p.Right.Execute(store, continuation, props)
	return Union(
		[]RecordCursor[*FDBStoredRecord[proto.Message]]{leftCursor, rightCursor},
		storedRecordComparisonKey,
		p.Reverse,
	)
}

func (p *UnionPlan) Explain(indent int) string {
	prefix := strings.Repeat("  ", indent)
	leftExplain := p.Left.Explain(indent + 1)
	rightExplain := p.Right.Explain(indent + 1)
	return fmt.Sprintf("%sUnion\n%s\n%s", prefix, leftExplain, rightExplain)
}

// IntersectionPlan returns only records present in both child plans.
// Both children must produce results in the same order.
// Matches Java's RecordQueryIntersectionPlan.
type IntersectionPlan struct {
	Left    RecordQueryPlan
	Right   RecordQueryPlan
	Reverse bool
}

func (p *IntersectionPlan) Execute(store *FDBRecordStore, continuation []byte, props ScanProperties) RecordCursor[*FDBStoredRecord[proto.Message]] {
	leftCursor := p.Left.Execute(store, continuation, props)
	rightCursor := p.Right.Execute(store, continuation, props)
	return Intersection(
		[]RecordCursor[*FDBStoredRecord[proto.Message]]{leftCursor, rightCursor},
		storedRecordComparisonKey,
		p.Reverse,
	)
}

func (p *IntersectionPlan) Explain(indent int) string {
	prefix := strings.Repeat("  ", indent)
	leftExplain := p.Left.Explain(indent + 1)
	rightExplain := p.Right.Explain(indent + 1)
	return fmt.Sprintf("%sIntersection\n%s\n%s", prefix, leftExplain, rightExplain)
}

// LimitPlan wraps a child plan and limits the number of returned results.
// Matches SQL's LIMIT N.
type LimitPlan struct {
	Child RecordQueryPlan
	Limit int
}

func (p *LimitPlan) Execute(store *FDBRecordStore, continuation []byte, props ScanProperties) RecordCursor[*FDBStoredRecord[proto.Message]] {
	inner := p.Child.Execute(store, continuation, props)
	return LimitRowsCursor(inner, p.Limit)
}

func (p *LimitPlan) Explain(indent int) string {
	prefix := strings.Repeat("  ", indent)
	childExplain := p.Child.Explain(indent + 1)
	return fmt.Sprintf("%sLimit(%d)\n%s", prefix, p.Limit, childExplain)
}

// storedRecordComparisonKey extracts the primary key tuple for merge comparison.
func storedRecordComparisonKey(r *FDBStoredRecord[proto.Message]) tuple.Tuple {
	return r.PrimaryKey
}

// singleRecordCursor returns one record then exhausts.
type singleRecordCursor struct {
	record *FDBStoredRecord[proto.Message]
	done   bool
}

func (c *singleRecordCursor) OnNext(_ context.Context) (RecordCursorResult[*FDBStoredRecord[proto.Message]], error) {
	if c.done || c.record == nil {
		return NewResultNoNext[*FDBStoredRecord[proto.Message]](SourceExhausted, &EndContinuation{}), nil
	}
	c.done = true
	return NewResultWithValue(c.record, &BytesContinuation{bytes: []byte{0}}), nil
}

func (c *singleRecordCursor) Close() error { return nil }
