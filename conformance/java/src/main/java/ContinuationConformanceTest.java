import com.apple.foundationdb.Database;
import com.apple.foundationdb.FDB;
import com.apple.foundationdb.record.RecordCursor;
import com.apple.foundationdb.record.RecordCursorResult;
import com.apple.foundationdb.record.ScanProperties;
import com.apple.foundationdb.record.ExecuteProperties;
import com.apple.foundationdb.record.provider.foundationdb.FDBDatabase;
import com.apple.foundationdb.record.provider.foundationdb.FDBDatabaseFactory;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordContext;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.record.provider.foundationdb.FDBStoredRecord;
import com.apple.foundationdb.record.RecordMetaData;
import com.apple.foundationdb.record.RecordMetaDataBuilder;
import com.apple.foundationdb.subspace.Subspace;
import com.apple.foundationdb.tuple.Tuple;
import com.google.protobuf.Message;

import com.apple.foundationdb.record.RecordLayerDemo;
import com.apple.foundationdb.record.RecordLayerDemo.Order;
import com.apple.foundationdb.record.RecordLayerDemo.Flower;
import com.apple.foundationdb.record.RecordLayerDemo.Color;

import java.util.concurrent.CompletableFuture;

public class ContinuationConformanceTest {
    
    // Helper class to return both continuation and record count
    static class ReadResult {
        final byte[] continuation;
        final long recordsRead;
        
        ReadResult(byte[] continuation, long recordsRead) {
            this.continuation = continuation;
            this.recordsRead = recordsRead;
        }
    }
    
    // Simple hex utility for Java 11 compatibility
    private static byte[] parseHex(String hex) {
        if (hex.length() % 2 != 0) {
            throw new IllegalArgumentException("Invalid hex string");
        }
        byte[] result = new byte[hex.length() / 2];
        for (int i = 0; i < result.length; i++) {
            result[i] = (byte) Integer.parseInt(hex.substring(2 * i, 2 * i + 2), 16);
        }
        return result;
    }
    
    private static String toHex(byte[] bytes) {
        StringBuilder sb = new StringBuilder();
        for (byte b : bytes) {
            sb.append(String.format("%02X", b));
        }
        return sb.toString();
    }
    
    public static void main(String[] args) throws Exception {
        if (args.length != 5) {
            System.err.println("Usage: ContinuationConformanceTest <subspace_hex> <continuation_hex> <limit> <expected_start_id> <expected_end_id>");
            System.exit(1);
        }
        
        // Parse arguments
        byte[] subspaceBytes = parseHex(args[0]);
        byte[] continuationBytes = args[1].equals("") ? null : parseHex(args[1]);
        int limit = Integer.parseInt(args[2]);
        long expectedStartId = Long.parseLong(args[3]);
        long expectedEndId = Long.parseLong(args[4]);
        
        // Initialize FDB
        FDB fdb = FDB.selectAPIVersion(630);
        FDBDatabaseFactory factory = FDBDatabaseFactory.instance();
        FDBDatabase fdbDatabase = factory.getDatabase();
        
        // Create metadata
        RecordMetaDataBuilder metaDataBuilder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        metaDataBuilder.getRecordType("Order").setPrimaryKey(com.apple.foundationdb.record.metadata.Key.Expressions.field("order_id"));
        RecordMetaData metaData = metaDataBuilder.build();
        
        // Run in transaction
        CompletableFuture<ReadResult> result = fdbDatabase.runAsync(context -> 
            readWithContinuation(context, metaData, subspaceBytes, continuationBytes, limit, expectedStartId, expectedEndId)
        );
        
        ReadResult readResult = result.get();
        
        // Output is parsed by Go test
        if (readResult.continuation != null) {
            System.out.println("CONTINUATION: " + toHex(readResult.continuation));
        } else {
            System.out.println("CONTINUATION: null");
        }
        // This line must be last - output actual records read
        System.out.println("RECORDS_READ: " + readResult.recordsRead);
    }
    
