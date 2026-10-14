package com.birdayz.conformance;

import com.apple.foundationdb.Tenant;
import com.apple.foundationdb.Transaction;
import com.apple.foundationdb.record.EndpointType;
import com.apple.foundationdb.record.ExecuteProperties;
import com.apple.foundationdb.record.RecordCursor;
import com.apple.foundationdb.record.RecordCursorResult;
import com.apple.foundationdb.record.RecordLayerDemo;
import com.apple.foundationdb.record.RecordMetaData;
import com.apple.foundationdb.record.RecordMetaDataBuilder;
import com.apple.foundationdb.record.ScanProperties;
import com.apple.foundationdb.record.metadata.Key;
import com.apple.foundationdb.record.provider.foundationdb.FDBDatabase;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordContext;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.record.provider.foundationdb.FDBStoredRecord;
import com.apple.foundationdb.subspace.Subspace;
import com.apple.foundationdb.tuple.Tuple;
import com.google.protobuf.ByteString;
import com.google.protobuf.Message;

import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.Base64;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.CompletionException;

/** FDBRecordStore.scanRecords sizes, stop points and scan accounting for each record layout. */
class RecordScanLimitSteps extends ConformanceBase {
    public static class Request {
        public String layout;
        public boolean reverse;
        public int rowLimit;
        public int skip;
        public int scannedLimit;
        public long byteLimit;
        public long timeLimit;
        public boolean failOnScanLimit;
        public boolean localSave;
        public boolean walk;
        public String continuation;
    }

    private static final class Layout {
        final boolean split;
        final boolean seedVersions;
        final boolean scanVersions;
        final int formatVersion;

        Layout(boolean split, boolean seedVersions, boolean scanVersions, int formatVersion) {
            this.split = split;
            this.seedVersions = seedVersions;
            this.scanVersions = scanVersions;
            this.formatVersion = formatVersion;
        }
    }

    // Format 4 omits the unsplit suffix; format 5 keeps versions in their own subspace.
    private static final Map<String, Layout> LAYOUTS = Map.of(
            "split_versions", new Layout(true, true, true, 14),
            "split", new Layout(true, false, false, 14),
            "split_stale_versions", new Layout(true, true, false, 14),
            "unsplit_versions", new Layout(false, true, true, 14),
            "unsplit", new Layout(false, false, false, 14),
            "omit_suffix_versions", new Layout(false, true, true, 4),
            "split_old_versions", new Layout(true, true, true, 5),
            "unsplit_old_versions", new Layout(false, true, true, 5));

    private static Layout layout(String name) {
        Layout layout = LAYOUTS.get(name);
        if (layout == null) {
            throw new IllegalArgumentException("unknown record scan layout " + name);
        }
        return layout;
    }

    private static String bytes(byte[] value) {
        return value == null ? "" : Base64.getEncoder().encodeToString(value);
    }

    private static RecordMetaData metadata(boolean split, boolean versions, boolean evolved) {
        RecordMetaDataBuilder builder = RecordMetaData.newBuilder().setRecords(RecordLayerDemo.getDescriptor());
        builder.getRecordType("Order").setPrimaryKey(Key.Expressions.field("order_id"));
        builder.getRecordType("Customer").setPrimaryKey(Key.Expressions.field("customer_id"));
        builder.getRecordType("TypedRecord").setPrimaryKey(Key.Expressions.field("id"));
        builder.setSplitLongRecords(split);
        builder.setStoreRecordVersions(versions);
        if (evolved) {
            // Disabling versions must be a newer metadata version than the seed's.
            builder.setVersion(100);
        }
        return builder.build();
    }

