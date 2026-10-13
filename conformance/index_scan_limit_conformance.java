package com.birdayz.conformance;

import com.apple.foundationdb.record.CursorStreamingMode;
import com.apple.foundationdb.record.ExecuteProperties;
import com.apple.foundationdb.record.IndexEntry;
import com.apple.foundationdb.record.IndexScanType;
import com.apple.foundationdb.record.RecordCursor;
import com.apple.foundationdb.record.RecordCursorResult;
import com.apple.foundationdb.record.RecordLayerDemo;
import com.apple.foundationdb.record.RecordMetaData;
import com.apple.foundationdb.record.RecordMetaDataBuilder;
import com.apple.foundationdb.record.ScanProperties;
import com.apple.foundationdb.record.TupleRange;
import com.apple.foundationdb.record.metadata.Index;
import com.apple.foundationdb.record.metadata.IndexTypes;
import com.apple.foundationdb.record.metadata.Key;
import com.apple.foundationdb.record.provider.foundationdb.FDBIndexedRecord;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordContext;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.record.provider.foundationdb.IndexOrphanBehavior;
import com.apple.foundationdb.record.provider.foundationdb.KeyValueCursor;
import com.apple.foundationdb.subspace.Subspace;
import com.google.protobuf.Message;

import java.util.ArrayList;
import java.util.Base64;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

/** Ordinary VALUE/VERSION cursor boundaries; deliberately no remote-fetch APIs. */
class IndexScanLimitSteps extends ConformanceBase {
    public static class Request {
        public String indexName = "price";
        public String streaming = "ITERATOR";
        public boolean records;
        public boolean reverse;
        public int rowLimit;
        public int scannedLimit;
        public long byteLimit;
        public long timeLimit;
        public boolean failOnScanLimit;
        public String continuation;
    }

    private static String bytes(byte[] value) {
        return value == null ? "" : Base64.getEncoder().encodeToString(value);
    }

    private static RecordMetaData metadata() {
        RecordMetaDataBuilder builder = RecordMetaData.newBuilder().setRecords(RecordLayerDemo.getDescriptor());
        builder.getRecordType("Order").setPrimaryKey(Key.Expressions.field("order_id"));
        builder.getRecordType("Customer").setPrimaryKey(Key.Expressions.field("customer_id"));
        builder.getRecordType("TypedRecord").setPrimaryKey(Key.Expressions.field("id"));
        builder.setStoreRecordVersions(true);
        Index price = new Index("price", Key.Expressions.field("price"));
        Index version = new Index("version", Key.Expressions.version(), IndexTypes.VERSION);
        price.setSubspaceKey("price");
        version.setSubspaceKey("version");
        builder.addIndex("Order", price);
        builder.addIndex("Order", version);
        return builder.build();
    }

    private static FDBRecordStore open(FDBRecordContext context, byte[] subspace) {
        return FDBRecordStore.newBuilder().setContext(context).setSubspace(new Subspace(subspace))
                .setMetaDataProvider(metadata()).setUserVersionChecker(ALWAYS_READABLE_CHECKER).setFormatVersion(14).createOrOpen();
    }

