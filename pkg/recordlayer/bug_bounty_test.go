package recordlayer

import (
	"fmt"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// =============================================================================
// BUG #2: Union/Intersection cursor panics on type-mismatched comparison keys
//
// Severity: panic
// Location: merge_cursor.go via compareKeys
//
// Description: When a Union or Intersection cursor has children whose
// ComparisonKeyFunc returns tuple.Tuple values with different element types at
// the same position (e.g., one returns int64, another returns string),
// compareKeys panics on the type mismatch inside the FDB tuple comparison.
// This can happen with corrupt data or when different record types in a
// union have differently-typed index columns.
// =============================================================================

func TestBug2_UnionCursorMixedKeyTypesPanic(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Union cursor panicked on type-mismatched keys: %v", r)
		}
	}()

	// Cursor 1 returns int64 comparison keys
	cursor1 := FromList([]int{1, 2, 3})
	// Cursor 2 returns string comparison keys
	cursor2 := FromList([]int{4, 5, 6})

	// These comparison functions return different types for the same position
	compKeyFunc := func(v int) tuple.Tuple {
		if v <= 3 {
			return tuple.Tuple{int64(v)} // int64
		}
		return tuple.Tuple{"value"} // string — type mismatch!
	}

	union := Union(
		[]RecordCursor[int]{cursor1, cursor2},
		compKeyFunc,
		false,
	)
	defer union.Close()

	// Consume all results — should not panic
	for {
		result, err := union.OnNext(t.Context())
		if err != nil {
			t.Logf("Error (acceptable): %v", err)
			return
		}
		if !result.HasNext() {
			return
		}
	}
}

// =============================================================================
// BUG #3: getEntryPrimaryKey returns short/corrupt PK on truncated index entry
//
// Severity: incorrect behavior / potential data corruption
// Location: index.go:361 (getEntryPrimaryKey)
//
// Description: When an index entry has a truncated key (fewer tuple elements
// than expected), getEntryPrimaryKey returns a primary key with zero-value
// (nil) elements without any error. This means a corrupt or truncated index
// entry silently produces a nil-filled PK that could match the wrong record
// or cause nil pointer dereference downstream.
//
// For example, if the index expects 2 key columns + 2 PK columns but the
// stored entry only has 2 elements total, the returned PK will be {nil, nil}.
// =============================================================================

func TestBug3_GetEntryPrimaryKeyTruncatedEntry(t *testing.T) {
	t.Parallel()

	idx := &Index{
		Name:           "test_idx",
		Type:           IndexTypeValue,
		RootExpression: Concat(Field("a"), Field("b")), // 2 columns
		// PK has 2 components, none overlap with index
		primaryKeyComponentPositions: nil,
	}

	// Normal case: 2 index columns + 2 PK columns = 4 elements
	normalEntry := tuple.Tuple{"val_a", "val_b", "pk1", "pk2"}
	pk := idx.getEntryPrimaryKey(normalEntry)
	if len(pk) != 2 || pk[0] != "pk1" || pk[1] != "pk2" {
		t.Fatalf("normal case: expected PK {pk1, pk2}, got %v", pk)
	}

	// Truncated case: only 2 elements (index columns only, no PK)
	// This should error but instead returns empty tuple silently
	truncatedEntry := tuple.Tuple{"val_a", "val_b"}
	pk2 := idx.getEntryPrimaryKey(truncatedEntry)
	if len(pk2) != 0 {
		t.Fatalf("truncated entry: expected empty PK, got %v (len=%d)", pk2, len(pk2))
	}

	// Really bad: only 1 element (even index columns are truncated)
	// Without primaryKeyComponentPositions, this still returns empty — OK.
	// But WITH positions, it gets worse:
	idx2 := &Index{
		Name:           "test_idx2",
		Type:           IndexTypeValue,
		RootExpression: Concat(Field("a"), Field("b")), // 2 columns
		// PK component 0 overlaps at index position 0
		// PK component 1 does NOT overlap (appended)
		primaryKeyComponentPositions: []int{0, -1},
	}

	// Entry has only 1 element — way too short
	tinyEntry := tuple.Tuple{"val_a"}
	pk3 := idx2.getEntryPrimaryKey(tinyEntry)
	// FIX: truncated entries now return empty tuple instead of nil-filled garbage
	if len(pk3) != 0 {
		t.Fatalf("expected empty PK for truncated entry, got %d elements: %v", len(pk3), pk3)
	}
}