    private static CompletableFuture<ReadResult> readWithContinuation(
            FDBRecordContext context,
            RecordMetaData metaData,
            byte[] subspaceBytes,
            byte[] continuationBytes,
            int limit,
            long expectedStartId,
            long expectedEndId) {
        
        try {
            // Create subspace from bytes
            Subspace subspace = new Subspace(subspaceBytes);
            
            // Open store
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setContext(context)
                .setSubspace(subspace)
                .setMetaDataProvider(metaData)
                .open();
            
            // Create scan properties with limit
            ScanProperties scanProps = new ScanProperties(
                ExecuteProperties.newBuilder()
                    .setReturnedRowLimit(limit)
                    .build()
            );
            
            // Debug: Log continuation info
            if (continuationBytes != null) {
                System.err.println("DEBUG: Using continuation of " + continuationBytes.length + " bytes: " + toHex(continuationBytes));
            } else {
                System.err.println("DEBUG: No continuation (starting from beginning)");
            }
            
            // Scan with continuation
            RecordCursor<FDBStoredRecord<Message>> cursor = store.scanRecords(
                continuationBytes,
                scanProps
            );
            
            // Read and validate records
            long currentId = expectedStartId;
            byte[] lastContinuation = null;
            long recordsRead = 0;
            
            RecordCursorResult<FDBStoredRecord<Message>> result;
            while ((result = cursor.onNext().get()).hasNext()) {
                FDBStoredRecord<Message> storedRecord = result.get();
                // Use mergeFrom pattern from Getting Started guide to handle DynamicMessage
                Order order = Order.newBuilder()
                    .mergeFrom(storedRecord.getRecord())
                    .build();
                
                // Validate ID sequence 
                if (order.getOrderId() != currentId) {
                    throw new RuntimeException(String.format(
                        "ID mismatch: expected %d, got %d", currentId, order.getOrderId()
                    ));
                }
                
                // Validate content
                validateOrderContent(order, currentId);
                
                currentId++;
                recordsRead++;
            }
            
            // Always get the final continuation state - this is what Java Record Layer returns
            if (result.getContinuation() != null) {
                lastContinuation = result.getContinuation().toBytes();
            }
            
            // Verify we read the expected range
            if (expectedStartId >= 0 && expectedEndId >= 0 && currentId - 1 != expectedEndId) {
                throw new RuntimeException(String.format(
                    "Expected to read up to ID %d, but only read up to %d", 
                    expectedEndId, currentId - 1
                ));
            }
            
            return CompletableFuture.completedFuture(new ReadResult(lastContinuation, recordsRead));
            
        } catch (Exception e) {
            e.printStackTrace();
            throw new RuntimeException("Failed to read records", e);
        }
    }
    
    private static void validateOrderContent(Order order, long expectedId) {
        // Validate price
        long expectedPrice = expectedId * 10 + expectedId % 7;
        if (order.getPrice() != expectedPrice) {
            throw new RuntimeException(String.format(
                "Price mismatch for record %d: expected %d, got %d",
                expectedId, expectedPrice, order.getPrice()
            ));
        }
        
        // Validate flower type
        String expectedFlowerType = String.format("flower_%04d", expectedId);
        if (!order.getFlower().getType().equals(expectedFlowerType)) {
            throw new RuntimeException(String.format(
                "Flower type mismatch for record %d: expected %s, got %s",
                expectedId, expectedFlowerType, order.getFlower().getType()
            ));
        }
        
        // Validate color (RED=1, YELLOW=2, BLUE=3, rotating)
        Color expectedColor = Color.forNumber((int)(expectedId % 3) + 1);
        if (order.getFlower().getColor() != expectedColor) {
            throw new RuntimeException(String.format(
                "Color mismatch for record %d: expected %s, got %s",
                expectedId, expectedColor, order.getFlower().getColor()
            ));
        }
    }
}