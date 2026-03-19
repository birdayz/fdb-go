package recordlayer

import (
	"context"
	"fmt"
	"math/big"
	"strconv"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/gen"
	"google.golang.org/protobuf/proto"
)

// multidimensionalIndexMaintainer maintains a MULTIDIMENSIONAL index using a Hilbert R-tree.
// Each unique prefix gets its own R-tree. Items are stored with dimensional coordinates
// as the Point and remaining key components as the key suffix.
// Matches Java's MultidimensionalIndexMaintainer.
type multidimensionalIndexMaintainer struct {
	standardIndexMaintainer
	rTreeConfig RTreeConfig
}

func newMultidimensionalIndexMaintainer(
	index *Index,
	indexSubspace subspace.Subspace,
	tx fdb.Transaction,
	store indexStoreContext,
	numDimensions int,
) *multidimensionalIndexMaintainer {
	config := parseRTreeConfig(index, numDimensions)
	return &multidimensionalIndexMaintainer{
		standardIndexMaintainer: *newStandardIndexMaintainer(index, indexSubspace, tx, store),
		rTreeConfig:             config,
	}
}

// R-tree index option keys for configuring the Hilbert R-tree.
const (
	IndexOptionRTreeMaxM  = "rtreeMaxM"
	IndexOptionRTreeMinM  = "rtreeMinM"
	IndexOptionRTreeSplitS = "rtreeSplitS"
)

// parseRTreeConfig reads R-tree configuration from index options.
// Supports IndexOptionRTreeMaxM, IndexOptionRTreeMinM, IndexOptionRTreeSplitS.
func parseRTreeConfig(index *Index, numDimensions int) RTreeConfig {
	config := DefaultRTreeConfig(numDimensions)
	if v, ok := index.Options[IndexOptionRTreeMaxM]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			config.MaxM = n
		}
	}
	if v, ok := index.Options[IndexOptionRTreeMinM]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			config.MinM = n
		}
	}
	if v, ok := index.Options[IndexOptionRTreeSplitS]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			config.SplitS = n
		}
	}
	return config
}

// getDimensionsExpression extracts the DimensionsKeyExpression from the index.
func (m *multidimensionalIndexMaintainer) getDimensionsExpression() *DimensionsKeyExpression {
	if d, ok := m.index.RootExpression.(*DimensionsKeyExpression); ok {
		return d
	}
	return nil
}

// Update handles insert/delete/update for the MULTIDIMENSIONAL index.
// Matches Java's MultidimensionalIndexMaintainer.updateIndexKeys().
func (m *multidimensionalIndexMaintainer) Update(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	dimExpr := m.getDimensionsExpression()
	if dimExpr == nil {
		return fmt.Errorf("MULTIDIMENSIONAL index %q: root expression must be DimensionsKeyExpression", m.index.Name)
	}

	var oldEntries, newEntries []indexEntry

	if oldRecord != nil {
		entries, err := m.evaluateIndex(oldRecord)
		if err != nil {
			return fmt.Errorf("evaluate index %q for old record: %w", m.index.Name, err)
		}
		oldEntries = entries
	}
	if newRecord != nil {
		entries, err := m.evaluateIndex(newRecord)
		if err != nil {
			return fmt.Errorf("evaluate index %q for new record: %w", m.index.Name, err)
		}
		newEntries = entries
	}

	// Skip entries that are identical between old and new — avoids unnecessary
	// R-tree delete+insert when coordinates/value haven't changed.
	if len(oldEntries) > 0 && len(newEntries) > 0 {
		var err error
		oldEntries, newEntries, err = removeCommonEntries(m.index, oldEntries, newEntries)
		if err != nil {
			return err
		}
	}

	// Process deletes.
	for _, entry := range oldEntries {
		if err := m.deleteEntry(dimExpr, entry); err != nil {
			return err
		}
	}

	// Process inserts.
	for _, entry := range newEntries {
		if err := m.insertEntry(dimExpr, entry); err != nil {
			return err
		}
	}

	return nil
}

