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
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.record.RecordLayerDemo;
import com.apple.foundationdb.record.RecordLayerDemo.Order;
import com.apple.foundationdb.subspace.Subspace;

import java.util.ArrayList;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

class RebuildIndexSteps extends ConformanceBase {

    /** Basic metadata WITH record counting enabled (for auto-rebuild tests). */
    private static RecordMetaData createMetaDataWithCounting() {
        RecordMetaDataBuilder builder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        builder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("order_id"));
        builder.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.field("customer_id"));
        builder.setRecordCountKey(Key.Expressions.empty());
        return builder.build();
    }

    /** Indexed metadata WITH record counting (for auto-rebuild tests). */
    private static RecordMetaData createIndexedMetaDataWithCounting() {
        RecordMetaDataBuilder builder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        builder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("order_id"));
        builder.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.field("customer_id"));
        builder.setRecordCountKey(Key.Expressions.empty());
        builder.addIndex("Order", new Index("Order$price", Key.Expressions.field("price"), IndexTypes.VALUE));
        return builder.build();
    }

    @ConformanceStep("rebuildIndex")
    public void rebuildIndex(String clusterFile, byte[] subspace, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            RecordMetaData metadata = createIndexedMetaData();
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metadata)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();

            Index index = metadata.getIndex("Order$price");
            store.rebuildIndex(index).join();
            return null;
        });
    }

    /**
     * Save an order using basic metadata WITH record counting.
     * Used as the "before" step in auto-rebuild tests.
     */
    @ConformanceStep("saveOrderForAutoRebuild")
    public void saveOrderForAutoRebuild(String clusterFile, byte[] subspace, Order order, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createMetaDataWithCounting())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .createOrOpen();
            store.saveRecord(order);
            return null;
        });
    }

    /**
     * Opens store with indexed+counting metadata using the DEFAULT UserVersionChecker
     * (no ALWAYS_READABLE_CHECKER). Java's checkPossiblyRebuild() detects the
     * metadata version change, sees recordCount <= MAX_RECORDS_FOR_REBUILD (200),
     * returns READABLE, and rebuilds the index inline. Then scans and returns entries.
     */
    @ConformanceStep("scanIndexAfterAutoRebuild")
    public List<Map<String, Object>> scanIndexAfterAutoRebuild(String clusterFile, byte[] subspace, String indexName, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            RecordMetaData metadata = createIndexedMetaDataWithCounting();
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metadata)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .createOrOpen();

            Index index = metadata.getIndex(indexName);
            List<IndexEntry> entries = store.scanIndex(
                index, IndexScanType.BY_VALUE, TupleRange.ALL, null, ScanProperties.FORWARD_SCAN)
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
