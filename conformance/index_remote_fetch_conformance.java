package com.birdayz.conformance;

import com.apple.foundationdb.FDBException;
import com.apple.foundationdb.Tenant;
import com.apple.foundationdb.Transaction;
import com.apple.foundationdb.record.CursorStreamingMode;
import com.apple.foundationdb.record.ExecuteProperties;
import com.apple.foundationdb.record.IndexFetchMethod;
import com.apple.foundationdb.record.IndexScanType;
import com.apple.foundationdb.record.IsolationLevel;
import com.apple.foundationdb.record.RecordCursor;
import com.apple.foundationdb.record.RecordCursorResult;
import com.apple.foundationdb.record.RecordLayerDemo;
import com.apple.foundationdb.record.RecordMetaData;
import com.apple.foundationdb.record.RecordMetaDataBuilder;
import com.apple.foundationdb.record.ScanProperties;
import com.apple.foundationdb.record.TupleRange;
import com.apple.foundationdb.record.metadata.Index;
import com.apple.foundationdb.record.metadata.IndexOptions;
import com.apple.foundationdb.record.metadata.IndexTypes;
import com.apple.foundationdb.record.metadata.Key;
import com.apple.foundationdb.record.provider.foundationdb.FDBIndexedRecord;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordContext;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.record.provider.foundationdb.FDBStoreTimer;
import com.apple.foundationdb.record.provider.foundationdb.FDBStoredRecord;
import com.apple.foundationdb.record.provider.foundationdb.IndexOrphanBehavior;
import com.apple.foundationdb.record.provider.foundationdb.IndexPrefetchRangeKeyValueCursor;
import com.apple.foundationdb.record.provider.foundationdb.IndexScanRange;
import com.apple.foundationdb.record.provider.foundationdb.indexes.StandardIndexMaintainer;
import com.apple.foundationdb.subspace.Subspace;
import com.apple.foundationdb.tuple.Tuple;
import com.google.protobuf.ByteString;
import com.google.protobuf.Message;

import java.lang.reflect.Method;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.Base64;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

/** RFC-258: Record Layer index scans with each IndexFetchMethod (the RemoteFetch* case list). */
class IndexRemoteFetchSteps extends ConformanceBase {
    public static class Request {
        public String shape = "basic";
        public String indexName = "price";
        public String method = "USE_REMOTE_FETCH";
        public String orphan = "ERROR";
        public String range = "all";
        public String streaming = "ITERATOR";
        public String scanType = "BY_VALUE";
        public int commonPKLength = 1;
        public int rowLimit;
        public int scannedLimit;
        public long byteLimit;
        public long timeLimit;
        public boolean reverse;
        public boolean snapshot;
        public boolean failOnScanLimit;
        public String continuation;
        public int localWriteID = -1;
        public int localPrimaryWriteID = -1;
    }

    private static String bytes(byte[] value) {
        return value == null ? "" : Base64.getEncoder().encodeToString(value);
    }

    private static boolean compound(String shape) {
        return shape.equals("compound");
    }

    private static int format(String shape) {
        switch (shape) {
            case "legacy": return 4;
            case "legacy_suffix": return 5;
            default: return 14;
        }
    }

    private static RecordMetaData metadata(String shape) {
        RecordMetaDataBuilder builder = RecordMetaData.newBuilder().setRecords(RecordLayerDemo.getDescriptor());
        builder.getRecordType("Order").setPrimaryKey(compound(shape)
                ? Key.Expressions.concat(Key.Expressions.field("quantity"), Key.Expressions.field("order_id"))
                : Key.Expressions.field("order_id"));
        builder.getRecordType("Customer").setPrimaryKey(Key.Expressions.field("customer_id"));
        builder.getRecordType("TypedRecord").setPrimaryKey(Key.Expressions.field("id"));
        builder.setSplitLongRecords(shape.equals("basic") || shape.equals("compound") || shape.equals("split") || shape.equals("many"));
        builder.setStoreRecordVersions(true);
        List<Index> indexes = List.of(
                new Index("price", Key.Expressions.field("price")),
                new Index("pk", Key.Expressions.field("order_id")),
                new Index("pk_first", Key.Expressions.concat(Key.Expressions.field("price"), Key.Expressions.field("quantity"))),
                new Index("pk_second", Key.Expressions.concat(Key.Expressions.field("price"), Key.Expressions.field("order_id"))),
                new Index("version", Key.Expressions.concat(Key.Expressions.field("price"), Key.Expressions.version()), IndexTypes.VERSION),
                new Index("bitmap", Key.Expressions.concatenateFields("price", "quantity", "order_id").group(1),
                        IndexTypes.BITMAP_VALUE, Map.of(IndexOptions.BITMAP_VALUE_ENTRY_SIZE_OPTION, "16")));
        for (Index index : indexes) {
            index.setSubspaceKey(index.getName());
            builder.addIndex("Order", index);
        }
        return builder.build();
    }

