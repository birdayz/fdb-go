package com.birdayz.conformance;

import com.apple.foundationdb.record.IndexEntry;
import com.apple.foundationdb.record.IndexScanType;
import com.apple.foundationdb.record.RecordMetaData;
import com.apple.foundationdb.record.RecordMetaDataBuilder;
import com.apple.foundationdb.record.RecordMetaDataOptionsProto;
import com.apple.foundationdb.record.RecordMetaDataProto;
import com.apple.foundationdb.record.ScanProperties;
import com.apple.foundationdb.record.TupleRange;
import com.apple.foundationdb.record.metadata.Index;
import com.apple.foundationdb.record.metadata.Key;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordContext;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.record.provider.foundationdb.FDBStoredRecord;
import com.apple.foundationdb.record.provider.foundationdb.SplitHelper;
import com.apple.foundationdb.record.RecordLayerDemo;
import com.apple.foundationdb.record.RecordLayerDemo.Order;
import com.apple.foundationdb.record.RecordCursor;
import com.apple.foundationdb.subspace.Subspace;
import com.apple.foundationdb.tuple.Tuple;

import com.google.protobuf.ExtensionRegistry;
import com.google.protobuf.InvalidProtocolBufferException;
import com.google.protobuf.Message;

import java.util.ArrayList;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

/**
 * Conformance steps for FDBMetaDataStore cross-language validation.
 * Uses non-tenant mode (null tenantName → db.run()) with unique subspace
 * prefixes for isolation. This avoids tenant-related issues with direct
 * SplitHelper calls.
 */
class MetaDataStoreSteps extends ConformanceBase {

    // Without this extension registry, parseFrom() drops the
    // [record].usage=UNION extension option on RecordLayerDemo's
    // UnionDescriptor message, and RecordMetaData.build() then can't
    // identify the union descriptor (the proto's union message is
    // named "UnionDescriptor", not Java's DEFAULT_UNION_NAME of
    // "RecordTypeUnion"). Result: "Union descriptor is required" at
    // RecordMetaDataBuilder.fetchUnionDescriptor.
    private static final ExtensionRegistry EXTENSION_REGISTRY;
    static {
        EXTENSION_REGISTRY = ExtensionRegistry.newInstance();
        RecordMetaDataOptionsProto.registerAllExtensions(EXTENSION_REGISTRY);
    }

    private static RecordMetaData createTestMetaData() {
        RecordMetaDataBuilder b = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        b.getRecordType("Order").setPrimaryKey(Key.Expressions.field("order_id"));
        b.getRecordType("Customer").setPrimaryKey(Key.Expressions.field("customer_id"));
        b.getRecordType("TypedRecord").setPrimaryKey(Key.Expressions.field("id"));
        return b.build();
    }

    /**
     * Save metadata proto using Java's SplitHelper (matching FDBMetaDataStore wire format).
     * Uses non-tenant mode for reliable cross-language testing.
     */
    @ConformanceStep("saveMetaDataJava")
    public Map<String, Object> saveMetaDataJava(String clusterFile, byte[] subspace, int version) {
        return runInContext(clusterFile, null, context -> {
            Subspace ss = new Subspace(subspace);
            Tuple currentKey = Tuple.from((Object) null);
            RecordMetaDataProto.MetaData.Builder proto = createTestMetaData().toProto().toBuilder();
            proto.setVersion(version);
            byte[] serialized = proto.build().toByteArray();
            SplitHelper.saveWithSplit(context, ss, currentKey, serialized, null);
            Map<String, Object> result = new HashMap<>();
            result.put("savedBytes", serialized.length);
            return result;
        });
    }

    /**
     * Load metadata proto using raw FDB read at unsplit key and return version.
     * Reads at subspace.pack(null, 0) — the unsplit suffix key.
     */
    @ConformanceStep("loadMetaDataJava")
    public Map<String, Object> loadMetaDataJava(String clusterFile, byte[] subspace) {
        return runInContext(clusterFile, null, context -> {
            Subspace ss = new Subspace(subspace);
            // Read at unsplit key: subspace.pack(null, 0L)
            byte[] unsplitKey = ss.pack(Tuple.from((Object) null, 0L));
            byte[] data = context.ensureActive().get(unsplitKey).join();
            Map<String, Object> result = new HashMap<>();
            if (data != null && data.length > 0) {
                RecordMetaDataProto.MetaData proto;
                try {
                    proto = RecordMetaDataProto.MetaData.parseFrom(data, EXTENSION_REGISTRY);
                } catch (InvalidProtocolBufferException e) {
                    throw new RuntimeException("Failed to parse metadata proto", e);
                }
                result.put("version", proto.getVersion());
                result.put("found", true);
            } else {
                result.put("found", false);
            }
            return result;
        });
    }

