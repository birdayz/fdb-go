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
import com.apple.foundationdb.subspace.Subspace;
import com.apple.foundationdb.tuple.Tuple;
import com.google.protobuf.ByteString;
import com.google.protobuf.Message;

import java.nio.ByteBuffer;
import java.util.ArrayList;
import java.util.HashMap;
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
