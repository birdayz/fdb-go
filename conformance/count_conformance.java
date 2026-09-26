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

    /**
     * getSnapshotRecordCountForRecordType("Order") after saving orders 1..3 and
     * customer 9 into a fresh store whose meta-data mode selects: "countKey"
     * (record count key grouped by record type, no COUNT index), "typeIndex" (an
     * ungrouped COUNT index on Order) or "universalByType" (a universal COUNT
     * index grouped by record type). Reports the count, or the root exception.
     */
    @ConformanceStep("perTypeRecordCountJava")
    public java.util.Map<String, Object> perTypeRecordCountJava(String clusterFile, byte[] subspace, String mode) {
        RecordMetaDataBuilder builder = RecordMetaData.newBuilder().setRecords(RecordLayerDemo.getDescriptor());
        builder.getRecordType("Order").setPrimaryKey(Key.Expressions.field("order_id"));
        builder.getRecordType("Customer").setPrimaryKey(Key.Expressions.field("customer_id"));
        builder.getRecordType("TypedRecord").setPrimaryKey(Key.Expressions.field("id"));
        switch (mode) {
            case "countKey":
                builder.setRecordCountKey(Key.Expressions.recordType());
                break;
            case "typeIndex":
                builder.addIndex("Order", new com.apple.foundationdb.record.metadata.Index("order_count",
                        new com.apple.foundationdb.record.metadata.expressions.GroupingKeyExpression(
                                com.apple.foundationdb.record.metadata.expressions.EmptyKeyExpression.EMPTY, 0),
                        com.apple.foundationdb.record.metadata.IndexTypes.COUNT));
                break;
            case "universalByType":
                builder.addUniversalIndex(new com.apple.foundationdb.record.metadata.Index("count_by_type",
                        new com.apple.foundationdb.record.metadata.expressions.GroupingKeyExpression(Key.Expressions.recordType(), 0),
                        com.apple.foundationdb.record.metadata.IndexTypes.COUNT));
                break;
            default:
                throw new IllegalArgumentException(mode);
        }
        final RecordMetaData md = builder.build();
        final java.util.Map<String, Object> out = new java.util.HashMap<>();
        try {
            final long n = runInContext(clusterFile, null, context -> {
                FDBRecordStore store = FDBRecordStore.newBuilder().setMetaDataProvider(md).setContext(context)
                        .setSubspace(new Subspace(subspace)).createOrOpen();
                for (long i = 1; i <= 3; i++) {
                    store.saveRecord(Order.newBuilder().setOrderId(i).build());
                }
                store.saveRecord(RecordLayerDemo.Customer.newBuilder().setCustomerId(9).build());
                return store.getSnapshotRecordCountForRecordType("Order").join();
            });
            out.put("count", n);
            out.put("class", "");
            out.put("error", "");
        } catch (RuntimeException ex) {
            Throwable t = ex;
            while (t instanceof java.util.concurrent.CompletionException && t.getCause() != null) {
                t = t.getCause();
            }
            out.put("count", -1L);
            out.put("class", t.getClass().getName());
            out.put("error", String.valueOf(t.getMessage()));
        }
        return out;
    }
}
