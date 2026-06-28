package recordlayer

import (
	"context"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("Bug Bounty Round 3", func() {
	ctx := context.Background()

	// =========================================================================
	// BUG #1: normalizeKeyForPositions missing *GroupingKeyExpression case
	//
	// Severity: incorrect behavior (data loss on DeleteRecordsWhere)
	// Location: key_expression.go:462-494
	//
	// Description: normalizeKeyForPositions is missing a case for
	// *GroupingKeyExpression. Java's GroupingKeyExpression.normalizeKeyForPositions()
	// delegates to getWholeKey().normalizeKeyForPositions(). Go's switch statement
	// has no case for *GroupingKeyExpression, so it falls through to default which
	// returns the entire GroupingKeyExpression as a single opaque element.
	//
	// Impact: computeIndexDeletePrefix (store_delete_where.go:203) uses
	// normalizeKeyForPositions to align index columns with PK columns. For
	// universal atomic indexes (COUNT, SUM, etc.) whose RootExpression is a
	// GroupingKeyExpression, the normalization produces a 1-element list (the
	// whole GroupingKeyExpression) instead of flattening to individual field
	// expressions. keyExpressionEquals then fails to match the PK columns,
	// and computeIndexDeletePrefix returns (nil, false). This causes
	// DeleteRecordsWhere to skip clearing that index, leaving stale count/sum
	// data behind.
	// =========================================================================

	It("BUG1: normalizeKeyForPositions flattens GroupingKeyExpression", func() {
		// Set up: universal COUNT index grouped by RecordType
		// Index expr: GroupAll(Concat(RecordType(), Field("price")))
		// PK: Concat(RecordType(), Field("order_id"))
		// prefix: (typeKey)
		//
		// Expected: normalizeKeyForPositions on the GroupingKeyExpression should
		// return [RecordTypeKey, Field("price")] — 2 components.
		// computeIndexDeletePrefix should match PK column 0 (RecordType) with
		// index column 0 (RecordType) and return (prefix, true).
		//
		// Actual (bug): normalizeKeyForPositions returns a 1-element list
		// containing the whole GroupingKeyExpression, so the position matching
		// fails and computeIndexDeletePrefix returns (nil, false).

		groupingExpr := GroupAll(Concat(RecordTypeKey(), Field("price")))

		// Verify the bug: normalizeKeyForPositions on the GroupingKeyExpression
		// should return 2 components but returns 1.
		normalized := normalizeKeyForPositions(groupingExpr)

		// FIX: now correctly returns 2 components: [RecordTypeKey, Field("price")]
		Expect(normalized).To(HaveLen(2), "normalizeKeyForPositions should flatten GroupingKeyExpression to 2 components")

		// Now show the downstream impact: computeIndexDeletePrefix fails.
		countIdx := NewCountIndex("count_by_type", groupingExpr)

		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), Field("order_id")))
		builder.GetRecordType("Customer").SetPrimaryKey(Concat(RecordTypeKey(), Field("customer_id")))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Concat(RecordTypeKey(), Field("id")))
		builder.AddUniversalIndex(countIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		orderTypeKey := md.GetRecordType("Order").GetRecordTypeKey()
		prefix := tuple.Tuple{orderTypeKey}

		// computeIndexDeletePrefix should return (prefix, true) because
		// the index's first column (RecordType) matches PK's first column.
		result, ok := computeIndexDeletePrefix(countIdx, prefix, md, []string{"Order"})

		// FIX: now correctly returns (prefix, true) since GroupingKeyExpression is flattened
		Expect(ok).To(BeTrue(), "computeIndexDeletePrefix should succeed for universal COUNT index with GroupingKeyExpression")
		Expect(result).To(Equal(prefix), "computeIndexDeletePrefix should return the type key prefix")
	})

	// =========================================================================
	// BUG #2: COUNT_NOT_NULL checks null on whole expression instead of grouped portion
	//
	// Severity: incorrect behavior (wrong count)
	// Location: count_not_null_index_maintainer.go:109-113
	//
	// Description: evaluateGroupingKeys in CountNotNullIndexMaintainer calls
	// keyExpressionHasNullField(record.Record, m.index.RootExpression) which
	// checks the FULL expression (including grouping columns) for null fields.
	//
	// Java's AtomicMutationIndexMaintainer.updateIndexKeys() splits the
	// evaluated entry into groupKey and groupedValue, then passes ONLY
	// groupedValue to getMutationParam(). COUNT_NOT_NULL's getMutationParam()
	// calls entry.keyContainsNonUniqueNull() on the grouped VALUE portion only.
	//
	// When a grouping column is null but the value (grouped) column is non-null,
	// Java correctly counts the entry (null check passes on non-null value),
	// but Go incorrectly skips it (null check fails on null grouping column).
	//
	// Impact: COUNT_NOT_NULL indexes with GroupBy expressions produce incorrect
	// counts when grouping columns contain null values. This is silent data
	// corruption — counts are too low.
	// =========================================================================

	// =========================================================================
	// BUG #3: SaveRecordWithOptions panics on nil proto.Message
	//
	// Severity: panic (crash)
	// Location: store.go:362
	//
	// Description: SaveRecordWithOptions dereferences the `record` parameter
	// immediately at line 362:
	//
	//   recordTypeName := string(record.ProtoReflect().Descriptor().Name())
	//
	// If `record` is nil (the zero value of proto.Message), this panics with
	// a nil pointer dereference. The function should return a descriptive
	// error instead of crashing.
	//
	// Java's equivalent throws NullPointerException, but Go library code
	// should never panic — the project's design principles explicitly state:
	// "Explicit errors — never panic in library code, always return errors."
	//
	// Impact: Any caller that passes a nil proto.Message (e.g., from a
	// conditional expression, a failed type assertion, or a function that
	// returns nil on error) will crash the entire application. The same
	// applies to SaveRecord, InsertRecord, UpdateRecord, and DryRunSaveRecord,
	// all of which delegate to SaveRecordWithOptions or share the same pattern.
	// =========================================================================
	It("BUG3: SaveRecordWithOptions/SaveRecord/InsertRecord/UpdateRecord/DryRunSaveRecord panic on nil proto.Message", func() {
		ks := specSubspace()

		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Each of these should return an error, not panic.
			// We test all 5 entry points.

			methods := []struct {
				name string
				call func() error
			}{
				{"SaveRecordWithOptions", func() error {
					var nilRecord proto.Message
					_, err := store.SaveRecordWithOptions(nilRecord, RecordExistenceCheckNone)
					return err
				}},
				{"SaveRecord", func() error {
					_, err := store.SaveRecord(nil)
					return err
				}},
				{"InsertRecord", func() error {
					_, err := store.InsertRecord(nil)
					return err
				}},
				{"UpdateRecord", func() error {
					_, err := store.UpdateRecord(nil)
					return err
				}},
				{"DryRunSaveRecord", func() error {
					var nilRecord proto.Message
					_, err := store.DryRunSaveRecord(nilRecord, RecordExistenceCheckNone)
					return err
				}},
			}

			for _, m := range methods {
				didPanic := false
				var callErr error
				func() {
					defer func() {
						if r := recover(); r != nil {
							didPanic = true
						}
					}()
					callErr = m.call()
				}()
				Expect(didPanic).To(BeFalse(),
					"FIX: %s should return error on nil proto.Message, not panic", m.name)
				Expect(callErr).To(HaveOccurred(),
					"FIX: %s should return error on nil proto.Message", m.name)
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// BUG #4: clearPreviousRecord with empty primary key + splitLongRecords
	// deletes ALL records in the store
	//
	// Severity: DATA LOSS
	// Location: split_helper.go:98-101 (clearPreviousRecord -> clearRecordKeyRange)
	//
	// Description: When splitLongRecords is true and the primary key is an
	// empty tuple (tuple.Tuple{}), clearRecordKeyRange computes:
	//
	//   pkSubspace := recordSubspace.Sub(primaryKey...)
	//   // With empty tuple, this is recordSubspace.Sub() == recordSubspace
	//   begin, end := pkSubspace.FDBRangeKeys()
	//   tx.ClearRange(...)  // Clears ENTIRE records subspace
	//
	// This happens because Go's variadic expansion of an empty slice is a
	// no-op: recordSubspace.Sub() returns recordSubspace itself.
	//
	// In the save path (saveWithSplit), clearPreviousRecord is always called
	// even for new records when splitLongRecords is true (because
	// oldsizeInfo.IsSplit || splitLongRecords evaluates to true). So saving
	// a record with an empty PK and splitLongRecords=true nukes the store.
	//
	// While EmptyKeyExpression as a primary key is unusual, the builder
	// doesn't reject it (see also BUG #5), and the result is silent data
	// destruction rather than a validation error. The fix should be to
	// validate at Build() time that primary keys produce at least one
	// component AND add a defensive check in clearRecordKeyRange.
	// =========================================================================
	It("BUG4: clearRecordKeyRange with empty PK + splitLongRecords deletes all records", func() {
		ks := specSubspace()

		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.SetSplitLongRecords(true)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		// First, save 5 normal records
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			for i := int64(1); i <= 5; i++ {
				price := int32(i * 100)
				if _, err := store.SaveRecord(&gen.Order{OrderId: &i, Price: &price}); err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Verify records exist
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}
			for i := int64(1); i <= 5; i++ {
				exists, err := store.RecordExists(tuple.Tuple{i}, SerializableIsolation)
				if err != nil {
					return nil, err
				}
				Expect(exists).To(BeTrue(), "Record %d should exist before the bug", i)
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Now demonstrate the data loss: clearPreviousRecord with empty PK
		// and splitLongRecords=true clears the entire records subspace.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			// Directly call clearPreviousRecord with empty PK and split=true.
			// This simulates what would happen if SaveRecord were called with
			// a record whose PK evaluates to an empty tuple.
			recordsSubspace := store.subspace.Sub(RecordKey)
			oldsizeInfo := &sizeInfo{} // Zero-value, IsSplit=false
			// Condition: oldsizeInfo.IsSplit || splitLongRecords = false || true = true
			clearPreviousRecord(
				store.context.Transaction(),
				recordsSubspace,
				tuple.Tuple{}, // empty primary key
				true,          // splitLongRecords
				oldsizeInfo,
			)

			// FIX: clearRecordKeyRange is now a no-op for empty PK, so records survive
			for i := int64(1); i <= 5; i++ {
				exists, err := store.RecordExists(tuple.Tuple{i}, SerializableIsolation)
				if err != nil {
					return nil, err
				}
				Expect(exists).To(BeTrue(),
					"FIX: Record %d should survive clearPreviousRecord with empty PK", i)
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// BUG #5: Build() accepts EmptyKeyExpression / Concat() as primary key
	//
	// Severity: enables DATA LOSS (BUG #4 precondition)
	// Location: metadata.go:393-403
	//
	// Description: The builder validates that primary keys are non-nil and
	// don't create duplicates, but does NOT validate that they produce at
	// least one column. Both EmptyKeyExpression (Empty()) and Concat()
	// (zero children) pass validation despite producing zero-length PKs.
	//
	// Java's MetaDataValidator.validatePrimaryKeyForRecordType() calls
	// primaryKey.validate(descriptor) which checks structural validity.
	// The Go equivalent is missing this validation.
	// =========================================================================
	It("BUG5: Build() accepts EmptyKeyExpression as primary key", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(EmptyKey())
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))

		// FIX: Build() now rejects zero-column primary keys
		_, err := builder.Build()
		Expect(err).To(HaveOccurred(),
			"FIX: Build() should reject EmptyKeyExpression as primary key")
	})

	It("BUG5b: Build() accepts Concat() (zero children) as primary key", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Concat()) // zero children
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))

		_, err := builder.Build()
		Expect(err).To(HaveOccurred(),
			"FIX: Build() should reject Concat() with zero children as primary key")
	})

	It("BUG2: COUNT_NOT_NULL should only check null on grouped portion, not grouping columns", func() {
		ks := specSubspace()

		// Index: COUNT_NOT_NULL where:
		//   grouped = Field("quantity") — the value being null-checked
		//   groupBy = Field("price")   — the grouping column
		//
		// wholeKey = Concat(price, quantity), groupedCount = 1
		// Grouping columns = [price], Grouped columns = [quantity]
		//
		// Java behavior: null check is ONLY on quantity (the grouped portion).
		// If price is null but quantity is non-null, Java counts it.
		// Go behavior (bug): checks the whole expression including price.
		// If price is null, Go skips the entry even if quantity is non-null.
		idx := NewCountNotNullIndex("count_notnull_qty", GroupBy(Field("quantity"), Field("price")))

		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Order 1: price=100, quantity=5 — both non-null, should be counted
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(5)})
			Expect(err).NotTo(HaveOccurred())

			// Order 2: price=nil (unset), quantity=10 — grouping column is null,
			// but the VALUE column (quantity) is non-null.
			// Java: counts this entry (null check on quantity passes)
			// Go (bug): skips this entry (null check on whole expr sees null price)
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Quantity: proto.Int32(10)})
			Expect(err).NotTo(HaveOccurred())

			// Order 3: price=200, quantity=nil — value column IS null, should be skipped
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())

			// Order 4: price=nil, quantity=nil — both null, should be skipped
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(4)})
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())

			// Java behavior (correct):
			//   Order 1: groupKey=(100), value=quantity=5 non-null → count. Entry: key=(100), count=1
			//   Order 2: groupKey=(nil), value=quantity=10 non-null → count. Entry: key=(nil), count=1
			//   Order 3: groupKey=(200), value=quantity=nil → skip (value IS null)
			//   Order 4: groupKey=(nil), value=quantity=nil → skip (value IS null)
			//   Total entries: 2 — (100) with count 1, (nil) with count 1

			// FIX: Now correctly produces 2 entries.
			// Order 1: price=100, quantity=5 → counted. Order 2: price=nil, quantity=10 → counted.
			// Order 3: price=200, quantity=nil → skipped. Order 4: both nil → skipped.
			Expect(entries).To(HaveLen(2), "FIX: should have 2 entries (null grouping key is OK, only null grouped column skips)")

			// FDB tuple ordering: nil sorts before int64, so nil-key entry comes first
			Expect(entries[0].Key).To(Equal(tuple.Tuple{nil}))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(1)}))

			// Entry for price=100
			Expect(entries[1].Key).To(Equal(tuple.Tuple{int64(100)}))
			Expect(entries[1].Value).To(Equal(tuple.Tuple{int64(1)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
