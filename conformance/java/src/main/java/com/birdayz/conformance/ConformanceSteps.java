package com.birdayz.conformance;

import com.apple.foundationdb.record.RecordMetaData;
import com.apple.foundationdb.record.RecordMetaDataBuilder;
import com.apple.foundationdb.record.provider.foundationdb.FDBDatabase;
import com.apple.foundationdb.record.provider.foundationdb.FDBDatabaseFactory;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.record.provider.foundationdb.FDBStoredRecord;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordContext;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordContextConfig;
import com.apple.foundationdb.record.metadata.Key;
import com.apple.foundationdb.subspace.Subspace;
import com.apple.foundationdb.tuple.Tuple;
import com.apple.foundationdb.Database;
import com.apple.foundationdb.Tenant;
import com.apple.foundationdb.Transaction;
import com.google.protobuf.Message;

import com.apple.foundationdb.record.RecordLayerDemo;
import com.apple.foundationdb.record.RecordLayerDemo.Order;

import java.io.File;
import java.io.FileWriter;
import java.io.IOException;
import java.lang.reflect.Constructor;
import java.nio.charset.StandardCharsets;

/**
 * Conformance test steps that can be invoked from Go via the generic invokeJava() function.
 * Each method is annotated with @ConformanceStep and can be called by name with JSON args.
 */
public class ConformanceSteps {

    private static String cachedClusterContent = null;
    private static FDBDatabase cachedDatabase = null;

    @FunctionalInterface
    private interface ContextAction<T> {
        T execute(FDBRecordContext context);
    }

    /**
     * Run an action within an FDBRecordContext, handling tenant vs non-tenant branching.
     *
     * @param clusterFile The FDB cluster file content
     * @param tenantName Optional tenant name for isolation (null or empty for no tenant)
     * @param action The action to execute with the context
     * @return The result of the action
     */
    private <T> T runInContext(String clusterFile, String tenantName, ContextAction<T> action) {
        FDBDatabase db = createDatabase(clusterFile);
        if (tenantName != null && !tenantName.isEmpty()) {
            Database nativeDb = db.database();
            Tenant tenant = nativeDb.openTenant(tenantName.getBytes(StandardCharsets.UTF_8));
            return tenant.run(transaction -> {
                FDBRecordContext context = createContextFromTransaction(db, transaction);
                return action.execute(context);
            });
        } else {
            return db.run(context -> action.execute(context));
        }
    }

    /**
     * Create an FDBDatabase instance using the provided cluster file content.
     * Caches the database and cluster file to avoid leaking connections and temp files.
     *
     * @param clusterFileContent The cluster file content as a string
     * @return FDBDatabase configured with the cluster file
     * @throws RuntimeException if cluster file cannot be created
     */
    private static synchronized FDBDatabase createDatabase(String clusterFileContent) {
        if (cachedDatabase != null && clusterFileContent.equals(cachedClusterContent)) {
            return cachedDatabase;
        }
        try {
            File tempFile = new File("/tmp/fdb_conformance.cluster");
            try (FileWriter writer = new FileWriter(tempFile)) {
                writer.write(clusterFileContent);
            }
            cachedClusterContent = clusterFileContent;
            cachedDatabase = FDBDatabaseFactory.instance().getDatabase(tempFile.getAbsolutePath());
            return cachedDatabase;
        } catch (IOException e) {
            throw new RuntimeException("Failed to create cluster file: " + e.getMessage(), e);
        }
    }

    /**
     * Create an FDBRecordContext from a tenant transaction using reflection.
     * This is needed because FDBRecordContext constructor is protected and doesn't
     * have built-in tenant support.
     */
    private static FDBRecordContext createContextFromTransaction(FDBDatabase db, Transaction transaction) {
        try {
            Constructor<FDBRecordContext> constructor = FDBRecordContext.class.getDeclaredConstructor(
                FDBDatabase.class,
                Transaction.class,
                FDBRecordContextConfig.class,
                com.apple.foundationdb.record.provider.foundationdb.FDBStoreTimer.class
            );
            constructor.setAccessible(true);
            FDBRecordContextConfig config = FDBRecordContextConfig.newBuilder().build();
            return constructor.newInstance(db, transaction, config, null);
        } catch (Exception e) {
            throw new RuntimeException("Failed to create FDBRecordContext from transaction: " + e.getMessage(), e);
        }
    }

    @ConformanceStep("saveOrder")
    public void saveOrder(String clusterFile, byte[] subspace, Order order, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .createOrOpen();
            store.saveRecord(order);
            return null;
        });
    }

    @ConformanceStep("loadOrder")
    public Order loadOrder(String clusterFile, byte[] subspace, long orderID, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .open();

            FDBStoredRecord<Message> record = store.loadRecord(Tuple.from(orderID));
            if (record == null) {
                throw new RuntimeException("Record not found: " + orderID);
            }

            return Order.newBuilder()
                .mergeFrom(record.getRecord())
                .setOrderId(orderID)
                .build();
        });
    }

    @ConformanceStep("deleteOrder")
    public boolean deleteOrder(String clusterFile, byte[] subspace, long orderID, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .open();
            return store.deleteRecord(Tuple.from(orderID));
        });
    }

    @ConformanceStep("recordExists")
    public boolean recordExists(String clusterFile, byte[] subspace, long orderID, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .open();
            return store.loadRecord(Tuple.from(orderID)) != null;
        });
    }

    private static RecordMetaData createMetaData() {
        RecordMetaDataBuilder metaDataBuilder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        metaDataBuilder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("order_id"));
        metaDataBuilder.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.field("customer_id"));
        return metaDataBuilder.build();
    }

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
                .setOrderId(orderID)
                .build();
        });
    }
}
