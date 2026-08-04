//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/gen"
	gofdb "fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/embedded"
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

	// RFC-209 §4.1 — the four interop caveats of the auto-emitted
	// group-existence companion, EXECUTED against the live Java engine rather
	// than argued from the stored bytes.
	//
	// The structural check (index_ddl_metadata_conformance_test.go) compares
	// metadata Java built for itself against metadata Go built for itself. Java
	// never opens Go's, so it never runs a line of Java against the companion.
	// Here the ONLY metadata in play is the one Go persisted: Java loads it
	// through the FDBMetaDataStore path, opens a store on it, scans the
	// companion, and then WRITES and DELETES through it.
	//
	// The write/delete half is the load-bearing part. A Java engine that
	// happily opened a store carrying an index it never declared but did not
	// MAINTAIN it would pass every read-only assertion while leaving a Go
	// reader merging against a group set frozen at whatever Go last wrote —
	// exactly the silent wrong-answer this companion exists to prevent.
	Describe("RFC-209 group-existence companion cross-language", func() {
		It("Java loads Go's stored metadata, scans the companion, and maintains it", func() {
			// A grouped SUM with no user-declared COUNT(*) over the same
			// grouping key: create-if-absent therefore MUST emit a companion,
			// and it lands in the persisted bytes.
			body := `CREATE TABLE T (id BIGINT, g STRING, v BIGINT, PRIMARY KEY(id)) ` +
				`CREATE INDEX i_sum AS SELECT SUM(v) FROM T GROUP BY g`
			tmpl, buildErr := embedded.BuildSchemaTemplateFromDDL(body)
			Expect(buildErr).NotTo(HaveOccurred())
			mdProto, protoErr := tmpl.Underlying().ToProto()
			Expect(protoErr).NotTo(HaveOccurred())

			const ownerName = "I_SUM"
			companionName := recordlayer.GroupCountCompanionName(ownerName)
			storedNames := make([]string, 0, len(mdProto.GetIndexes()))
			for _, idx := range mdProto.GetIndexes() {
				storedNames = append(storedNames, idx.GetName())
			}
			Expect(storedNames).To(ContainElement(companionName),
				"the companion must be in the PERSISTED metadata — Java can only see it "+
					"by loading the stored template, so an unpersisted companion makes this "+
					"whole interop claim vacuous (stored: %v)", storedNames)

			// Go's own store is opened from the SAME proto it persists, so
			// nothing in this test reads a companion that only exists in
			// memory.
			md, fromProtoErr := recordlayer.RecordMetaDataFromProto(mdProto)
			Expect(fromProtoErr).NotTo(HaveOccurred())
			desc := md.GetRecordType("T").Descriptor
			rec := func(id int64, g string, v int64) proto.Message {
				m := dynamicpb.NewMessage(desc)
				m.Set(desc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
				m.Set(desc.Fields().ByName("G"), protoreflect.ValueOfString(g))
				m.Set(desc.Fields().ByName("V"), protoreflect.ValueOfInt64(v))
				return m
			}

			_, err := goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				mdStore := recordlayer.NewFDBMetaDataStore(ss)
				if saveErr := mdStore.SaveRecordMetaData(rtx.Transaction(), mdProto); saveErr != nil {
					return nil, saveErr
				}
				store, openErr := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(storeSS).CreateOrOpen()
				if openErr != nil {
					return nil, openErr
				}
				// Group "a" has two rows, group "b" exactly one — "b" is the
				// group Java will vacate.
				for _, r := range []proto.Message{
					rec(1, "a", 10), rec(2, "a", 20), rec(3, "b", 7),
				} {
					if _, saveErr := store.SaveRecord(r); saveErr != nil {
						return nil, saveErr
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// goGroups reads a grouped atomic-mutation index — one long per
			// group, which is what Java's step reads BY_GROUP. Go reaches those
			// entries through the maintainer's plain scan: ScanIndexByType with
			// IndexScanByGroup is reserved for PERMUTED_MIN_MAX and
			// BITMAP_VALUE, and rejects a "count" index outright
			// (index_scan.go:346-357), so the equivalent Go call is ScanIndex.
			// The two sides' numbers are comparable without either side
			// interpreting the other's.
			goGroups := func(indexName string) map[string]int64 {
				out := map[string]int64{}
				_, scanErr := goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
					store, openErr := recordlayer.NewStoreBuilder().
						SetContext(rtx).SetMetaDataProvider(md).SetSubspace(storeSS).Open()
					if openErr != nil {
						return nil, openErr
					}
					idx := md.GetIndex(indexName)
					if idx == nil {
						return nil, fmt.Errorf("index %s not in metadata", indexName)
					}
					entries, listErr := recordlayer.AsList(ctx, store.ScanIndex(
						idx, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()))
					if listErr != nil {
						return nil, listErr
					}
					for _, e := range entries {
						g, ok := e.Key[0].(string)
						if !ok {
							return nil, fmt.Errorf("group key %v is %T, want string", e.Key[0], e.Key[0])
						}
						v, ok := e.Value[0].(int64)
						if !ok {
							return nil, fmt.Errorf("group value %v is %T, want int64", e.Value[0], e.Value[0])
						}
						out[g] = v
					}
					return nil, nil
				})
				Expect(scanErr).NotTo(HaveOccurred())
				return out
			}

			// Java goes FIRST, deliberately. Every caveat in §4.1 is a claim
			// about what the JAVA engine does with metadata it did not write, so
			// a companion Go emitted wrong must be reported as Java refusing it,
			// not as Go's own scan coming back odd — otherwise the failure names
			// the wrong engine and the caveat it fired is guesswork.
			//
			// Caveats 1-3, executed: Java loads the STORED metadata (not a
			// locally compiled RecordMetaData), which forces every index in the
			// proto through RecordMetaData.build — the version fields must
			// satisfy 0 < added <= lastModified <= metadata.version, and the
			// companion, being a grouped COUNT, must carry groupedCount == 0 or
			// AtomicMutationIndexMaintainerFactory refuses to build a
			// maintainer for it and the store never opens.
			scanParams := map[string]any{
				"clusterFile":   clusterFile,
				"mdSubspace":    BytesToIntArray(ss.Bytes()),
				"storeSubspace": BytesToIntArray(storeSS.Bytes()),
				"indexName":     companionName,
			}
			var scanResult struct {
				Found           bool `json:"found"`
				MetadataVersion int  `json:"metadataVersion"`
				Rows            []struct {
					Key   []any `json:"key"`
					Count int64 `json:"count"`
				} `json:"rows"`
			}
			err = java.InvokeAs(ctx, "loadMetaDataAndScanCountIndexJava", scanParams, &scanResult)
			Expect(err).NotTo(HaveOccurred(),
				"Java must OPEN a store on Go's stored metadata and scan the auto-emitted "+
					"companion; a failure here is one of RFC-209 §4.1's caveats 1-3 firing")
			Expect(scanResult.Found).To(BeTrue())
			Expect(scanResult.MetadataVersion).To(Equal(int(mdProto.GetVersion())))
			javaBefore := map[string]int64{}
			for _, r := range scanResult.Rows {
				javaBefore[fmt.Sprint(r.Key[0])] = r.Count
			}
			Expect(javaBefore).To(Equal(map[string]int64{"a": 2, "b": 1}),
				"Java's reading of the companion must equal Go's")
			Expect(goGroups(companionName)).To(Equal(javaBefore),
				"Go must read the same companion entries Java just read")
			Expect(goGroups(ownerName)).To(Equal(map[string]int64{"a": 30, "b": 7}))

			// The maintenance half. Java inserts into an EXISTING group (a:
			// 2→3), inserts into a group that does not exist yet (c: absent→1),
			// and deletes the only row of group b (1→0, vacating it).
			mutateParams := map[string]any{
				"clusterFile":    clusterFile,
				"mdSubspace":     BytesToIntArray(ss.Bytes()),
				"storeSubspace":  BytesToIntArray(storeSS.Bytes()),
				"recordTypeName": "T",
				"countIndexName": companionName,
				"sumIndexName":   ownerName,
				"pkFieldName":    "ID",
				"insertsJson":    `[{"ID":10,"G":"a","V":5},{"ID":11,"G":"c","V":7}]`,
				"deletePkJson":   `[3]`,
			}
			var mutateResult struct {
				Found     bool `json:"found"`
				Inserted  int  `json:"inserted"`
				Deleted   int  `json:"deleted"`
				CountRows []struct {
					Key   []any `json:"key"`
					Value int64 `json:"value"`
				} `json:"countRows"`
				SumRows []struct {
					Key   []any `json:"key"`
					Value int64 `json:"value"`
				} `json:"sumRows"`
			}
			err = java.InvokeAs(ctx, "mutateAndScanGroupCountIndexJava", mutateParams, &mutateResult)
			Expect(err).NotTo(HaveOccurred())
			Expect(mutateResult.Found).To(BeTrue())
			Expect(mutateResult.Inserted).To(Equal(2))
			Expect(mutateResult.Deleted).To(Equal(1),
				"Java must actually have deleted the row — a no-op delete would make the "+
					"vacated-group assertion below pass for the wrong reason")

			javaCounts := map[string]int64{}
			for _, r := range mutateResult.CountRows {
				javaCounts[fmt.Sprint(r.Key[0])] = r.Value
			}
			javaSums := map[string]int64{}
			for _, r := range mutateResult.SumRows {
				javaSums[fmt.Sprint(r.Key[0])] = r.Value
			}

			// Caveat 4, measured rather than assumed: neither side sets
			// clearWhenZero, so Java's atomic-mutation maintainer decrements
			// group b's counter to 0 and LEAVES THE KEY. The zero entry is
			// therefore what both engines see at the index, and dropping it is
			// the READ side's job — Go's aggregate-index cursor does it via
			// liveGroupsOnly, so Go's write path never depends on Java clearing
			// anything. If Java ever started clearing zeroed groups, this
			// expectation is where it surfaces.
			Expect(javaCounts).To(Equal(map[string]int64{"a": 3, "b": 0, "c": 1}),
				"Java did not MAINTAIN the companion across its own write/delete: "+
					"a must have been incremented, c must have appeared, b must have been "+
					"decremented to a vacated 0")
			Expect(javaSums).To(Equal(map[string]int64{"a": 35, "b": 0, "c": 7}),
				"Java did not maintain the OWNING SUM index the same way")

			// The round trip: Go re-reads the very entries Java's maintainer
			// wrote. Agreement here is what makes this an interop test rather
			// than a Java smoke test — Java's numbers could be self-consistent
			// and still encoded so Go cannot read them.
			Expect(goGroups(companionName)).To(Equal(javaCounts),
				"Go and Java disagree about the companion's contents after Java's mutations")
			Expect(goGroups(ownerName)).To(Equal(javaSums),
				"Go and Java disagree about the owning SUM index after Java's mutations")
		})

		// The spec above proves Java can READ and MAINTAIN a store whose
		// metadata carries the companion. It says nothing about a Java
		// application EVOLVING that schema, and evolution is a different code
		// path: FDBMetaDataStore's read entry points build with validate=false
		// (FDBMetaDataStore.java:252, :279, :366), while every mutating entry
		// point funnels into saveAndSetCurrent, which builds the new proto with
		// validate=true (FDBMetaDataStore.java:376) AND re-builds the OLD,
		// Go-written proto with validate=true before running the evolution
		// validator (FDBMetaDataStore.java:394-396).
		//
		// If the companion did not survive that, a Go-written schema would be
		// a poison pill: readable by a Java app forever, evolvable never. So it
		// gets executed, not argued.
		It("Java evolves Go's stored metadata through the validating FDBMetaDataStore path", func() {
			body := `CREATE TABLE T (id BIGINT, g STRING, v BIGINT, PRIMARY KEY(id)) ` +
				`CREATE INDEX i_sum AS SELECT SUM(v) FROM T GROUP BY g`
			tmpl, buildErr := embedded.BuildSchemaTemplateFromDDL(body)
			Expect(buildErr).NotTo(HaveOccurred())
			mdProto, protoErr := tmpl.Underlying().ToProto()
			Expect(protoErr).NotTo(HaveOccurred())

			const ownerName = "I_SUM"
			companionName := recordlayer.GroupCountCompanionName(ownerName)
			md, fromProtoErr := recordlayer.RecordMetaDataFromProto(mdProto)
			Expect(fromProtoErr).NotTo(HaveOccurred())
			desc := md.GetRecordType("T").Descriptor
			rec := func(id int64, g string, v int64) proto.Message {
				m := dynamicpb.NewMessage(desc)
				m.Set(desc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
				m.Set(desc.Fields().ByName("G"), protoreflect.ValueOfString(g))
				m.Set(desc.Fields().ByName("V"), protoreflect.ValueOfInt64(v))
				return m
			}

			_, err := goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				mdStore := recordlayer.NewFDBMetaDataStore(ss)
				if saveErr := mdStore.SaveRecordMetaData(rtx.Transaction(), mdProto); saveErr != nil {
					return nil, saveErr
				}
				store, openErr := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(storeSS).CreateOrOpen()
				if openErr != nil {
					return nil, openErr
				}
				for _, r := range []proto.Message{rec(1, "a", 10), rec(2, "a", 20), rec(3, "b", 7)} {
					if _, saveErr := store.SaveRecord(r); saveErr != nil {
						return nil, saveErr
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			type evolveOutcome struct {
				Ok           bool     `json:"ok"`
				Version      int      `json:"version"`
				IndexNames   []string `json:"indexNames"`
				ErrorClass   string   `json:"errorClass"`
				ErrorMessage string   `json:"errorMessage"`
				ErrorChain   string   `json:"errorChain"`
			}
			evolve := func(mdSS subspace.Subspace, newIndexName string) evolveOutcome {
				var out evolveOutcome
				invokeErr := java.InvokeAs(ctx, "evolveMetaDataViaFDBMetaDataStoreJava", map[string]any{
					"clusterFile":       clusterFile,
					"mdSubspace":        BytesToIntArray(mdSS.Bytes()),
					"recordTypeName":    "T",
					"newIndexName":      newIndexName,
					"newIndexFieldName": "V",
				}, &out)
				Expect(invokeErr).NotTo(HaveOccurred())
				return out
			}

			// A routine schema evolution: add an unrelated VALUE index. Nothing
			// about the companion is touched, so a refusal here can only come
			// from validation of what Go already wrote.
			evolved := evolve(ss, "J_V_IDX")
			Expect(evolved.Ok).To(BeTrue(),
				"Java refused to EVOLVE metadata it can read: %s", evolved.ErrorChain)
			Expect(evolved.IndexNames).To(ContainElement(companionName),
				"Java's evolution dropped the companion")
			Expect(evolved.IndexNames).To(ContainElement("J_V_IDX"))
			Expect(evolved.Version).To(BeNumerically(">", int(mdProto.GetVersion())),
				"saveAndSetCurrent demands a strictly increasing version")

			// Go re-opens what Java wrote. Java's evolution being self-consistent
			// is not enough — the point of the companion is that Go's read path
			// merges against it, so Go has to still find and scan it.
			var reloaded *gen.MetaData
			_, err = goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				var loadErr error
				reloaded, loadErr = recordlayer.NewFDBMetaDataStore(ss).
					LoadRecordMetaDataProto(rtx.Transaction())
				return nil, loadErr
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(reloaded).NotTo(BeNil())
			reloadedNames := make([]string, 0, len(reloaded.GetIndexes()))
			for _, idx := range reloaded.GetIndexes() {
				reloadedNames = append(reloadedNames, idx.GetName())
			}
			Expect(reloadedNames).To(ContainElement(companionName),
				"the companion is gone from the metadata Java evolved and stored (have: %v)",
				reloadedNames)
			Expect(reloadedNames).To(ContainElement("J_V_IDX"))

			evolvedMD, evolvedErr := recordlayer.RecordMetaDataFromProto(reloaded)
			Expect(evolvedErr).NotTo(HaveOccurred())
			groups := map[string]int64{}
			_, err = goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, openErr := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(evolvedMD).SetSubspace(storeSS).CreateOrOpen()
				if openErr != nil {
					return nil, openErr
				}
				entries, listErr := recordlayer.AsList(ctx, store.ScanIndex(
					evolvedMD.GetIndex(companionName), recordlayer.TupleRangeAll, nil,
					recordlayer.ForwardScan()))
				if listErr != nil {
					return nil, listErr
				}
				for _, e := range entries {
					groups[e.Key[0].(string)] = e.Value[0].(int64)
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(groups).To(Equal(map[string]int64{"a": 2, "b": 1}),
				"the companion no longer scans after Java's evolution")

			// The control. The validating path DOES reject atomic-mutation
			// indexes whose type and grouping disagree, and the two ways it can
			// disagree produce two DIFFERENT messages — so a bare recollection
			// of "Java refused companion-shaped metadata" is not evidence about
			// the companion unless the corrupted shapes are reproduced and read
			// side by side with the clean run above.
			//
			// The corrupted copies are written with SplitHelper directly, so
			// neither engine validates them on the way in; only the evolution
			// path Java runs afterwards does.
			corrupt := func(child string, indexName, newType string) subspace.Subspace {
				dst := ss.Sub(child)
				var copied struct {
					Found bool `json:"found"`
				}
				copyErr := java.InvokeAs(ctx, "copyMetaDataWithIndexTypeJava", map[string]any{
					"clusterFile":  clusterFile,
					"srcSubspace":  BytesToIntArray(ss.Bytes()),
					"dstSubspace":  BytesToIntArray(dst.Bytes()),
					"indexName":    indexName,
					"newIndexType": newType,
				}, &copied)
				Expect(copyErr).NotTo(HaveOccurred())
				Expect(copied.Found).To(BeTrue())
				return dst
			}

			// The companion's own expression retyped as SUM. Java rejects it,
			// but NOT for the reason the type/grouping mismatch suggests: the
			// per-record-type hook runs first (IndexValidator.java:51-52 calls
			// metaDataValidator.validateIndexForRecordTypes before any grouping
			// check), and SUM demands a long-valued last column
			// (AtomicMutationIndexMaintainerFactory.java:131-150). The
			// companion's only column is the STRING grouping column G, so the
			// integer check fires before validateGrouping(1) ever sees the zero
			// grouped fields. Pinned as the engine actually words it — a
			// guessed message here would defeat the point of the control.
			sumOverGroupAll := evolve(corrupt("corruptCompanionSum", companionName, "sum"), "J_V_IDX_A")
			Expect(sumOverGroupAll.Ok).To(BeFalse(),
				"a SUM over the companion's expression must not validate")
			Expect(sumOverGroupAll.ErrorClass).
				To(Equal("com.apple.foundationdb.record.metadata.expressions.KeyExpression$InvalidExpressionException"))
			Expect(sumOverGroupAll.ErrorMessage).To(Equal("index type only supports integer field"))
			Expect(sumOverGroupAll.ErrorMessage).NotTo(ContainSubstring("non-group fields"),
				"this corruption is NOT the origin of the non-group-fields exception")

			// The owning SUM's expression retyped as COUNT. COUNT stores no
			// value, so validateGrouping(0) passes and the ONE grouped column
			// trips the other throw —
			// AtomicMutationIndexMaintainerFactory.java:99-104. This is the
			// shape that produces "does not support non-group fields", and it is
			// reachable only by a type that disagrees with its expression, never
			// by the companion as emitted.
			countOverGrouped := evolve(corrupt("corruptOwnerCount", ownerName, "count"), "J_V_IDX_B")
			Expect(countOverGrouped.Ok).To(BeFalse(),
				"a COUNT over an expression with grouped fields must not validate")
			Expect(countOverGrouped.ErrorClass).
				To(Equal("com.apple.foundationdb.record.metadata.expressions.KeyExpression$InvalidExpressionException"))
			Expect(countOverGrouped.ErrorMessage).
				To(Equal("index type does not support non-group fields; use COUNT_NOT_NULL"))
		})
	})
})