    private static FDBRecordStore open(FDBRecordContext context, byte[] subspace, String shape) {
        return FDBRecordStore.newBuilder().setContext(context).setSubspace(new Subspace(subspace))
                .setMetaDataProvider(metadata(shape)).setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .setFormatVersion(format(shape)).createOrOpen();
    }

    private static Tuple primaryKey(String shape, int id) {
        return compound(shape) ? Tuple.from((long) (id % 3), (long) id) : Tuple.from((long) id);
    }

    private static long price(String shape, int id) {
        return shape.equals("many") ? id : 1000 - id;
    }

    private static TupleRange range(String name) {
        switch (name) {
            case "price_tail": return TupleRange.betweenInclusive(Tuple.from(994L), Tuple.from(999L));
            case "price_prefix": return TupleRange.allOf(Tuple.from(997L));
            default: return TupleRange.ALL;
        }
    }

    private static void localWrite(FDBRecordStore store, String shape, int id) {
        RecordLayerDemo.Order previous = (RecordLayerDemo.Order) store.loadRecord(primaryKey(shape, id)).getRecord();
        store.saveRecord(previous.toBuilder().clearTags().addTags("local-write").build());
    }

    @ConformanceStep("seedIndexRemoteFetch")
    public int seed(String clusterFile, String tenantName, byte[] subspace, String shape, int count, boolean orphan) {
        for (int first = 0; first < count; first += 50) {
            final int start = first;
            runInContext(clusterFile, tenantName, context -> {
                FDBRecordStore store = open(context, subspace, shape);
                for (int id = start; id < Math.min(count, start + 50); id++) {
                    RecordLayerDemo.Order.Builder order = RecordLayerDemo.Order.newBuilder()
                            .setOrderId(id).setPrice((int) price(shape, id))
                            .setQuantity(id % 3).addTags(id % 2 == 0 ? "even" : "odd");
                    if (shape.equals("split") && id == 3) {
                        byte[] data = new byte[150_017];
                        Arrays.fill(data, (byte) 0x80);
                        order.setVectorData(ByteString.copyFrom(data));
                    }
                    store.saveRecord(order.build());
                }
                return null;
            });
        }
        if (orphan) {
            runInContext(clusterFile, tenantName, context -> {
                FDBRecordStore store = open(context, subspace, shape);
                Tuple key = primaryKey(shape, 2);
                context.ensureActive().clear(TupleRange.allOf(key).toRange(store.recordsSubspace()));
                context.ensureActive().clear(store.recordsSubspace().pack(key));
                return null;
            });
        }
        return count;
    }

    @ConformanceStep("indexRemoteFetchMapper")
    public Map<String, Object> mapper(String clusterFile, String tenantName, byte[] subspace, Request request) throws Exception {
        try (Tenant tenant = createDatabase(clusterFile).database().openTenant(tenantName.getBytes(StandardCharsets.UTF_8));
             Transaction transaction = tenant.createTransaction();
             FDBRecordContext context = createContextFromTransaction(createDatabase(clusterFile), transaction)) {
            FDBRecordStore store = open(context, subspace, request.shape);
            Index index = store.getRecordMetaData().getIndex(request.indexName);
            Object maintainer = store.getIndexMaintainer(index);
            Method createMapper = StandardIndexMaintainer.class.getDeclaredMethod("createRemoteFetchMapper", int.class);
            createMapper.setAccessible(true);
            Tuple mapper = (Tuple) createMapper.invoke(maintainer, request.commonPKLength);
            IndexPrefetchRangeKeyValueCursor.Builder builder = IndexPrefetchRangeKeyValueCursor.Builder
                    .newBuilder(store.indexSubspace(index), mapper.pack()).setContext(context)
                    .setRange(range(request.range)).setScanProperties(ScanProperties.FORWARD_SCAN);
            try (IndexPrefetchRangeKeyValueCursor cursor = builder.build()) {
                return Map.of("mapper", bytes(mapper.pack()),
                        "indexSubspace", bytes(store.indexSubspace(index).pack()),
                        "recordsSubspace", bytes(store.recordsSubspace().pack()),
                        "continuationPrefixLength", builder.getPrefixLength(),
                        "begin", bytes(builder.getBegin().getKey()), "end", bytes(builder.getEnd().getKey()),
                        "commonPKLength", store.getCommonPrimaryKeyLength(index));
            }
        }
    }

