package com.birdayz.conformance;

import com.apple.foundationdb.record.RecordMetaData;
import com.apple.foundationdb.record.RecordMetaDataBuilder;
import com.apple.foundationdb.record.provider.foundationdb.FDBDatabase;
import com.apple.foundationdb.record.provider.foundationdb.FDBDatabaseFactory;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.record.provider.foundationdb.FDBStoredRecord;
import com.apple.foundationdb.record.metadata.Key;
import com.apple.foundationdb.subspace.Subspace;
import com.apple.foundationdb.tuple.Tuple;
import com.google.protobuf.Message;

import com.apple.foundationdb.record.RecordLayerDemo;
import com.apple.foundationdb.record.RecordLayerDemo.Order;

import java.io.File;
import java.io.FileWriter;
import java.io.IOException;
import java.nio.file.Files;

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
     * Save an order record to the specified subspace.
     *
     * @param clusterFile The FDB cluster file content
     * @param subspace The subspace bytes to use for the record store
     * @param order The order to save
     */
    @ConformanceStep("saveOrder")
    public void saveOrder(String clusterFile, byte[] subspace, Order order) {
        FDBDatabase db = createDatabase(clusterFile);
        RecordMetaData metaData = createMetaData();
        Subspace sub = new Subspace(subspace);

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

    /**
     * Load an order record from the specified subspace.
     *
     * @param clusterFile The FDB cluster file content
     * @param subspace The subspace bytes to use for the record store
     * @param orderID The order ID to load
     * @return The loaded order
     * @throws RuntimeException if record not found
     */
    @ConformanceStep("loadOrder")
    public Order loadOrder(String clusterFile, byte[] subspace, long orderID) {
        FDBDatabase db = createDatabase(clusterFile);
        RecordMetaData metaData = createMetaData();
        Subspace sub = new Subspace(subspace);

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
     *
     * @param clusterFile The FDB cluster file content
     * @param subspace The subspace bytes to use for the record store
     * @param orderID The order ID to check
     * @return true if the record exists, false otherwise
     */
    @ConformanceStep("recordExists")
    public boolean recordExists(String clusterFile, byte[] subspace, long orderID) {
        FDBDatabase db = createDatabase(clusterFile);
        RecordMetaData metaData = createMetaData();
        Subspace sub = new Subspace(subspace);

        return db.run(context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metaData)
                .setContext(context)
                .setSubspace(sub)
                .open();

            return store.loadRecord(Tuple.from(orderID)) != null;
        });
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
