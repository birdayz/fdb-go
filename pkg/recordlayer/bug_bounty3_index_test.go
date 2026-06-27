package recordlayer

import (
	"context"
	"errors"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("BugBounty3Index", func() {
	ctx := context.Background()

	// =========================================================================
	// BUG #1: permutedMinMaxIndexMaintainer.updatePermutedForRemove uses MustGet()
	//
	// Severity: panic ($100)
	// Location: permuted_min_max_index_maintainer.go:223
	//
	// Description: Line 223 uses MustGet() which panics on FDB errors
	// (timeouts, conflicts, etc.) instead of returning an error. Go library
	// code must never panic — this violates the project's design principle:
	// "Explicit errors — never panic in library code, always return errors."
	//
	// The code:
	//   existing := m.tx.Get(fdb.Key(permutedKeyBytes)).MustGet()
	//
	// Should be:
	//   existing, err := m.tx.Get(fdb.Key(permutedKeyBytes)).Get()
	//   if err != nil { return fmt.Errorf("...") }
	//
	// Impact: When an FDB transaction hits a conflict or timeout during
	// a PERMUTED_MIN/MAX delete, the entire application crashes instead
	// of getting a retryable error. The crash would happen inside
	// db.Run()'s retry loop, preventing the retry mechanism from working.
	//
	// This test exercises the delete path to confirm the MustGet is reachable
	// and documents the production panic risk.
	// =========================================================================
	Describe("PERMUTED_MAX MustGet panic on delete path", func() {
		It("exercises the permuted delete path (MustGet reachable)", func() {
			ks := specSubspace()

			// PERMUTED_MAX index: group by nothing, value = price, permutedSize = 0
			// Simpler: GroupBy(Field("price"), Field("order_id")) with permutedSize=1
			// This means: group=(order_id), value=(price), permuted trailing 1 col of group
			// Actually let's use the simplest pattern that exercises updatePermutedForRemove.
			idx := NewPermutedMaxIndex("permuted_max_price",
				GroupBy(Field("price"), Field("order_id")),
				1,
			)

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", idx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Insert a record
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(500)})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Delete the record — this exercises updatePermutedForRemove which calls MustGet()
			// On the happy path this works fine, but any FDB error would cause a panic.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				ok, err := store.DeleteRecord(tuple.Tuple{int64(1)})
				if err != nil {
					return nil, err
				}
				Expect(ok).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify the record and permuted entry are gone
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				exists, err := store.RecordExists(tuple.Tuple{int64(1)}, SerializableIsolation)
				Expect(err).NotTo(HaveOccurred())
				Expect(exists).To(BeFalse())

				// Scan permuted subspace — should be empty
				entries, err := AsList(ctx, store.ScanIndexByType(idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(BeEmpty(), "permuted subspace should be empty after delete")
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("PERMUTED_MAX update where old was extremum exercises MustGet path", func() {
			ks := specSubspace()

			idx := NewPermutedMaxIndex("permuted_max_price2",
				GroupBy(Field("price"), Field("order_id")),
				1,
			)

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", idx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Insert with high price
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(1000)})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Update to lower price — this triggers updatePermutedForRemove with
			// old value being the extremum, which calls MustGet() on the permuted entry.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify permuted entry now has the lower price
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				entries, err := AsList(ctx, store.ScanIndexByType(idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(1))
				// Permuted key format: [groupPrefix, value, groupSuffix]
				// groupPrefix = order_id[:permutePosition=0] = empty (permutePosition = groupPrefixSize - permutedSize = 1 - 1 = 0)
				// Actually: groupPrefixSize = 1 (order_id), permutedSize = 1
				// permutePosition = 1 - 1 = 0
				// groupPrefix = groupKey[:0] = empty
				// groupSuffix = groupKey[0:1] = [order_id]
				// value = [price]
				// permutedKey = [price, order_id]
				Expect(entries[0].Key[0]).To(Equal(int64(100)), "permuted entry should have updated price")
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// =========================================================================
	// BUG #2: COUNT_NOT_NULL without GroupingKeyExpression silently counts nulls
	//
	// Severity: incorrect behavior ($100)
	// Location: count_not_null_index_maintainer.go evaluateGroupingKeys
	//
	// Description: When a COUNT_NOT_NULL index has a root expression that is
	// NOT a GroupingKeyExpression (e.g., plain Field("x")), Go's
	// indexGroupingCount() returns keyExpressionColumnSize, making
	// groupingCount == totalColumns, so groupedCount == 0. The null check
	// loop range [groupingCount, totalColumns) is empty, so hasNull stays
	// false and ALL entries are counted regardless of null values.
	//
	// Java's AtomicMutationIndexMaintainer.getGroupingCount() casts to
	// GroupingKeyExpression and would throw ClassCastException if the
	// expression isn't one. So Java forces users to wrap the expression
	// in a GroupingKeyExpression.
	//
	// Impact: A COUNT_NOT_NULL index without GroupingKeyExpression silently
	// counts null values, defeating its entire purpose. The count would be
	// identical to a regular COUNT index. This is a silent semantic error —
	// no error, no panic, just wrong data.
	// =========================================================================
	Describe("COUNT_NOT_NULL without GroupingKeyExpression", func() {
		It("should not count records where the indexed field is null", func() {
			ks := specSubspace()

			// COUNT_NOT_NULL on Field("price") without GroupingKeyExpression.
			// Expected: records with null price are NOT counted.
			// Bug: without GroupingKeyExpression, the null check is skipped
			// and ALL records are counted (including null-price ones).
			//
			// We wrap in Ungrouped() to make it a proper GroupingKeyExpression
			// with all columns as "grouped" (no grouping columns).
			// Java requires GroupingKeyExpression; Go should too for correctness.
			idx := NewCountNotNullIndex("count_notnull_price_ungrouped", Ungrouped(Field("price")))

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

				// Record 1: price=100 (non-null) → should be counted
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())

				// Record 2: price=200 (non-null) → should be counted
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
				Expect(err).NotTo(HaveOccurred())

				// Record 3: price=nil (null) → should NOT be counted
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3)})
				Expect(err).NotTo(HaveOccurred())

				// Record 4: price=nil (null) → should NOT be counted
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(4)})
				Expect(err).NotTo(HaveOccurred())

				// Scan the COUNT_NOT_NULL index.
				// With Ungrouped(Field("price")):
				//   groupingCount = 0, totalColumns = 1, groupedCount = 1
				//   Null check on columns [0, 1) → checks column 0 (price)
				//   Records with null price are skipped. Correct!
				entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())

				// Should have exactly 1 entry: ungrouped count = 2 (only non-null prices)
				Expect(entries).To(HaveLen(1), "should have one ungrouped count entry")
				Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(2)}),
					"count should be 2 (only records with non-null price)")

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("without Ungrouped wrapper is rejected at Build time", func() {
			// COUNT_NOT_NULL on bare Field("price") — NOT a GroupingKeyExpression.
			// Java's AtomicMutationIndexMaintainerFactory.validateGrouping() throws if
			// the root is not a GroupingKeyExpression. Our Build() now does the same.
			idx := NewCountNotNullIndex("count_notnull_bare_field", Field("price"))

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", idx)
			_, err := builder.Build()
			var mdErr *MetaDataError
			Expect(errors.As(err, &mdErr)).To(BeTrue())
			Expect(mdErr.Message).To(ContainSubstring("requires a GroupingKeyExpression"))
			Expect(mdErr.Message).To(ContainSubstring("count_notnull_bare_field"))
		})
	})

	// =========================================================================
	// BUG #3: SUM index without GroupingKeyExpression silently sums nothing
	//
	// Severity: incorrect behavior ($100)
	// Location: sum_index_maintainer.go:178
	//
	// Description: When a SUM index has a root expression that is NOT a
	// GroupingKeyExpression (e.g., Field("price")), the evaluateSumEntries
	// function computes groupingCount = columnSize = 1 and then checks
	// if groupingCount >= len(values). Since groupingCount == len(values),
	// it hits the "no aggregated column" continue and produces ZERO entries.
	// The SUM index is silently empty.
	//
	// Java would crash with ClassCastException (requires GroupingKeyExpression).
	// Go silently produces an empty index — no error, no data, just silence.
	//
	// Impact: A user who creates a SUM index without wrapping in
	// Ungrouped() gets an index that stores nothing and returns 0 for
	// all queries. No error is raised.
	// =========================================================================
	Describe("SUM index without GroupingKeyExpression", func() {
		It("without GroupingKeyExpression is rejected at Build time", func() {
			// SUM on bare Field("price") — NOT a GroupingKeyExpression.
			// Java's AtomicMutationIndexMaintainerFactory.validateGrouping() throws.
			// Our Build() now does the same.
			idx := NewSumIndex("sum_bare_price", Field("price"))

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", idx)
			_, err := builder.Build()
			var mdErr *MetaDataError
			Expect(errors.As(err, &mdErr)).To(BeTrue())
			Expect(mdErr.Message).To(ContainSubstring("requires a GroupingKeyExpression"))
			Expect(mdErr.Message).To(ContainSubstring("sum_bare_price"))
		})

		It("works correctly with Ungrouped wrapper", func() {
			ks := specSubspace()

			// SUM with Ungrouped(Field("price")) — proper GroupingKeyExpression.
			// groupingCount = 0, groupedCount = 1. Works correctly.
			idx := NewSumIndex("sum_ungrouped_price", Ungrouped(Field("price")))

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

				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())

				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
				Expect(err).NotTo(HaveOccurred())

				entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())

				// With proper Ungrouped wrapping: one entry, sum = 300
				Expect(entries).To(HaveLen(1), "should have one ungrouped sum entry")
				Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(300)}),
					"sum should be 300 (100 + 200)")
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// =========================================================================
	// BUG #4: MIN_EVER_LONG / MAX_EVER_LONG without GroupingKeyExpression
	// silently stores nothing
	//
	// Severity: incorrect behavior ($100)
	// Location: min_max_index_maintainer.go:122 (evaluateEntries)
	//
	// Description: Same pattern as SUM — without GroupingKeyExpression,
	// groupingCount == totalColumns, so "no aggregated column" → skip.
	// The index is silently empty. Java would crash.
	//
	// Impact: Silent empty index, no error, wrong aggregate results.
	// =========================================================================
	Describe("MIN_EVER_LONG / MAX_EVER_LONG without GroupingKeyExpression", func() {
		It("MAX_EVER_LONG without GroupingKeyExpression is rejected at Build time", func() {
			idx := NewMaxEverLongIndex("max_ever_bare_price", Field("price"))

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", idx)
			_, err := builder.Build()
			var mdErr *MetaDataError
			Expect(errors.As(err, &mdErr)).To(BeTrue())
			Expect(mdErr.Message).To(ContainSubstring("requires a GroupingKeyExpression"))
			Expect(mdErr.Message).To(ContainSubstring("max_ever_bare_price"))
		})

		It("MIN_EVER_LONG without GroupingKeyExpression is rejected at Build time", func() {
			idx := NewMinEverLongIndex("min_ever_bare_price", Field("price"))

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", idx)
			_, err := builder.Build()
			var mdErr *MetaDataError
			Expect(errors.As(err, &mdErr)).To(BeTrue())
			Expect(mdErr.Message).To(ContainSubstring("requires a GroupingKeyExpression"))
			Expect(mdErr.Message).To(ContainSubstring("min_ever_bare_price"))
		})
	})

	// =========================================================================
	// BUG #5: removeCommonEntries uses set (not multiset) — loses entries
	// on fan-out duplicate keys for VALUE index
	//
	// Severity: incorrect behavior ($100) — orphan index entries
	// Location: index_maintainer.go:369 (removeCommonEntries)
	//
	// Description: removeCommonEntries uses map[string]struct{} (a set) to
	// track common entries. When a fan-out produces duplicate index entries
	// (e.g., repeated field with value [1, 1] produces two entries with key=1),
	// the set collapses them to one. During an update where the number of
	// duplicate entries changes (e.g., old=[1,1,2], new=[1,2]), ALL matching
	// entries are removed from both sides instead of tracking multiplicity.
	//
	// For VALUE indexes: Set/Clear are idempotent, so the FDB state is correct
	// even with missing Clear operations. Old duplicate Set() calls produce
	// the same result as a single Set(). Missing Clear() is safe because the
	// new Set() overwrites.
	//
	// BUT: removeCommonGroupingKeys for COUNT indexes has the SAME set-based
	// logic, and COUNT uses non-idempotent ADD. If old record fans out to
	// [groupA, groupA] and new fans to [groupA], the count should decrease
	// by 1 (from two +1 to one +1). But removeCommonGroupingKeys removes
	// ALL groupA from both sides, producing zero mutations. The count stays
	// at old+2 instead of being corrected to old+1.
	//
	// This matches Java's behavior (List.removeAll has the same semantics),
	// but both are wrong. This test documents the shared limitation.
	//
	// Impact: COUNT indexes over-count when fan-out duplicate grouping keys
	// change in multiplicity during updates. The error is one-directional:
	// growing duplicates (e.g., [A] → [A,A]) produces the same net as
	// [A] → [A] (zero change) instead of the correct +1.
	// =========================================================================
	Describe("removeCommonGroupingKeys fan-out multiplicity", func() {
		It("COUNT index miscounts when fan-out duplicate grouping keys change", func() {
			ks := specSubspace()

			// COUNT index grouped by tags (fan-out on repeated field)
			// Each tag value gets +1 in the count index
			idx := NewCountIndex("count_by_tag", GroupAll(FanOut("tags")))

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", idx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Insert record with tags=["alpha", "alpha"] — two entries for "alpha"
			// COUNT("alpha") should be 2
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(1),
					Price:   proto.Int32(100),
					Tags:    []string{"alpha", "alpha"},
				})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify initial count
			var initialCount int64
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
				if err != nil {
					return nil, err
				}
				Expect(entries).To(HaveLen(1))
				Expect(entries[0].Key).To(Equal(tuple.Tuple{"alpha"}))
				initialCount = entries[0].Value[0].(int64)
				// Initial count should be 2 (two fan-out entries for "alpha")
				Expect(initialCount).To(Equal(int64(2)))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Update: change tags from ["alpha", "alpha"] to ["alpha"]
			// Expected delta: -1 (from 2 entries to 1 entry)
			// Bug: removeCommonGroupingKeys sees "alpha" in both old and new,
			// removes ALL from both sides, delta = 0. Count stays at 2.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(1),
					Price:   proto.Int32(100),
					Tags:    []string{"alpha"},
				})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Check the count after update
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
				if err != nil {
					return nil, err
				}
				Expect(entries).To(HaveLen(1))
				finalCount := entries[0].Value[0].(int64)

				// BUG: Count is 2 (should be 1). The removeCommonGroupingKeys
				// removed ALL "alpha" from both old and new, so no -1 was issued.
				// This matches Java's behavior (List.removeAll has same semantics).
				Expect(finalCount).To(Equal(int64(2)),
					"BUG CONFIRMED: COUNT index over-counts after fan-out duplicate update. "+
						"Count should be 1 but is 2 because removeCommonGroupingKeys uses set "+
						"semantics instead of multiset. This matches Java's behavior.")
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// =========================================================================
	// BUG #6: PERMUTED_MIN/MAX getExtremum returns IndexEntry.Key which
	// includes PK suffix — slicing at totalSize is correct, but only if
	// the entry key has exactly the right number of elements. With
	// primaryKeyComponentPositions, entries may have fewer PK elements.
	//
	// This is actually NOT a bug — totalSize is the key expression column
	// count (excluding PK), so slicing at [groupPrefixSize:totalSize] correctly
	// extracts the value portion regardless of PK dedup.
	//
	// But there IS a related bug: getExtremum returns entry.Key from the
	// index cursor, which includes PK elements AFTER the value. The slicing
	// at [groupPrefixSize:totalSize] works correctly only when
	// totalSize <= len(entry.Key). If the entry has fewer elements than
	// totalSize (corrupted or truncated), we'd get a runtime panic from
	// slice out-of-bounds.
	// =========================================================================
})
