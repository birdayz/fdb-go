package com.birdayz.conformance;

import com.apple.foundationdb.record.IndexEntry;
import com.apple.foundationdb.record.RecordCursor;
import com.apple.foundationdb.record.RecordMetaData;
import com.apple.foundationdb.record.RecordMetaDataBuilder;
import com.apple.foundationdb.record.ScanProperties;
import com.apple.foundationdb.record.TupleRange;
import com.apple.foundationdb.record.metadata.Index;
import com.apple.foundationdb.record.metadata.IndexOptions;
import com.apple.foundationdb.record.metadata.IndexPredicate;
import com.apple.foundationdb.record.metadata.IndexTypes;
import com.apple.foundationdb.record.metadata.Key;
import com.apple.foundationdb.record.metadata.expressions.KeyWithValueExpression;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordContext;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
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

import java.nio.ByteBuffer;
import java.util.ArrayList;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

/**
 * Conformance steps for the SLIDING WINDOW decorator over a VECTOR (HNSW) index —
 * the only user of record-store keyspace 10.
 *
 * <p>This is the test that makes the Go port a port rather than a re-invention.
 * Everything in the Go unit suite proves self-consistency: that Go reads back what
 * Go wrote. Only a run where one engine writes and the other reads proves the
 * layout under prefix 10 is Java's — the entry list keyed by
 * {@code [windowValue..., primaryKey...]}, the count at meta key 3 as a PACKED
 * TUPLE rather than a raw long, and the boundary pointer at meta key 4.</p>
 *
 * <p>The window is ASC (so MIN semantics: the smallest prices are kept) with
 * {@code size = 2}, ordered by {@code price} and unpartitioned — {@code Tuple.from()}
 * is a real code path, not a degenerate one.</p>
 */
class SlidingWindowIndexSteps extends ConformanceBase {

    private static final int NUM_DIMENSIONS = 3;
    private static final int WINDOW_SIZE = 2;
    static final String INDEX_NAME = "order_sw_vector";

    /**
     * A VECTOR index carrying a RowNumberWindowPredicate. The registry
     * (IndexMaintainerFactoryRegistryImpl) detects the predicate and wraps the
     * vector factory with SlidingWindowIndexMaintainerFactory, so the decoration
     * is implied by the metadata alone — exactly as it must be for Go to infer
     * the same thing from the same stored bytes.
     */
    private static RecordMetaData createSlidingWindowMetaData() {
        RecordMetaDataBuilder metaDataBuilder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        metaDataBuilder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("order_id"));
        metaDataBuilder.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.field("customer_id"));
        metaDataBuilder.getRecordType("TypedRecord")
            .setPrimaryKey(Key.Expressions.field("id"));
        metaDataBuilder.addIndex("Order", new Index(INDEX_NAME,
            new KeyWithValueExpression(Key.Expressions.field("vector_data"), 0),
            IndexTypes.VECTOR,
            Map.of(
                IndexOptions.HNSW_NUM_DIMENSIONS, String.valueOf(NUM_DIMENSIONS),
                IndexOptions.HNSW_METRIC, "EUCLIDEAN_SQUARE_METRIC"
            ),
            new IndexPredicate.RowNumberWindowPredicate(
                List.of("price"),
                IndexPredicate.RowNumberWindowPredicate.Direction.ASC,
                WINDOW_SIZE)));
        return metaDataBuilder.build();
    }

    private static FDBRecordStore openSlidingWindowStore(FDBRecordContext context, byte[] subspace) {
        return FDBRecordStore.newBuilder()
            .setMetaDataProvider(createSlidingWindowMetaData())
            .setContext(context)
            .setSubspace(new Subspace(subspace))
            .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
            .createOrOpen();
    }

    /** Parse a JSON array of doubles, e.g. "[1.0, 2.0, 3.0]". */
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

    /** type byte 2 (VectorType.DOUBLE.ordinal()) then big-endian float64s. */
    private static byte[] serializeVector(double[] values) {
        ByteBuffer buf = ByteBuffer.allocate(1 + 8 * values.length);
        buf.put((byte) 2);
        for (double v : values) {
            buf.putDouble(v);
        }
        return buf.array();
    }

    @ConformanceStep("saveOrdersWithSlidingWindowIndex")
    public void saveOrdersWithSlidingWindowIndex(String clusterFile, byte[] subspace,
            String ordersJson, String tenantName) {
        com.google.gson.Gson gson = new com.google.gson.GsonBuilder()
            .setObjectToNumberStrategy(com.google.gson.ToNumberPolicy.LONG_OR_DOUBLE)
            .create();
        @SuppressWarnings("unchecked")
        List<Map<String, Object>> orderList = gson.fromJson(ordersJson, List.class);
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openSlidingWindowStore(context, subspace);
            for (Map<String, Object> o : orderList) {
                long id = ((Number) o.get("orderId")).longValue();
                int price = ((Number) o.get("price")).intValue();
                @SuppressWarnings("unchecked")
                List<Number> vec = (List<Number>) o.get("vector");
                double[] vector = new double[vec.size()];
                for (int i = 0; i < vec.size(); i++) {
                    vector[i] = vec.get(i).doubleValue();
                }
                store.saveRecord(Order.newBuilder()
                    .setOrderId(id)
                    .setPrice(price)
                    .setVectorData(ByteString.copyFrom(serializeVector(vector)))
                    .build());
            }
            return null;
        });
    }

    @ConformanceStep("deleteOrderWithSlidingWindowIndex")
    public boolean deleteOrderWithSlidingWindowIndex(String clusterFile, byte[] subspace,
            long orderId, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openSlidingWindowStore(context, subspace);
            return store.deleteRecord(Tuple.from(orderId));
        });
    }

    /**
     * The primary keys currently present in the wrapped HNSW graph — that is,
     * the records the window has elected. k is deliberately far larger than the
     * window so the answer is "everything the graph holds", not "the k nearest".
     */
    @ConformanceStep("searchSlidingWindowIndex")
    public List<Map<String, Object>> searchSlidingWindowIndex(String clusterFile, byte[] subspace,
            String vectorJson, long k, String tenantName) {
        double[] queryVec = parseVector(vectorJson);
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openSlidingWindowStore(context, subspace);
            RecordMetaData md = createSlidingWindowMetaData();
            Index index = md.getIndex(INDEX_NAME);
            RealVector queryVector = new DoubleRealVector(queryVec);
            VectorIndexScanBounds bounds = new VectorIndexScanBounds(
                TupleRange.ALL,
                Comparisons.Type.DISTANCE_RANK_LESS_THAN_OR_EQUAL,
                queryVector,
                (int) k,
                VectorIndexScanOptions.empty());
            RecordCursor<IndexEntry> cursor = store.scanIndex(
                index, bounds, null, ScanProperties.FORWARD_SCAN);
            List<Map<String, Object>> results = new ArrayList<>();
            for (IndexEntry entry : cursor.asList().join()) {
                Map<String, Object> m = new HashMap<>();
                m.put("orderId", entry.getPrimaryKey().getLong(0));
                results.add(m);
            }
            return results;
        });
    }
}
