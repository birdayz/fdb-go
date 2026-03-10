package com.birdayz.conformance;

import com.apple.foundationdb.record.RecordMetaData;
import com.apple.foundationdb.record.RecordMetaDataBuilder;
import com.apple.foundationdb.record.metadata.Key;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.record.provider.foundationdb.FDBStoredRecord;
import com.apple.foundationdb.record.RecordLayerDemo;
import com.apple.foundationdb.record.RecordLayerDemo.Order;
import com.apple.foundationdb.subspace.Subspace;
import com.apple.foundationdb.tuple.Tuple;
import com.google.protobuf.Message;

class SplitSteps extends ConformanceBase {
    private static RecordMetaData createSplitMetaData() {
        RecordMetaDataBuilder metaDataBuilder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        metaDataBuilder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("order_id"));
        metaDataBuilder.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.field("customer_id"));
        metaDataBuilder.setSplitLongRecords(true);
        return metaDataBuilder.build();
    }

    @ConformanceStep("saveSplitOrder")
    public void saveSplitOrder(String clusterFile, byte[] subspace, Order order, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createSplitMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .createOrOpen();
            store.saveRecord(order);
            return null;
        });
    }

    @ConformanceStep("loadSplitOrder")
    public Order loadSplitOrder(String clusterFile, byte[] subspace, long orderID, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createSplitMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .open();
            FDBStoredRecord<Message> record = store.loadRecord(Tuple.from(orderID));
            if (record == null) {
                throw new RuntimeException("Record not found: " + orderID);
            }
            return Order.newBuilder()
                .mergeFrom(record.getRecord())
                .build();
        });
    }
}
