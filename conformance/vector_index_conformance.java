package com.birdayz.conformance;

import com.apple.foundationdb.record.IndexEntry;
import com.apple.foundationdb.record.RecordCursor;
import com.apple.foundationdb.record.RecordMetaData;
import com.apple.foundationdb.record.RecordMetaDataBuilder;
import com.apple.foundationdb.record.ScanProperties;
import com.apple.foundationdb.record.TupleRange;
import com.apple.foundationdb.record.metadata.Index;
import com.apple.foundationdb.record.metadata.IndexOptions;
import com.apple.foundationdb.record.metadata.IndexTypes;
import com.apple.foundationdb.record.metadata.Key;
import com.apple.foundationdb.record.metadata.expressions.KeyWithValueExpression;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordContext;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.record.provider.foundationdb.FDBStoredRecord;
import com.apple.foundationdb.record.provider.foundationdb.VectorIndexScanBounds;
import com.apple.foundationdb.record.provider.foundationdb.VectorIndexScanOptions;
import com.apple.foundationdb.record.query.expressions.Comparisons;
import com.apple.foundationdb.record.RecordLayerDemo;
import com.apple.foundationdb.record.RecordLayerDemo.Order;
import com.apple.foundationdb.linear.DoubleRealVector;
import com.apple.foundationdb.linear.RealVector;
import com.apple.foundationdb.linear.Metric;
import com.apple.foundationdb.rabitq.RaBitQuantizer;
import com.apple.foundationdb.rabitq.EncodedRealVector;
import com.apple.foundationdb.subspace.Subspace;
import com.apple.foundationdb.tuple.Tuple;
import com.google.protobuf.ByteString;
import com.google.protobuf.Message;

import java.nio.ByteBuffer;
import java.util.ArrayList;
import java.util.HashMap;
import java.util.HexFormat;
import java.util.List;
import java.util.Map;

/**
 * Conformance steps for VECTOR (HNSW) index cross-language testing.
 *
 * Tests that Go and Java can both maintain the same HNSW graph:
 * - Go saves records with VECTOR index -> Java opens and saves more
 * - Java saves records -> Go opens and loads
 * - Record counts agree after cross-language writes
 */
class VectorIndexSteps extends ConformanceBase {

    private static final int NUM_DIMENSIONS = 3;

    @ConformanceStep("encodeRaBitQVector")
    public String encodeRaBitQVector(String vectorJson, long numExBits) {
        RaBitQuantizer quantizer = new RaBitQuantizer(Metric.EUCLIDEAN_SQUARE_METRIC,
            Math.toIntExact(numExBits));
        return HexFormat.of().formatHex(quantizer.encode(
            new DoubleRealVector(parseVector(vectorJson))).getRawData());
    }

    @ConformanceStep("decodeRaBitQVector")
    public String decodeRaBitQVector(String encodedHex, long numDimensions, long numExBits) {
        EncodedRealVector vector = EncodedRealVector.fromBytes(HexFormat.of().parseHex(encodedHex),
            Math.toIntExact(numDimensions), Math.toIntExact(numExBits));
        return HexFormat.of().formatHex(serializeVector(vector.getData()));
    }