    private static Map<String, Object> record(FDBIndexedRecord<Message> indexed, byte[] continuation) {
        Map<String, Object> row = new HashMap<>();
        row.put("indexKey", bytes(indexed.getIndexEntry().getKey().pack()));
        row.put("indexValue", bytes(indexed.getIndexEntry().getValue().pack()));
        row.put("primaryKey", bytes(indexed.getIndexEntry().getPrimaryKey().pack()));
        row.put("continuation", bytes(continuation));
        row.put("present", indexed.hasStoredRecord());
        row.put("data", "");
        row.put("version", "");
        row.put("keyCount", 0);
        row.put("keySize", 0);
        row.put("valueSize", 0);
        row.put("split", false);
        if (indexed.hasStoredRecord()) {
            FDBStoredRecord<Message> record = indexed.getStoredRecord();
            row.put("data", bytes(record.getRecord().toByteArray()));
            row.put("version", record.hasVersion() ? bytes(record.getVersion().toBytes()) : "");
            row.put("keyCount", record.getKeyCount());
            row.put("keySize", record.getKeySize());
            row.put("valueSize", record.getValueSize());
            row.put("split", record.isSplit());
        }
        return row;
    }

    @ConformanceStep("scanIndexRemoteFetch")
    public Map<String, Object> scan(String clusterFile, String tenantName, byte[] subspace, Request request) throws InterruptedException {
        try (Tenant tenant = createDatabase(clusterFile).database().openTenant(tenantName.getBytes(StandardCharsets.UTF_8));
             Transaction transaction = tenant.createTransaction();
             FDBRecordContext context = createContextFromTransaction(createDatabase(clusterFile), transaction)) {
            FDBRecordStore store = open(context, subspace, request.shape);
            if (request.localWriteID >= 0) {
                localWrite(store, request.shape, request.localWriteID);
            }
            if (request.localPrimaryWriteID >= 0) {
                int id = request.localPrimaryWriteID;
                context.ensureActive().set(store.indexSubspace(store.getRecordMetaData().getIndex("price"))
                        .pack(Tuple.from(price(request.shape, id), (long) id)), Tuple.from(42L).pack());
            }
            ExecuteProperties.Builder execution = ExecuteProperties.newBuilder().setReturnedRowLimit(request.rowLimit)
                    .setDefaultCursorStreamingMode(CursorStreamingMode.valueOf(request.streaming))
                    .setIsolationLevel(request.snapshot ? IsolationLevel.SNAPSHOT : IsolationLevel.SERIALIZABLE)
                    .setFailOnScanLimitReached(request.failOnScanLimit);
            if (request.scannedLimit > 0) execution.setScannedRecordsLimit(request.scannedLimit);
            if (request.byteLimit > 0) execution.setScannedBytesLimit(request.byteLimit);
            if (request.timeLimit > 0) {
                execution.setTimeLimit(request.timeLimit);
                Thread.sleep(request.timeLimit + 25);
            }
            ExecuteProperties properties = execution.build();
            context.getTimer().reset();
            Map<String, Object> result = new HashMap<>();
            List<Map<String, Object>> rows = new ArrayList<>();
            result.put("rows", rows);
            result.put("continuation", "");
            result.put("end", false);
            result.put("reason", "");
            result.put("exceptionClass", "");
            result.put("errorCode", 0);
            byte[] continuation = request.continuation == null || request.continuation.isEmpty()
                    ? null : Base64.getDecoder().decode(request.continuation);
            try (RecordCursor<FDBIndexedRecord<Message>> cursor = store.scanIndexRecords(
                    store.getRecordMetaData().getIndex(request.indexName), IndexFetchMethod.valueOf(request.method),
                    new IndexScanRange(new IndexScanType(request.scanType), range(request.range)), request.commonPKLength,
                    continuation, IndexOrphanBehavior.valueOf(request.orphan), new ScanProperties(properties, request.reverse))) {
                while (true) {
                    RecordCursorResult<FDBIndexedRecord<Message>> next = cursor.onNext().join();
                    if (!next.hasNext()) {
                        result.put("continuation", bytes(next.getContinuation().toBytes()));
                        result.put("end", next.getContinuation().isEnd());
                        result.put("reason", next.getNoNextReason().name());
                        break;
                    }
                    rows.add(record(next.get(), next.getContinuation().toBytes()));
                }
            } catch (RuntimeException error) {
                Throwable root = error;
                while (root.getCause() != null) {
                    root = root.getCause();
                }
                result.put("exceptionClass", root.getClass().getSimpleName());
                for (Throwable cause = error; cause != null; cause = cause.getCause()) {
                    if (cause instanceof FDBException) {
                        result.put("errorCode", ((FDBException) cause).getCode());
                    }
                }
            }
            result.put("scannedRecords", properties.getState().getRecordsScanned());
            result.put("scannedBytes", properties.getState().getBytesScanned());
            result.put("remoteEntries", context.getTimer().getCount(FDBStoreTimer.Events.SCAN_REMOTE_FETCH_ENTRY));
            result.put("badEntries", context.getTimer().getCount(FDBStoreTimer.Counts.BAD_INDEX_ENTRY));
            return result;
        }
    }
}
