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
import java.nio.file.Files;
import java.nio.charset.StandardCharsets;

/**
 * Conformance test steps that can be invoked from Go via the generic invokeJava() function.
 * Each method is annotated with @ConformanceStep and can be called by name with JSON args.
 */
public class ConformanceSteps {

    /**
     * Create an FDBDatabase instance using the provided cluster file content.
     *
     * @param clusterFileContent The cluster file content as a string
     * @return FDBDatabase configured with the cluster file
     * @throws RuntimeException if cluster file cannot be created
     */
    private static FDBDatabase createDatabase(String clusterFileContent) {
        try {
            // Create a temporary file for the cluster file
            File tempFile = File.createTempFile("fdb_cluster_", ".cluster");
            tempFile.deleteOnExit();

            // Write the cluster file content
            try (FileWriter writer = new FileWriter(tempFile)) {
                writer.write(clusterFileContent);
            }

            // Create database with the cluster file
            return FDBDatabaseFactory.instance().getDatabase(tempFile.getAbsolutePath());
        } catch (IOException e) {
            throw new RuntimeException("Failed to create cluster file: " + e.getMessage(), e);
        }
    }

    /**
     * Create an FDBRecordContext from a tenant transaction using reflection.
     * This is needed because FDBRecordContext constructor is protected and doesn't
     * have built-in tenant support.
     *
     * @param db The FDBDatabase instance
     * @param transaction The tenant transaction
     * @return FDBRecordContext wrapping the tenant transaction
     * @throws RuntimeException if reflection fails
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

            // Create with default config and null timer
            FDBRecordContextConfig config = FDBRecordContextConfig.newBuilder().build();
            return constructor.newInstance(db, transaction, config, null);
        } catch (Exception e) {
            throw new RuntimeException("Failed to create FDBRecordContext from transaction: " + e.getMessage(), e);
        }
    }

    /**
     * Save an order record to the specified subspace.
     * Supports optional tenant isolation.
     *
     * @param clusterFile The FDB cluster file content
     * @param subspace The subspace bytes to use for the record store
     * @param order The order to save
     * @param tenantName Optional tenant name for isolation (can be null)
     */
    @ConformanceStep("saveOrder")
    public void saveOrder(String clusterFile, byte[] subspace, Order order, String tenantName) {
        FDBDatabase db = createDatabase(clusterFile);
        RecordMetaData metaData = createMetaData();
        Subspace sub = new Subspace(subspace);

        if (tenantName != null && !tenantName.isEmpty()) {
            // Use tenant for isolation
            Database nativeDb = db.database();
            Tenant tenant = nativeDb.openTenant(tenantName.getBytes(StandardCharsets.UTF_8));
            tenant.run(transaction -> {
                FDBRecordContext context = createContextFromTransaction(db, transaction);
                FDBRecordStore store = FDBRecordStore.newBuilder()
                    .setMetaDataProvider(metaData)
                    .setContext(context)
                    .setSubspace(sub)
                    .createOrOpen();

                store.saveRecord(order);
                return null;
            });
        } else {
            // Use database directly (legacy mode)
            db.run(context -> {
                FDBRecordStore store = FDBRecordStore.newBuilder()
                    .setMetaDataProvider(metaData)
                    .setContext(context)
                    .setSubspace(sub)
                    .createOrOpen();

                store.saveRecord(order);
                return null;
            });
        }
    }