    @ConformanceStep("exerciseRebuiltRaBitQIndex")
    public Map<String, Object> exerciseRebuiltRaBitQIndex(String clusterFile, byte[] subspace,
            byte[] metadataBytes, String indexName, String action, long orderId,
            String vectorJson, String tenantName) throws com.google.protobuf.InvalidProtocolBufferException {
        com.google.protobuf.ExtensionRegistry registry = com.google.protobuf.ExtensionRegistry.newInstance();
        com.apple.foundationdb.record.RecordMetaDataOptionsProto.registerAllExtensions(registry);
        RecordMetaData metadata = RecordMetaData.build(
            com.apple.foundationdb.record.RecordMetaDataProto.MetaData.parseFrom(metadataBytes, registry));
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder().setContext(context)
                .setSubspace(new Subspace(subspace)).setMetaDataProvider(metadata).open();
            Index index = metadata.getIndex(indexName);
            if (action.equals("save")) {
                com.google.protobuf.Descriptors.Descriptor descriptor = metadata.getRecordType("Order").getDescriptor();
                store.saveRecord(com.google.protobuf.DynamicMessage.newBuilder(descriptor)
                    .setField(descriptor.findFieldByName("order_id"), orderId)
                    .setField(descriptor.findFieldByName("vector_data"), ByteString.copyFrom(serializeVector(parseVector(vectorJson))))
                    .build());
            } else if (action.equals("delete")) {
                if (!store.deleteRecord(Tuple.from(orderId))) {
                    throw new IllegalStateException("migration test record missing: " + orderId);
                }
            } else if (!action.equals("search")) {
                throw new IllegalArgumentException("unknown migration test action: " + action);
            }
            Map<String, Object> result = new HashMap<>();
            result.put("state", store.getIndexState(index).name());
            VectorIndexScanBounds bounds = new VectorIndexScanBounds(TupleRange.ALL,
                Comparisons.Type.DISTANCE_RANK_LESS_THAN_OR_EQUAL,
                new DoubleRealVector(parseVector(vectorJson)), 100, VectorIndexScanOptions.empty());
            try {
                List<IndexEntry> entries = store.scanIndex(index, bounds, null, ScanProperties.FORWARD_SCAN).asList().join();
                List<Long> ids = new ArrayList<>();
                for (IndexEntry entry : entries) {
                    ids.add(entry.getPrimaryKey().getLong(0));
                }
                result.put("ids", ids);
                result.put("refused", false);
            } catch (com.apple.foundationdb.record.provider.foundationdb.ScanNonReadableIndexException exception) {
                result.put("refused", true);
            }
            return result;
        });
    }

    /**
     * Create metadata with an ungrouped VECTOR index on Order.vector_data.
     * Uses KeyWithValueExpression(field("vector_data"), 0) matching Java's standard pattern.
     */
    private static RecordMetaData createVectorMetaData() {
        RecordMetaDataBuilder metaDataBuilder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        metaDataBuilder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("order_id"));
        metaDataBuilder.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.field("customer_id"));
        metaDataBuilder.getRecordType("TypedRecord")
            .setPrimaryKey(Key.Expressions.field("id"));
        metaDataBuilder.addIndex("Order", new Index("order_vector",
            new KeyWithValueExpression(Key.Expressions.field("vector_data"), 0),
            IndexTypes.VECTOR,
            Map.of(
                IndexOptions.HNSW_NUM_DIMENSIONS, String.valueOf(NUM_DIMENSIONS),
                IndexOptions.HNSW_METRIC, "EUCLIDEAN_SQUARE_METRIC"
            )));
        return metaDataBuilder.build();
    }

    private static FDBRecordStore openVectorStore(FDBRecordContext context, byte[] subspace) {
        return FDBRecordStore.newBuilder()
            .setMetaDataProvider(createVectorMetaData())
            .setContext(context)
            .setSubspace(new Subspace(subspace))
            .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
            .createOrOpen();
    }

    /**
     * Serialize a double[] vector to bytes in the format used by both Go and Java.
     * Format: byte[0] = VectorType.DOUBLE.ordinal() = 2, rest = big-endian float64 values.
     */
    private static byte[] serializeVector(double[] values) {
        ByteBuffer buf = ByteBuffer.allocate(1 + 8 * values.length);
        buf.put((byte) 2); // VectorType.DOUBLE.ordinal() = 2
        for (double v : values) {
            buf.putDouble(v);
        }
        return buf.array();
    }

    @ConformanceStep("replayVectorPendingEntry")
    public Map<String, Object> replayVectorPendingEntry(String clusterFile, byte[] subspace,
            String operation, String payloadHex, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openVectorStore(context, subspace);
            Index index = store.getRecordMetaData().getIndex("order_vector");
            FDBStoredRecord<Order> oldRecord = FDBStoredRecord.newBuilder(Order.newBuilder()
                .setOrderId(42).setVectorData(ByteString.copyFrom(serializeVector(new double[]{1, 2, 3}))).build())
                .setPrimaryKey(Tuple.from(42L)).setRecordType(store.getRecordMetaData().getRecordType("Order")).build();
            FDBStoredRecord<Order> newRecord = FDBStoredRecord.newBuilder(Order.newBuilder()
                .setOrderId(42).setVectorData(ByteString.copyFrom(serializeVector(new double[]{4, 5, 6}))).build())
                .setPrimaryKey(Tuple.from(42L)).setRecordType(store.getRecordMetaData().getRecordType("Order")).build();
            com.apple.foundationdb.record.provider.foundationdb.IndexMaintainer maintainer = store.getIndexMaintainer(index);
            com.google.protobuf.Any captured;
            if (operation.equals("insert")) {
                captured = maintainer.serializePendingWriteQueue(null, oldRecord);
            } else if (operation.equals("update")) {
                captured = maintainer.serializePendingWriteQueue(oldRecord, newRecord);
            } else if (operation.equals("delete")) {
                captured = maintainer.serializePendingWriteQueue(newRecord, null);
            } else {
                throw new IllegalArgumentException("unknown pending operation: " + operation);
            }
            try {
                maintainer.updateFromQueue(com.google.protobuf.Any.parseFrom(HexFormat.of().parseHex(payloadHex))).join();
            } catch (com.google.protobuf.InvalidProtocolBufferException ex) {
                throw new IllegalArgumentException(ex);
            }
            VectorIndexScanBounds bounds = new VectorIndexScanBounds(TupleRange.ALL,
                Comparisons.Type.DISTANCE_RANK_LESS_THAN_OR_EQUAL,
                new DoubleRealVector(new double[]{4, 5, 6}), 100, VectorIndexScanOptions.empty());
            List<Long> ids = new ArrayList<>();
            for (IndexEntry entry : store.scanIndex(index, bounds, null, ScanProperties.FORWARD_SCAN).asList().join()) {
                ids.add(entry.getPrimaryKey().getLong(0));
            }
            Map<String, Object> result = new HashMap<>();
            result.put("payload", HexFormat.of().formatHex(captured.toByteArray()));
            result.put("ids", ids);
            result.put("recordCount", store.scanRecords(null, ScanProperties.FORWARD_SCAN).getCount().join());
            return result;
        });
    }

    @ConformanceStep("saveOrderWithVectorIndex")
    public void saveOrderWithVectorIndex(String clusterFile, byte[] subspace,
            long orderId, String vectorJson, String tenantName) {
        double[] vector = parseVector(vectorJson);
        byte[] vectorBytes = serializeVector(vector);
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openVectorStore(context, subspace);
            Order order = Order.newBuilder()
                .setOrderId(orderId)
                .setVectorData(ByteString.copyFrom(vectorBytes))
                .build();
            store.saveRecord(order);
            return null;
        });
    }

    @ConformanceStep("loadOrderWithVectorIndex")
    public Map<String, Object> loadOrderWithVectorIndex(String clusterFile, byte[] subspace,
            long orderId, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openVectorStore(context, subspace);
            FDBStoredRecord<Message> record = store.loadRecord(Tuple.from(orderId));
            if (record == null) {
                return null;
            }
            Order order = Order.newBuilder().mergeFrom(record.getRecord()).build();
            Map<String, Object> result = new HashMap<>();
            result.put("orderId", order.getOrderId());
            if (order.hasVectorData()) {
                result.put("vectorData", encodeVector(order.getVectorData().toByteArray()));
            }
            return result;
        });
    }

    @ConformanceStep("countRecordsWithVectorIndex")
    public long countRecordsWithVectorIndex(String clusterFile, byte[] subspace, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openVectorStore(context, subspace);
            // Count by scanning all records
            return store.scanRecords(null,
                com.apple.foundationdb.record.ScanProperties.FORWARD_SCAN)
                .getCount()
                .join();
        });
    }

    @ConformanceStep("deleteOrderWithVectorIndex")
    public boolean deleteOrderWithVectorIndex(String clusterFile, byte[] subspace,
            long orderId, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openVectorStore(context, subspace);
            return store.deleteRecord(Tuple.from(orderId));
        });
    }

    @ConformanceStep("saveMultipleOrdersWithVectorIndex")
    public void saveMultipleOrdersWithVectorIndex(String clusterFile, byte[] subspace,
            String ordersJson, String tenantName) {
        com.google.gson.Gson gson = new com.google.gson.GsonBuilder()
            .setObjectToNumberStrategy(com.google.gson.ToNumberPolicy.LONG_OR_DOUBLE)
            .create();
        @SuppressWarnings("unchecked")
        List<Map<String, Object>> orderList = gson.fromJson(ordersJson, List.class);
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openVectorStore(context, subspace);
            for (Map<String, Object> o : orderList) {
                long id = ((Number) o.get("orderId")).longValue();
                @SuppressWarnings("unchecked")
                List<Number> vec = (List<Number>) o.get("vector");
                double[] vector = new double[vec.size()];
                for (int i = 0; i < vec.size(); i++) {
                    vector[i] = vec.get(i).doubleValue();
                }
                byte[] vectorBytes = serializeVector(vector);
                Order order = Order.newBuilder()
                    .setOrderId(id)
                    .setVectorData(ByteString.copyFrom(vectorBytes))
                    .build();
                store.saveRecord(order);
            }
            return null;
        });
    }

    @ConformanceStep("searchVectorIndex")
    public List<Map<String, Object>> searchVectorIndex(String clusterFile, byte[] subspace,
            String vectorJson, long k, String tenantName) {
        double[] queryVec = parseVector(vectorJson);
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openVectorStore(context, subspace);
            RecordMetaData md = createVectorMetaData();
            Index index = md.getIndex("order_vector");
            RealVector queryVector = new DoubleRealVector(queryVec);
            VectorIndexScanBounds bounds = new VectorIndexScanBounds(
                TupleRange.ALL,
                Comparisons.Type.DISTANCE_RANK_LESS_THAN_OR_EQUAL,
                queryVector,
                (int) k,
                VectorIndexScanOptions.empty());
            RecordCursor<IndexEntry> cursor = store.scanIndex(
                index, bounds, null, ScanProperties.FORWARD_SCAN);
            List<IndexEntry> entries = cursor.asList().join();
            List<Map<String, Object>> results = new ArrayList<>();
            for (IndexEntry entry : entries) {
                Map<String, Object> m = new HashMap<>();
                Tuple pk = entry.getPrimaryKey();
                m.put("orderId", pk.getLong(0));
                results.add(m);
            }
            return results;
        });
    }

    // --- RaBitQ-enabled VECTOR index steps ---

    private static final int RABITQ_NUM_DIMENSIONS = 8;

    /**
     * Create metadata with a VECTOR index that enables RaBitQ quantization.
     * Uses COSINE_METRIC so RaBitQ activates immediately from the first insert
     * (Euclidean requires centroid sampling which needs 1000+ vectors).
     */
    private static RecordMetaData createVectorMetaDataWithRaBitQ() {
        RecordMetaDataBuilder metaDataBuilder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        metaDataBuilder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("order_id"));
        metaDataBuilder.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.field("customer_id"));
        metaDataBuilder.getRecordType("TypedRecord")
            .setPrimaryKey(Key.Expressions.field("id"));
        metaDataBuilder.addIndex("Order", new Index("order_vector_rabitq",
            new KeyWithValueExpression(Key.Expressions.field("vector_data"), 0),
            IndexTypes.VECTOR,
            Map.of(
                IndexOptions.HNSW_NUM_DIMENSIONS, String.valueOf(RABITQ_NUM_DIMENSIONS),
                IndexOptions.HNSW_METRIC, "COSINE_METRIC",
                IndexOptions.HNSW_USE_RABITQ, "true",
                IndexOptions.HNSW_RABITQ_NUM_EX_BITS, "4"
            )));
        return metaDataBuilder.build();
    }

    private static FDBRecordStore openVectorStoreWithRaBitQ(FDBRecordContext context, byte[] subspace) {
        return FDBRecordStore.newBuilder()
            .setMetaDataProvider(createVectorMetaDataWithRaBitQ())
            .setContext(context)
            .setSubspace(new Subspace(subspace))
            .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
            .createOrOpen();
    }

    @ConformanceStep("saveOrderWithRaBitQIndex")
    public void saveOrderWithRaBitQIndex(String clusterFile, byte[] subspace,
            long orderId, String vectorJson, String tenantName) {
        double[] vector = parseVector(vectorJson);
        byte[] vectorBytes = serializeVector(vector);
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openVectorStoreWithRaBitQ(context, subspace);
            Order order = Order.newBuilder()
                .setOrderId(orderId)
                .setVectorData(ByteString.copyFrom(vectorBytes))
                .build();
            store.saveRecord(order);
            return null;
        });
    }

    @ConformanceStep("loadOrderWithRaBitQIndex")
    public Map<String, Object> loadOrderWithRaBitQIndex(String clusterFile, byte[] subspace,
            long orderId, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openVectorStoreWithRaBitQ(context, subspace);
            FDBStoredRecord<Message> record = store.loadRecord(Tuple.from(orderId));
            if (record == null) {
                return null;
            }
            Order order = Order.newBuilder().mergeFrom(record.getRecord()).build();
            Map<String, Object> result = new HashMap<>();
            result.put("orderId", order.getOrderId());
            if (order.hasVectorData()) {
                result.put("vectorData", encodeVector(order.getVectorData().toByteArray()));
            }
            return result;
        });
    }

    @ConformanceStep("countRecordsWithRaBitQIndex")
    public long countRecordsWithRaBitQIndex(String clusterFile, byte[] subspace, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openVectorStoreWithRaBitQ(context, subspace);
            return store.scanRecords(null, ScanProperties.FORWARD_SCAN)
                .getCount()
                .join();
        });
    }

    @ConformanceStep("searchRaBitQIndex")
    public List<Map<String, Object>> searchRaBitQIndex(String clusterFile, byte[] subspace,
            String vectorJson, long k, String tenantName) {
        double[] queryVec = parseVector(vectorJson);
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openVectorStoreWithRaBitQ(context, subspace);
            RecordMetaData md = createVectorMetaDataWithRaBitQ();
            Index index = md.getIndex("order_vector_rabitq");
            RealVector queryVector = new DoubleRealVector(queryVec);
            VectorIndexScanBounds bounds = new VectorIndexScanBounds(
                TupleRange.ALL,
                Comparisons.Type.DISTANCE_RANK_LESS_THAN_OR_EQUAL,
                queryVector,
                (int) k,
                VectorIndexScanOptions.empty());
            RecordCursor<IndexEntry> cursor = store.scanIndex(
                index, bounds, null, ScanProperties.FORWARD_SCAN);
            List<IndexEntry> entries = cursor.asList().join();
            List<Map<String, Object>> results = new ArrayList<>();
            for (IndexEntry entry : entries) {
                Map<String, Object> m = new HashMap<>();
                Tuple pk = entry.getPrimaryKey();
                m.put("orderId", pk.getLong(0));
                results.add(m);
            }
            return results;
        });
    }

    private static RecordMetaData createExtraBitsMetaData(String metric, long numExBits) {
        RecordMetaDataBuilder metaDataBuilder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        metaDataBuilder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("order_id"));
        metaDataBuilder.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.field("customer_id"));
        metaDataBuilder.getRecordType("TypedRecord")
            .setPrimaryKey(Key.Expressions.field("id"));
        metaDataBuilder.addIndex("Order", new Index("order_vector_bits",
            new KeyWithValueExpression(Key.Expressions.field("vector_data"), 0),
            IndexTypes.VECTOR,
            Map.of(
                IndexOptions.HNSW_NUM_DIMENSIONS, String.valueOf(RABITQ_NUM_DIMENSIONS),
                IndexOptions.HNSW_METRIC, metric,
                IndexOptions.HNSW_USE_RABITQ, "true",
                IndexOptions.HNSW_RABITQ_NUM_EX_BITS, String.valueOf(numExBits),
                IndexOptions.HNSW_SAMPLE_VECTOR_STATS_PROBABILITY, "1.0",
                IndexOptions.HNSW_MAINTAIN_STATS_PROBABILITY, "1.0",
                IndexOptions.HNSW_STATS_THRESHOLD, "11"
            )));
        return metaDataBuilder.build();
    }

    /** "ok", or the root cause's class of what the action threw. */
    private static String extraBitsOutcome(Runnable action) {
        try {
            action.run();
            return "ok";
        } catch (RuntimeException e) {
            Throwable t = e;
            while (t.getCause() != null && t.getCause() != t) {
                t = t.getCause();
            }
            return t.getClass().getName();
        }
    }

    /**
     * An HNSW index with RaBitQ at the given extra-bit count, maintained through
     * the record store: a search of the empty index, then one save per vector,
     * each in its own transaction, then a save of record 1 with its vector
     * unchanged, a search and a delete of record 0, each reported as "ok" or
     * its root cause's class. Statistics are sampled
     * and maintained on every insert with threshold 11, so a Euclidean index
     * establishes its centroid on a known insert.
     */
    @ConformanceStep("hnswExtraBitsProbe")
    public Map<String, Object> hnswExtraBitsProbe(String clusterFile, byte[] subspace, String tenantName,
            String metric, long numExBits, List<List<Number>> vectors) {
        RecordMetaData md = createExtraBitsMetaData(metric, numExBits);
        java.util.function.Function<FDBRecordContext, FDBRecordStore> open = context -> FDBRecordStore.newBuilder()
            .setMetaDataProvider(md)
            .setContext(context)
            .setSubspace(new Subspace(subspace))
            .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
            .createOrOpen();
        java.util.function.Function<double[], String> search = query -> extraBitsOutcome(() ->
            runInContext(clusterFile, tenantName, context -> {
                VectorIndexScanBounds bounds = new VectorIndexScanBounds(TupleRange.ALL,
                    Comparisons.Type.DISTANCE_RANK_LESS_THAN_OR_EQUAL, new DoubleRealVector(query), 3,
                    VectorIndexScanOptions.empty());
                return open.apply(context).scanIndex(md.getIndex("order_vector_bits"), bounds, null,
                    ScanProperties.FORWARD_SCAN).asList().join();
            }));
        double[][] vecs = new double[vectors.size()][];
        for (int i = 0; i < vecs.length; i++) {
            vecs[i] = new double[vectors.get(i).size()];
            for (int d = 0; d < vecs[i].length; d++) {
                vecs[i][d] = vectors.get(i).get(d).doubleValue();
            }
        }
        Map<String, Object> result = new java.util.LinkedHashMap<>();
        result.put("searchEmpty", search.apply(vecs[0]));
        List<String> inserts = new ArrayList<>();
        for (int i = 0; i < vecs.length; i++) {
            final long id = i;
            final byte[] bytes = serializeVector(vecs[i]);
            inserts.add(extraBitsOutcome(() -> runInContext(clusterFile, tenantName, context -> {
                open.apply(context).saveRecord(Order.newBuilder().setOrderId(id)
                    .setVectorData(ByteString.copyFrom(bytes)).build());
                return null;
            })));
        }
        result.put("inserts", inserts);
        // Record 1 saved again with its vector unchanged and another field
        // changed: its index entry is common to the old and the new record,
        // so the maintainer makes no graph call.
        final byte[] firstBytes = serializeVector(vecs[1]);
        result.put("resaveUnchangedVector", extraBitsOutcome(() -> runInContext(clusterFile, tenantName, context -> {
            open.apply(context).saveRecord(Order.newBuilder().setOrderId(1L).setPrice(7)
                .setVectorData(ByteString.copyFrom(firstBytes)).build());
            return null;
        })));
        result.put("search", search.apply(vecs[0]));
        result.put("deleteFirst", extraBitsOutcome(() -> runInContext(clusterFile, tenantName, context ->
            open.apply(context).deleteRecord(Tuple.from(0L)))));
        return result;
    }

    /**
     * Parse a JSON array of doubles, e.g. "[1.0, 2.0, 3.0]".
     */
    private static double[] parseVector(String json) {
        com.google.gson.Gson gson = new com.google.gson.Gson();
        @SuppressWarnings("unchecked")
        List<Number> list = gson.fromJson(json, List.class);
        double[] result = new double[list.size()];
        for (int i = 0; i < list.size(); i++) {
            result[i] = list.get(i).doubleValue();
        }
        return result;
    }

    /**
     * Encode vector bytes as a list of doubles for JSON serialization.
     */
    private static List<Double> encodeVector(byte[] data) {
        if (data == null || data.length < 1) {
            return new ArrayList<>();
        }
        // Skip type byte
        int numFloats = (data.length - 1) / 8;
        ByteBuffer buf = ByteBuffer.wrap(data, 1, data.length - 1);
        List<Double> result = new ArrayList<>();
        for (int i = 0; i < numFloats; i++) {
            result.add(buf.getDouble());
        }
        return result;
    }
}