// insertEntry inserts a single index entry into the appropriate R-tree.
func (m *multidimensionalIndexMaintainer) insertEntry(dimExpr *DimensionsKeyExpression, entry indexEntry) error {
	prefix, dims, suffix := dimExpr.SplitIndexEntry(entry.key)

	// Build the R-tree subspace (per-prefix).
	rtSubspace := m.indexSubspace
	if len(prefix) > 0 {
		rtSubspace = m.indexSubspace.Sub(prefix...)
	}

	// Create point from dimensional coordinates.
	coords := make([]int64, len(dims))
	for i, d := range dims {
		v, ok := asInt64(d)
		if !ok {
			return fmt.Errorf("MULTIDIMENSIONAL index %q: dimension %d must be int64, got %T", m.index.Name, i, d)
		}
		coords[i] = v
	}
	point := Point{Coordinates: dims}

	// Build key suffix: remaining index columns + trimmed PK.
	trimmedPK, err := m.index.TrimPrimaryKey(entry.primaryKey)
	if err != nil {
		return err
	}
	keySuffix := make(tuple.Tuple, 0, len(suffix)+len(trimmedPK))
	keySuffix = append(keySuffix, suffix...)
	keySuffix = append(keySuffix, trimmedPK...)

	// Value from the index entry.
	value := entry.value
	if value == nil {
		value = tuple.Tuple{}
	}

	storage := newRTreeStorage(rtSubspace, m.rTreeConfig)
	rtree, err := NewRTree(storage, m.rTreeConfig)
	if err != nil {
		return fmt.Errorf("MULTIDIMENSIONAL index %q: %w", m.index.Name, err)
	}
	_ = coords // used above to validate int64
	return rtree.InsertOrUpdate(m.tx, point, keySuffix, value)
}

// deleteEntry removes a single index entry from the appropriate R-tree.
func (m *multidimensionalIndexMaintainer) deleteEntry(dimExpr *DimensionsKeyExpression, entry indexEntry) error {
	prefix, dims, suffix := dimExpr.SplitIndexEntry(entry.key)

	rtSubspace := m.indexSubspace
	if len(prefix) > 0 {
		rtSubspace = m.indexSubspace.Sub(prefix...)
	}

	point := Point{Coordinates: dims}

	trimmedPK, err := m.index.TrimPrimaryKey(entry.primaryKey)
	if err != nil {
		return err
	}
	keySuffix := make(tuple.Tuple, 0, len(suffix)+len(trimmedPK))
	keySuffix = append(keySuffix, suffix...)
	keySuffix = append(keySuffix, trimmedPK...)

	storage := newRTreeStorage(rtSubspace, m.rTreeConfig)
	rtree, err := NewRTree(storage, m.rTreeConfig)
	if err != nil {
		return fmt.Errorf("MULTIDIMENSIONAL index %q: %w", m.index.Name, err)
	}
	return rtree.Delete(m.tx, point, keySuffix)
}

// UpdateWhileWriteOnly handles updates during WRITE_ONLY state.
// MULTIDIMENSIONAL is idempotent (insertOrUpdate is upsert-safe).
func (m *multidimensionalIndexMaintainer) UpdateWhileWriteOnly(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	return m.Update(oldRecord, newRecord)
}

