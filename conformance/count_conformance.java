package com.birdayz.conformance;

import com.apple.foundationdb.record.RecordMetaData;
import com.apple.foundationdb.record.RecordMetaDataBuilder;
import com.apple.foundationdb.record.metadata.Key;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.record.RecordLayerDemo;
import com.apple.foundationdb.record.RecordLayerDemo.Order;
import com.apple.foundationdb.subspace.Subspace;
import com.apple.foundationdb.tuple.Tuple;

class CountSteps extends ConformanceBase {
    private static RecordMetaData createCountingMetaData() {
        RecordMetaDataBuilder builder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        builder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("order_id"));
        builder.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.field("customer_id"));
        builder.getRecordType("TypedRecord")
            .setPrimaryKey(Key.Expressions.field("id"));
        builder.setRecordCountKey(Key.Expressions.empty());
        return builder.build();
    }

    @ConformanceStep("saveOrderCounting")
    public void saveOrderCounting(String clusterFile, byte[] subspace, Order order, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createCountingMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .createOrOpen();
            store.saveRecord(order);
            return null;
        });
    }

    @ConformanceStep("deleteOrderCounting")
    public boolean deleteOrderCounting(String clusterFile, byte[] subspace, long orderID, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createCountingMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .createOrOpen();
            return store.deleteRecord(Tuple.from(orderID));
        });
    }

    @ConformanceStep("getRecordCount")
    public long getRecordCount(String clusterFile, byte[] subspace, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createCountingMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .createOrOpen();
            return store.getSnapshotRecordCount().join();
        });
    }
}