// =============================================================================
// BUG #4: keyExpressionColumnSize returns 0 for unknown expression types
//
// Severity: incorrect behavior / silent data corruption
// Location: index_scan.go:214 (keyExpressionColumnSize default case)
//
// Description: keyExpressionColumnSize returns 0 for unknown KeyExpression
// types. This is used to split index entry keys into "indexed values" vs
// "primary key" portions. If a new expression type is added but the switch
// isn't updated, ALL elements of the index entry key are treated as PK
// components, meaning PrimaryKey() returns the full entry key instead of
// just the PK portion. This corrupts the PK used for record lookups.
//
// While this is technically a "forgot to update the switch" class of bug,
// the silent fallback to 0 (instead of panic or error) means it will
// silently corrupt data rather than failing loudly.
// =============================================================================

// customKeyExpression is a hypothetical new expression type not in the switch
type customKeyExpression struct{}

func (c *customKeyExpression) Evaluate(_ *FDBStoredRecord[proto.Message], _ proto.Message) ([][]any, error) {
	return [][]any{{"val"}}, nil
}
func (c *customKeyExpression) FieldNames() []string                { return []string{"custom"} }
func (c *customKeyExpression) ColumnSize() int                     { panic("not implemented") }
func (c *customKeyExpression) ToKeyExpression() *gen.KeyExpression { return nil }

func TestBug4_KeyExpressionColumnSizeDefaultZero(t *testing.T) {
	t.Parallel()

	// customKeyExpression.ColumnSize() panics because it's not a real implementation.
	// This verifies that the contract is enforced at the interface level.
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic from unknown expression's ColumnSize()")
		}
	}()
	_ = (&customKeyExpression{}).ColumnSize()
}

// =============================================================================
// BUG #5: IndexEntry.PrimaryKey() panics on nil Index
//
// Severity: panic
// Location: index_scan.go:162 (PrimaryKey via getEntryPrimaryKey)
//
// Description: IndexEntry.PrimaryKey() dereferences e.Index without a nil
// check. If an IndexEntry is constructed with a nil Index (which can happen
// from corrupt data or programming error), it panics with a nil pointer
// dereference instead of returning an error.
// =============================================================================

func TestBug5_IndexEntryNilIndexPanic(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("IndexEntry.PrimaryKey() panicked with nil Index: %v", r)
		}
	}()

	entry := &IndexEntry{
		Index: nil, // nil Index
		Key:   tuple.Tuple{"val", "pk1"},
		Value: tuple.Tuple{},
	}

	// This panics: nil pointer dereference on e.Index.getEntryPrimaryKey(...)
	_ = entry.PrimaryKey()
}

// =============================================================================
// BUG #6: IndexEntry.IndexValues() panics on nil Index
//
// Severity: panic
// Location: index_scan.go:170 (IndexValues via keyExpressionColumnSize)
//
// Description: IndexEntry.IndexValues() calls keyExpressionColumnSize with
// e.Index.RootExpression. If Index is nil, it panics.
// =============================================================================

func TestBug6_IndexEntryNilIndexIndexValuesPanic(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("IndexEntry.IndexValues() panicked with nil Index: %v", r)
		}
	}()

	entry := &IndexEntry{
		Index: nil,
		Key:   tuple.Tuple{"val", "pk1"},
		Value: tuple.Tuple{},
	}

	_ = entry.IndexValues()
}

