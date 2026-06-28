package recordlayer

import (
	"context"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("TEXT index", func() {
	ctx := context.Background()

	baseMetaData := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return builder
	}

	// collectPositions extracts the int64 position list from an IndexEntry value.
	// TEXT index entries store positions as Tuple(Tuple(pos0, pos1, ...)).
	collectPositions := func(entry *IndexEntry) []int64 {
		if len(entry.Value) == 0 {
			return nil
		}
		inner, ok := entry.Value[0].(tuple.Tuple)
		if !ok {
			return nil
		}
		positions := make([]int64, len(inner))
		for i, v := range inner {
			positions[i] = v.(int64)
		}
		return positions
	}

	// =========================================================================
	// 1. Save and scan by token
	// =========================================================================
	It("save and scan by token: single record with single word", func() {
		ks := specSubspace()

		idx := NewTextIndex("customer_name_text", Field("name"))
		builder := baseMetaData()
		builder.AddIndex("Customer", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(1),
				Name:       proto.String("hello"),
			})
			Expect(err).NotTo(HaveOccurred())

			// Scan for token "hello".
			entries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"hello"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))

			// Key: [token, primaryKey...]
			Expect(entries[0].Key[0]).To(Equal("hello"))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 2. Multiple tokens
	// =========================================================================
	It("multiple tokens: hello world produces entries for both tokens", func() {
		ks := specSubspace()

		idx := NewTextIndex("customer_name_text", Field("name"))
		builder := baseMetaData()
		builder.AddIndex("Customer", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(1),
				Name:       proto.String("hello world"),
			})
			Expect(err).NotTo(HaveOccurred())

			// Scan for "hello".
			helloEntries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"hello"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(helloEntries).To(HaveLen(1))
			Expect(helloEntries[0].Key[0]).To(Equal("hello"))

			// Scan for "world".
			worldEntries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"world"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(worldEntries).To(HaveLen(1))
			Expect(worldEntries[0].Key[0]).To(Equal("world"))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 3. Position list
	// =========================================================================
	It("position list: records token positions correctly", func() {
		ks := specSubspace()

		idx := NewTextIndex("customer_name_text", Field("name"))
		builder := baseMetaData()
		builder.AddIndex("Customer", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// "hello world hello" — "hello" at positions 0 and 2, "world" at position 1.
			_, err = store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(1),
				Name:       proto.String("hello world hello"),
			})
			Expect(err).NotTo(HaveOccurred())

			// Scan for "hello" — should have positions [0, 2].
			helloEntries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"hello"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(helloEntries).To(HaveLen(1))

			positions := collectPositions(helloEntries[0])
			Expect(positions).To(Equal([]int64{0, 2}))

			// Scan for "world" — should have position [1].
			worldEntries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"world"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(worldEntries).To(HaveLen(1))

			worldPositions := collectPositions(worldEntries[0])
			Expect(worldPositions).To(Equal([]int64{1}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 4. Multiple records same token
	// =========================================================================
	It("multiple records same token: both records found when scanning", func() {
		ks := specSubspace()

		idx := NewTextIndex("customer_name_text", Field("name"))
		builder := baseMetaData()
		builder.AddIndex("Customer", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(1),
				Name:       proto.String("hello alice"),
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(2),
				Name:       proto.String("hello bob"),
			})
			Expect(err).NotTo(HaveOccurred())

			// Scan for "hello" — both records should be returned.
			entries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"hello"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))

			// Collect primary keys from entries.
			pks := make(map[int64]bool)
			for _, e := range entries {
				// The key is [token, pk...], last element is the primary key.
				pk := e.Key[len(e.Key)-1].(int64)
				pks[pk] = true
			}
			Expect(pks).To(HaveKey(int64(1)))
			Expect(pks).To(HaveKey(int64(2)))

			// "alice" should only appear for customer 1.
			aliceEntries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"alice"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(aliceEntries).To(HaveLen(1))
			Expect(aliceEntries[0].Key[len(aliceEntries[0].Key)-1]).To(Equal(int64(1)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 5. Delete removes tokens
	// =========================================================================
	It("delete removes all tokens from the index", func() {
		ks := specSubspace()

		idx := NewTextIndex("customer_name_text", Field("name"))
		builder := baseMetaData()
		builder.AddIndex("Customer", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(1),
				Name:       proto.String("hello world"),
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify tokens exist.
			entries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"hello"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))

			// Delete the record.
			deleted, err := store.DeleteRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Verify both tokens are gone.
			helloEntries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"hello"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(helloEntries).To(BeEmpty())

			worldEntries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"world"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(worldEntries).To(BeEmpty())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 6. Update re-indexes
	// =========================================================================
	It("update re-indexes: old tokens removed, new tokens added", func() {
		ks := specSubspace()

		idx := NewTextIndex("customer_name_text", Field("name"))
		builder := baseMetaData()
		builder.AddIndex("Customer", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(1),
				Name:       proto.String("hello world"),
			})
			Expect(err).NotTo(HaveOccurred())

			// Update: change name from "hello world" to "goodbye world".
			_, err = store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(1),
				Name:       proto.String("goodbye world"),
			})
			Expect(err).NotTo(HaveOccurred())

			// "hello" should be gone.
			helloEntries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"hello"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(helloEntries).To(BeEmpty())

			// "goodbye" should be present.
			goodbyeEntries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"goodbye"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(goodbyeEntries).To(HaveLen(1))

			// "world" should still be present (it existed in both old and new).
			worldEntries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"world"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(worldEntries).To(HaveLen(1))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 7. Grouped text index
	// =========================================================================
	It("grouped text index: groups by price, scans per group", func() {
		ks := specSubspace()

		// TEXT index on customer name, grouped by price.
		idx := NewTextIndex("customer_name_text_grouped", GroupBy(Field("name"), Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Customer", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Customer 1: group=100, name="hello world"
			_, err = store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(1),
				Name:       proto.String("hello world"),
				Price:      proto.Int32(100),
			})
			Expect(err).NotTo(HaveOccurred())

			// Customer 2: group=200, name="hello there"
			_, err = store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(2),
				Name:       proto.String("hello there"),
				Price:      proto.Int32(200),
			})
			Expect(err).NotTo(HaveOccurred())

			// Scan for "hello" in group 100.
			group100Entries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken,
				TupleRangeAllOf(tuple.Tuple{int64(100), "hello"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(group100Entries).To(HaveLen(1))

			// Scan for "hello" in group 200.
			group200Entries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken,
				TupleRangeAllOf(tuple.Tuple{int64(200), "hello"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(group200Entries).To(HaveLen(1))

			// Scan for "world" in group 200 — should be empty.
			noWorldEntries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken,
				TupleRangeAllOf(tuple.Tuple{int64(200), "world"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(noWorldEntries).To(BeEmpty())

			// Scan for "world" in group 100 — should have customer 1.
			worldGroup100, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken,
				TupleRangeAllOf(tuple.Tuple{int64(100), "world"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(worldGroup100).To(HaveLen(1))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 8. Scan all tokens (full range)
	// =========================================================================
	It("scan full range: returns all tokens sorted", func() {
		ks := specSubspace()

		idx := NewTextIndex("customer_name_text", Field("name"))
		builder := baseMetaData()
		builder.AddIndex("Customer", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(1),
				Name:       proto.String("alpha beta"),
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(2),
				Name:       proto.String("gamma delta"),
			})
			Expect(err).NotTo(HaveOccurred())

			// Full range scan — should get all token-document pairs.
			entries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			// 4 tokens total: alpha(1), beta(1), delta(2), gamma(2)
			Expect(entries).To(HaveLen(4))

			// Verify sorted by token name (lexicographic).
			tokens := make([]string, len(entries))
			for i, e := range entries {
				tokens[i] = e.Key[0].(string)
			}
			Expect(tokens).To(Equal([]string{"alpha", "beta", "delta", "gamma"}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 9. Scan with continuation (pagination)
	// =========================================================================
	It("scan with continuation: paginate through results", func() {
		ks := specSubspace()

		idx := NewTextIndex("customer_name_text", Field("name"))
		builder := baseMetaData()
		builder.AddIndex("Customer", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save many records to produce many token-document pairs.
			_, err = store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(1),
				Name:       proto.String("alpha bravo charlie"),
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(2),
				Name:       proto.String("delta echo foxtrot"),
			})
			Expect(err).NotTo(HaveOccurred())

			// 6 total entries: alpha(1), bravo(1), charlie(1), delta(2), echo(2), foxtrot(2)

			// Page 1: limit=2
			props := ScanProperties{
				ExecuteProperties: DefaultExecuteProperties().WithReturnedRowLimit(2),
			}
			page1, cont1, err := AsListWithContinuation(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAll, nil, props))
			Expect(err).NotTo(HaveOccurred())
			Expect(page1).To(HaveLen(2))
			Expect(cont1).NotTo(BeNil())

			// Page 2: limit=2, use continuation.
			page2, cont2, err := AsListWithContinuation(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAll, cont1, props))
			Expect(err).NotTo(HaveOccurred())
			Expect(page2).To(HaveLen(2))
			Expect(cont2).NotTo(BeNil())

			// Page 3: limit=2, should get remaining 2.
			page3, _, err := AsListWithContinuation(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAll, cont2, props))
			Expect(err).NotTo(HaveOccurred())
			Expect(page3).To(HaveLen(2))

			// Collect all tokens across pages.
			var allTokens []string
			for _, entries := range [][]*IndexEntry{page1, page2, page3} {
				for _, e := range entries {
					allTokens = append(allTokens, e.Key[0].(string))
				}
			}
			Expect(allTokens).To(Equal([]string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot"}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 10. Omit positions option
	// =========================================================================
	It("TEXT_OMIT_POSITIONS option: position lists are empty", func() {
		ks := specSubspace()

		idx := NewTextIndex("customer_name_nopos", Field("name"))
		idx.Options[IndexOptionTextOmitPositions] = "true"
		builder := baseMetaData()
		builder.AddIndex("Customer", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(1),
				Name:       proto.String("hello world hello"),
			})
			Expect(err).NotTo(HaveOccurred())

			// Scan for "hello" — should exist but with empty position list.
			entries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"hello"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))

			// Value should have an empty tuple for positions.
			positions := collectPositions(entries[0])
			Expect(positions).To(BeEmpty())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 11. Empty text field
	// =========================================================================
	It("empty text field: no index entries created", func() {
		ks := specSubspace()

		idx := NewTextIndex("customer_name_text", Field("name"))
		builder := baseMetaData()
		builder.AddIndex("Customer", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(1),
				Name:       proto.String(""),
			})
			Expect(err).NotTo(HaveOccurred())

			// Full scan — no entries.
			entries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(BeEmpty())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 12. Null (unset) text field
	// =========================================================================
	It("null text field: no index entries created", func() {
		ks := specSubspace()

		idx := NewTextIndex("customer_name_text", Field("name"))
		builder := baseMetaData()
		builder.AddIndex("Customer", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Customer with no name set (nil in proto2).
			_, err = store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(1),
			})
			Expect(err).NotTo(HaveOccurred())

			// Full scan — no entries.
			entries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(BeEmpty())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 13. BunchedMap integrity after many inserts
	// =========================================================================
	It("BunchedMap integrity: many records all scannable", func() {
		ks := specSubspace()

		idx := NewTextIndex("customer_name_text", Field("name"))
		builder := baseMetaData()
		builder.AddIndex("Customer", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save 50 customers all containing the word "customer".
			for i := int64(1); i <= 50; i++ {
				_, err = store.SaveRecord(&gen.Customer{
					CustomerId: proto.Int64(i),
					Name:       proto.String("customer"),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan for "customer" — should return all 50.
			entries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"customer"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(50))

			// All PKs should be present (1..50).
			pks := make(map[int64]bool)
			for _, e := range entries {
				pk := e.Key[len(e.Key)-1].(int64)
				pks[pk] = true
			}
			for i := int64(1); i <= 50; i++ {
				Expect(pks).To(HaveKey(i), "PK %d should be in index", i)
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 14. Forward and reverse scan
	// =========================================================================
	It("forward and reverse scan produce opposite orderings", func() {
		ks := specSubspace()

		idx := NewTextIndex("customer_name_text", Field("name"))
		builder := baseMetaData()
		builder.AddIndex("Customer", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(1),
				Name:       proto.String("alpha bravo"),
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(2),
				Name:       proto.String("charlie delta"),
			})
			Expect(err).NotTo(HaveOccurred())

			// Forward scan.
			fwdEntries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(fwdEntries).To(HaveLen(4))

			fwdTokens := make([]string, len(fwdEntries))
			for i, e := range fwdEntries {
				fwdTokens[i] = e.Key[0].(string)
			}

			// Reverse scan.
			revEntries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAll, nil, ReverseScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(revEntries).To(HaveLen(4))

			revTokens := make([]string, len(revEntries))
			for i, e := range revEntries {
				revTokens[i] = e.Key[0].(string)
			}

			// Forward should be sorted ascending: alpha, bravo, charlie, delta.
			Expect(fwdTokens).To(Equal([]string{"alpha", "bravo", "charlie", "delta"}))

			// Reverse should be opposite: delta, charlie, bravo, alpha.
			Expect(revTokens).To(Equal([]string{"delta", "charlie", "bravo", "alpha"}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 15. DeleteAllRecords clears TEXT index
	// =========================================================================
	It("DeleteAllRecords clears all TEXT index entries", func() {
		ks := specSubspace()

		idx := NewTextIndex("customer_name_text", Field("name"))
		builder := baseMetaData()
		builder.AddIndex("Customer", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(1),
				Name:       proto.String("hello world"),
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(2),
				Name:       proto.String("foo bar"),
			})
			Expect(err).NotTo(HaveOccurred())

			// Confirm entries exist.
			entries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).NotTo(BeEmpty())

			// Delete all records.
			err = store.DeleteAllRecords()
			Expect(err).NotTo(HaveOccurred())

			// All TEXT index entries should be gone.
			entries, err = AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(BeEmpty())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 16. ScanIndex (BY_VALUE) on TEXT index returns error
	// =========================================================================
	It("ScanIndex (BY_VALUE) on TEXT index returns error", func() {
		ks := specSubspace()

		idx := NewTextIndex("customer_name_text", Field("name"))
		builder := baseMetaData()
		builder.AddIndex("Customer", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Using ScanIndex (not ScanIndexByType) on a TEXT index should error.
			_, scanErr := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(scanErr).To(HaveOccurred())
			Expect(scanErr.Error()).To(ContainSubstring("BY_TEXT_TOKEN"))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 17. Tokenizer lowercases and strips diacritics
	// =========================================================================
	It("tokenizer normalizes: lowercases and strips diacritics", func() {
		ks := specSubspace()

		idx := NewTextIndex("customer_name_text", Field("name"))
		builder := baseMetaData()
		builder.AddIndex("Customer", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save with mixed case and diacritics.
			_, err = store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(1),
				Name:       proto.String("Caf\u00e9 HELLO"),
			})
			Expect(err).NotTo(HaveOccurred())

			// "cafe" (lowercase, diacritics stripped) should match.
			cafeEntries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"cafe"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(cafeEntries).To(HaveLen(1))

			// "hello" (lowercased from "HELLO") should match.
			helloEntries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"hello"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(helloEntries).To(HaveLen(1))

			// Original mixed case should NOT match.
			upperEntries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"HELLO"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(upperEntries).To(BeEmpty())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 18. Whitespace-only text field
	// =========================================================================
	It("whitespace-only text field: no index entries created", func() {
		ks := specSubspace()

		idx := NewTextIndex("customer_name_text", Field("name"))
		builder := baseMetaData()
		builder.AddIndex("Customer", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(1),
				Name:       proto.String("   "),
			})
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(BeEmpty())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 19. Duplicate token same record gets correct position list
	// =========================================================================
	It("duplicate token in same text: position list has all occurrences", func() {
		ks := specSubspace()

		idx := NewTextIndex("customer_name_text", Field("name"))
		builder := baseMetaData()
		builder.AddIndex("Customer", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// "the quick brown fox jumped over the lazy dog" — "the" at positions 0 and 6.
			_, err = store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(1),
				Name:       proto.String("the quick brown fox jumped over the lazy dog"),
			})
			Expect(err).NotTo(HaveOccurred())

			theEntries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"the"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(theEntries).To(HaveLen(1))

			positions := collectPositions(theEntries[0])
			Expect(positions).To(Equal([]int64{0, 6}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 20. Update from text to empty clears all tokens
	// =========================================================================
	It("update from text to empty string clears all tokens", func() {
		ks := specSubspace()

		idx := NewTextIndex("customer_name_text", Field("name"))
		builder := baseMetaData()
		builder.AddIndex("Customer", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(1),
				Name:       proto.String("hello world"),
			})
			Expect(err).NotTo(HaveOccurred())

			// Update to empty string.
			_, err = store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(1),
				Name:       proto.String(""),
			})
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(BeEmpty())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 21. Nested field TEXT index (Order.flower.type)
	// =========================================================================
	It("nested field: TEXT index on Order.flower.type", func() {
		ks := specSubspace()

		idx := NewTextIndex("flower_type_text", Nest("flower", Field("type")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(1),
				Flower:  &gen.Flower{Type: proto.String("red roses")},
				Price:   proto.Int32(100),
			})
			Expect(err).NotTo(HaveOccurred())

			// Scan for "red".
			redEntries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"red"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(redEntries).To(HaveLen(1))

			// Scan for "roses".
			rosesEntries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"roses"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(rosesEntries).To(HaveLen(1))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 22. Idempotent re-save
	// =========================================================================
	It("re-saving identical record is idempotent", func() {
		ks := specSubspace()

		idx := NewTextIndex("customer_name_text", Field("name"))
		builder := baseMetaData()
		builder.AddIndex("Customer", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			cust := &gen.Customer{
				CustomerId: proto.Int64(1),
				Name:       proto.String("hello world"),
			}
			_, err = store.SaveRecord(cust)
			Expect(err).NotTo(HaveOccurred())

			// Re-save the same record.
			_, err = store.SaveRecord(cust)
			Expect(err).NotTo(HaveOccurred())

			// "hello" should still have exactly one entry.
			entries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"hello"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))

			// "world" also exactly one.
			worldEntries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"world"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(worldEntries).To(HaveLen(1))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 23. Punctuation handling
	// =========================================================================
	It("punctuation: periods and commas are not part of tokens", func() {
		ks := specSubspace()

		idx := NewTextIndex("customer_name_text", Field("name"))
		builder := baseMetaData()
		builder.AddIndex("Customer", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(1),
				Name:       proto.String("hello, world! foo."),
			})
			Expect(err).NotTo(HaveOccurred())

			// "hello" should match (comma stripped).
			helloEntries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"hello"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(helloEntries).To(HaveLen(1))

			// "world" should match (exclamation stripped).
			worldEntries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"world"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(worldEntries).To(HaveLen(1))

			// "foo" should match (period stripped).
			fooEntries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"foo"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(fooEntries).To(HaveLen(1))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 24. Multiple records, delete one, other survives
	// =========================================================================
	It("delete one of two records with shared token: survivor token remains", func() {
		ks := specSubspace()

		idx := NewTextIndex("customer_name_text", Field("name"))
		builder := baseMetaData()
		builder.AddIndex("Customer", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(1),
				Name:       proto.String("hello alice"),
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(2),
				Name:       proto.String("hello bob"),
			})
			Expect(err).NotTo(HaveOccurred())

			// Delete customer 1.
			deleted, err := store.DeleteRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// "hello" should still have customer 2.
			helloEntries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"hello"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(helloEntries).To(HaveLen(1))
			Expect(helloEntries[0].Key[len(helloEntries[0].Key)-1]).To(Equal(int64(2)))

			// "alice" should be gone.
			aliceEntries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"alice"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(aliceEntries).To(BeEmpty())

			// "bob" should still be present.
			bobEntries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"bob"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(bobEntries).To(HaveLen(1))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 25. Multi-type: TEXT index on Customer, unrelated Order records don't appear
	// =========================================================================
	It("multi-type: TEXT index on Customer does not index Order records", func() {
		ks := specSubspace()

		idx := NewTextIndex("customer_name_text", Field("name"))
		builder := baseMetaData()
		builder.AddIndex("Customer", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save a Customer with "hello".
			_, err = store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(1),
				Name:       proto.String("hello"),
			})
			Expect(err).NotTo(HaveOccurred())

			// Save an Order with a DIFFERENT primary key (TEXT index is only on Customer).
			// Using PK 999 to avoid collision with Customer PK 1 in shared key space.
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(999),
				Price:   proto.Int32(100),
			})
			Expect(err).NotTo(HaveOccurred())

			// "hello" scan should only return the Customer's entry.
			entries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"hello"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 26. Aggressive conflict ranges option
	// =========================================================================
	It("aggressive conflict ranges: does not change scan behavior", func() {
		ks := specSubspace()

		idx := NewTextIndex("customer_name_aggressive", Field("name"))
		idx.Options[IndexOptionTextAddAggressiveConflictRanges] = "true"
		builder := baseMetaData()
		builder.AddIndex("Customer", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(1),
				Name:       proto.String("hello world"),
			})
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"hello"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 27. Text with numbers
	// =========================================================================
	It("text with numbers: digits form separate tokens", func() {
		ks := specSubspace()

		idx := NewTextIndex("customer_name_text", Field("name"))
		builder := baseMetaData()
		builder.AddIndex("Customer", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(1),
				Name:       proto.String("order 12345 confirmed"),
			})
			Expect(err).NotTo(HaveOccurred())

			// "12345" should be a token.
			numEntries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"12345"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(numEntries).To(HaveLen(1))

			// "order" should be a token.
			orderEntries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"order"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(orderEntries).To(HaveLen(1))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 28. Reverse scan with continuation
	// =========================================================================
	It("reverse scan with continuation: pages in reverse order", func() {
		ks := specSubspace()

		idx := NewTextIndex("customer_name_text", Field("name"))
		builder := baseMetaData()
		builder.AddIndex("Customer", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(1),
				Name:       proto.String("alpha bravo charlie delta"),
			})
			Expect(err).NotTo(HaveOccurred())

			// 4 tokens: alpha, bravo, charlie, delta.

			// Reverse scan, page of 2.
			props := ScanProperties{
				ExecuteProperties: DefaultExecuteProperties().WithReturnedRowLimit(2),
				Reverse:           true,
			}
			page1, cont1, err := AsListWithContinuation(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAll, nil, props))
			Expect(err).NotTo(HaveOccurred())
			Expect(page1).To(HaveLen(2))
			Expect(cont1).NotTo(BeNil())

			// First two in reverse: delta, charlie.
			Expect(page1[0].Key[0]).To(Equal("delta"))
			Expect(page1[1].Key[0]).To(Equal("charlie"))

			// Page 2 with continuation.
			page2, _, err := AsListWithContinuation(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAll, cont1, props))
			Expect(err).NotTo(HaveOccurred())
			Expect(page2).To(HaveLen(2))
			Expect(page2[0].Key[0]).To(Equal("bravo"))
			Expect(page2[1].Key[0]).To(Equal("alpha"))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 29. Update only changes affected tokens
	// =========================================================================
	It("update: shared token between old and new text survives", func() {
		ks := specSubspace()

		idx := NewTextIndex("customer_name_text", Field("name"))
		builder := baseMetaData()
		builder.AddIndex("Customer", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(1),
				Name:       proto.String("the quick brown fox"),
			})
			Expect(err).NotTo(HaveOccurred())

			// Update to different text with "the" still present.
			_, err = store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(1),
				Name:       proto.String("the slow gray cat"),
			})
			Expect(err).NotTo(HaveOccurred())

			// "the" should still be present.
			theEntries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"the"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(theEntries).To(HaveLen(1))

			// "quick" should be gone.
			quickEntries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"quick"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(quickEntries).To(BeEmpty())

			// "slow" should be present.
			slowEntries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"slow"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(slowEntries).To(HaveLen(1))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 30. Long text with many unique tokens
	// =========================================================================
	It("long text: many unique tokens all indexed correctly", func() {
		ks := specSubspace()

		idx := NewTextIndex("customer_name_text", Field("name"))
		builder := baseMetaData()
		builder.AddIndex("Customer", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// 26 distinct words.
			_, err = store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(1),
				Name:       proto.String("alpha bravo charlie delta echo foxtrot golf hotel india juliet kilo lima mike november oscar papa quebec romeo sierra tango uniform victor whiskey xray yankee zulu"),
			})
			Expect(err).NotTo(HaveOccurred())

			// All tokens should scan.
			entries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(26))

			// Spot-check specific tokens.
			zuluEntries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"zulu"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(zuluEntries).To(HaveLen(1))
			// "zulu" is position 25 (0-indexed).
			positions := collectPositions(zuluEntries[0])
			Expect(positions).To(Equal([]int64{25}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// (Test #31 removed — DeleteAllRecords already covered by test #15.)

	// =========================================================================
	// 32. Single character tokens
	// =========================================================================
	It("single character tokens: each letter is a valid token", func() {
		ks := specSubspace()

		idx := NewTextIndex("customer_name_text", Field("name"))
		builder := baseMetaData()
		builder.AddIndex("Customer", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(1),
				Name:       proto.String("a b c"),
			})
			Expect(err).NotTo(HaveOccurred())

			// "a", "b", "c" should all be tokens.
			for _, token := range []string{"a", "b", "c"} {
				entries, err := AsList(ctx, store.ScanIndexByType(
					idx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{token}), nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(1), "token %q should be indexed", token)
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 33. Scan with token prefix range
	// =========================================================================
	It("scan with prefix: range over token namespace", func() {
		ks := specSubspace()

		idx := NewTextIndex("customer_name_text", Field("name"))
		builder := baseMetaData()
		builder.AddIndex("Customer", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(1),
				Name:       proto.String("apple apricot banana cherry"),
			})
			Expect(err).NotTo(HaveOccurred())

			// Scan for tokens starting with "ap" using a range [("ap"), ("ap\xff")].
			scanRange := TupleRange{
				Low:          tuple.Tuple{"ap"},
				High:         tuple.Tuple{"ap\xff"},
				LowEndpoint:  EndpointTypeRangeInclusive,
				HighEndpoint: EndpointTypeRangeExclusive,
			}
			entries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, scanRange, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))

			tokens := make([]string, len(entries))
			for i, e := range entries {
				tokens[i] = e.Key[0].(string)
			}
			Expect(tokens).To(Equal([]string{"apple", "apricot"}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 34. Empty scan returns no results
	// =========================================================================
	It("empty index scan: no records returns empty results", func() {
		ks := specSubspace()

		idx := NewTextIndex("customer_name_text", Field("name"))
		builder := baseMetaData()
		builder.AddIndex("Customer", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(BeEmpty())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 35. BunchedMap bunching: many entries per token use bunches
	// =========================================================================
	It("BunchedMap bunching: 30+ records per token use bunching correctly", func() {
		ks := specSubspace()

		idx := NewTextIndex("customer_name_text", Field("name"))
		builder := baseMetaData()
		builder.AddIndex("Customer", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Create 30 customers, all with "common" in their name.
			// bunchSize=20, so this forces at least 2 bunches for the "common" token.
			for i := int64(1); i <= 30; i++ {
				_, err = store.SaveRecord(&gen.Customer{
					CustomerId: proto.Int64(i),
					Name:       proto.String("common"),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			entries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"common"}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(30))

			// PKs should be 1..30 in sorted order (forward scan).
			for i, e := range entries {
				pk := e.Key[len(e.Key)-1].(int64)
				Expect(pk).To(Equal(int64(i + 1)))
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 36. OnlineIndexer builds TEXT index on pre-existing records
	// =========================================================================
	It("OnlineIndexer builds TEXT index on pre-existing records", func() {
		ks := specSubspace()

		// Phase 1: Save 5 customers WITHOUT the TEXT index.
		builder1 := baseMetaData()
		md1, err := builder1.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md1).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(1), Name: proto.String("alpha bravo")})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(2), Name: proto.String("charlie delta")})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(3), Name: proto.String("echo foxtrot")})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(4), Name: proto.String("golf hotel")})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(5), Name: proto.String("india juliet")})
			Expect(err).NotTo(HaveOccurred())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Phase 2: Add TEXT index and build with OnlineIndexer.
		builder2 := baseMetaData()
		textIdx := NewTextIndex("customer_name_text", Field("name"))
		builder2.AddIndex("Customer", textIdx)
		md2, err := builder2.Build()
		Expect(err).NotTo(HaveOccurred())

		indexer, err := NewOnlineIndexerBuilder().
			SetDatabase(sharedDB).
			SetMetaData(md2).
			SetIndex(textIdx).
			SetSubspace(ks).
			Build()
		Expect(err).NotTo(HaveOccurred())

		total, err := indexer.BuildIndex(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(total).To(BeNumerically(">=", 5))

		// Phase 3: Verify all tokens are scannable.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md2).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())

			Expect(store.IsIndexReadable("customer_name_text")).To(BeTrue())

			// Each customer has 2 words -> 10 total tokens.
			allEntries, err := AsList(ctx, store.ScanIndexByType(
				textIdx, IndexScanByTextToken, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(allEntries).To(HaveLen(10))

			// Spot-check specific tokens.
			for _, token := range []string{"alpha", "bravo", "charlie", "echo", "india"} {
				entries, err := AsList(ctx, store.ScanIndexByType(
					textIdx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{token}), nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(1), "token %q should have exactly 1 entry", token)
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 37. OnlineIndexer with limit=2 forces multi-transaction chunked build
	// =========================================================================
	It("OnlineIndexer with limit=2 chunks TEXT index build correctly", func() {
		ks := specSubspace()

		// Phase 1: Save 5 customers WITHOUT the TEXT index.
		builder1 := baseMetaData()
		md1, err := builder1.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md1).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(1), Name: proto.String("alpha bravo")})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(2), Name: proto.String("charlie delta")})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(3), Name: proto.String("echo foxtrot")})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(4), Name: proto.String("golf hotel")})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(5), Name: proto.String("india juliet")})
			Expect(err).NotTo(HaveOccurred())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Phase 2: Build with limit=2 to force multiple chunks.
		builder2 := baseMetaData()
		textIdx := NewTextIndex("customer_name_text", Field("name"))
		builder2.AddIndex("Customer", textIdx)
		md2, err := builder2.Build()
		Expect(err).NotTo(HaveOccurred())

		indexer, err := NewOnlineIndexerBuilder().
			SetDatabase(sharedDB).
			SetMetaData(md2).
			SetIndex(textIdx).
			SetSubspace(ks).
			SetLimit(2).
			Build()
		Expect(err).NotTo(HaveOccurred())

		total, err := indexer.BuildIndex(ctx)
		Expect(err).NotTo(HaveOccurred())
		// With limit=2 and 5 records (+Customers mixed with Orders), boundary
		// records may be re-scanned across chunk boundaries.
		Expect(total).To(BeNumerically(">=", 5))

		// Phase 3: Verify all 10 tokens are present.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md2).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())

			Expect(store.IsIndexReadable("customer_name_text")).To(BeTrue())

			allEntries, err := AsList(ctx, store.ScanIndexByType(
				textIdx, IndexScanByTextToken, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(allEntries).To(HaveLen(10))

			// Every expected token should be present.
			expectedTokens := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel", "india", "juliet"}
			for _, token := range expectedTokens {
				entries, err := AsList(ctx, store.ScanIndexByType(
					textIdx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{token}), nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(1), "token %q should have exactly 1 entry after chunked build", token)
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 38. RebuildIndex on TEXT index
	// =========================================================================
	It("RebuildIndex on TEXT index rebuilds all entries", func() {
		ks := specSubspace()

		textIdx := NewTextIndex("customer_name_text", Field("name"))
		builder := baseMetaData()
		builder.AddIndex("Customer", textIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		// Save records with the TEXT index already present (READABLE).
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(1), Name: proto.String("alpha bravo")})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(2), Name: proto.String("charlie delta")})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(3), Name: proto.String("echo foxtrot")})
			Expect(err).NotTo(HaveOccurred())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// RebuildIndex clears and re-populates the TEXT index.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())

			err = store.RebuildIndex(textIdx)
			Expect(err).NotTo(HaveOccurred())

			// Verify: 6 tokens total (2 per customer * 3 customers).
			allEntries, err := AsList(ctx, store.ScanIndexByType(
				textIdx, IndexScanByTextToken, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(allEntries).To(HaveLen(6))

			// Spot-check specific tokens.
			for _, token := range []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot"} {
				entries, err := AsList(ctx, store.ScanIndexByType(
					textIdx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{token}), nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(1), "token %q should have exactly 1 entry after rebuild", token)
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 39. RebuildIndex after record update reflects new tokens
	// =========================================================================
	It("RebuildIndex after record update reflects new tokens", func() {
		ks := specSubspace()

		textIdx := NewTextIndex("customer_name_text", Field("name"))
		builder := baseMetaData()
		builder.AddIndex("Customer", textIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		// Save initial records.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(1), Name: proto.String("alpha bravo")})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(2), Name: proto.String("charlie delta")})
			Expect(err).NotTo(HaveOccurred())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Update customer 1's name (removes "alpha bravo", adds "xray yankee").
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(1), Name: proto.String("xray yankee")})
			Expect(err).NotTo(HaveOccurred())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// RebuildIndex should produce the current state.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())

			err = store.RebuildIndex(textIdx)
			Expect(err).NotTo(HaveOccurred())

			// Should have: xray, yankee (customer 1), charlie, delta (customer 2) = 4 tokens.
			allEntries, err := AsList(ctx, store.ScanIndexByType(
				textIdx, IndexScanByTextToken, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(allEntries).To(HaveLen(4))

			// "alpha" and "bravo" should be gone.
			for _, gone := range []string{"alpha", "bravo"} {
				entries, err := AsList(ctx, store.ScanIndexByType(
					textIdx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{gone}), nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(BeEmpty(), "token %q should be gone after rebuild", gone)
			}

			// New tokens should be present.
			for _, present := range []string{"xray", "yankee", "charlie", "delta"} {
				entries, err := AsList(ctx, store.ScanIndexByType(
					textIdx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{present}), nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(1), "token %q should be present after rebuild", present)
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 40. commonKeys optimization: update with unchanged text is no-op
	// =========================================================================
	It("commonKeys optimization: update with unchanged text is no-op", func() {
		ks := specSubspace()

		textIdx := NewTextIndex("customer_name_text", Field("name"))
		builder := baseMetaData()
		builder.AddIndex("Customer", textIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		// Save initial record.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(1),
				Name:       proto.String("hello world"),
				Price:      proto.Int32(100),
			})
			Expect(err).NotTo(HaveOccurred())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Capture tokens before update.
		var tokensBefore []string
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndexByType(
				textIdx, IndexScanByTextToken, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			for _, e := range entries {
				tokensBefore = append(tokensBefore, e.Key[0].(string))
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(tokensBefore).To(ConsistOf("hello", "world"))

		// Update the same record with same text but different price.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(1),
				Name:       proto.String("hello world"),
				Price:      proto.Int32(999),
			})
			Expect(err).NotTo(HaveOccurred())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Capture tokens after update.
		var tokensAfter []string
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndexByType(
				textIdx, IndexScanByTextToken, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			for _, e := range entries {
				tokensAfter = append(tokensAfter, e.Key[0].(string))
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Tokens should be identical: no duplicates, no missing.
		Expect(tokensAfter).To(ConsistOf("hello", "world"))
		Expect(tokensAfter).To(Equal(tokensBefore))
	})

	// =========================================================================
	// 41. DeleteRecordsWhere clears type-specific TEXT index entries
	// =========================================================================
	It("DeleteRecordsWhere clears type-specific TEXT index entries", func() {
		ks := specSubspace()

		textIdx := NewTextIndex("customer_name_text", Field("name"))
		builder := baseMetaData()
		builder.AddIndex("Customer", textIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		// Save customers and an order (different type).
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(1), Name: proto.String("hello world")})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(2), Name: proto.String("foo bar")})
			Expect(err).NotTo(HaveOccurred())
			// Use non-overlapping PK for order.
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(100), Price: proto.Int32(42)})
			Expect(err).NotTo(HaveOccurred())

			// Verify TEXT index has 4 tokens before delete.
			entries, err := AsList(ctx, store.ScanIndexByType(
				textIdx, IndexScanByTextToken, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(4)) // hello, world, foo, bar

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// DeleteRecordsWhere with prefix matching customer PK. For type-specific
		// TEXT index, this clears ALL index entries (not just the prefix range).
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())

			err = store.DeleteRecordsWhere(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())

			// All TEXT index entries cleared (type-specific index fully cleared).
			entries, err := AsList(ctx, store.ScanIndexByType(
				textIdx, IndexScanByTextToken, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(BeEmpty())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// ByteScanLimit in TEXT index scans
	// =========================================================================
	It("TEXT scan stops at byte scan limit and resumes with continuation", func() {
		ks := specSubspace()

		idx := NewTextIndex("customer_name_text", Field("name"))
		builder := baseMetaData()
		builder.AddIndex("Customer", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save 3 customers with distinct single-token names to produce
			// 3 separate BunchedMap entries (each in its own token subspace).
			for i := int64(1); i <= 3; i++ {
				names := []string{"alpha", "bravo", "charlie"}
				_, err = store.SaveRecord(&gen.Customer{
					CustomerId: proto.Int64(i),
					Name:       proto.String(names[i-1]),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan all tokens without limit — should find 3 entries.
			allEntries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(allEntries).To(HaveLen(3))

			// Scan with a very small byte limit (1 byte) — should return
			// exactly 1 entry (free initial pass) then stop.
			props := ScanProperties{
				ExecuteProperties: DefaultExecuteProperties().WithScannedBytesLimit(1),
			}
			cursor := store.ScanIndexByType(idx, IndexScanByTextToken, TupleRangeAll, nil, props)
			result, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.HasNext()).To(BeTrue(), "free initial pass should return first entry")
			firstToken := result.GetValue().Key[0]

			// Second call should hit byte limit.
			result, err = cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.HasNext()).To(BeFalse())
			Expect(result.GetNoNextReason()).To(Equal(ByteLimitReached))
			cont, contErr := result.GetContinuation().ToBytes()
			Expect(contErr).NotTo(HaveOccurred())
			Expect(cont).NotTo(BeNil())
			cursor.Close()

			// Resume with continuation — should get remaining entries.
			var remaining []*IndexEntry
			props2 := ForwardScan()
			cursor2 := store.ScanIndexByType(idx, IndexScanByTextToken, TupleRangeAll, cont, props2)
			for {
				r, err := cursor2.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				if !r.HasNext() {
					Expect(r.GetNoNextReason()).To(Equal(SourceExhausted))
					break
				}
				remaining = append(remaining, r.GetValue())
			}
			cursor2.Close()

			Expect(len(remaining)).To(Equal(2), "should get remaining 2 entries after resume")
			// Verify no duplicates — first token should not appear in remaining.
			for _, e := range remaining {
				Expect(e.Key[0]).NotTo(Equal(firstToken))
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("TEXT scan with byte limit returns all entries when limit is large", func() {
		ks := specSubspace()

		idx := NewTextIndex("customer_name_text", Field("name"))
		builder := baseMetaData()
		builder.AddIndex("Customer", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i := int64(1); i <= 3; i++ {
				names := []string{"alpha", "bravo", "charlie"}
				_, err = store.SaveRecord(&gen.Customer{
					CustomerId: proto.Int64(i),
					Name:       proto.String(names[i-1]),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Large byte limit should not interfere — all 3 entries returned.
			props := ScanProperties{
				ExecuteProperties: DefaultExecuteProperties().WithScannedBytesLimit(1_000_000),
			}
			entries, err := AsList(ctx, store.ScanIndexByType(
				idx, IndexScanByTextToken, TupleRangeAll, nil, props))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(3))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
