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

/**
 * Java conformance test for FoundationDB Record Layer.
 * This class can read data written by the Go implementation and write data for Go to read.
 */
public class ConformanceTest {
    private static final String SUBSPACE_NAME = "conformance_test";
    private static final FDBDatabase db = FDBDatabaseFactory.instance().getDatabase();
    
    public static void main(String[] args) {
        if (args.length == 0) {
            System.err.println("Usage: ConformanceTest <read|write>");
            System.exit(1);
        }
        
        String mode = args[0];
        
        try {
            if ("read".equals(mode)) {
                readData();
            } else if ("write".equals(mode)) {
                writeData();
            } else {
                System.err.println("Invalid mode: " + mode + ". Use 'read' or 'write'");
                System.exit(1);
            }
        } catch (Exception e) {
            System.err.println("Error: " + e.getMessage());
            e.printStackTrace();
            System.exit(1);
        }
    }
    
    private static void readData() {
        System.out.println("=== JAVA READ DEBUG ===");
        // Create metadata - must match Go implementation exactly
        RecordMetaDataBuilder metaDataBuilder = RecordMetaData.newBuilder()
                .setRecords(RecordLayerDemoProto.getDescriptor());
        
        metaDataBuilder.getRecordType("Order")
                .setPrimaryKey(Key.Expressions.field("order_id"));
        
        RecordMetaData recordMetaData = metaDataBuilder.build();
        
        // Create subspace - must match Go implementation
        Subspace subspace = new Subspace(Tuple.from(SUBSPACE_NAME));
        System.out.println("Java subspace bytes: " + java.util.Arrays.toString(subspace.pack()));
        
        // Read the data Go wrote
        FDBStoredRecord<Message> storedRecord = db.run(context -> {
            FDBRecordStore recordStore = FDBRecordStore.newBuilder()
                    .setMetaDataProvider(recordMetaData)
                    .setContext(context)
                    .setSubspace(subspace)
                    .open();
            
            // Try to load order ID 1001 (what Go writes)
            Tuple primaryKey = Tuple.from(1001L);
            System.out.println("Java primary key: " + primaryKey);
            System.out.println("Java looking for record with this key structure");
            return recordStore.loadRecord(primaryKey);
        });
        
        if (storedRecord == null) {
            System.out.println("ERROR: No record found with order_id 1001");
            return;
        }
        
        // Parse the protobuf record
        RecordLayerDemoProto.Order order = RecordLayerDemoProto.Order.newBuilder()
                .mergeFrom(storedRecord.getRecord())
                .build();
        
        // Verify the data matches what Go wrote
        if (order.getOrderId() == 1001 && 
            order.getPrice() == 25 && 
            order.getFlower().getType().equals("Rose") &&
            order.getFlower().getColor() == RecordLayerDemoProto.Color.RED) {
            
            System.out.println("SUCCESS: Found order " + order.getOrderId() + 
                             " with price " + order.getPrice() + 
                             " and flower " + order.getFlower().getType() + 
                             " (" + order.getFlower().getColor() + ")");
        } else {
            System.out.println("ERROR: Data mismatch. Got: " + order);
        }
    }
    
    private static void writeData() {
        // Create metadata - must match Go implementation exactly
        RecordMetaDataBuilder metaDataBuilder = RecordMetaData.newBuilder()
                .setRecords(RecordLayerDemoProto.getDescriptor());
        
        metaDataBuilder.getRecordType("Order")
                .setPrimaryKey(Key.Expressions.field("order_id"));
        
        RecordMetaData recordMetaData = metaDataBuilder.build();
        
        // Create subspace - must match Go implementation
        Subspace subspace = new Subspace(Tuple.from(SUBSPACE_NAME));
        
        // Write test data for Go to read
        db.run(context -> {
            FDBRecordStore recordStore = FDBRecordStore.newBuilder()
                    .setMetaDataProvider(recordMetaData)
                    .setContext(context)
                    .setSubspace(subspace)
                    .createOrOpen();
            
            // Create test order - Go will try to read this (order ID 2002)
            RecordLayerDemoProto.Order order = RecordLayerDemoProto.Order.newBuilder()
                    .setOrderId(2002)
                    .setPrice(50)
                    .setFlower(RecordLayerDemoProto.Flower.newBuilder()
                            .setType("Tulip")
                            .setColor(RecordLayerDemoProto.Color.BLUE))
                    .build();
            
            recordStore.saveRecord(order);
            System.out.println("Java wrote order: " + order.getOrderId());
            return null;
        });
    }
}