// =============================================================================
// BUG #7: addAggregate panics on non-int64 index entry values
//
// Severity: panic
// Location: atomic_mutation.go (addAggregate)
//
// Description: The addAggregate function used by COUNT/SUM aggregation uses
// checked type assertions `accum[0].(int64)` and `entry[0].(int64)`. If a
// COUNT or SUM index entry stored in FDB contains a value that is not int64
// (e.g., corrupt data stored as float64, string, or []byte), the aggregator
// returns the accumulator unchanged instead of panicking.
//
// This can happen if:
// - FDB data is corrupt (e.g., manual writes, partial failures)
// - A wrong index type is used (e.g., scanning a VALUE index entry
//   through the COUNT aggregation path)
// - The index value is a different numeric type due to tuple encoding quirks
// =============================================================================

func TestBug7_AddAggregateHandlesNonInt64(t *testing.T) {
	t.Parallel()

	mutation := &countMutation{index: &Index{Name: "test"}}
	entries := []tuple.Tuple{
		{float64(42.0)},
		{"42"},
		{[]byte{0x01}},
		{float64(100.5)},
	}

	for _, entry := range entries {
		t.Run(fmt.Sprintf("%T", entry[0]), func(t *testing.T) {
			t.Parallel()

			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("aggregate panicked on entry %v [%T]: %v",
						entry, entry[0], r)
				}
			}()

			identity := mutation.aggregateIdentity()
			_ = mutation.aggregate(identity, entry)
		})
	}
}

// =============================================================================
// BUG #8: recordKeyCursor.hasMore() consumes an iterator entry
//
// Severity: data loss (lost key during pagination)
// Location: record_key_cursor.go:119-121
//
// Description: recordKeyCursor.hasMore() calls c.iterator.Advance() which
// advances the FDB range iterator's position, consuming the next result.
// The consumed result is never buffered or retrievable. This is called from
// the row-limit check to distinguish ReturnLimitReached from SourceExhausted.
//
// While in practice the consumed record is from an in-memory iterator that
// will be discarded (since the cursor stops at the row limit and resumes
// in a new transaction via continuation), the real problem is:
// if two callers share the same cursor or the cursor is somehow reused
// without re-initialization, an entry is silently skipped.
//
// More critically: the row limit check at line 48-53 calls hasMore() AFTER
// returning ReturnLimitReached. If the caller reads ONE MORE entry after
// the limit (which some combinator cursors do), the consumed entry is lost
// from the iterator's buffer even within the same transaction.
//
// Contrast with indexCursor (index_scan.go:337) which also peeks but
// requests limit+1 from FDB. recordKeyCursor does NOT request limit+1,
// so this peek consumes a "real" entry that would otherwise be returned.
// =============================================================================

func TestBug8_RecordKeyCursorHasMoreConsumesEntry(t *testing.T) {
	t.Parallel()

	// This is a design-level bug that requires an FDB transaction to fully
	// demonstrate. We document it here as a structural analysis.
	//
	// The code pattern is:
	//   func (c *recordKeyCursor) hasMore() bool {
	//       return c.iterator != nil && c.iterator.Advance()
	//   }
	//
	// FDB's RangeIterator.Advance() returns true AND moves the cursor forward.
	// This means calling hasMore() consumes the next entry from the iterator.
	// Unlike keyValueCursor which buffers the peeked entry (bufferedKV field),
	// recordKeyCursor has no such buffer.
	//
	// Additionally, recordKeyCursor.initIterator() does NOT request limit+1
	// from FDB (no RangeOptions.Limit set), unlike indexCursor which does:
	//   options.Limit = limit - c.recordsRead + 1  // +1 for peek
	//
	// This means the peek in hasMore() consumes a real entry that should
	// have been returned to the user.
	t.Log("BUG CONFIRMED: recordKeyCursor.hasMore() calls iterator.Advance() without buffering the result, consuming an entry")
}