    /**
     * Save historical metadata version using Java's SplitHelper.
     */
    @ConformanceStep("saveMetaDataHistoryJava")
    public void saveMetaDataHistoryJava(String clusterFile, byte[] subspace, int version) {
        runInContext(clusterFile, null, context -> {
            Subspace ss = new Subspace(subspace);
            Tuple historyKey = Tuple.from("H", (long) version);
            RecordMetaDataProto.MetaData.Builder proto = createTestMetaData().toProto().toBuilder();
            proto.setVersion(version);
            byte[] serialized = proto.build().toByteArray();
            SplitHelper.saveWithSplit(context, ss, historyKey, serialized, null);
            return null;
        });
    }

    /**
     * Save metadata at mdSubspace AND a list of Order records at storeSubspace,
     * exercising the full FDBMetaDataStore + FDBRecordStore wire format. Used
     * by Track A2 (catalog wire format Go↔Java round-trip): with this, a Go
     * test can have Java write a complete (metadata, records) pair, then read
     * it back through the Go side and assert byte-equality of both layers.
     *
     * orderIds and prices are parallel arrays — one Order per index.
     */
    @ConformanceStep("saveMetaDataAndOrdersJava")
    public Map<String, Object> saveMetaDataAndOrdersJava(String clusterFile,
                                                         byte[] mdSubspace,
                                                         byte[] storeSubspace,
                                                         List<Long> orderIds,
                                                         List<Long> prices,
                                                         int version) {
        return runInContext(clusterFile, null, context -> {
            // Step 1: save the metadata proto via FDBMetaDataStore wire format.
            Subspace mdSS = new Subspace(mdSubspace);
            Tuple currentKey = Tuple.from((Object) null);
            RecordMetaData metaData = createTestMetaData();
            RecordMetaDataProto.MetaData.Builder protoBuilder = metaData.toProto().toBuilder();
            protoBuilder.setVersion(version);
            byte[] serialized = protoBuilder.build().toByteArray();
            SplitHelper.saveWithSplit(context, mdSS, currentKey, serialized, null);

            // Step 2: open a record store at storeSubspace using the SAME
            // metadata, save each Order. ALWAYS_READABLE_CHECKER tolerates
            // store-state cache cold start.
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metaData)
                .setContext(context)
                .setSubspace(new Subspace(storeSubspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();
            int saved = 0;
            for (int i = 0; i < orderIds.size(); i++) {
                Order order = Order.newBuilder()
                    .setOrderId(orderIds.get(i))
                    .setPrice(Math.toIntExact(prices.get(i)))
                    .build();
                store.saveRecord(order);
                saved++;
            }

            Map<String, Object> result = new HashMap<>();
            result.put("metadataBytes", serialized.length);
            result.put("recordsSaved", saved);
            return result;
        });
    }

    /**
     * Load metadata via FDBMetaDataStore wire format from mdSubspace, build
     * a RecordMetaData, open the record store at storeSubspace using THAT
     * metadata (proving the cross-language metadata is functionally usable),
     * scan all Order records, and return them as a list of {orderId, price}
     * maps. The list is sorted by orderId for deterministic comparison.
     */
    @ConformanceStep("loadMetaDataAndScanOrdersJava")
    public Map<String, Object> loadMetaDataAndScanOrdersJava(String clusterFile,
                                                             byte[] mdSubspace,
                                                             byte[] storeSubspace) {
        return runInContext(clusterFile, null, context -> {
            // Step 1: read metadata bytes at the unsplit key. Same path
            // loadMetaDataJava uses.
            Subspace mdSS = new Subspace(mdSubspace);
            byte[] unsplitKey = mdSS.pack(Tuple.from((Object) null, 0L));
            byte[] data = context.ensureActive().get(unsplitKey).join();
            Map<String, Object> result = new HashMap<>();
            if (data == null || data.length == 0) {
                result.put("found", false);
                return result;
            }
            RecordMetaDataProto.MetaData proto;
            try {
                proto = RecordMetaDataProto.MetaData.parseFrom(data, EXTENSION_REGISTRY);
            } catch (InvalidProtocolBufferException e) {
                throw new RuntimeException("Failed to parse metadata proto", e);
            }
            RecordMetaData metaData = RecordMetaData.build(proto);

            // Step 2: open the record store using the LOADED metadata (not
            // a freshly-built one — this is the cross-language test's whole
            // point), scan Orders.
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metaData)
                .setContext(context)
                .setSubspace(new Subspace(storeSubspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();
            RecordCursor<FDBStoredRecord<Message>> cursor = store.scanRecords(
                TupleRange.ALL, null, ScanProperties.FORWARD_SCAN);

            List<Map<String, Object>> rows = new ArrayList<>();
            cursor.forEach(rec -> {
                // Java-loaded RecordMetaData uses DynamicMessage, not the
                // static Order class, so mergeFrom across descriptors
                // fails ("can only merge messages of the same type").
                // Round-trip via raw bytes — proto wire format is
                // shared, so re-parsing with the static descriptor is
                // safe.
                try {
                    Order order = Order.parseFrom(rec.getRecord().toByteArray());
                    Map<String, Object> row = new HashMap<>();
                    row.put("orderId", order.getOrderId());
                    row.put("price", (long) order.getPrice());
                    rows.add(row);
                } catch (InvalidProtocolBufferException e) {
                    throw new RuntimeException("Failed to parse Order: " + e.getMessage(), e);
                }
            }).join();
            // Defensive sort by orderId for deterministic Go-side
            // comparison. scanRecords is PK-ordered already and current
            // tests use positive ids, so this is a no-op today; the
            // explicit sort is a safety net for future tests that might
            // use negative ids (where tuple-component ordering would
            // surface ahead of strict numeric ordering).
            rows.sort((a, b) -> Long.compare((Long) a.get("orderId"), (Long) b.get("orderId")));
            result.put("found", true);
            result.put("metadataVersion", proto.getVersion());
            result.put("rows", rows);
            return result;
        });
    }

    /**
     * Load metadata, open store, scan a COUNT index BY_GROUP, return
     * per-group counts. Used by A2 to pin that COUNT-index wire format
     * (atomic-mutation maintainer, different from VALUE) round-trips
     * correctly across engines. Returns rows of {key: [groupKeyParts],
     * count: <long>}.
     */
    @ConformanceStep("loadMetaDataAndScanCountIndexJava")
    public Map<String, Object> loadMetaDataAndScanCountIndexJava(String clusterFile,
                                                                 byte[] mdSubspace,
                                                                 byte[] storeSubspace,
                                                                 String indexName) {
        return runInContext(clusterFile, null, context -> {
            Subspace mdSS = new Subspace(mdSubspace);
            byte[] unsplitKey = mdSS.pack(Tuple.from((Object) null, 0L));
            byte[] data = context.ensureActive().get(unsplitKey).join();
            Map<String, Object> result = new HashMap<>();
            if (data == null || data.length == 0) {
                result.put("found", false);
                return result;
            }
            RecordMetaDataProto.MetaData proto;
            try {
                proto = RecordMetaDataProto.MetaData.parseFrom(data, EXTENSION_REGISTRY);
            } catch (InvalidProtocolBufferException e) {
                throw new RuntimeException("Failed to parse metadata proto", e);
            }
            RecordMetaData metaData = RecordMetaData.build(proto);
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metaData)
                .setContext(context)
                .setSubspace(new Subspace(storeSubspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();

            Index index = metaData.getIndex(indexName);
            List<IndexEntry> entries = store.scanIndex(
                index, IndexScanType.BY_GROUP, TupleRange.ALL, null,
                ScanProperties.FORWARD_SCAN
            ).asList().join();

            List<Map<String, Object>> rows = new ArrayList<>();
            for (IndexEntry e : entries) {
                Map<String, Object> row = new HashMap<>();
                List<Object> keyValues = new ArrayList<>();
                for (Object o : e.getKey()) {
                    keyValues.add(o);
                }
                row.put("key", keyValues);
                // COUNT-index value is the atomic counter (a long stored
                // as the first tuple element).
                row.put("count", e.getValue().getLong(0));
                rows.add(row);
            }
            result.put("found", true);
            result.put("metadataVersion", proto.getVersion());
            result.put("rows", rows);
            return result;
        });
    }

    /**
     * Load metadata, open store, scan a MAX_EVER_LONG index BY_GROUP,
     * return the single ungrouped max entry. MAX_EVER tracks the
     * maximum value EVER seen at the index — a one-way atomic
     * (FDB BYTE_MAX), distinct from SUM (commutative ADD) and COUNT
     * (+1 atomic). Pins MAX_EVER wire format independently.
     */
    @ConformanceStep("loadMetaDataAndScanMaxEverIndexJava")
    public Map<String, Object> loadMetaDataAndScanMaxEverIndexJava(String clusterFile,
                                                                   byte[] mdSubspace,
                                                                   byte[] storeSubspace,
                                                                   String indexName) {
        return runInContext(clusterFile, null, context -> {
            Subspace mdSS = new Subspace(mdSubspace);
            byte[] unsplitKey = mdSS.pack(Tuple.from((Object) null, 0L));
            byte[] data = context.ensureActive().get(unsplitKey).join();
            Map<String, Object> result = new HashMap<>();
            if (data == null || data.length == 0) {
                result.put("found", false);
                return result;
            }
            RecordMetaDataProto.MetaData proto;
            try {
                proto = RecordMetaDataProto.MetaData.parseFrom(data, EXTENSION_REGISTRY);
            } catch (InvalidProtocolBufferException e) {
                throw new RuntimeException("Failed to parse metadata proto", e);
            }
            RecordMetaData metaData = RecordMetaData.build(proto);
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metaData)
                .setContext(context)
                .setSubspace(new Subspace(storeSubspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();

            Index index = metaData.getIndex(indexName);
            List<IndexEntry> entries = store.scanIndex(
                index, IndexScanType.BY_GROUP, TupleRange.ALL, null,
                ScanProperties.FORWARD_SCAN
            ).asList().join();

            long maxEver = 0;
            int entryCount = entries.size();
            if (entryCount > 0) {
                maxEver = entries.get(0).getValue().getLong(0);
            }
            result.put("found", true);
            result.put("metadataVersion", proto.getVersion());
            result.put("entryCount", entryCount);
            result.put("maxEver", maxEver);
            return result;
        });
    }

    /**
     * Multi-record-type variant: load metadata, scan ALL records (no
     * type filter), classify each by record-type tag and return per-type
     * row counts + summaries. Used to verify that the union descriptor
     * round-trips with non-trivial multi-type populations — the
     * Customers and Orders subspaces share the same record-store
     * subspace but use distinct record-type prefixes, and the loaded
     * RecordMetaData must dispatch each stored bytes blob to the right
     * type.
     */
    @ConformanceStep("loadMetaDataAndScanAllRecordsJava")
    public Map<String, Object> loadMetaDataAndScanAllRecordsJava(String clusterFile,
                                                                 byte[] mdSubspace,
                                                                 byte[] storeSubspace) {
        return runInContext(clusterFile, null, context -> {
            Subspace mdSS = new Subspace(mdSubspace);
            byte[] unsplitKey = mdSS.pack(Tuple.from((Object) null, 0L));
            byte[] data = context.ensureActive().get(unsplitKey).join();
            Map<String, Object> result = new HashMap<>();
            if (data == null || data.length == 0) {
                result.put("found", false);
                return result;
            }
            RecordMetaDataProto.MetaData proto;
            try {
                proto = RecordMetaDataProto.MetaData.parseFrom(data, EXTENSION_REGISTRY);
            } catch (InvalidProtocolBufferException e) {
                throw new RuntimeException("Failed to parse metadata proto", e);
            }
            RecordMetaData metaData = RecordMetaData.build(proto);
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metaData)
                .setContext(context)
                .setSubspace(new Subspace(storeSubspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();

            // Scan all records — no type filter. The store dispatches by
            // record-type tag from the loaded union descriptor.
            List<FDBStoredRecord<Message>> records = store.scanRecords(
                null, ScanProperties.FORWARD_SCAN).asList().join();

            int orderCount = 0;
            int customerCount = 0;
            for (FDBStoredRecord<Message> rec : records) {
                String typeName = rec.getRecordType().getName();
                if ("Order".equals(typeName)) {
                    orderCount++;
                } else if ("Customer".equals(typeName)) {
                    customerCount++;
                }
            }
            result.put("found", true);
            result.put("metadataVersion", proto.getVersion());
            result.put("totalRecords", records.size());
            result.put("orderCount", orderCount);
            result.put("customerCount", customerCount);
            return result;
        });
    }

    /**
     * Load metadata, open store, scan a SUM index BY_GROUP, return the
     * single ungrouped sum entry. Used by A2 to pin SUM-index wire
     * format (atomic-mutation maintainer, like COUNT but with the
     * summed value rather than a counter).
     */
    @ConformanceStep("loadMetaDataAndScanSumIndexJava")
    public Map<String, Object> loadMetaDataAndScanSumIndexJava(String clusterFile,
                                                               byte[] mdSubspace,
                                                               byte[] storeSubspace,
                                                               String indexName) {
        return runInContext(clusterFile, null, context -> {
            Subspace mdSS = new Subspace(mdSubspace);
            byte[] unsplitKey = mdSS.pack(Tuple.from((Object) null, 0L));
            byte[] data = context.ensureActive().get(unsplitKey).join();
            Map<String, Object> result = new HashMap<>();
            if (data == null || data.length == 0) {
                result.put("found", false);
                return result;
            }
            RecordMetaDataProto.MetaData proto;
            try {
                proto = RecordMetaDataProto.MetaData.parseFrom(data, EXTENSION_REGISTRY);
            } catch (InvalidProtocolBufferException e) {
                throw new RuntimeException("Failed to parse metadata proto", e);
            }
            RecordMetaData metaData = RecordMetaData.build(proto);
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metaData)
                .setContext(context)
                .setSubspace(new Subspace(storeSubspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();

            Index index = metaData.getIndex(indexName);
            List<IndexEntry> entries = store.scanIndex(
                index, IndexScanType.BY_GROUP, TupleRange.ALL, null,
                ScanProperties.FORWARD_SCAN
            ).asList().join();

            // Ungrouped SUM index: there's exactly one entry with the
            // total under an empty grouping key.
            long sum = 0;
            int entryCount = entries.size();
            if (entryCount > 0) {
                sum = entries.get(0).getValue().getLong(0);
            }
            result.put("found", true);
            result.put("metadataVersion", proto.getVersion());
            result.put("entryCount", entryCount);
            result.put("sum", sum);
            return result;
        });
    }

    /**
     * Save metadata WITH a COUNT index ("Order$count_by_price" grouped by
     * Order.price) at mdSubspace, plus Order records at storeSubspace.
     * Symmetric counterpart to spec #6 (Go-writes side); Go-side test
     * loads this metadata, scans the index, asserts per-group counts.
     */
    @ConformanceStep("saveMetaDataWithCountIndexAndOrdersJava")
    public Map<String, Object> saveMetaDataWithCountIndexAndOrdersJava(
            String clusterFile,
            byte[] mdSubspace,
            byte[] storeSubspace,
            List<Long> orderIds,
            List<Long> prices,
            int version) {
        return runInContext(clusterFile, null, context -> {
            RecordMetaDataBuilder builder = RecordMetaData.newBuilder()
                .setRecords(RecordLayerDemo.getDescriptor());
            builder.getRecordType("Order").setPrimaryKey(Key.Expressions.field("order_id"));
            builder.getRecordType("Customer").setPrimaryKey(Key.Expressions.field("customer_id"));
            builder.getRecordType("TypedRecord").setPrimaryKey(Key.Expressions.field("id"));
            // GroupingKeyExpression(field("price"), 0): whole key is
            // field("price"), grouped_count=0 → all of `price` is the
            // GROUPING part (none is the grouped/aggregated part).
            // Java's IndexTypes.COUNT requires the grouped part be
            // empty (count is implicitly +1 per row).
            builder.addIndex("Order", new Index("Order$count_by_price",
                new com.apple.foundationdb.record.metadata.expressions.GroupingKeyExpression(
                    Key.Expressions.field("price"), 0),
                "count"));
            RecordMetaData metaData = builder.build();

            Subspace mdSS = new Subspace(mdSubspace);
            RecordMetaDataProto.MetaData.Builder protoBuilder = metaData.toProto().toBuilder();
            protoBuilder.setVersion(version);
            byte[] serialized = protoBuilder.build().toByteArray();
            SplitHelper.saveWithSplit(context, mdSS, Tuple.from((Object) null), serialized, null);

            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metaData)
                .setContext(context)
                .setSubspace(new Subspace(storeSubspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();
            for (int i = 0; i < orderIds.size(); i++) {
                Order order = Order.newBuilder()
                    .setOrderId(orderIds.get(i))
                    .setPrice(Math.toIntExact(prices.get(i)))
                    .build();
                store.saveRecord(order);
            }
            Map<String, Object> result = new HashMap<>();
            result.put("metadataBytes", serialized.length);
            result.put("recordsSaved", orderIds.size());
            return result;
        });
    }

    /**
     * Save metadata WITH a SUM index ("Order$total_price", ungrouped, on
     * Order.price) at mdSubspace, plus Order records at storeSubspace.
     * Symmetric counterpart to spec #8 (Go-writes side); Go-side test
     * loads this metadata, scans the index, asserts the total.
     */
    @ConformanceStep("saveMetaDataWithSumIndexAndOrdersJava")
    public Map<String, Object> saveMetaDataWithSumIndexAndOrdersJava(
            String clusterFile,
            byte[] mdSubspace,
            byte[] storeSubspace,
            List<Long> orderIds,
            List<Long> prices,
            int version) {
        return runInContext(clusterFile, null, context -> {
            RecordMetaDataBuilder builder = RecordMetaData.newBuilder()
                .setRecords(RecordLayerDemo.getDescriptor());
            builder.getRecordType("Order").setPrimaryKey(Key.Expressions.field("order_id"));
            builder.getRecordType("Customer").setPrimaryKey(Key.Expressions.field("customer_id"));
            builder.getRecordType("TypedRecord").setPrimaryKey(Key.Expressions.field("id"));
            // GroupingKeyExpression(field("price"), 1): whole key is
            // field("price"), grouped_count=1 → all 1 column is the
            // GROUPED/aggregated part, none is the grouping part. So
            // every row contributes its price to a single ungrouped sum.
            // Matches sum_index_conformance.java's idiom.
            builder.addIndex("Order", new Index("Order$total_price",
                new com.apple.foundationdb.record.metadata.expressions.GroupingKeyExpression(
                    Key.Expressions.field("price"), 1),
                "sum"));
            RecordMetaData metaData = builder.build();

            Subspace mdSS = new Subspace(mdSubspace);
            RecordMetaDataProto.MetaData.Builder protoBuilder = metaData.toProto().toBuilder();
            protoBuilder.setVersion(version);
            byte[] serialized = protoBuilder.build().toByteArray();
            SplitHelper.saveWithSplit(context, mdSS, Tuple.from((Object) null), serialized, null);

            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metaData)
                .setContext(context)
                .setSubspace(new Subspace(storeSubspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();
            for (int i = 0; i < orderIds.size(); i++) {
                Order order = Order.newBuilder()
                    .setOrderId(orderIds.get(i))
                    .setPrice(Math.toIntExact(prices.get(i)))
                    .build();
                store.saveRecord(order);
            }
            Map<String, Object> result = new HashMap<>();
            result.put("metadataBytes", serialized.length);
            result.put("recordsSaved", orderIds.size());
            return result;
        });
    }

    /**
     * Save metadata WITH a VALUE index ("Order$price" on Order.price)
     * at mdSubspace, plus Order records at storeSubspace. The index is
     * built as records are saved. Used by Track A2 reverse direction:
     * Go can then load the metadata and scan the index, verifying that
     * Java-written index entries are readable via Go's loaded
     * metadata.
     */
    @ConformanceStep("saveMetaDataWithIndexAndOrdersJava")
    public Map<String, Object> saveMetaDataWithIndexAndOrdersJava(
            String clusterFile,
            byte[] mdSubspace,
            byte[] storeSubspace,
            List<Long> orderIds,
            List<Long> prices,
            int version) {
        return runInContext(clusterFile, null, context -> {
            // Build metadata with the VALUE index.
            RecordMetaDataBuilder builder = RecordMetaData.newBuilder()
                .setRecords(RecordLayerDemo.getDescriptor());
            builder.getRecordType("Order").setPrimaryKey(Key.Expressions.field("order_id"));
            builder.getRecordType("Customer").setPrimaryKey(Key.Expressions.field("customer_id"));
            builder.getRecordType("TypedRecord").setPrimaryKey(Key.Expressions.field("id"));
            builder.addIndex("Order", new Index("Order$price", Key.Expressions.field("price")));
            RecordMetaData metaData = builder.build();

            // Persist metadata.
            Subspace mdSS = new Subspace(mdSubspace);
            RecordMetaDataProto.MetaData.Builder protoBuilder = metaData.toProto().toBuilder();
            protoBuilder.setVersion(version);
            byte[] serialized = protoBuilder.build().toByteArray();
            SplitHelper.saveWithSplit(context, mdSS, Tuple.from((Object) null), serialized, null);

            // Save records (index built as records are saved).
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metaData)
                .setContext(context)
                .setSubspace(new Subspace(storeSubspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();
            for (int i = 0; i < orderIds.size(); i++) {
                Order order = Order.newBuilder()
                    .setOrderId(orderIds.get(i))
                    .setPrice(Math.toIntExact(prices.get(i)))
                    .build();
                store.saveRecord(order);
            }
            Map<String, Object> result = new HashMap<>();
            result.put("metadataBytes", serialized.length);
            result.put("recordsSaved", orderIds.size());
            return result;
        });
    }

    /**
     * Load metadata at mdSubspace, open record store at storeSubspace using
     * the LOADED metadata, scan the named index by VALUE, return entries
     * as a list of {indexKey, primaryKey} maps.
     *
     * Used by Track A2: proves that index entries Go wrote at storeSubspace
     * (using metadata that was then saved at mdSubspace) are readable when
     * Java loads the same metadata fresh and points its store at the same
     * record subspace. Catches any divergence in:
     *   - record-layer index subspace layout
     *   - index entry tuple shape
     *   - VALUE-index key encoding
     */
    @ConformanceStep("loadMetaDataAndScanIndexJava")
    public Map<String, Object> loadMetaDataAndScanIndexJava(String clusterFile,
                                                            byte[] mdSubspace,
                                                            byte[] storeSubspace,
                                                            String indexName) {
        return runInContext(clusterFile, null, context -> {
            Subspace mdSS = new Subspace(mdSubspace);
            byte[] unsplitKey = mdSS.pack(Tuple.from((Object) null, 0L));
            byte[] data = context.ensureActive().get(unsplitKey).join();
            Map<String, Object> result = new HashMap<>();
            if (data == null || data.length == 0) {
                result.put("found", false);
                return result;
            }
            RecordMetaDataProto.MetaData proto;
            try {
                proto = RecordMetaDataProto.MetaData.parseFrom(data, EXTENSION_REGISTRY);
            } catch (InvalidProtocolBufferException e) {
                throw new RuntimeException("Failed to parse metadata proto", e);
            }
            RecordMetaData metaData = RecordMetaData.build(proto);
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metaData)
                .setContext(context)
                .setSubspace(new Subspace(storeSubspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();

            Index index = metaData.getIndex(indexName);
            List<IndexEntry> entries = store.scanIndex(
                index, IndexScanType.BY_VALUE, TupleRange.ALL, null,
                ScanProperties.FORWARD_SCAN
            ).asList().join();

            List<Map<String, Object>> rows = new ArrayList<>();
            for (IndexEntry e : entries) {
                Map<String, Object> row = new HashMap<>();
                List<Object> indexKey = new ArrayList<>();
                for (Object o : e.getKey()) {
                    indexKey.add(o);
                }
                List<Object> pk = new ArrayList<>();
                for (Object o : e.getPrimaryKey()) {
                    pk.add(o);
                }
                row.put("indexKey", indexKey);
                row.put("primaryKey", pk);
                rows.add(row);
            }
            result.put("found", true);
            result.put("metadataVersion", proto.getVersion());
            result.put("rows", rows);
            return result;
        });
    }

    /**
     * Load historical metadata version using raw FDB read at unsplit key.
     */
    @ConformanceStep("loadMetaDataHistoryJava")
    public Map<String, Object> loadMetaDataHistoryJava(String clusterFile, byte[] subspace, int version) {
        return runInContext(clusterFile, null, context -> {
            Subspace ss = new Subspace(subspace);
            // Read at unsplit key: subspace.pack("H", version, 0L)
            byte[] unsplitKey = ss.pack(Tuple.from("H", (long) version, 0L));
            byte[] data = context.ensureActive().get(unsplitKey).join();
            Map<String, Object> result = new HashMap<>();
            if (data != null && data.length > 0) {
                RecordMetaDataProto.MetaData proto;
                try {
                    proto = RecordMetaDataProto.MetaData.parseFrom(data, EXTENSION_REGISTRY);
                } catch (InvalidProtocolBufferException e) {
                    throw new RuntimeException("Failed to parse metadata proto", e);
                }
                result.put("version", proto.getVersion());
                result.put("found", true);
            } else {
                result.put("found", false);
            }
            return result;
        });
    }
}
