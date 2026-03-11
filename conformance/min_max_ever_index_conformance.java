package com.birdayz.conformance;

import com.apple.foundationdb.record.IndexScanType;
import com.apple.foundationdb.record.RecordMetaData;
import com.apple.foundationdb.record.RecordMetaDataBuilder;
import com.apple.foundationdb.record.ScanProperties;
import com.apple.foundationdb.record.TupleRange;
import com.apple.foundationdb.record.IndexEntry;
import com.apple.foundationdb.record.metadata.Index;
import com.apple.foundationdb.record.metadata.IndexTypes;
import com.apple.foundationdb.record.metadata.Key;
import com.apple.foundationdb.record.metadata.expressions.GroupingKeyExpression;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordContext;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.record.RecordLayerDemo;
import com.apple.foundationdb.record.RecordLayerDemo.Order;
import com.apple.foundationdb.subspace.Subspace;
import com.apple.foundationdb.tuple.Tuple;

import java.util.ArrayList;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

class MinMaxEverIndexSteps extends ConformanceBase {
    // ========== MAX_EVER_LONG ==========

    private static RecordMetaData createMaxEverLongIndexedMetaData() {
        RecordMetaDataBuilder metaDataBuilder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        metaDataBuilder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("order_id"));
        metaDataBuilder.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.field("customer_id"));
        metaDataBuilder.getRecordType("TypedRecord")
            .setPrimaryKey(Key.Expressions.field("id"));
        metaDataBuilder.addIndex("Order", new Index("max_ever_price",
            new GroupingKeyExpression(Key.Expressions.field("price"), 1),
            IndexTypes.MAX_EVER_LONG));
        return metaDataBuilder.build();
    }

    private static FDBRecordStore openMaxEverLongIndexedStore(FDBRecordContext context, byte[] subspace) {
        return FDBRecordStore.newBuilder()
            .setMetaDataProvider(createMaxEverLongIndexedMetaData())
            .setContext(context)
            .setSubspace(new Subspace(subspace))
            .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
            .createOrOpen();
    }

    @ConformanceStep("saveOrderWithMaxEverLongIndex")
    public void saveOrderWithMaxEverLongIndex(String clusterFile, byte[] subspace, Order order, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openMaxEverLongIndexedStore(context, subspace);
            store.saveRecord(order);
            return null;
        });
    }

    @ConformanceStep("deleteOrderWithMaxEverLongIndex")
    public boolean deleteOrderWithMaxEverLongIndex(String clusterFile, byte[] subspace, long orderID, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openMaxEverLongIndexedStore(context, subspace);
            return store.deleteRecord(Tuple.from(orderID));
        });
    }

    @ConformanceStep("scanMaxEverLongIndex")
    public List<Map<String, Object>> scanMaxEverLongIndex(String clusterFile, byte[] subspace, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            RecordMetaData metadata = createMaxEverLongIndexedMetaData();
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metadata)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();

            Index index = metadata.getIndex("max_ever_price");
            List<IndexEntry> entries = store.scanIndex(
                index, IndexScanType.BY_GROUP, TupleRange.ALL, null, ScanProperties.FORWARD_SCAN)
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
                map.put("value", entry.getValue().getLong(0));
                result.add(map);
            }
            return result;
        });
    }

    // ========== MIN_EVER_LONG ==========

    private static RecordMetaData createMinEverLongIndexedMetaData() {
        RecordMetaDataBuilder metaDataBuilder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        metaDataBuilder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("order_id"));
        metaDataBuilder.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.field("customer_id"));
        metaDataBuilder.getRecordType("TypedRecord")
            .setPrimaryKey(Key.Expressions.field("id"));
        metaDataBuilder.addIndex("Order", new Index("min_ever_price",
            new GroupingKeyExpression(Key.Expressions.field("price"), 1),
            IndexTypes.MIN_EVER_LONG));
        return metaDataBuilder.build();
    }

    private static FDBRecordStore openMinEverLongIndexedStore(FDBRecordContext context, byte[] subspace) {
        return FDBRecordStore.newBuilder()
            .setMetaDataProvider(createMinEverLongIndexedMetaData())
            .setContext(context)
            .setSubspace(new Subspace(subspace))
            .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
            .createOrOpen();
    }

    @ConformanceStep("saveOrderWithMinEverLongIndex")
    public void saveOrderWithMinEverLongIndex(String clusterFile, byte[] subspace, Order order, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openMinEverLongIndexedStore(context, subspace);
            store.saveRecord(order);
            return null;
        });
    }

    @ConformanceStep("deleteOrderWithMinEverLongIndex")
    public boolean deleteOrderWithMinEverLongIndex(String clusterFile, byte[] subspace, long orderID, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openMinEverLongIndexedStore(context, subspace);
            return store.deleteRecord(Tuple.from(orderID));
        });
    }

    @ConformanceStep("scanMinEverLongIndex")
    public List<Map<String, Object>> scanMinEverLongIndex(String clusterFile, byte[] subspace, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            RecordMetaData metadata = createMinEverLongIndexedMetaData();
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metadata)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();

            Index index = metadata.getIndex("min_ever_price");
            List<IndexEntry> entries = store.scanIndex(
                index, IndexScanType.BY_GROUP, TupleRange.ALL, null, ScanProperties.FORWARD_SCAN)
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
                map.put("value", entry.getValue().getLong(0));
                result.add(map);
            }
            return result;
        });
    }
}