    /**
     * Load an order record from the specified subspace.
     * Supports optional tenant isolation.
     *
     * @param clusterFile The FDB cluster file content
     * @param subspace The subspace bytes to use for the record store
     * @param orderID The order ID to load
     * @param tenantName Optional tenant name for isolation (can be null)
     * @return The loaded order
     * @throws RuntimeException if record not found
     */
    @ConformanceStep("loadOrder")
    public Order loadOrder(String clusterFile, byte[] subspace, long orderID, String tenantName) {
        FDBDatabase db = createDatabase(clusterFile);
        RecordMetaData metaData = createMetaData();
        Subspace sub = new Subspace(subspace);

        if (tenantName != null && !tenantName.isEmpty()) {
            // Use tenant for isolation
            Database nativeDb = db.database();
            Tenant tenant = nativeDb.openTenant(tenantName.getBytes(StandardCharsets.UTF_8));
            return tenant.run(transaction -> {
                FDBRecordContext context = createContextFromTransaction(db, transaction);
                FDBRecordStore store = FDBRecordStore.newBuilder()
                    .setMetaDataProvider(metaData)
                    .setContext(context)
                    .setSubspace(sub)
                    .open();

                FDBStoredRecord<Message> record = store.loadRecord(Tuple.from(orderID));
                if (record == null) {
                    throw new RuntimeException("Record not found: " + orderID);
                }

                // The Record Layer stores the primary key separately from the protobuf message
                // We need to explicitly set the order_id from the primary key
                return Order.newBuilder()
                    .mergeFrom(record.getRecord())
                    .setOrderId(orderID)  // Set order_id from primary key
                    .build();
            });
        } else {
            // Use database directly (legacy mode)
            return db.run(context -> {
                FDBRecordStore store = FDBRecordStore.newBuilder()
                    .setMetaDataProvider(metaData)
                    .setContext(context)
                    .setSubspace(sub)
                    .open();

                FDBStoredRecord<Message> record = store.loadRecord(Tuple.from(orderID));
                if (record == null) {
                    throw new RuntimeException("Record not found: " + orderID);
                }

                // The Record Layer stores the primary key separately from the protobuf message
                // We need to explicitly set the order_id from the primary key
                return Order.newBuilder()
                    .mergeFrom(record.getRecord())
                    .setOrderId(orderID)  // Set order_id from primary key
                    .build();
            });
        }
    }

    /**
     * Delete an order record from the specified subspace.
     *
     * @param clusterFile The FDB cluster file content
     * @param subspace The subspace bytes to use for the record store
     * @param orderID The order ID to delete
     * @return true if the record was deleted, false if it didn't exist
     */
    @ConformanceStep("deleteOrder")
    public boolean deleteOrder(String clusterFile, byte[] subspace, long orderID) {
        FDBDatabase db = createDatabase(clusterFile);
        RecordMetaData metaData = createMetaData();
        Subspace sub = new Subspace(subspace);

        return db.run(context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metaData)
                .setContext(context)
                .setSubspace(sub)
                .open();

            return store.deleteRecord(Tuple.from(orderID));
        });
    }

    /**
     * Check if an order record exists in the specified subspace.
     * Supports optional tenant isolation.
     *
     * @param clusterFile The FDB cluster file content
     * @param subspace The subspace bytes to use for the record store
     * @param orderID The order ID to check
     * @param tenantName Optional tenant name for isolation (can be null)
     * @return true if the record exists, false otherwise
     */
    @ConformanceStep("recordExists")
    public boolean recordExists(String clusterFile, byte[] subspace, long orderID, String tenantName) {
        FDBDatabase db = createDatabase(clusterFile);
        RecordMetaData metaData = createMetaData();
        Subspace sub = new Subspace(subspace);

        if (tenantName != null && !tenantName.isEmpty()) {
            // Use tenant for isolation
            Database nativeDb = db.database();
            Tenant tenant = nativeDb.openTenant(tenantName.getBytes(StandardCharsets.UTF_8));
            return tenant.run(transaction -> {
                FDBRecordContext context = createContextFromTransaction(db, transaction);
                FDBRecordStore store = FDBRecordStore.newBuilder()
                    .setMetaDataProvider(metaData)
                    .setContext(context)
                    .setSubspace(sub)
                    .open();

                return store.loadRecord(Tuple.from(orderID)) != null;
            });
        } else {
            // Use database directly (legacy mode)
            return db.run(context -> {
                FDBRecordStore store = FDBRecordStore.newBuilder()
                    .setMetaDataProvider(metaData)
                    .setContext(context)
                    .setSubspace(sub)
                    .open();

                return store.loadRecord(Tuple.from(orderID)) != null;
            });
        }
    }

    /**
     * Create the record metadata for the Order record type.
     *
     * @return RecordMetaData configured for Order records
     */
    private static RecordMetaData createMetaData() {
        RecordMetaDataBuilder metaDataBuilder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());

        metaDataBuilder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("order_id"));

        return metaDataBuilder.build();
    }
}
