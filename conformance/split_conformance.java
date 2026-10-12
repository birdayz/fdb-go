package com.birdayz.conformance;

import com.apple.foundationdb.FDBException;
import com.apple.foundationdb.Tenant;
import com.apple.foundationdb.Transaction;
import com.apple.foundationdb.record.FDBRecordStoreProperties;
import com.apple.foundationdb.record.RecordMetaData;
import com.apple.foundationdb.record.RecordMetaDataBuilder;
import com.apple.foundationdb.record.metadata.Key;
import com.apple.foundationdb.record.provider.foundationdb.FDBDatabase;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordContext;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordContextConfig;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.record.provider.foundationdb.properties.RecordLayerPropertyStorage;
import com.apple.foundationdb.record.provider.foundationdb.FDBStoredRecord;
import com.apple.foundationdb.record.RecordLayerDemo;
import com.apple.foundationdb.record.RecordLayerDemo.Order;
import com.apple.foundationdb.subspace.Subspace;
import com.apple.foundationdb.tuple.Tuple;
import com.google.protobuf.Message;

import java.nio.charset.StandardCharsets;
import java.util.Map;

class SplitSteps extends ConformanceBase {
    private static RecordMetaData createSplitMetaData() {
        RecordMetaDataBuilder metaDataBuilder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        metaDataBuilder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("order_id"));
        metaDataBuilder.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.field("customer_id"));
        metaDataBuilder.getRecordType("TypedRecord")
            .setPrimaryKey(Key.Expressions.field("id"));
        metaDataBuilder.setSplitLongRecords(true);
        return metaDataBuilder.build();
    }

    /** Measures the read conflict without runner retries or a timing-dependent interleaving. */
    @ConformanceStep("splitReadConflictProbe")
    public Map<String, Object> splitReadConflictProbe(String clusterFile, byte[] subspace, String tenantName,
                                                     String operation, boolean existing, boolean viaGets,
                                                     long writerPrimaryKey, long suffix) {
        if (tenantName == null || tenantName.isEmpty()) {
            throw new IllegalArgumentException("split conflict probe requires a tenant");
        }
        FDBDatabase db = createDatabase(clusterFile);
        Subspace root = new Subspace(subspace);
        RecordMetaData metadata = createSplitMetaData();
        FDBRecordContextConfig.Builder configBuilder = FDBRecordContextConfig.newBuilder();
        if (viaGets) {
            configBuilder.setRecordContextProperties(RecordLayerPropertyStorage.newBuilder()
                .addProp(FDBRecordStoreProperties.LOAD_RECORDS_VIA_GETS, true).build());
        }
        FDBRecordContextConfig config = configBuilder.build();
        try (Tenant tenant = db.database().openTenant(tenantName.getBytes(StandardCharsets.UTF_8))) {
            try (FDBRecordContext setup = createContextFromTransaction(db, tenant.createTransaction())) {
                FDBRecordStore store = FDBRecordStore.newBuilder().setContext(setup)
                    .setMetaDataProvider(metadata).setSubspace(root).create();
                if (existing) {
                    store.saveRecord(Order.newBuilder().setOrderId(1L).setPrice(1).build());
                }
                setup.commit();
            }
            try (FDBRecordContext reader = createContextFromTransaction(db, tenant.createTransaction(), config)) {
                FDBRecordStore store = FDBRecordStore.newBuilder().setContext(reader)
                    .setMetaDataProvider(metadata).setSubspace(root).open();
                Tuple primaryKey = Tuple.from(1L);
                boolean operationResult;
                switch (operation) {
                    case "load":
                        operationResult = store.loadRecord(primaryKey) != null;
                        break;
                    case "save":
                        operationResult = store.saveRecord(Order.newBuilder().setOrderId(1L).setPrice(2).build()) != null;
                        break;
                    case "delete":
                        operationResult = store.deleteRecord(primaryKey);
                        break;
                    case "exists":
                        operationResult = store.recordExists(primaryKey);
                        break;
                    default:
                        throw new IllegalArgumentException("unknown split conflict operation: " + operation);
                }
                // A read-only commit would not check conflicts. This key is outside the record range.
                reader.ensureActive().set(root.pack(Tuple.from("reader-marker")), new byte[]{1});
                try (Transaction writer = tenant.createTransaction()) {
                    writer.set(store.recordsSubspace().pack(Tuple.from(writerPrimaryKey, suffix)), new byte[]{42});
                    writer.commit().join();
                }
                int commitCode = 0;
                try {
                    reader.commit();
                } catch (RuntimeException failure) {
                    Throwable cause = failure;
                    while (cause != null && !(cause instanceof FDBException)) {
                        cause = cause.getCause();
                    }
                    if (!(cause instanceof FDBException)) {
                        throw failure;
                    }
                    commitCode = ((FDBException)cause).getCode();
                }
                return Map.of("operationResult", operationResult, "commitCode", commitCode);
            }
        }
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