    @ConformanceStep("seedIndexScanLimits")
    public int seed(String clusterFile, String tenantName, byte[] subspace, int count) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = open(context, subspace);
            for (int id = 0; id < count; id++) {
                store.saveRecord(RecordLayerDemo.Order.newBuilder().setOrderId(id).setPrice(100 - id)
                        .addTags(id % 2 == 0 ? "even" : "odd").build());
            }
            return count;
        });
    }

    private static Map<String, Object> terminal(RecordCursorResult<?> next, ExecuteProperties properties) {
        return Map.of("reason", next.getNoNextReason().name(), "continuation", bytes(next.getContinuation().toBytes()),
                "end", next.getContinuation().isEnd(), "scannedRecords", properties.getState().getRecordsScanned(),
                "scannedBytes", properties.getState().getBytesScanned());
    }

    private static Map<String, Object> row(RecordCursorResult<?> next) {
        Object value = next.get();
        IndexEntry entry;
        String data = "";
        String version = "";
        if (value instanceof FDBIndexedRecord) {
            @SuppressWarnings("unchecked")
            FDBIndexedRecord<Message> indexed = (FDBIndexedRecord<Message>) value;
            entry = indexed.getIndexEntry();
            data = bytes(indexed.getStoredRecord().getRecord().toByteArray());
            if (indexed.getStoredRecord().hasVersion()) {
                version = bytes(indexed.getStoredRecord().getVersion().toBytes());
            }
        } else {
            entry = (IndexEntry) value;
        }
        return Map.of("indexKey", bytes(entry.getKey().pack()), "indexValue", bytes(entry.getValue().pack()),
                "primaryKey", bytes(entry.getPrimaryKey().pack()), "data", data, "version", version,
                "continuation", bytes(next.getContinuation().toBytes()));
    }

    @ConformanceStep("scanIndexScanLimits")
    public Map<String, Object> scan(String clusterFile, String tenantName, byte[] subspace, Request request) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = open(context, subspace);
            // The clock belongs to the transaction, not the subsequently-created ExecuteState.
            if (request.timeLimit > 0) {
                try {
                    Thread.sleep(request.timeLimit + 25);
                } catch (InterruptedException error) {
                    Thread.currentThread().interrupt();
                    throw new RuntimeException(error);
                }
            }
            ExecuteProperties.Builder builder = ExecuteProperties.newBuilder().setReturnedRowLimit(request.rowLimit)
                    .setDefaultCursorStreamingMode(CursorStreamingMode.valueOf(request.streaming))
                    .setFailOnScanLimitReached(request.failOnScanLimit);
            if (request.scannedLimit > 0) builder.setScannedRecordsLimit(request.scannedLimit);
            if (request.byteLimit > 0) builder.setScannedBytesLimit(request.byteLimit);
            if (request.timeLimit > 0) builder.setTimeLimit(request.timeLimit);
            ExecuteProperties properties = builder.build();
            ScanProperties scanProperties = new ScanProperties(properties, request.reverse);
            Index index = store.getRecordMetaData().getIndex(request.indexName);
            byte[] continuation = request.continuation == null || request.continuation.isEmpty()
                    ? null : Base64.getDecoder().decode(request.continuation);
            KeyValueCursor.Builder leafBuilder = KeyValueCursor.Builder.newBuilder(store.indexSubspace(index))
                    .setContext(context).setRange(TupleRange.ALL).setContinuation(continuation).setScanProperties(scanProperties);
            List<Map<String, Object>> rangeOptions;
            try (KeyValueCursor leaf = leafBuilder.build()) {
                // Native Java and Go streaming enum values differ by one.
                rangeOptions = List.of(Map.of("mode", leafBuilder.getStreamingMode().code() + 1,
                        "limit", leafBuilder.getLimit(), "reverse", request.reverse));
            }
            RecordCursor<?> cursor = request.records
                    ? store.scanIndexRecords(index, IndexScanType.BY_VALUE, TupleRange.ALL, continuation,
                            IndexOrphanBehavior.ERROR, scanProperties)
                    : store.scanIndex(index, IndexScanType.BY_VALUE, TupleRange.ALL, continuation, scanProperties);
            Map<String, Object> result = new HashMap<>();
            List<Map<String, Object>> rows = new ArrayList<>();
            List<Map<String, Object>> repeats = new ArrayList<>();
            result.put("rows", rows);
            result.put("repeats", repeats);
            result.put("rangeOptions", rangeOptions);
            result.put("reason", "");
            result.put("continuation", "");
            result.put("end", false);
            result.put("exceptionClass", "");
            try (RecordCursor<?> closing = cursor) {
                while (true) {
                    RecordCursorResult<?> next = cursor.onNext().join();
                    if (next.hasNext()) {
                        rows.add(row(next));
                    } else {
                        result.putAll(terminal(next, properties));
                        for (int repeat = 0; repeat < 2; repeat++) {
                            repeats.add(terminal(cursor.onNext().join(), properties));
                        }
                        break;
                    }
                }
            } catch (RuntimeException error) {
                Throwable root = error;
                while (root.getCause() != null) root = root.getCause();
                result.put("exceptionClass", root.getClass().getSimpleName());
            }
            result.put("scannedRecords", properties.getState().getRecordsScanned());
            result.put("scannedBytes", properties.getState().getBytesScanned());
            return result;
        });
    }
}
