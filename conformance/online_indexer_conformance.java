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

class OnlineIndexerSteps extends ConformanceBase {
    /**
     * Metadata WITHOUT index — used for saving records before online build.
     */
    private static RecordMetaData createBaseMetaData() {
        RecordMetaDataBuilder builder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        builder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("order_id"));
        builder.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.field("customer_id"));
        builder.getRecordType("TypedRecord")
            .setPrimaryKey(Key.Expressions.field("id"));
        return builder.build();
    }

    /**
     * Save an order using base metadata (no index).
     */
    @ConformanceStep("saveOrderForOnlineBuild")
    public void saveOrderForOnlineBuild(String clusterFile, byte[] subspace, Order order, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createBaseMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .createOrOpen();
            store.saveRecord(order);
            return null;
        });
    }

    /**
     * Scan the index after build. Uses ALWAYS_READABLE_CHECKER.
     */
    @ConformanceStep("scanIndexAfterOnlineBuild")
    public List<Map<String, Object>> scanIndexAfterOnlineBuild(String clusterFile, byte[] subspace, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            RecordMetaData metadata = createIndexedMetaData();
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metadata)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();

            Index index = metadata.getIndex("Order$price");
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

    /**
     * Check if the index is in READABLE state.
     */
    @ConformanceStep("isIndexReadableAfterBuild")
    public boolean isIndexReadableAfterBuild(String clusterFile, byte[] subspace, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            RecordMetaData metadata = createIndexedMetaData();
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metadata)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();

            Index index = metadata.getIndex("Order$price");
            return store.isIndexReadable(index);
        });
    }
}