    private static FDBRecordStore open(FDBRecordContext context, byte[] subspace, String name, boolean seeding) {
        Layout layout = layout(name);
        return FDBRecordStore.newBuilder().setContext(context).setSubspace(new Subspace(subspace).subspace(Tuple.from(name)))
                .setMetaDataProvider(metadata(layout.split, seeding ? layout.seedVersions : layout.scanVersions,
                        !seeding && layout.seedVersions != layout.scanVersions))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER).setFormatVersion(layout.formatVersion).createOrOpen();
    }

    // Split layouts get records of two and three chunks.
    private static RecordLayerDemo.Order order(long id, boolean split) {
        int size = 8;
        if (split && id == 2) {
            size = 150_000;
        } else if (split && id == 4) {
            size = 250_000;
        } else if (split && id == 7) {
            size = 120_000;
        }
        byte[] payload = new byte[size];
        Arrays.fill(payload, (byte) id);
        return RecordLayerDemo.Order.newBuilder().setOrderId(id).setPrice((int) (100 - id))
                .setVectorData(ByteString.copyFrom(payload)).build();
    }

    @ConformanceStep("seedRecordScanLimits")
    public int seed(String clusterFile, String tenantName, byte[] subspace, String layout) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = open(context, subspace, layout, true);
            for (long id = 0; id < 6; id++) {
                store.saveRecord(order(id, layout(layout).split));
            }
            return 6;
        });
    }

    private static String sha256(byte[] value) {
        try {
            return bytes(MessageDigest.getInstance("SHA-256").digest(value));
        } catch (NoSuchAlgorithmException error) {
            throw new IllegalStateException(error);
        }
    }

    private static Map<String, Object> row(RecordCursorResult<FDBStoredRecord<Message>> next) {
        FDBStoredRecord<Message> record = next.get();
        Map<String, Object> row = new HashMap<>();
        row.put("primaryKey", bytes(record.getPrimaryKey().pack()));
        row.put("dataHash", sha256(record.getRecord().toByteArray()));
        row.put("version", record.hasVersion() ? bytes(record.getVersion().toBytes()) : "");
        row.put("keyCount", record.getKeyCount());
        row.put("keySize", record.getKeySize());
        row.put("valueSize", record.getValueSize());
        row.put("split", record.isSplit());
        row.put("versionedInline", record.isVersionedInline());
        row.put("continuation", bytes(next.getContinuation().toBytes()));
        return row;
    }

    private static Map<String, Object> terminal(RecordCursorResult<?> next, ExecuteProperties properties) {
        Map<String, Object> terminal = new HashMap<>();
        terminal.put("reason", next.getNoNextReason().name());
        terminal.put("continuation", bytes(next.getContinuation().toBytes()));
        terminal.put("end", next.getContinuation().isEnd());
        terminal.put("scannedRecords", properties.getState().getRecordsScanned());
        terminal.put("scannedBytes", properties.getState().getBytesScanned());
        return terminal;
    }

    // The Record Layer exception under the future's CompletionException wrapper.
    private static String thrown(Throwable error) {
        Throwable thrown = error;
        while (thrown instanceof CompletionException && thrown.getCause() != null) {
            thrown = thrown.getCause();
        }
        return thrown.getClass().getSimpleName();
    }

    private static Map<String, Object> page(FDBRecordContext context, byte[] subspace, Request request, String continuation) {
        FDBRecordStore store = open(context, subspace, request.layout, false);
        if (request.localSave) {
            store.saveRecord(order(6, layout(request.layout).split));
            store.saveRecord(order(7, layout(request.layout).split));
        }
        // The time limit runs from the transaction's creation.
        if (request.timeLimit > 0) {
            try {
                Thread.sleep(request.timeLimit + 25);
            } catch (InterruptedException error) {
                Thread.currentThread().interrupt();
                throw new IllegalStateException(error);
            }
        }
        ExecuteProperties.Builder builder = ExecuteProperties.newBuilder().setReturnedRowLimit(request.rowLimit)
                .setSkip(request.skip).setFailOnScanLimitReached(request.failOnScanLimit);
        if (request.scannedLimit > 0) builder.setScannedRecordsLimit(request.scannedLimit);
        if (request.byteLimit > 0) builder.setScannedBytesLimit(request.byteLimit);
        if (request.timeLimit > 0) builder.setTimeLimit(request.timeLimit);
        ExecuteProperties properties = builder.build();
        byte[] resume = continuation == null || continuation.isEmpty() ? null : Base64.getDecoder().decode(continuation);
        Map<String, Object> page = new HashMap<>();
        List<Map<String, Object>> rows = new ArrayList<>();
        List<Map<String, Object>> repeats = new ArrayList<>();
        page.put("rows", rows);
        page.put("repeats", repeats);
        page.put("reason", "");
        page.put("continuation", "");
        page.put("end", false);
        page.put("exceptionClass", "");
        try (RecordCursor<FDBStoredRecord<Message>> cursor = store.scanRecords(resume, new ScanProperties(properties, request.reverse))) {
            while (true) {
                RecordCursorResult<FDBStoredRecord<Message>> next = cursor.onNext().join();
                if (next.hasNext()) {
                    rows.add(row(next));
                    continue;
                }
                page.putAll(terminal(next, properties));
                for (int repeat = 0; repeat < 2; repeat++) {
                    repeats.add(terminal(cursor.onNext().join(), properties));
                }
                break;
            }
        } catch (RuntimeException error) {
            page.put("exceptionClass", thrown(error));
        }
        page.put("scannedRecords", properties.getState().getRecordsScanned());
        page.put("scannedBytes", properties.getState().getBytesScanned());
        return page;
    }

    // Each page reads in its own transaction, which is never committed.
    private static Map<String, Object> readPage(String clusterFile, String tenantName, byte[] subspace, Request request, String continuation) {
        FDBDatabase database = createDatabase(clusterFile);
        try (Tenant tenant = database.database().openTenant(tenantName.getBytes(StandardCharsets.UTF_8))) {
            Transaction transaction = tenant.createTransaction();
            try (FDBRecordContext context = createContextFromTransaction(database, transaction)) {
                return page(context, subspace, request, continuation);
            }
        }
    }

    private static Map<String, Object> count(FDBRecordContext context, byte[] subspace, Request request) {
        FDBRecordStore store = open(context, subspace, request.layout, false);
        if (request.timeLimit > 0) {
            try {
                Thread.sleep(request.timeLimit + 25);
            } catch (InterruptedException error) {
                Thread.currentThread().interrupt();
                throw new IllegalStateException(error);
            }
        }
        ExecuteProperties.Builder builder = ExecuteProperties.newBuilder().setReturnedRowLimit(request.rowLimit)
                .setSkip(request.skip).setFailOnScanLimitReached(request.failOnScanLimit);
        if (request.scannedLimit > 0) builder.setScannedRecordsLimit(request.scannedLimit);
        if (request.byteLimit > 0) builder.setScannedBytesLimit(request.byteLimit);
        if (request.timeLimit > 0) builder.setTimeLimit(request.timeLimit);
        ExecuteProperties properties = builder.build();
        Map<String, Object> result = new HashMap<>();
        result.put("count", -1);
        result.put("exceptionClass", "");
        try {
            result.put("count", store.countRecords(null, null, EndpointType.TREE_START, EndpointType.TREE_END, null,
                    new ScanProperties(properties, request.reverse)).join());
        } catch (RuntimeException error) {
            result.put("exceptionClass", thrown(error));
        }
        result.put("scannedRecords", properties.getState().getRecordsScanned());
        result.put("scannedBytes", properties.getState().getBytesScanned());
        return result;
    }

    @ConformanceStep("countRecordScanLimits")
    public List<Map<String, Object>> countRecords(String clusterFile, String tenantName, byte[] subspace, Request[] requests) {
        FDBDatabase database = createDatabase(clusterFile);
        List<Map<String, Object>> results = new ArrayList<>();
        for (Request request : requests) {
            try (Tenant tenant = database.database().openTenant(tenantName.getBytes(StandardCharsets.UTF_8))) {
                Transaction transaction = tenant.createTransaction();
                try (FDBRecordContext context = createContextFromTransaction(database, transaction)) {
                    results.add(count(context, subspace, request));
                }
            }
        }
        return results;
    }

    @ConformanceStep("scanRecordScanLimits")
    public List<List<Map<String, Object>>> scan(String clusterFile, String tenantName, byte[] subspace, Request[] requests) {
        List<List<Map<String, Object>>> results = new ArrayList<>();
        for (Request request : requests) {
            List<Map<String, Object>> pages = new ArrayList<>();
            String continuation = request.continuation;
            while (true) {
                Map<String, Object> page = readPage(clusterFile, tenantName, subspace, request, continuation);
                pages.add(page);
                if (!request.walk || !page.get("exceptionClass").equals("") || page.get("reason").equals("SOURCE_EXHAUSTED")
                        || page.get("reason").equals("") || pages.size() >= 40) {
                    break;
                }
                continuation = (String) page.get("continuation");
            }
            results.add(pages);
        }
        return results;
    }
}
