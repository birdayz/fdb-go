package embedded

import (
	"context"
	"fmt"
	"strings"

	"github.com/antlr4-go/antlr/v4"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/functions"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Covering-index pushdown: when every column referenced by SELECT projection,
// WHERE residual and ORDER BY is derivable from the index key + primary key,
// the by-PK record fetch that `indexRecordCursor` would perform (one FDB
// round-trip per row at pkg/recordlayer/index_scan.go:718) can be skipped.
// We synthesise a `*FDBStoredRecord[proto.Message]` directly from the
// `IndexEntry`, populating only the covered fields of a `dynamicpb` message.
// The downstream scan loop reads fields via `msg.ProtoReflect().Has/Get`,
// which works uniformly on a dynamic message.
//
// Scope:
//   - Only the non-aggregate single-table path in execSelectQueryFull (the
//     aggregate / COUNT(*) / GROUP BY paths are already gated out of
//     secondary-index pushdown by their try-*Pushdown SELECT shape checks).
//   - Only VALUE indexes whose key expression is FieldKeyExpression or
//     CompositeKeyExpression (the shapes SQL DDL emits).
//   - `SELECT *` and `SELECT <qualifier>.*` always bail — they reference
//     every column and the set is fragile against schema additions.
//   - Field kinds restricted to scalar types with a round-trip through
//     convertToProtoValue (bool / int / uint / float / double / string /
//     bytes). Enum / Message / repeated fields bail.

// collectColumnRefs walks an ANTLR subtree and inserts every bare column name
// found inside `FullColumnNameExpressionAtomContext` nodes into `out`
// (uppercased for case-insensitive comparison with index columns / PK cols).
// Qualified references (`t.col`, `a.b.col`) contribute only the last segment.
func collectColumnRefs(tree antlr.Tree, out map[string]struct{}) {
	if tree == nil {
		return
	}
	if atom, ok := tree.(*antlrgen.FullColumnNameExpressionAtomContext); ok {
		name := functions.FullIdToName(atom.FullColumnName().FullId())
		bare := name[strings.LastIndex(name, ".")+1:]
		out[strings.ToUpper(bare)] = struct{}{}
	}
	for i := 0; i < tree.GetChildCount(); i++ {
		collectColumnRefs(tree.GetChild(i), out)
	}
}

// columnsNeededBySelect gathers the set of bare column names (uppercased)
// that the single-table non-aggregate SELECT path will read from each scanned
// record. Returns ok=false when the SELECT shape makes covering fragile or
// impossible (SELECT *, qualifier.*, joins, derived tables, aggregates).
func columnsNeededBySelect(sq *selectQuery) (map[string]struct{}, bool) {
	// Shapes outside our covering MVP.
	if sq == nil {
		return nil, false
	}
	if sq.projCols == nil {
		return nil, false // SELECT * or qualifier.*
	}
	if sq.projQualifier != "" {
		return nil, false
	}
	for _, q := range sq.projStarQualifiers {
		if q != "" {
			return nil, false
		}
	}
	if len(sq.joins) > 0 || sq.derivedQuery != nil {
		return nil, false
	}
	// Aggregates / COUNT(*) / GROUP BY are already gated out of the
	// try*Pushdown SELECT shape checks, so we never get here with them —
	// but defend anyway so the helper is safe to call on any selectQuery.
	if sq.countStar || len(sq.aggCols) > 0 || len(sq.groupBy) > 0 || sq.havingExpr != nil {
		return nil, false
	}

	need := make(map[string]struct{})

	// Projection: bare column names contribute directly; computed
	// expressions contribute every column reference inside them.
	for i, col := range sq.projCols {
		if i < len(sq.projExprs) && sq.projExprs[i] != nil {
			collectColumnRefs(sq.projExprs[i], need)
			continue
		}
		if col == "" {
			// A blank bare-column slot with no expression means the
			// projection couldn't be bound to a concrete column —
			// bail rather than risk missing a reference.
			return nil, false
		}
		need[strings.ToUpper(col)] = struct{}{}
	}

	// WHERE: every column in the residual predicate.
	if sq.whereExpr != nil {
		collectColumnRefs(sq.whereExpr, need)
	}

	// ORDER BY: bare colName contributes directly; expr / rawExpr get
	// walked for embedded column references.
	for _, ob := range sq.orderBy {
		if ob.colName != "" {
			need[strings.ToUpper(ob.colName)] = struct{}{}
		}
		if ob.expr != nil {
			collectColumnRefs(ob.expr, need)
		}
		if ob.rawExpr != nil {
			collectColumnRefs(ob.rawExpr, need)
		}
	}

	return need, true
}

// coveringAvailableCols returns the uppercase set of column names available
// without a record fetch: the index's key columns plus the user primary-key
// columns. When the index has `primaryKeyComponentPositions`, the PK
// components appended to the index key are the ones NOT already covered by
// the indexed value positions; but from the "which columns can I read off
// the entry" perspective, the union of (index cols, PK cols) is what matters
// — the entry itself always carries every PK column value (some inline with
// indexed cols, the rest as trailing tuple elements).
func coveringAvailableCols(idx *recordlayer.Index, rt *recordlayer.RecordType) map[string]struct{} {
	avail := make(map[string]struct{})
	for _, c := range secondaryIndexColumns(idx) {
		avail[strings.ToUpper(c)] = struct{}{}
	}
	for _, c := range rt.PrimaryKey.FieldNames() {
		avail[strings.ToUpper(c)] = struct{}{}
	}
	return avail
}

// coveringKindsAllowed reports whether every needed column has a field kind
// we know how to stuff back into a dynamicpb message from a tuple component
// (via convertToProtoValue). Enum / Message / Group / repeated fields
// currently bail — they'd need bespoke conversion rules we haven't written.
func coveringKindsAllowed(rt *recordlayer.RecordType, needed map[string]struct{}) bool {
	fields := rt.Descriptor.Fields()
	for col := range needed {
		// Case-insensitive field lookup. rt.Descriptor field names are
		// lowercased (proto convention); SQL columns are uppercased in
		// our needed-set. Iterate to find a case-insensitive match.
		var fd protoreflect.FieldDescriptor
		for i := 0; i < fields.Len(); i++ {
			f := fields.Get(i)
			if strings.EqualFold(string(f.Name()), col) {
				fd = f
				break
			}
		}
		if fd == nil {
			return false // unknown column — bail
		}
		if fd.IsList() || fd.IsMap() {
			return false
		}
		switch fd.Kind() {
		case protoreflect.BoolKind,
			protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
			protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind,
			protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
			protoreflect.Uint64Kind, protoreflect.Fixed64Kind,
			protoreflect.FloatKind, protoreflect.DoubleKind,
			protoreflect.StringKind, protoreflect.BytesKind:
			// OK
		default:
			return false
		}
	}
	return true
}

// findFieldByLowerName looks up a field descriptor by case-insensitive name.
// Column names in the SQL layer are uppercased; proto field names are
// lowercased by convention.
func findFieldByLowerName(desc protoreflect.MessageDescriptor, name string) protoreflect.FieldDescriptor {
	fields := desc.Fields()
	for i := 0; i < fields.Len(); i++ {
		f := fields.Get(i)
		if strings.EqualFold(string(f.Name()), name) {
			return f
		}
	}
	return nil
}

// canCoverIndex ties together the checks: SELECT shape, column availability,
// kind support. Returns true iff secondary-index pushdown on `idx` can skip
// the by-PK fetch for this SELECT.
func canCoverIndex(sq *selectQuery, idx *recordlayer.Index, rt *recordlayer.RecordType) bool {
	needed, ok := columnsNeededBySelect(sq)
	if !ok {
		return false
	}
	if !coveringKindsAllowed(rt, needed) {
		return false
	}
	avail := coveringAvailableCols(idx, rt)
	for col := range needed {
		if _, has := avail[col]; !has {
			return false
		}
	}
	return true
}

// synthesizeCoveredRecord builds a *FDBStoredRecord[proto.Message] from just
// the index entry, populating only the fields carried by the indexed values
// and the primary-key components. The returned record's `.Record` is a
// `*dynamicpb.Message` of rt.Descriptor — downstream SQL evaluation reads
// fields via ProtoReflect().Has/Get which works uniformly on dynamic
// messages.
//
// Tuple elements from FDB come back as native Go types (int64, float64,
// string, []byte, bool) — the same shape `convertToProtoValue` already
// consumes for the INSERT path. Nil tuple components (NULL index values)
// leave the field absent, matching proto2 optional semantics.
func synthesizeCoveredRecord(
	rt *recordlayer.RecordType,
	entry *recordlayer.IndexEntry,
	store *recordlayer.FDBRecordStore,
) (*recordlayer.FDBStoredRecord[proto.Message], error) {
	msg := dynamicpb.NewMessage(rt.Descriptor)

	// Indexed columns come from the entry's key prefix.
	idxCols := secondaryIndexColumns(entry.Index)
	idxVals := entry.IndexValues()
	for i, col := range idxCols {
		if i >= len(idxVals) {
			break
		}
		val := idxVals[i]
		if val == nil {
			continue
		}
		fd := findFieldByLowerName(rt.Descriptor, col)
		if fd == nil {
			continue
		}
		protoVal, err := functions.ConvertToProtoValue(fd, val)
		if err != nil {
			// canCoverIndex restricts needed columns to kinds supported
			// by convertToProtoValue, so reaching this branch means the
			// tuple element shape diverged from the proto field shape —
			// a real bug. Surface it rather than silently dropping the
			// field (which would become NULL and pass through the WHERE
			// as a wrong row).
			return nil, fmt.Errorf("covering index %q: cannot convert indexed column %q from tuple: %w", entry.Index.Name, col, err)
		}
		msg.Set(fd, protoVal)
	}

	// PK columns come from the PK tuple. When the PK expression starts
	// with RecordTypeKey, the first tuple component is the record-type
	// key and the user-declared PK columns start at index 1.
	pkTuple := entry.PrimaryKey()
	pkCols := rt.PrimaryKey.FieldNames()
	pkOffset := 0
	if recordlayer.KeyExpressionHasRecordTypePrefix(rt.PrimaryKey) {
		pkOffset = 1
	}
	for i, col := range pkCols {
		idx := pkOffset + i
		if idx >= len(pkTuple) {
			break
		}
		val := pkTuple[idx]
		if val == nil {
			continue
		}
		fd := findFieldByLowerName(rt.Descriptor, col)
		if fd == nil {
			continue
		}
		protoVal, err := functions.ConvertToProtoValue(fd, val)
		if err != nil {
			return nil, fmt.Errorf("covering index %q: cannot convert PK column %q from tuple: %w", entry.Index.Name, col, err)
		}
		msg.Set(fd, protoVal)
	}

	return &recordlayer.FDBStoredRecord[proto.Message]{
		PrimaryKey: pkTuple,
		RecordType: rt,
		Record:     msg,
		Store:      store,
	}, nil
}

// coveringIndexRangeScanCursor runs an index scan over `scanRange` and maps
// each IndexEntry to a synthetic FDBStoredRecord without touching the record
// subspace. Mirrors secondaryIndexRangeScanCursor / secondaryIndexComposite
// RangeScanCursor's wrapping pattern but uses store.ScanIndex (raw entry
// stream) instead of ScanIndexRecords (which fetches records).
//
// Takes the Index directly (not a name) so all metadata resolution happens
// at the call site — there's exactly one lookup, and the "missing index"
// case is impossible to express.
func coveringIndexRangeScanCursor(
	store *recordlayer.FDBRecordStore,
	rt *recordlayer.RecordType,
	idx *recordlayer.Index,
	scanRange recordlayer.TupleRange,
	scanProps recordlayer.ScanProperties,
) recordlayer.RecordCursor[*recordlayer.FDBStoredRecord[proto.Message]] {
	inner := store.ScanIndex(idx, scanRange, nil, scanProps)
	return &coveringCursor{
		inner: inner,
		rt:    rt,
		store: store,
	}
}

// coveringCursor adapts a *IndexEntry stream into a *FDBStoredRecord stream
// by synthesising the record from the entry alone. Errors from the inner
// cursor propagate unchanged; synthesis errors (which shouldn't occur given
// the kind pre-check) surface to the caller so the SELECT fails loudly
// rather than silently returning partial rows.
type coveringCursor struct {
	inner  recordlayer.RecordCursor[*recordlayer.IndexEntry]
	rt     *recordlayer.RecordType
	store  *recordlayer.FDBRecordStore
	closed bool
}

func (c *coveringCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[*recordlayer.FDBStoredRecord[proto.Message]], error) {
	result, err := c.inner.OnNext(ctx)
	if err != nil {
		return recordlayer.RecordCursorResult[*recordlayer.FDBStoredRecord[proto.Message]]{}, err
	}
	if !result.HasNext() {
		return recordlayer.NewResultNoNext[*recordlayer.FDBStoredRecord[proto.Message]](
			result.GetNoNextReason(),
			result.GetContinuation(),
		), nil
	}
	entry := result.GetValue()
	rec, err := synthesizeCoveredRecord(c.rt, entry, c.store)
	if err != nil {
		return recordlayer.RecordCursorResult[*recordlayer.FDBStoredRecord[proto.Message]]{}, err
	}
	return recordlayer.NewResultWithValue(rec, result.GetContinuation()), nil
}

func (c *coveringCursor) Close() error {
	c.closed = true
	return c.inner.Close()
}

func (c *coveringCursor) IsClosed() bool {
	return c.closed
}

// buildSecondaryIndexRangeTupleRange builds the scan range for a single-column
// secondary index from open/closed bounds — identical to the range-build
// block in secondaryIndexRangeScanCursor, factored out so covering and
// non-covering cursors stay in lockstep.
func buildSecondaryIndexRangeTupleRange(bounds pkRangeBounds) recordlayer.TupleRange {
	var scanRange recordlayer.TupleRange
	if bounds.hasLow {
		scanRange.Low = tuple.Tuple{bounds.low}
		if bounds.lowInclusive {
			scanRange.LowEndpoint = recordlayer.EndpointTypeRangeInclusive
		} else {
			scanRange.LowEndpoint = recordlayer.EndpointTypeRangeExclusive
		}
	} else {
		scanRange.LowEndpoint = recordlayer.EndpointTypeTreeStart
	}
	if bounds.hasHigh {
		scanRange.High = tuple.Tuple{bounds.high}
		if bounds.highInclusive {
			scanRange.HighEndpoint = recordlayer.EndpointTypeRangeInclusive
		} else {
			scanRange.HighEndpoint = recordlayer.EndpointTypeRangeExclusive
		}
	} else {
		scanRange.HighEndpoint = recordlayer.EndpointTypeTreeEnd
	}
	return scanRange
}

// buildSecondaryIndexCompositeRangeTupleRange builds the scan range for a
// composite secondary-index range pushdown. Shared between covering and
// non-covering cursor builders.
func buildSecondaryIndexCompositeRangeTupleRange(cr secondaryIndexCompositeRange) recordlayer.TupleRange {
	prefix := make(tuple.Tuple, 0, len(cr.prefixVals))
	for _, v := range cr.prefixVals {
		prefix = append(prefix, v)
	}
	low := append(tuple.Tuple{}, prefix...)
	high := append(tuple.Tuple{}, prefix...)
	lowEp := recordlayer.EndpointTypeRangeInclusive
	highEp := recordlayer.EndpointTypeRangeInclusive
	if cr.lastBounds.hasLow {
		low = append(low, cr.lastBounds.low)
		if !cr.lastBounds.lowInclusive {
			lowEp = recordlayer.EndpointTypeRangeExclusive
		}
	}
	if cr.lastBounds.hasHigh {
		high = append(high, cr.lastBounds.high)
		if !cr.lastBounds.highInclusive {
			highEp = recordlayer.EndpointTypeRangeExclusive
		}
	}
	return recordlayer.TupleRange{
		Low:          low,
		High:         high,
		LowEndpoint:  lowEp,
		HighEndpoint: highEp,
	}
}

// buildSecondaryIndexEqualityTupleRange builds the scan range for a
// single-value equality pushdown on a secondary index (either a single-col
// index value or a composite-index key tuple).
func buildSecondaryIndexEqualityTupleRange(keyVal any) recordlayer.TupleRange {
	var keyTuple tuple.Tuple
	if composite, ok := keyVal.(secondaryIndexKeyTuple); ok {
		keyTuple = make(tuple.Tuple, 0, len(composite.values))
		for _, v := range composite.values {
			keyTuple = append(keyTuple, v)
		}
	} else {
		keyTuple = tuple.Tuple{keyVal}
	}
	return recordlayer.TupleRange{
		Low:          keyTuple,
		High:         keyTuple,
		LowEndpoint:  recordlayer.EndpointTypeRangeInclusive,
		HighEndpoint: recordlayer.EndpointTypeRangeInclusive,
	}
}
