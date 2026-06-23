//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
	gofdb "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

var _ = Describe("FDBMetaDataStore Conformance", func() {
	var (
		ctx         context.Context
		java        *JavaInvoker
		goRecordDB  *recordlayer.FDBDatabase
		ss          subspace.Subspace
		storeSS     subspace.Subspace
		clusterFile string
	)

	BeforeEach(func() {
		ctx = context.Background()
		java = NewJavaInvoker()
		// Use non-tenant database directly — avoids tenant prefixing issues
		// with direct SplitHelper calls in Java
		goRecordDB = recordlayer.NewFDBDatabase(sharedDB)
		// Unique subspace per spec for isolation
		prefix := fmt.Sprintf("mdstore_%s", uuid.New().String())
		ss = subspace.Sub(tuple.Tuple{prefix}...)
		// Separate sibling subspace for the record store, so the metadata
		// store and the record store don't share a key prefix (matches the
		// real-world deployment pattern where mdSS lives at /__SYS/META and
		// the user store is at /<dbpath>).
		storeSS = subspace.Sub(tuple.Tuple{prefix + "_store"}...)

		var err error
		clusterFile, err = sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		// Clean up subspace
		_, _ = sharedDB.Transact(func(tr gofdb.WritableTransaction) (any, error) {
			begin, end := ss.FDBRangeKeys()
			tr.ClearRange(gofdb.KeyRange{Begin: begin, End: end})
			begin2, end2 := storeSS.FDBRangeKeys()
			tr.ClearRange(gofdb.KeyRange{Begin: begin2, End: end2})
			return nil, nil
		})
	})

	buildMetaDataProto := func(version int32) *gen.MetaData {
		builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		mdProto, err := md.ToProto()
		Expect(err).NotTo(HaveOccurred())
		mdProto.Version = proto.Int32(version)
		return mdProto
	}

	Describe("Go writes, Java reads", func() {
		It("Java can read metadata stored by Go", func() {
			// Go saves metadata with version 42
			_, err := goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store := recordlayer.NewFDBMetaDataStore(ss)
				return nil, store.SaveRecordMetaData(rtx.Transaction(), buildMetaDataProto(42))
			})
			Expect(err).NotTo(HaveOccurred())

			// Java loads and verifies
			params := map[string]any{
				"clusterFile": clusterFile,
				"subspace":    BytesToIntArray(ss.Bytes()),
			}
			var result struct {
				Found   bool `json:"found"`
				Version int  `json:"version"`
			}
			err = java.InvokeAs(ctx, "loadMetaDataJava", params, &result)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Found).To(BeTrue())
			Expect(result.Version).To(Equal(42))
		})
	})

	Describe("Java writes, Go reads", func() {
		It("Go can read metadata stored by Java", func() {
			// Java saves metadata with version 99
			params := map[string]any{
				"clusterFile": clusterFile,
				"subspace":    BytesToIntArray(ss.Bytes()),
				"version":     99,
			}
			var saveResult struct {
				SavedBytes int `json:"savedBytes"`
			}
			err := java.InvokeAs(ctx, "saveMetaDataJava", params, &saveResult)
			Expect(err).NotTo(HaveOccurred())
			Expect(saveResult.SavedBytes).To(BeNumerically(">", 0))

			// Go loads and verifies
			var loaded *gen.MetaData
			_, err = goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store := recordlayer.NewFDBMetaDataStore(ss)
				var loadErr error
				loaded, loadErr = store.LoadRecordMetaDataProto(rtx.Transaction())
				return nil, loadErr
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(loaded).NotTo(BeNil())
			Expect(loaded.GetVersion()).To(Equal(int32(99)))
		})
	})

	// Track A2 — Catalog wire format Go↔Java functional round-trip.
	//
	// The earlier "Go writes, Java reads" / "Java writes, Go reads" specs
	// only verify that the metadata's `version` field round-trips. That's
	// proto byte-equivalence, but it doesn't prove the loaded metadata is
	// USABLE — that the receiving engine can open a record store with it
	// and read records the other engine wrote. This block fills that gap:
	// one engine saves (metadata + records), the other engine LOADS the
	// metadata and uses it to scan the records.
	//
	// Pinned separately because of the catalog-subspace
	// scar: the byte-level metadata round-trip can be byte-equal at the
	// proto level while the on-disk subspace LAYOUT is incompatible.
	// This functional test catches that class of bug — the loaded metadata
	// has to drive a real record store at the right subspace tuple shape.
	//
	// HOW TO ADD A NEW CROSS-LANGUAGE SPEC:
	//
	//   1. Pick the shape of the test:
	//        a. Records-only (just save + scan records)
	//        b. With an index (save records, then scan the index)
	//        c. With a non-VALUE maintainer (COUNT/SUM/MAX_EVER — atomic-mutation,
	//           BY_GROUP scan, value is the atomic counter)
	//        d. Wire-format flag (e.g. splitLongRecords) where the metadata
	//           bool is what controls receiving-engine decode behaviour.
	//
	//   2. If the shape isn't already covered, add a Java step in
	//      `conformance/metadata_store_conformance.java` modeled on
	//      `loadMetaDataAndScanOrdersJava` (records) / `loadMetaDataAndScanIndexJava`
	//      (VALUE) / `loadMetaDataAndScanCountIndexJava` (atomic-mutation /
	//      BY_GROUP) / `loadMetaDataAndScanAllRecordsJava` (multi-type
	//      classification). All steps follow the same pattern:
	//        - read metadata bytes at unsplit key under mdSubspace
	//        - parseFrom WITH `EXTENSION_REGISTRY` (CRITICAL — see CLAUDE.md
	//          "Cross-language metadata wire-format gotchas")
	//        - RecordMetaData.build(proto)
	//        - open FDBRecordStore at storeSubspace with that metadata,
	//          ALWAYS_READABLE_CHECKER
	//        - read records / scan index / inspect type tags as appropriate
	//        - DynamicMessage workaround for record content: round-trip
	//          via `Order.parseFrom(rec.getRecord().toByteArray())`,
	//          NEVER `Order.newBuilder().mergeFrom(rec.getRecord())`
	//
	//   3. Add the corresponding Go-side spec here. Follow the existing
	//      pattern: build metadata, save proto, derive runtime metadata
	//      via `RecordMetaDataFromProto(mdProto)` (NOT a parallel builder
	//      rebuild — single-source-of-truth schema), open store, save
	//      records, then call `java.InvokeAs(ctx, "<stepName>", params, &result)`.
	//
	//   4. For the reverse direction (Java writes, Go reads), add a
	//      `save*Java` step that does the metadata save + the record
	//      writes from the Java side; the Go-side spec then loads the
	//      metadata via `LoadRecordMetaDataProto` + `RecordMetaDataFromProto`
	//      and exercises whatever read path you want to pin.
	//
	// The harness already covers (currently):
	//   - Records (Go→Java, Java→Go)
	//   - VALUE index (Go→Java, Java→Go)
	//   - Multi-record-type type-tag dispatch (Go→Java)
	//   - Split records (Go→Java)
	//   - COUNT index BY_GROUP (Go→Java, Java→Go)
	//   - SUM index BY_GROUP (Go→Java, Java→Go)
	//   - MAX_EVER_LONG index BY_GROUP (Go→Java)
	//
	// Mechanical follow-ons (same pattern, no new harness mechanism needed):
	//   - MIN_EVER_LONG / MAX_EVER_TUPLE / MIN_EVER_TUPLE index BY_GROUP
	//   - Reverse direction for MAX_EVER / multi-type / split records
	//
	// What this harness does NOT yet cover (gated work):
	//   - SchemaTemplateCatalog wire format (the relational/SQL catalog).
	//     Blocked on Go sqldriver keyspace divergence — Go writes to
	//     `__SYS/__SYS/CATALOG` while Java reads from `(NULL, NULL,
	//     int64(0))`. See `pkg/relational/core/catalog/fdb_store_catalog.go:62-67`.
	Describe("Cross-language functional round-trip (A2)", func() {
		It("Java loads Go-written metadata and scans Go-written records", func() {
			// Direction: Go saves both metadata and Order records; Java
			// LOADS the metadata fresh and uses it to scan.
			orders := []*gen.Order{
				{OrderId: proto.Int64(1), Price: proto.Int32(100)},
				{OrderId: proto.Int64(2), Price: proto.Int32(250)},
				{OrderId: proto.Int64(3), Price: proto.Int32(50)},
			}

			_, err := goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				// Save metadata at mdSS.
				mdStore := recordlayer.NewFDBMetaDataStore(ss)
				mdProto := buildMetaDataProto(7)
				if saveErr := mdStore.SaveRecordMetaData(rtx.Transaction(), mdProto); saveErr != nil {
					return nil, saveErr
				}
				// Build the runtime RecordMetaData from the SAME proto we
				// just persisted (instead of rebuilding via the builder)
				// — guarantees the store uses the exact schema bytes
				// that crossed the wire to Java, removing any chance of
				// the Go save path and the records save path diverging.
				md, buildErr := recordlayer.RecordMetaDataFromProto(mdProto)
				if buildErr != nil {
					return nil, buildErr
				}
				store, openErr := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(storeSS).CreateOrOpen()
				if openErr != nil {
					return nil, openErr
				}
				for _, o := range orders {
					if _, saveErr := store.SaveRecord(o); saveErr != nil {
						return nil, saveErr
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Java loads metadata + scans.
			params := map[string]any{
				"clusterFile":   clusterFile,
				"mdSubspace":    BytesToIntArray(ss.Bytes()),
				"storeSubspace": BytesToIntArray(storeSS.Bytes()),
			}
			var result struct {
				Found           bool `json:"found"`
				MetadataVersion int  `json:"metadataVersion"`
				Rows            []struct {
					OrderId int64 `json:"orderId"`
					Price   int64 `json:"price"`
				} `json:"rows"`
			}
			err = java.InvokeAs(ctx, "loadMetaDataAndScanOrdersJava", params, &result)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Found).To(BeTrue(), "Java should find the metadata Go wrote")
			Expect(result.MetadataVersion).To(Equal(7))
			Expect(result.Rows).To(HaveLen(3))
			// PK-ordered scan: 1, 2, 3.
			Expect(result.Rows[0].OrderId).To(Equal(int64(1)))
			Expect(result.Rows[0].Price).To(Equal(int64(100)))
			Expect(result.Rows[1].OrderId).To(Equal(int64(2)))
			Expect(result.Rows[1].Price).To(Equal(int64(250)))
			Expect(result.Rows[2].OrderId).To(Equal(int64(3)))
			Expect(result.Rows[2].Price).To(Equal(int64(50)))
		})

		It("Java scans Go-built VALUE index using cross-language metadata", func() {
			// Stricter: Go saves metadata WITH a VALUE index on Order.price,
			// inserts records (which builds the index entries), and Java
			// LOADS the metadata fresh, opens the store at the records
			// subspace, and scans the index. Pins:
			//   - record-layer index subspace layout (matches across engines)
			//   - VALUE-index key tuple shape
			//   - per-record index entries written by Go are readable by Java
			//     using only the proto-serialized metadata.
			orders := []*gen.Order{
				{OrderId: proto.Int64(1), Price: proto.Int32(100)},
				{OrderId: proto.Int64(2), Price: proto.Int32(50)},
				{OrderId: proto.Int64(3), Price: proto.Int32(150)},
			}

			_, err := goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				// Build metadata with a VALUE index — same shape as
				// Java's createCompositeIndexedMetaData uses
				// "Order$price_id" so the index name is byte-identical
				// across the wire and the Java step can reference it.
				builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
				builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
				builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
				builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
				idx := recordlayer.NewIndex("Order$price",
					recordlayer.Field("price"))
				builder.AddIndex("Order", idx)
				md, buildErr := builder.Build()
				if buildErr != nil {
					return nil, buildErr
				}
				mdProto, protoErr := md.ToProto()
				if protoErr != nil {
					return nil, protoErr
				}
				mdProto.Version = proto.Int32(13)

				mdStore := recordlayer.NewFDBMetaDataStore(ss)
				if saveErr := mdStore.SaveRecordMetaData(rtx.Transaction(), mdProto); saveErr != nil {
					return nil, saveErr
				}
				store, openErr := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(storeSS).CreateOrOpen()
				if openErr != nil {
					return nil, openErr
				}
				for _, o := range orders {
					if _, saveErr := store.SaveRecord(o); saveErr != nil {
						return nil, saveErr
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			params := map[string]any{
				"clusterFile":   clusterFile,
				"mdSubspace":    BytesToIntArray(ss.Bytes()),
				"storeSubspace": BytesToIntArray(storeSS.Bytes()),
				"indexName":     "Order$price",
			}
			var result struct {
				Found           bool `json:"found"`
				MetadataVersion int  `json:"metadataVersion"`
				Rows            []struct {
					IndexKey   []any `json:"indexKey"`
					PrimaryKey []any `json:"primaryKey"`
				} `json:"rows"`
			}
			err = java.InvokeAs(ctx, "loadMetaDataAndScanIndexJava", params, &result)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Found).To(BeTrue())
			Expect(result.MetadataVersion).To(Equal(13))
			Expect(result.Rows).To(HaveLen(3))
			// VALUE-index ordered by price: [50, 2], [100, 1], [150, 3].
			// JSON unmarshal renders longs as float64.
			Expect(result.Rows[0].IndexKey[0]).To(BeNumerically("==", 50))
			Expect(result.Rows[0].PrimaryKey[0]).To(BeNumerically("==", 2))
			Expect(result.Rows[1].IndexKey[0]).To(BeNumerically("==", 100))
			Expect(result.Rows[1].PrimaryKey[0]).To(BeNumerically("==", 1))
			Expect(result.Rows[2].IndexKey[0]).To(BeNumerically("==", 150))
			Expect(result.Rows[2].PrimaryKey[0]).To(BeNumerically("==", 3))
		})

		It("Go scans Java-built VALUE index using cross-language metadata", func() {
			// Reverse direction of the index test: Java saves metadata
			// (with VALUE index) + records; Go LOADS the metadata fresh,
			// opens the store, scans the index. Pins symmetry of the
			// Java→Go index path.
			params := map[string]any{
				"clusterFile":   clusterFile,
				"mdSubspace":    BytesToIntArray(ss.Bytes()),
				"storeSubspace": BytesToIntArray(storeSS.Bytes()),
				"orderIds":      []int64{1, 2, 3},
				"prices":        []int64{100, 50, 150},
				"version":       17,
			}
			var saveResult struct {
				MetadataBytes int `json:"metadataBytes"`
				RecordsSaved  int `json:"recordsSaved"`
			}
			err := java.InvokeAs(ctx, "saveMetaDataWithIndexAndOrdersJava", params, &saveResult)
			Expect(err).NotTo(HaveOccurred())
			Expect(saveResult.RecordsSaved).To(Equal(3))

			type entry struct {
				priceKey int64
				pk       int64
			}
			var indexEntries []entry
			_, err = goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				mdStore := recordlayer.NewFDBMetaDataStore(ss)
				loaded, loadErr := mdStore.LoadRecordMetaDataProto(rtx.Transaction())
				if loadErr != nil {
					return nil, loadErr
				}
				if loaded == nil {
					return nil, fmt.Errorf("Go: no metadata at subspace")
				}
				md, buildErr := recordlayer.RecordMetaDataFromProto(loaded)
				if buildErr != nil {
					return nil, buildErr
				}
				store, openErr := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(storeSS).Open()
				if openErr != nil {
					return nil, openErr
				}
				idx := md.GetIndex("Order$price")
				cursor := store.ScanIndex(idx, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan())
				entries, scanErr := recordlayer.AsList(ctx, cursor)
				if scanErr != nil {
					return nil, scanErr
				}
				for _, e := range entries {
					price, ok := e.Key[0].(int64)
					if !ok {
						return nil, fmt.Errorf("expected int64 in index key, got %T (%v)", e.Key[0], e.Key[0])
					}
					pk, ok := e.PrimaryKey()[0].(int64)
					if !ok {
						return nil, fmt.Errorf("expected int64 in primary key, got %T (%v)", e.PrimaryKey()[0], e.PrimaryKey()[0])
					}
					indexEntries = append(indexEntries, entry{priceKey: price, pk: pk})
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(indexEntries).To(HaveLen(3))
			// VALUE-index ordered by price: [50→2], [100→1], [150→3].
			Expect(indexEntries[0].priceKey).To(Equal(int64(50)))
			Expect(indexEntries[0].pk).To(Equal(int64(2)))
			Expect(indexEntries[1].priceKey).To(Equal(int64(100)))
			Expect(indexEntries[1].pk).To(Equal(int64(1)))
			Expect(indexEntries[2].priceKey).To(Equal(int64(150)))
			Expect(indexEntries[2].pk).To(Equal(int64(3)))
		})

		It("Java scans Go-built SUM index using cross-language metadata", func() {
			// SUM uses the same atomic-mutation maintainer as COUNT but
			// the atomic value is the sum of the indexed expression
			// rather than +1 per row. Pins the SUM wire format
			// independent of COUNT — both are atomic but a bug in the
			// SUM maintainer's atomic-ADD payload (e.g. encoding the
			// added value as int32 instead of int64, or the wrong
			// endianness) would silently produce a wrong total without
			// breaking COUNT.
			//
			// Setup: 4 Orders with prices 100, 50, 150, 200 → SUM=500.
			orders := []*gen.Order{
				{OrderId: proto.Int64(1), Price: proto.Int32(100)},
				{OrderId: proto.Int64(2), Price: proto.Int32(50)},
				{OrderId: proto.Int64(3), Price: proto.Int32(150)},
				{OrderId: proto.Int64(4), Price: proto.Int32(200)},
			}

			_, err := goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
				builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
				builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
				builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
				idx := recordlayer.NewSumIndex("Order$total_price",
					recordlayer.Ungrouped(recordlayer.Field("price")))
				builder.AddIndex("Order", idx)
				built, buildErr := builder.Build()
				if buildErr != nil {
					return nil, buildErr
				}
				mdProto, protoErr := built.ToProto()
				if protoErr != nil {
					return nil, protoErr
				}
				mdProto.Version = proto.Int32(43)

				mdStore := recordlayer.NewFDBMetaDataStore(ss)
				if saveErr := mdStore.SaveRecordMetaData(rtx.Transaction(), mdProto); saveErr != nil {
					return nil, saveErr
				}
				md, fromProtoErr := recordlayer.RecordMetaDataFromProto(mdProto)
				if fromProtoErr != nil {
					return nil, fromProtoErr
				}
				store, openErr := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(storeSS).CreateOrOpen()
				if openErr != nil {
					return nil, openErr
				}
				for _, o := range orders {
					if _, saveErr := store.SaveRecord(o); saveErr != nil {
						return nil, saveErr
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			params := map[string]any{
				"clusterFile":   clusterFile,
				"mdSubspace":    BytesToIntArray(ss.Bytes()),
				"storeSubspace": BytesToIntArray(storeSS.Bytes()),
				"indexName":     "Order$total_price",
			}
			var result struct {
				Found           bool  `json:"found"`
				MetadataVersion int   `json:"metadataVersion"`
				EntryCount      int   `json:"entryCount"`
				Sum             int64 `json:"sum"`
			}
			err = java.InvokeAs(ctx, "loadMetaDataAndScanSumIndexJava", params, &result)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Found).To(BeTrue())
			Expect(result.MetadataVersion).To(Equal(43))
			Expect(result.EntryCount).To(Equal(1), "ungrouped SUM has exactly one entry")
			Expect(result.Sum).To(Equal(int64(500)))
		})

		It("Java scans Go-built COUNT index using cross-language metadata", func() {
			// COUNT indexes use the atomic-mutation maintainer — entries
			// are atomic counters stored at one key per grouping value
			// (vs VALUE which stores one entry per record). Different
			// maintainer + different scan type (BY_GROUP) than the prior
			// VALUE-index test. Pins that COUNT wire format AND atomic
			// counter values round-trip across engines.
			//
			// Setup: 3 Orders at price=100, 1 at price=50, 1 at price=150
			// → COUNT(price=100)=3, COUNT(price=50)=1, COUNT(price=150)=1.
			orders := []*gen.Order{
				{OrderId: proto.Int64(1), Price: proto.Int32(100)},
				{OrderId: proto.Int64(2), Price: proto.Int32(100)},
				{OrderId: proto.Int64(3), Price: proto.Int32(50)},
				{OrderId: proto.Int64(4), Price: proto.Int32(100)},
				{OrderId: proto.Int64(5), Price: proto.Int32(150)},
			}

			_, err := goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
				builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
				builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
				builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
				idx := recordlayer.NewCountIndex("Order$count_by_price",
					recordlayer.GroupAll(recordlayer.Field("price")))
				builder.AddIndex("Order", idx)
				built, buildErr := builder.Build()
				if buildErr != nil {
					return nil, buildErr
				}
				mdProto, protoErr := built.ToProto()
				if protoErr != nil {
					return nil, protoErr
				}
				mdProto.Version = proto.Int32(37)

				mdStore := recordlayer.NewFDBMetaDataStore(ss)
				if saveErr := mdStore.SaveRecordMetaData(rtx.Transaction(), mdProto); saveErr != nil {
					return nil, saveErr
				}
				md, fromProtoErr := recordlayer.RecordMetaDataFromProto(mdProto)
				if fromProtoErr != nil {
					return nil, fromProtoErr
				}
				store, openErr := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(storeSS).CreateOrOpen()
				if openErr != nil {
					return nil, openErr
				}
				for _, o := range orders {
					if _, saveErr := store.SaveRecord(o); saveErr != nil {
						return nil, saveErr
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			params := map[string]any{
				"clusterFile":   clusterFile,
				"mdSubspace":    BytesToIntArray(ss.Bytes()),
				"storeSubspace": BytesToIntArray(storeSS.Bytes()),
				"indexName":     "Order$count_by_price",
			}
			var result struct {
				Found           bool `json:"found"`
				MetadataVersion int  `json:"metadataVersion"`
				Rows            []struct {
					Key   []any `json:"key"`
					Count int64 `json:"count"`
				} `json:"rows"`
			}
			err = java.InvokeAs(ctx, "loadMetaDataAndScanCountIndexJava", params, &result)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Found).To(BeTrue())
			Expect(result.MetadataVersion).To(Equal(37))
			// Three groups (price=50, 100, 150), tuple-ordered by price.
			Expect(result.Rows).To(HaveLen(3))
			Expect(result.Rows[0].Key[0]).To(BeNumerically("==", 50))
			Expect(result.Rows[0].Count).To(Equal(int64(1)))
			Expect(result.Rows[1].Key[0]).To(BeNumerically("==", 100))
			Expect(result.Rows[1].Count).To(Equal(int64(3)))
			Expect(result.Rows[2].Key[0]).To(BeNumerically("==", 150))
			Expect(result.Rows[2].Count).To(Equal(int64(1)))
		})

		It("Java scans Go-built MAX_EVER_LONG index using cross-language metadata", func() {
			// MAX_EVER_LONG tracks the maximum value EVER inserted —
			// uses FDB's BYTE_MAX atomic (one-way), distinct from SUM
			// (ADD) and COUNT (+1). Pins MAX_EVER wire format
			// independently. Setup: 4 prices [100, 50, 150, 200] → max=200.
			orders := []*gen.Order{
				{OrderId: proto.Int64(1), Price: proto.Int32(100)},
				{OrderId: proto.Int64(2), Price: proto.Int32(50)},
				{OrderId: proto.Int64(3), Price: proto.Int32(150)},
				{OrderId: proto.Int64(4), Price: proto.Int32(200)},
			}

			_, err := goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
				builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
				builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
				builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
				idx := recordlayer.NewMaxEverLongIndex("Order$max_price",
					recordlayer.Ungrouped(recordlayer.Field("price")))
				builder.AddIndex("Order", idx)
				built, buildErr := builder.Build()
				if buildErr != nil {
					return nil, buildErr
				}
				mdProto, protoErr := built.ToProto()
				if protoErr != nil {
					return nil, protoErr
				}
				mdProto.Version = proto.Int32(59)

				mdStore := recordlayer.NewFDBMetaDataStore(ss)
				if saveErr := mdStore.SaveRecordMetaData(rtx.Transaction(), mdProto); saveErr != nil {
					return nil, saveErr
				}
				md, fromProtoErr := recordlayer.RecordMetaDataFromProto(mdProto)
				if fromProtoErr != nil {
					return nil, fromProtoErr
				}
				store, openErr := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(storeSS).CreateOrOpen()
				if openErr != nil {
					return nil, openErr
				}
				for _, o := range orders {
					if _, saveErr := store.SaveRecord(o); saveErr != nil {
						return nil, saveErr
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			params := map[string]any{
				"clusterFile":   clusterFile,
				"mdSubspace":    BytesToIntArray(ss.Bytes()),
				"storeSubspace": BytesToIntArray(storeSS.Bytes()),
				"indexName":     "Order$max_price",
			}
			var result struct {
				Found           bool  `json:"found"`
				MetadataVersion int   `json:"metadataVersion"`
				EntryCount      int   `json:"entryCount"`
				MaxEver         int64 `json:"maxEver"`
			}
			err = java.InvokeAs(ctx, "loadMetaDataAndScanMaxEverIndexJava", params, &result)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Found).To(BeTrue())
			Expect(result.MetadataVersion).To(Equal(59))
			Expect(result.EntryCount).To(Equal(1), "ungrouped MAX_EVER has exactly one entry")
			Expect(result.MaxEver).To(Equal(int64(200)))
		})

		It("Java scans multi-record-type store (Orders + Customers) using cross-language metadata", func() {
			// The union-descriptor wire-format covers ALL record types in
			// one descriptor. This test pins that the multi-type union
			// (Order + Customer + TypedRecord, all defined in the demo
			// proto) round-trips through metadata serialization, AND
			// that the loaded RecordMetaData dispatches stored bytes
			// blobs to the correct record-type by tag. A bug in the
			// type-tag prefix (or the union field-tag mapping) would
			// surface here even when single-type tests pass.
			_, err := goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				mdProto := buildMetaDataProto(31)
				mdStore := recordlayer.NewFDBMetaDataStore(ss)
				if saveErr := mdStore.SaveRecordMetaData(rtx.Transaction(), mdProto); saveErr != nil {
					return nil, saveErr
				}
				md, fromProtoErr := recordlayer.RecordMetaDataFromProto(mdProto)
				if fromProtoErr != nil {
					return nil, fromProtoErr
				}
				store, openErr := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(storeSS).CreateOrOpen()
				if openErr != nil {
					return nil, openErr
				}
				// Two Orders + three Customers — asymmetric counts so a
				// type-tag mix-up (Order misclassified as Customer or
				// vice versa) shows up cleanly.
				orders := []*gen.Order{
					{OrderId: proto.Int64(1), Price: proto.Int32(100)},
					{OrderId: proto.Int64(2), Price: proto.Int32(200)},
				}
				for _, o := range orders {
					if _, saveErr := store.SaveRecord(o); saveErr != nil {
						return nil, saveErr
					}
				}
				customers := []*gen.Customer{
					{CustomerId: proto.Int64(10), Name: proto.String("alice")},
					{CustomerId: proto.Int64(20), Name: proto.String("bob")},
					{CustomerId: proto.Int64(30), Name: proto.String("carol")},
				}
				for _, c := range customers {
					if _, saveErr := store.SaveRecord(c); saveErr != nil {
						return nil, saveErr
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			params := map[string]any{
				"clusterFile":   clusterFile,
				"mdSubspace":    BytesToIntArray(ss.Bytes()),
				"storeSubspace": BytesToIntArray(storeSS.Bytes()),
			}
			var result struct {
				Found           bool `json:"found"`
				MetadataVersion int  `json:"metadataVersion"`
				TotalRecords    int  `json:"totalRecords"`
				OrderCount      int  `json:"orderCount"`
				CustomerCount   int  `json:"customerCount"`
			}
			err = java.InvokeAs(ctx, "loadMetaDataAndScanAllRecordsJava", params, &result)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Found).To(BeTrue())
			Expect(result.MetadataVersion).To(Equal(31))
			Expect(result.TotalRecords).To(Equal(5))
			Expect(result.OrderCount).To(Equal(2))
			Expect(result.CustomerCount).To(Equal(3))
		})

		It("Go scans Java-built COUNT index using cross-language metadata", func() {
			// Reverse direction of spec #6: Java writes metadata + records;
			// Go LOADS the metadata, opens the store, scans BY_GROUP. Pins
			// symmetry of the COUNT-index Java→Go path (the atomic-mutation
			// maintainer wire format works in both directions).
			params := map[string]any{
				"clusterFile":   clusterFile,
				"mdSubspace":    BytesToIntArray(ss.Bytes()),
				"storeSubspace": BytesToIntArray(storeSS.Bytes()),
				"orderIds":      []int64{1, 2, 3, 4, 5},
				"prices":        []int64{100, 100, 50, 100, 150},
				"version":       47,
			}
			var saveResult struct {
				MetadataBytes int `json:"metadataBytes"`
				RecordsSaved  int `json:"recordsSaved"`
			}
			err := java.InvokeAs(ctx, "saveMetaDataWithCountIndexAndOrdersJava", params, &saveResult)
			Expect(err).NotTo(HaveOccurred())
			Expect(saveResult.RecordsSaved).To(Equal(5))

			type entry struct {
				priceKey int64
				count    int64
			}
			var indexEntries []entry
			_, err = goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				mdStore := recordlayer.NewFDBMetaDataStore(ss)
				loaded, loadErr := mdStore.LoadRecordMetaDataProto(rtx.Transaction())
				if loadErr != nil {
					return nil, loadErr
				}
				if loaded == nil {
					return nil, fmt.Errorf("Go: no metadata at subspace")
				}
				md, buildErr := recordlayer.RecordMetaDataFromProto(loaded)
				if buildErr != nil {
					return nil, buildErr
				}
				store, openErr := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(storeSS).Open()
				if openErr != nil {
					return nil, openErr
				}
				idx := md.GetIndex("Order$count_by_price")
				cursor := store.ScanIndex(idx, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan())
				entries, scanErr := recordlayer.AsList(ctx, cursor)
				if scanErr != nil {
					return nil, scanErr
				}
				for _, e := range entries {
					if len(e.Key) == 0 {
						return nil, fmt.Errorf("expected non-empty COUNT-index key tuple")
					}
					price, ok := e.Key[0].(int64)
					if !ok {
						return nil, fmt.Errorf("expected int64 in COUNT-index key, got %T (%v)", e.Key[0], e.Key[0])
					}
					// COUNT-index value is the atomic counter, stored as
					// the leading int64 in the value tuple.
					if len(e.Value) == 0 {
						return nil, fmt.Errorf("expected non-empty COUNT-index value tuple (atomic counter missing)")
					}
					count, ok := e.Value[0].(int64)
					if !ok {
						return nil, fmt.Errorf("expected int64 in COUNT-index value, got %T (%v)", e.Value[0], e.Value[0])
					}
					indexEntries = append(indexEntries, entry{priceKey: price, count: count})
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(indexEntries).To(HaveLen(3), "three distinct prices → three groups")
			// price=50 → 1, price=100 → 3, price=150 → 1.
			Expect(indexEntries[0].priceKey).To(Equal(int64(50)))
			Expect(indexEntries[0].count).To(Equal(int64(1)))
			Expect(indexEntries[1].priceKey).To(Equal(int64(100)))
			Expect(indexEntries[1].count).To(Equal(int64(3)))
			Expect(indexEntries[2].priceKey).To(Equal(int64(150)))
			Expect(indexEntries[2].count).To(Equal(int64(1)))
		})

		It("Go scans Java-built SUM index using cross-language metadata", func() {
			// Reverse direction of spec #8: Java writes metadata + records;
			// Go LOADS, scans the ungrouped SUM. Pins SUM-index Java→Go
			// symmetry — atomic-ADD payload is decoded the same way going
			// the other direction.
			params := map[string]any{
				"clusterFile":   clusterFile,
				"mdSubspace":    BytesToIntArray(ss.Bytes()),
				"storeSubspace": BytesToIntArray(storeSS.Bytes()),
				"orderIds":      []int64{1, 2, 3, 4},
				"prices":        []int64{100, 50, 150, 200},
				"version":       53,
			}
			var saveResult struct {
				MetadataBytes int `json:"metadataBytes"`
				RecordsSaved  int `json:"recordsSaved"`
			}
			err := java.InvokeAs(ctx, "saveMetaDataWithSumIndexAndOrdersJava", params, &saveResult)
			Expect(err).NotTo(HaveOccurred())
			Expect(saveResult.RecordsSaved).To(Equal(4))

			var sum int64
			var entryCount int
			_, err = goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				mdStore := recordlayer.NewFDBMetaDataStore(ss)
				loaded, loadErr := mdStore.LoadRecordMetaDataProto(rtx.Transaction())
				if loadErr != nil {
					return nil, loadErr
				}
				if loaded == nil {
					return nil, fmt.Errorf("Go: no metadata at subspace")
				}
				md, buildErr := recordlayer.RecordMetaDataFromProto(loaded)
				if buildErr != nil {
					return nil, buildErr
				}
				store, openErr := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(storeSS).Open()
				if openErr != nil {
					return nil, openErr
				}
				idx := md.GetIndex("Order$total_price")
				cursor := store.ScanIndex(idx, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan())
				entries, scanErr := recordlayer.AsList(ctx, cursor)
				if scanErr != nil {
					return nil, scanErr
				}
				entryCount = len(entries)
				if entryCount > 0 {
					if len(entries[0].Value) == 0 {
						return nil, fmt.Errorf("expected non-empty SUM-index value tuple (atomic counter missing)")
					}
					v, ok := entries[0].Value[0].(int64)
					if !ok {
						return nil, fmt.Errorf("expected int64 in SUM-index value, got %T (%v)", entries[0].Value[0], entries[0].Value[0])
					}
					sum = v
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(entryCount).To(Equal(1), "ungrouped SUM has exactly one entry")
			Expect(sum).To(Equal(int64(500)))
		})

		It("Java reads Go-written split records (>100KB) using cross-language metadata", func() {
			// Stricter still: split-long-records is a wire-format feature
			// — records >100KB are split across keys with suffixes 1, 2,
			// 3 etc. (vs the unsplit suffix 0). The metadata flag
			// `splitLongRecords` flips this behaviour. This test pins
			// that:
			//   - Go-saved metadata's `splitLongRecords=true` flag survives
			//     the proto wire format;
			//   - the loaded RecordMetaData on Java's side respects that
			//     flag when decoding records;
			//   - split-record reassembly works cross-engine.
			// We craft a >100KB Order via the `tags` repeated-string field.
			largeTag := make([]byte, 1024)
			for i := range largeTag {
				largeTag[i] = byte('A' + (i % 26))
			}
			tags := make([]string, 130) // 130 * 1KB ≈ 130KB > 100KB
			for i := range tags {
				tags[i] = string(largeTag)
			}
			bigOrder := &gen.Order{
				OrderId: proto.Int64(7),
				Price:   proto.Int32(999),
				Tags:    tags,
			}

			_, err := goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				builder := recordlayer.NewRecordMetaDataBuilder().
					SetRecords(gen.File_record_layer_demo_proto).
					SetSplitLongRecords(true)
				builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
				builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
				builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
				built, buildErr := builder.Build()
				if buildErr != nil {
					return nil, buildErr
				}
				mdProto, protoErr := built.ToProto()
				if protoErr != nil {
					return nil, protoErr
				}
				mdProto.Version = proto.Int32(23)

				mdStore := recordlayer.NewFDBMetaDataStore(ss)
				if saveErr := mdStore.SaveRecordMetaData(rtx.Transaction(), mdProto); saveErr != nil {
					return nil, saveErr
				}
				// Re-derive the runtime metadata from the persisted proto
				// (consistent with the records-only test): the store's
				// schema is now byte-identical to what crossed the wire.
				md, fromProtoErr := recordlayer.RecordMetaDataFromProto(mdProto)
				if fromProtoErr != nil {
					return nil, fromProtoErr
				}
				store, openErr := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(storeSS).CreateOrOpen()
				if openErr != nil {
					return nil, openErr
				}
				_, saveErr := store.SaveRecord(bigOrder)
				return nil, saveErr
			})
			Expect(err).NotTo(HaveOccurred())

			params := map[string]any{
				"clusterFile":   clusterFile,
				"mdSubspace":    BytesToIntArray(ss.Bytes()),
				"storeSubspace": BytesToIntArray(storeSS.Bytes()),
			}
			var result struct {
				Found           bool `json:"found"`
				MetadataVersion int  `json:"metadataVersion"`
				Rows            []struct {
					OrderId int64 `json:"orderId"`
					Price   int64 `json:"price"`
				} `json:"rows"`
			}
			err = java.InvokeAs(ctx, "loadMetaDataAndScanOrdersJava", params, &result)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Found).To(BeTrue())
			Expect(result.MetadataVersion).To(Equal(23))
			Expect(result.Rows).To(HaveLen(1), "expected exactly one Order; split-record reassembly should yield a single record")
			Expect(result.Rows[0].OrderId).To(Equal(int64(7)))
			Expect(result.Rows[0].Price).To(Equal(int64(999)))
		})

		It("Go loads Java-written metadata and scans Java-written records", func() {
			// Direction: Java saves both metadata and Orders; Go LOADS the
			// metadata fresh and uses it to scan.
			params := map[string]any{
				"clusterFile":   clusterFile,
				"mdSubspace":    BytesToIntArray(ss.Bytes()),
				"storeSubspace": BytesToIntArray(storeSS.Bytes()),
				// JSON sends ints as doubles — pass through []int64 so
				// Java's Long unmarshal works.
				"orderIds": []int64{10, 20, 30, 40},
				"prices":   []int64{100, 200, 300, 400},
				"version":  11,
			}
			var saveResult struct {
				MetadataBytes int `json:"metadataBytes"`
				RecordsSaved  int `json:"recordsSaved"`
			}
			err := java.InvokeAs(ctx, "saveMetaDataAndOrdersJava", params, &saveResult)
			Expect(err).NotTo(HaveOccurred())
			Expect(saveResult.RecordsSaved).To(Equal(4))
			Expect(saveResult.MetadataBytes).To(BeNumerically(">", 0))

			// Go loads metadata, opens store, scans Orders.
			var loadedMD *gen.MetaData
			var scannedOrders []*gen.Order
			_, err = goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				mdStore := recordlayer.NewFDBMetaDataStore(ss)
				var loadErr error
				loadedMD, loadErr = mdStore.LoadRecordMetaDataProto(rtx.Transaction())
				if loadErr != nil {
					return nil, loadErr
				}
				if loadedMD == nil {
					return nil, fmt.Errorf("Go: no metadata at subspace")
				}
				// Build a RecordMetaData from the LOADED proto (this is
				// the cross-language test's whole point — using the
				// metadata Java wrote to drive Go's record store).
				md, buildErr := recordlayer.RecordMetaDataFromProto(loadedMD)
				if buildErr != nil {
					return nil, buildErr
				}
				store, openErr := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(storeSS).Open()
				if openErr != nil {
					return nil, openErr
				}
				cursor := store.ScanRecords(nil, recordlayer.ForwardScan())
				records, scanErr := recordlayer.AsList(ctx, cursor)
				if scanErr != nil {
					return nil, scanErr
				}
				for _, rec := range records {
					if order, ok := rec.Record.(*gen.Order); ok {
						scannedOrders = append(scannedOrders, order)
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(loadedMD).NotTo(BeNil())
			Expect(loadedMD.GetVersion()).To(Equal(int32(11)))
			Expect(scannedOrders).To(HaveLen(4))
			// PK-ordered scan: 10, 20, 30, 40.
			Expect(scannedOrders[0].GetOrderId()).To(Equal(int64(10)))
			Expect(scannedOrders[0].GetPrice()).To(Equal(int32(100)))
			Expect(scannedOrders[3].GetOrderId()).To(Equal(int64(40)))
			Expect(scannedOrders[3].GetPrice()).To(Equal(int32(400)))
		})
	})

	Describe("History cross-language", func() {
		It("Java can read historical version stored by Go", func() {
			// Go saves v1, then v2 (archives v1)
			_, err := goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store := recordlayer.NewFDBMetaDataStore(ss)
				return nil, store.SaveRecordMetaData(rtx.Transaction(), buildMetaDataProto(1))
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store := recordlayer.NewFDBMetaDataStore(ss)
				return nil, store.SaveRecordMetaData(rtx.Transaction(), buildMetaDataProto(2))
			})
			Expect(err).NotTo(HaveOccurred())

			// Java reads historical v1
			params := map[string]any{
				"clusterFile": clusterFile,
				"subspace":    BytesToIntArray(ss.Bytes()),
				"version":     1,
			}
			var result struct {
				Found   bool `json:"found"`
				Version int  `json:"version"`
			}
			err = java.InvokeAs(ctx, "loadMetaDataHistoryJava", params, &result)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Found).To(BeTrue())
			Expect(result.Version).To(Equal(1))
		})
	})
})
