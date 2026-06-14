package recordlayer

import (
	"context"
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("Tenant isolation for record stores", func() {
	ctx := context.Background()

	// Build metadata with Order type + VALUE index on price + record count.
	buildMetaData := func() *RecordMetaData {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.SetRecordCountKey(EmptyKey())
		priceIdx := NewIndex("Order$price", Field("price"))
		builder.AddIndex("Order", priceIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}

	// createTenantDB creates a tenant on the shared FDB cluster and returns an
	// FDBDatabase scoped to that tenant. Cleans up on test completion.
	createTenantDB := func(name string) *FDBDatabase {
		err := sharedDB.db.CreateTenant(fdb.Key(name))
		Expect(err).NotTo(HaveOccurred())

		tenant, err := sharedDB.db.OpenTenant(fdb.Key(name))
		Expect(err).NotTo(HaveOccurred())

		DeferCleanup(func() {
			_, _ = tenant.Transact(func(tr fdb.WritableTransaction) (any, error) {
				tr.ClearRange(fdb.KeyRange{Begin: fdb.Key(""), End: fdb.Key("\xff")})
				return nil, nil
			})
			_ = sharedDB.db.DeleteTenant(fdb.Key(name))
		})

		return NewFDBDatabaseFromTenant(tenant)
	}

	// openStore opens a record store within a tenant DB at the given subspace.
	openStore := func(tenantDB *FDBDatabase, rtx *FDBRecordContext, md *RecordMetaData, ss subspace.Subspace) *FDBRecordStore {
		store, err := NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(md).
			SetSubspace(ss).
			CreateOrOpen()
		Expect(err).NotTo(HaveOccurred())
		return store
	}

	It("records saved in tenant A are invisible from tenant B", func() {
		specName := CurrentSpecReport().FullText()
		tenantNameA := fmt.Sprintf("iso-a-%s", specName)
		tenantNameB := fmt.Sprintf("iso-b-%s", specName)
		dbA := createTenantDB(tenantNameA)
		dbB := createTenantDB(tenantNameB)
		md := buildMetaData()

		// Use the SAME subspace path in both tenants — this is the critical part.
		// If tenant isolation works, the same subspace in different tenants is
		// completely disjoint.
		ss := subspace.FromBytes(tuple.Tuple{"tenant-iso-test"}.Pack())

		// Save 5 records in tenant A.
		_, err := dbA.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store := openStore(dbA, rtx, md, ss)
			for i := range int64(5) {
				_, err := store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(i + 1),
					Price:   proto.Int32(int32((i + 1) * 100)),
				})
				Expect(err).NotTo(HaveOccurred())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Tenant B should see 0 records in the same subspace.
		_, err = dbB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store := openStore(dbB, rtx, md, ss)

			cursor := store.ScanRecords(nil, ForwardScan())
			defer func() { _ = cursor.Close() }()

			records, err := AsList(ctx, cursor)
			Expect(err).NotTo(HaveOccurred())
			Expect(records).To(BeEmpty(),
				"tenant B must see 0 records — tenant A's data must not leak")

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("both tenants maintain independent record stores", func() {
		specName := CurrentSpecReport().FullText()
		tenantNameA := fmt.Sprintf("iso-a-%s", specName)
		tenantNameB := fmt.Sprintf("iso-b-%s", specName)
		dbA := createTenantDB(tenantNameA)
		dbB := createTenantDB(tenantNameB)
		md := buildMetaData()
		ss := subspace.FromBytes(tuple.Tuple{"tenant-iso-both"}.Pack())

		// Save 3 orders in tenant A.
		_, err := dbA.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store := openStore(dbA, rtx, md, ss)
			for i := range int64(3) {
				_, err := store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(i + 1),
					Price:   proto.Int32(int32((i + 1) * 10)),
				})
				Expect(err).NotTo(HaveOccurred())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Save 7 orders in tenant B (different IDs, different prices).
		_, err = dbB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store := openStore(dbB, rtx, md, ss)
			for i := range int64(7) {
				_, err := store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(i + 100),
					Price:   proto.Int32(int32((i + 100) * 10)),
				})
				Expect(err).NotTo(HaveOccurred())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Verify tenant A sees exactly 3 records.
		_, err = dbA.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store := openStore(dbA, rtx, md, ss)

			records, err := AsList(ctx, store.ScanRecords(nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(records).To(HaveLen(3), "tenant A must see exactly 3 records")

			// Verify the IDs are 1, 2, 3 — not any of B's 100-106.
			for i, rec := range records {
				order := rec.Record.(*gen.Order)
				Expect(order.GetOrderId()).To(Equal(int64(i + 1)))
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Verify tenant B sees exactly 7 records.
		_, err = dbB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store := openStore(dbB, rtx, md, ss)

			records, err := AsList(ctx, store.ScanRecords(nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(records).To(HaveLen(7), "tenant B must see exactly 7 records")

			for i, rec := range records {
				order := rec.Record.(*gen.Order)
				Expect(order.GetOrderId()).To(Equal(int64(i + 100)))
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("record counts are isolated between tenants", func() {
		specName := CurrentSpecReport().FullText()
		tenantNameA := fmt.Sprintf("iso-a-%s", specName)
		tenantNameB := fmt.Sprintf("iso-b-%s", specName)
		dbA := createTenantDB(tenantNameA)
		dbB := createTenantDB(tenantNameB)
		md := buildMetaData()
		ss := subspace.FromBytes(tuple.Tuple{"tenant-iso-count"}.Pack())

		// Save 4 records in tenant A.
		_, err := dbA.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store := openStore(dbA, rtx, md, ss)
			for i := range int64(4) {
				_, err := store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(i + 1),
					Price:   proto.Int32(int32(i * 50)),
				})
				Expect(err).NotTo(HaveOccurred())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Save 2 records in tenant B.
		_, err = dbB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store := openStore(dbB, rtx, md, ss)
			for i := range int64(2) {
				_, err := store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(i + 1),
					Price:   proto.Int32(int32(i * 99)),
				})
				Expect(err).NotTo(HaveOccurred())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Verify tenant A count = 4.
		_, err = dbA.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store := openStore(dbA, rtx, md, ss)
			count, err := store.GetRecordCount()
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(Equal(int64(4)),
				"tenant A record count must be 4, not contaminated by B")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Verify tenant B count = 2.
		_, err = dbB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store := openStore(dbB, rtx, md, ss)
			count, err := store.GetRecordCount()
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(Equal(int64(2)),
				"tenant B record count must be 2, not contaminated by A")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("index scans are isolated between tenants", func() {
		specName := CurrentSpecReport().FullText()
		tenantNameA := fmt.Sprintf("iso-a-%s", specName)
		tenantNameB := fmt.Sprintf("iso-b-%s", specName)
		dbA := createTenantDB(tenantNameA)
		dbB := createTenantDB(tenantNameB)
		md := buildMetaData()
		ss := subspace.FromBytes(tuple.Tuple{"tenant-iso-index"}.Pack())
		priceIdx := md.GetIndex("Order$price")
		Expect(priceIdx).NotTo(BeNil())

		// Tenant A: orders with prices 100, 200, 300.
		_, err := dbA.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store := openStore(dbA, rtx, md, ss)
			for i := range int64(3) {
				_, err := store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(i + 1),
					Price:   proto.Int32(int32((i + 1) * 100)),
				})
				Expect(err).NotTo(HaveOccurred())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Tenant B: orders with prices 999, 998.
		_, err = dbB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store := openStore(dbB, rtx, md, ss)
			for i := range int64(2) {
				_, err := store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(i + 50),
					Price:   proto.Int32(int32(999 - i)),
				})
				Expect(err).NotTo(HaveOccurred())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Index scan in tenant A: should see prices [100, 200, 300] only.
		_, err = dbA.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store := openStore(dbA, rtx, md, ss)
			entries, err := AsList(ctx, store.ScanIndex(priceIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(3), "tenant A index scan must return 3 entries")

			prices := make([]int64, len(entries))
			for i, e := range entries {
				prices[i] = e.IndexValues()[0].(int64)
			}
			Expect(prices).To(Equal([]int64{100, 200, 300}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Index scan in tenant B: should see prices [998, 999] only.
		_, err = dbB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store := openStore(dbB, rtx, md, ss)
			entries, err := AsList(ctx, store.ScanIndex(priceIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2), "tenant B index scan must return 2 entries")

			prices := make([]int64, len(entries))
			for i, e := range entries {
				prices[i] = e.IndexValues()[0].(int64)
			}
			Expect(prices).To(Equal([]int64{998, 999}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("LoadRecord by primary key does not leak across tenants", func() {
		specName := CurrentSpecReport().FullText()
		tenantNameA := fmt.Sprintf("iso-a-%s", specName)
		tenantNameB := fmt.Sprintf("iso-b-%s", specName)
		dbA := createTenantDB(tenantNameA)
		dbB := createTenantDB(tenantNameB)
		md := buildMetaData()
		ss := subspace.FromBytes(tuple.Tuple{"tenant-iso-load"}.Pack())

		// Save order ID=42 in tenant A with price 500.
		_, err := dbA.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store := openStore(dbA, rtx, md, ss)
			_, err := store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(42),
				Price:   proto.Int32(500),
			})
			Expect(err).NotTo(HaveOccurred())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Save order ID=42 in tenant B with a DIFFERENT price (999).
		_, err = dbB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store := openStore(dbB, rtx, md, ss)
			_, err := store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(42),
				Price:   proto.Int32(999),
			})
			Expect(err).NotTo(HaveOccurred())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Load from tenant A — must see price 500.
		_, err = dbA.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store := openStore(dbA, rtx, md, ss)
			rec, err := store.LoadRecord(tuple.Tuple{int64(42)})
			Expect(err).NotTo(HaveOccurred())
			Expect(rec).NotTo(BeNil())
			order := rec.Record.(*gen.Order)
			Expect(order.GetPrice()).To(Equal(int32(500)),
				"tenant A must see its own data, not B's")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Load from tenant B — must see price 999.
		_, err = dbB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store := openStore(dbB, rtx, md, ss)
			rec, err := store.LoadRecord(tuple.Tuple{int64(42)})
			Expect(err).NotTo(HaveOccurred())
			Expect(rec).NotTo(BeNil())
			order := rec.Record.(*gen.Order)
			Expect(order.GetPrice()).To(Equal(int32(999)),
				"tenant B must see its own data, not A's")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("DeleteRecord in one tenant does not affect the other", func() {
		specName := CurrentSpecReport().FullText()
		tenantNameA := fmt.Sprintf("iso-a-%s", specName)
		tenantNameB := fmt.Sprintf("iso-b-%s", specName)
		dbA := createTenantDB(tenantNameA)
		dbB := createTenantDB(tenantNameB)
		md := buildMetaData()
		ss := subspace.FromBytes(tuple.Tuple{"tenant-iso-delete"}.Pack())

		// Save order ID=1 in both tenants.
		for _, tenantDB := range []*FDBDatabase{dbA, dbB} {
			db := tenantDB
			_, err := db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store := openStore(db, rtx, md, ss)
				_, err := store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(1),
					Price:   proto.Int32(100),
				})
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		}

		// Delete the record in tenant A.
		_, err := dbA.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store := openStore(dbA, rtx, md, ss)
			existed, err := store.DeleteRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(existed).To(BeTrue())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Tenant A: record should be gone, count should be 0.
		_, err = dbA.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store := openStore(dbA, rtx, md, ss)
			rec, err := store.LoadRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(rec).To(BeNil(), "tenant A record must be deleted")

			count, err := store.GetRecordCount()
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(Equal(int64(0)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Tenant B: record must still exist, count must be 1.
		_, err = dbB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store := openStore(dbB, rtx, md, ss)
			rec, err := store.LoadRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(rec).NotTo(BeNil(), "tenant B record must NOT be deleted by A's delete")
			Expect(rec.Record.(*gen.Order).GetPrice()).To(Equal(int32(100)))

			count, err := store.GetRecordCount()
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(Equal(int64(1)),
				"tenant B count must still be 1 after A's delete")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
