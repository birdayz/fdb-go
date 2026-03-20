package com.birdayz.conformance;

import com.apple.foundationdb.record.ExecuteProperties;
import com.apple.foundationdb.record.IndexEntry;
import com.apple.foundationdb.record.IndexScanType;
import com.apple.foundationdb.record.RecordCursorResult;
import com.apple.foundationdb.record.RecordMetaData;
import com.apple.foundationdb.record.RecordMetaDataBuilder;
import com.apple.foundationdb.record.ScanProperties;
import com.apple.foundationdb.record.TupleRange;
import com.apple.foundationdb.record.metadata.Index;
import com.apple.foundationdb.record.metadata.IndexTypes;
import com.apple.foundationdb.record.metadata.Key;
import com.apple.foundationdb.record.metadata.expressions.DimensionsKeyExpression;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordContext;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.record.provider.foundationdb.MultidimensionalIndexScanBounds;
import com.apple.foundationdb.record.RecordLayerDemo;
import com.apple.foundationdb.record.RecordLayerDemo.Order;
import com.apple.foundationdb.subspace.Subspace;
import com.apple.foundationdb.tuple.Tuple;

import java.util.ArrayList;
import java.util.Arrays;
import java.util.Base64;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

class MultidimensionalIndexSteps extends ConformanceBase {
    private static RecordMetaData createMultidimensionalMetaData() {
        RecordMetaDataBuilder metaDataBuilder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        metaDataBuilder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("order_id"));
        metaDataBuilder.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.field("customer_id"));
        metaDataBuilder.getRecordType("TypedRecord")
            .setPrimaryKey(Key.Expressions.field("id"));
        metaDataBuilder.addIndex("Order", new Index("order_coord_md",
            DimensionsKeyExpression.of(null,
                Key.Expressions.concat(
                    Key.Expressions.field("coord_x"),
                    Key.Expressions.field("coord_y")
                )),
            IndexTypes.MULTIDIMENSIONAL));
        return metaDataBuilder.build();
    }

    private static FDBRecordStore openMultidimensionalStore(FDBRecordContext context, byte[] subspace) {
        return FDBRecordStore.newBuilder()
            .setMetaDataProvider(createMultidimensionalMetaData())
            .setContext(context)
            .setSubspace(new Subspace(subspace))
            .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
            .createOrOpen();
    }

    @ConformanceStep("saveOrderWithMultidimensionalIndex")
    public void saveOrderWithMultidimensionalIndex(String clusterFile, byte[] subspace, Order order, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openMultidimensionalStore(context, subspace);
            store.saveRecord(order);
            return null;
        });
    }

    @ConformanceStep("deleteOrderWithMultidimensionalIndex")
    public boolean deleteOrderWithMultidimensionalIndex(String clusterFile, byte[] subspace, long orderID, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openMultidimensionalStore(context, subspace);
            return store.deleteRecord(Tuple.from(orderID));
        });
    }

    @ConformanceStep("scanMultidimensionalIndex")
    public List<Map<String, Object>> scanMultidimensionalIndex(String clusterFile, byte[] subspace, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            RecordMetaData metadata = createMultidimensionalMetaData();
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metadata)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();

            Index index = metadata.getIndex("order_coord_md");
            MultidimensionalIndexScanBounds scanBounds = new MultidimensionalIndexScanBounds(
                TupleRange.ALL,
                MultidimensionalIndexScanBounds.SpatialPredicate.TAUTOLOGY,
                TupleRange.ALL);

            List<IndexEntry> entries = store.scanIndex(
                index, scanBounds, null, ScanProperties.FORWARD_SCAN)
                .asList()
                .join();

            List<Map<String, Object>> result = new ArrayList<>();
            for (IndexEntry entry : entries) {
                Map<String, Object> map = new HashMap<>();
                List<Object> keyValues = new ArrayList<>();
                for (Object item : entry.getKey()) {
                    keyValues.add(item);
                }
                map.put("key", keyValues);
                List<Object> pkValues = new ArrayList<>();
                for (Object item : entry.getPrimaryKey()) {
                    pkValues.add(item);
                }
                map.put("primaryKey", pkValues);
                result.add(map);
            }
            return result;
        });
    }

    @ConformanceStep("saveMultipleOrdersWithMultidimensionalIndex")
    public void saveMultipleOrdersWithMultidimensionalIndex(String clusterFile, byte[] subspace, String ordersJson, String tenantName) {
        // ordersJson is a JSON array like [{"orderId":1,"coordX":100,"coordY":200},...]
        com.google.gson.Gson gson = new com.google.gson.GsonBuilder()
            .setObjectToNumberStrategy(com.google.gson.ToNumberPolicy.LONG_OR_DOUBLE)
            .create();
        @SuppressWarnings("unchecked")
        List<Map<String, Object>> orderList = gson.fromJson(ordersJson, List.class);
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openMultidimensionalStore(context, subspace);
            for (Map<String, Object> o : orderList) {
                long id = ((Number) o.get("orderId")).longValue();
                long cx = ((Number) o.get("coordX")).longValue();
                long cy = ((Number) o.get("coordY")).longValue();
                Order order = Order.newBuilder()
                    .setOrderId(id)
                    .setCoordX(cx)
                    .setCoordY(cy)
                    .build();
                store.saveRecord(order);
            }
            return null;
        });
    }

    @ConformanceStep("deleteMultipleOrdersWithMultidimensionalIndex")
    public void deleteMultipleOrdersWithMultidimensionalIndex(String clusterFile, byte[] subspace, String orderIdsJson, String tenantName) {
        // orderIdsJson is a JSON array like [1,2,3]
        com.google.gson.Gson gson = new com.google.gson.GsonBuilder()
            .setObjectToNumberStrategy(com.google.gson.ToNumberPolicy.LONG_OR_DOUBLE)
            .create();
        @SuppressWarnings("unchecked")
        List<Number> ids = gson.fromJson(orderIdsJson, List.class);
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openMultidimensionalStore(context, subspace);
            for (Number id : ids) {
                store.deleteRecord(Tuple.from(id.longValue()));
            }
            return null;
        });
    }

    @ConformanceStep("scanMultidimensionalIndexWithLimit")
    public Map<String, Object> scanMultidimensionalIndexWithLimit(String clusterFile, byte[] subspace, int limit, String continuation, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            RecordMetaData metadata = createMultidimensionalMetaData();
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metadata)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();

            byte[] contBytes = null;
            if (continuation != null && !continuation.isEmpty()) {
                contBytes = Base64.getDecoder().decode(continuation);
            }

            Index index = metadata.getIndex("order_coord_md");
            MultidimensionalIndexScanBounds scanBounds = new MultidimensionalIndexScanBounds(
                TupleRange.ALL,
                MultidimensionalIndexScanBounds.SpatialPredicate.TAUTOLOGY,
                TupleRange.ALL);

            ScanProperties scanProps = new ScanProperties(ExecuteProperties.newBuilder()
                .setReturnedRowLimit(limit)
                .build());

            com.apple.foundationdb.record.RecordCursor<IndexEntry> cursor = store.scanIndex(
                index, scanBounds, contBytes, scanProps);

            List<Map<String, Object>> entries = new ArrayList<>();
            byte[] nextContinuation = null;
            boolean sourceExhausted = false;

            RecordCursorResult<IndexEntry> result;
            while ((result = cursor.getNext()) != null && result.hasNext()) {
                IndexEntry entry = result.get();
                Map<String, Object> entryMap = new HashMap<>();
                List<Object> keyValues = new ArrayList<>();
                for (Object item : entry.getKey()) {
                    keyValues.add(item);
                }
                entryMap.put("key", keyValues);
                List<Object> pkValues = new ArrayList<>();
                for (Object item : entry.getPrimaryKey()) {
                    pkValues.add(item);
                }
                entryMap.put("primaryKey", pkValues);
                entries.add(entryMap);
            }
            if (result != null) {
                nextContinuation = result.getContinuation().toBytes();
                sourceExhausted = result.getNoNextReason().isSourceExhausted();
            }

            Map<String, Object> response = new HashMap<>();
            response.put("entries", entries);
            if (nextContinuation != null) {
                response.put("continuation", Base64.getEncoder().encodeToString(nextContinuation));
            }
            response.put("exhausted", sourceExhausted);
            return response;
        });
    }

    @ConformanceStep("scanMultidimensionalIndexBounded")
    public List<Map<String, Object>> scanMultidimensionalIndexBounded(
            String clusterFile, byte[] subspace, long lowX, long lowY, long highX, long highY, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            RecordMetaData metadata = createMultidimensionalMetaData();
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metadata)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();

            Index index = metadata.getIndex("order_coord_md");

            // Build a Hypercube spatial predicate with inclusive bounds on both dimensions.
            MultidimensionalIndexScanBounds.Hypercube hypercube =
                new MultidimensionalIndexScanBounds.Hypercube(Arrays.asList(
                    TupleRange.betweenInclusive(Tuple.from(lowX), Tuple.from(highX)),
                    TupleRange.betweenInclusive(Tuple.from(lowY), Tuple.from(highY))
                ));

            MultidimensionalIndexScanBounds scanBounds = new MultidimensionalIndexScanBounds(
                TupleRange.ALL, hypercube, TupleRange.ALL);

            List<IndexEntry> entries = store.scanIndex(
                index, scanBounds, null, ScanProperties.FORWARD_SCAN)
                .asList()
                .join();

            List<Map<String, Object>> result = new ArrayList<>();
            for (IndexEntry entry : entries) {
                Map<String, Object> map = new HashMap<>();
                List<Object> keyValues = new ArrayList<>();
                for (Object item : entry.getKey()) {
                    keyValues.add(item);
                }
                map.put("key", keyValues);
                List<Object> pkValues = new ArrayList<>();
                for (Object item : entry.getPrimaryKey()) {
                    pkValues.add(item);
                }
                map.put("primaryKey", pkValues);
                result.add(map);
            }
            return result;
        });
    }
}
