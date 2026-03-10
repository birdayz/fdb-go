package com.birdayz.conformance;

import com.apple.foundationdb.record.RecordMetaData;
import com.apple.foundationdb.record.RecordMetaDataBuilder;
import com.apple.foundationdb.record.metadata.Key;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordVersion;
import com.apple.foundationdb.record.provider.foundationdb.FDBStoredRecord;
import com.apple.foundationdb.record.RecordLayerDemo;
import com.apple.foundationdb.record.RecordLayerDemo.Order;
import com.apple.foundationdb.subspace.Subspace;
import com.apple.foundationdb.tuple.Tuple;
import com.google.protobuf.Message;

import java.util.Base64;
import java.util.HashMap;
import java.util.Map;

class VersionSteps extends ConformanceBase {
    private static RecordMetaData createVersionedMetaData() {
        RecordMetaDataBuilder builder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        builder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("order_id"));
        builder.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.field("customer_id"));
        builder.setStoreRecordVersions(true);
        return builder.build();
    }

    @ConformanceStep("saveOrderVersioned")
    public void saveOrderVersioned(String clusterFile, byte[] subspace, Order order, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createVersionedMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .createOrOpen();
            store.saveRecord(order);
            return null;
        });
    }

    @ConformanceStep("loadOrderWithVersion")
    public Map<String, Object> loadOrderWithVersion(String clusterFile, byte[] subspace, long orderID, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createVersionedMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .open();

            FDBStoredRecord<Message> record = store.loadRecord(Tuple.from(orderID));
            if (record == null) {
                throw new RuntimeException("Record not found: " + orderID);
            }

            Order order = Order.newBuilder().mergeFrom(record.getRecord()).build();

            Map<String, Object> result = new HashMap<>();
            result.put("orderId", order.getOrderId());
            if (order.hasPrice()) {
                result.put("price", order.getPrice());
            }

            if (record.hasVersion()) {
                FDBRecordVersion version = record.getVersion();
                result.put("hasVersion", true);
                result.put("globalVersion", Base64.getEncoder().encodeToString(version.getGlobalVersion()));
                result.put("localVersion", version.getLocalVersion());
                result.put("isComplete", version.isComplete());
            } else {
                result.put("hasVersion", false);
            }

            return result;
        });
    }
}