// Scan scans the R-tree for items matching an MBR predicate.
// The scanRange is used for prefix filtering (first PrefixSize elements scope the R-tree subspace).
// Supports proto-wrapped continuation tokens (MultidimensionalIndexScanContinuation) and row limits.
// For basic scans without spatial predicates, returns all items in Hilbert order.
func (m *multidimensionalIndexMaintainer) Scan(
	scanRange TupleRange,
	continuation []byte,
	scanProperties ScanProperties,
) RecordCursor[*IndexEntry] {
	dimExpr := m.getDimensionsExpression()
	if dimExpr == nil {
		return &errorCursor[*IndexEntry]{
			err: fmt.Errorf("MULTIDIMENSIONAL index %q: root expression must be DimensionsKeyExpression", m.index.Name),
		}
	}

	// 1. Extract prefix from scanRange to scope the R-tree subspace.
	var prefix tuple.Tuple
	rtSubspace := m.indexSubspace
	if dimExpr.PrefixSize > 0 && scanRange.Low != nil && len(scanRange.Low) >= dimExpr.PrefixSize {
		prefix = scanRange.Low[:dimExpr.PrefixSize]
		rtSubspace = m.indexSubspace.Sub(prefix...)
	}

	// 2. Parse continuation token.
	var lastHV *big.Int
	var lastKey tuple.Tuple
	if len(continuation) > 0 {
		var cont gen.MultidimensionalIndexScanContinuation
		if err := proto.Unmarshal(continuation, &cont); err == nil {
			if cont.LastHilbertValue != nil {
				lastHV = new(big.Int).SetBytes(cont.LastHilbertValue)
			}
			if cont.LastKey != nil {
				var err error
				lastKey, err = tuple.Unpack(cont.LastKey)
				if err != nil {
					return &errorCursor[*IndexEntry]{
						err: fmt.Errorf("MULTIDIMENSIONAL index %q: invalid continuation lastKey: %w", m.index.Name, err),
					}
				}
			}
		}
	}

	// 3. Scan R-tree.
	storage := newRTreeStorage(rtSubspace, m.rTreeConfig)
	rtree, err := NewRTree(storage, m.rTreeConfig)
	if err != nil {
		return &errorCursor[*IndexEntry]{err: fmt.Errorf("MULTIDIMENSIONAL index %q: %w", m.index.Name, err)}
	}
	items, err := rtree.Scan(m.tx, lastHV, lastKey, nil)
	if err != nil {
		return &errorCursor[*IndexEntry]{err: err}
	}

	// 4. Convert to IndexEntry, tracking HV/key for continuations.
	entries := make([]*IndexEntry, 0, len(items))
	hvs := make([]*big.Int, 0, len(items))
	keys := make([]tuple.Tuple, 0, len(items))
	for _, item := range items {
		// Reconstruct the full index key: prefix + dims + suffix.
		key := make(tuple.Tuple, 0, len(prefix)+len(item.Point.Coordinates)+len(item.KeySuffix))
		if len(prefix) > 0 {
			key = append(key, prefix...)
		}
		key = append(key, item.Point.Coordinates...)
		key = append(key, item.KeySuffix...)
		entries = append(entries, &IndexEntry{
			Index: m.index,
			Key:   key,
			Value: item.Value,
		})
		hvs = append(hvs, item.HilbertValue)
		keys = append(keys, item.ItemKey())
	}

	// 5. Apply row limit.
	limit := 0
	if scanProperties.ExecuteProperties.ReturnedRowLimit > 0 {
		limit = scanProperties.ExecuteProperties.ReturnedRowLimit
	}

	return &rtreeScanCursor{items: entries, hvs: hvs, keys: keys, limit: limit}
}

// DeleteWhere clears all R-tree data for the given prefix.
func (m *multidimensionalIndexMaintainer) DeleteWhere(prefix tuple.Tuple) error {
	rtSubspace := m.indexSubspace
	if len(prefix) > 0 {
		rtSubspace = m.indexSubspace.Sub(prefix...)
	}
	storage := newRTreeStorage(rtSubspace, m.rTreeConfig)
	rtree, err := NewRTree(storage, m.rTreeConfig)
	if err != nil {
		return fmt.Errorf("MULTIDIMENSIONAL index %q: %w", m.index.Name, err)
	}
	return rtree.Clear(m.tx)
}

// rtreeScanCursor wraps materialized R-tree scan results into a RecordCursor
// with support for row limits and proto-wrapped continuation tokens.
type rtreeScanCursor struct {
	items []*IndexEntry
	hvs   []*big.Int    // parallel: hilbert value for each item
	keys  []tuple.Tuple // parallel: item key for each item
	pos   int
	limit int // 0 = unlimited
}

func (c *rtreeScanCursor) OnNext(_ context.Context) (RecordCursorResult[*IndexEntry], error) {
	// Source exhausted.
	if c.pos >= len(c.items) {
		return NewResultNoNext[*IndexEntry](SourceExhausted, &EndContinuation{}), nil
	}
	// Row limit reached.
	if c.limit > 0 && c.pos >= c.limit {
		cont := c.buildContinuation(c.pos - 1)
		return NewResultNoNext[*IndexEntry](ReturnLimitReached, &BytesContinuation{bytes: cont}), nil
	}

	item := c.items[c.pos]
	c.pos++

	// Build continuation for the just-delivered item.
	cont := c.buildContinuation(c.pos - 1)
	return NewResultWithValue(item, &BytesContinuation{bytes: cont}), nil
}

// buildContinuation serializes the position into a MultidimensionalIndexScanContinuation proto.
func (c *rtreeScanCursor) buildContinuation(idx int) []byte {
	msg := &gen.MultidimensionalIndexScanContinuation{
		LastHilbertValue: c.hvs[idx].Bytes(),
		LastKey:          c.keys[idx].Pack(),
	}
	data, _ := proto.Marshal(msg)
	return data
}

func (c *rtreeScanCursor) Close() error { return nil }
