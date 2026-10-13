package com.birdayz.conformance;

import com.apple.foundationdb.Database;
import com.apple.foundationdb.FDBException;
import com.apple.foundationdb.KeySelector;
import com.apple.foundationdb.KeyValue;
import com.apple.foundationdb.MappedKeyValue;
import com.apple.foundationdb.MappedRangeConformanceAccess;
import com.apple.foundationdb.StreamingMode;
import com.apple.foundationdb.Tenant;
import com.apple.foundationdb.Transaction;
import com.apple.foundationdb.async.AsyncIterator;
import com.apple.foundationdb.tuple.ByteArrayUtil;
import com.apple.foundationdb.tuple.Tuple;

import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.Base64;
import java.util.List;
import java.util.Map;

/** Raw binding conformance for RFC-258 PR1, not Record Layer reconstruction/dispatch. */
class MappedRangeSteps extends ConformanceBase {
    private static String encode(byte[] bytes) {
        return Base64.getEncoder().encodeToString(bytes);
    }

    private static Map<String, Object> row(MappedKeyValue value) {
        return row(value.getKey(), value.getValue(), value.getRangeBegin(), value.getRangeEnd(), value.getRangeResult());
    }

    private static Map<String, Object> row(byte[] key, byte[] value, byte[] begin, byte[] end, List<KeyValue> ranges) {
        List<Map<String, Object>> secondary = new ArrayList<>();
        for (KeyValue kv : ranges) {
            secondary.add(Map.of("key", encode(kv.getKey()), "value", encode(kv.getValue())));
        }
        return Map.of("key", encode(key), "value", encode(value),
                "begin", encode(begin), "end", encode(end),
                "secondary", secondary);
    }

    private static byte[] mapper(String root) {
        // Index tuple: root, index, id, group. Record tuple: root, records, group, id.
        return Tuple.from(root, "records", "{K[3]}", "{K[2]}", "{...}").pack();
    }

    @ConformanceStep("seedMappedRangeFixture")
    public Map<String, Object> seed(String clusterFile, String tenantName, String root, int count) {
        Database database = createDatabase(clusterFile).database();
        try (Tenant tenant = database.openTenant(tenantName.getBytes(StandardCharsets.UTF_8))) {
            List<Map<String, Object>> expected = new ArrayList<>();
            try (Transaction transaction = tenant.createTransaction()) {
                for (int i = 0; i < count; i++) {
                    byte[] key = Tuple.from(root, "index", (long) i, "group\u0000" + (i % 3)).pack();
                    byte[] value = new byte[i % 7 == 0 ? 0 : 512];
                    Arrays.fill(value, (byte) i);
                    transaction.set(key, value);
                    Tuple record = Tuple.from(root, "records", "group\u0000" + (i % 3), (long) i);
                    List<KeyValue> secondary = new ArrayList<>();
                    if (i != count - 1) { // Empty secondary range is an orphan, not an absent index entry.
                        if (i % 4 == 0) {
                            secondary.add(new KeyValue(record.add(-1L).pack(),
                                    new byte[] {0, 1, 2, 3, 4, 5, 6, 7, 8, 9, (byte) i, 0}));
                        }
                        if (i == 17) { // More than 100 KB, with the real split-record suffix shape.
                            byte[] chunk = new byte[100_000];
                            Arrays.fill(chunk, (byte) 0x80);
                            secondary.add(new KeyValue(record.add(1L).pack(), chunk));
                            secondary.add(new KeyValue(record.add(2L).pack(), new byte[] {0, (byte) 0xff, 17}));
                        } else {
                            secondary.add(new KeyValue(record.add(0L).pack(),
                                    i % 5 == 0 ? new byte[0] : Tuple.from("record", (long) i).pack()));
                        }
                    }
                    for (KeyValue kv : secondary) {
                        transaction.set(kv.getKey(), kv.getValue());
                    }
                    byte[] prefix = record.pack();
                    // getMappedRange uses [prefix, strinc(prefix)), not Tuple.range().
                    expected.add(row(key, value, prefix, ByteArrayUtil.strinc(prefix), secondary));
                }
                transaction.commit().join();
            }
            com.apple.foundationdb.Range range = Tuple.from(root, "index").range();
            return Map.of("mapper", encode(mapper(root)), "begin", encode(range.begin),
                    "end", encode(range.end), "rows", expected);
        }
    }

    @ConformanceStep("scanMappedRangeFixture")
    public Map<String, Object> scan(String clusterFile, String tenantName, String root,
                                    String mode, int limit, boolean reverse) {
        Database database = createDatabase(clusterFile).database();
        StreamingMode streaming = StreamingMode.valueOf(mode);
        com.apple.foundationdb.Range range = Tuple.from(root, "index").range();
        try (Tenant tenant = database.openTenant(tenantName.getBytes(StandardCharsets.UTF_8));
             Transaction transaction = tenant.createTransaction()) {
            List<Map<String, Object>> rows = new ArrayList<>();
            AsyncIterator<MappedKeyValue> iterator = transaction.getMappedRange(
                    KeySelector.firstGreaterOrEqual(range.begin), KeySelector.firstGreaterOrEqual(range.end),
                    mapper(root), limit, reverse, streaming).iterator();
            try {
                while (iterator.hasNext()) {
                    rows.add(row(iterator.next()));
                }
            } finally {
                iterator.cancel();
            }

            List<Map<String, Object>> batchRows = new ArrayList<>();
            List<Map<String, Object>> batches = new ArrayList<>();
            KeySelector begin = KeySelector.firstGreaterOrEqual(range.begin);
            KeySelector end = KeySelector.firstGreaterOrEqual(range.end);
            int remaining = limit;
            for (int iteration = 1; ; iteration++) {
                MappedRangeConformanceAccess.Batch batch = MappedRangeConformanceAccess.fetch(
                        transaction, begin, end, mapper(root), remaining, streaming, iteration, reverse);
                batches.add(Map.of("iteration", iteration, "requested", remaining,
                        "returned", batch.rows.size(), "more", batch.more));
                for (MappedKeyValue value : batch.rows) {
                    batchRows.add(row(value));
                }
                if (limit > 0) {
                    remaining -= batch.rows.size();
                }
                if (batch.rows.isEmpty() || !batch.more || (limit > 0 && remaining <= 0)) {
                    break;
                }
                byte[] last = batch.rows.get(batch.rows.size() - 1).getKey();
                if (reverse) {
                    end = KeySelector.firstGreaterOrEqual(last);
                } else {
                    begin = KeySelector.firstGreaterThan(last);
                }
            }
            return Map.of("rows", rows, "batchRows", batchRows, "batches", batches);
        }
    }

    @ConformanceStep("mappedRangeErrorCode")
    public int errorCode(String clusterFile, String tenantName, String root, byte[] mapper,
                         boolean writePrimary, boolean writeSecondary, boolean reverse) {
        Database database = createDatabase(clusterFile).database();
        com.apple.foundationdb.Range range = Tuple.from(root, "index").range();
        try (Tenant tenant = database.openTenant(tenantName.getBytes(StandardCharsets.UTF_8));
             Transaction transaction = tenant.createTransaction()) {
            if (writePrimary) {
                transaction.set(Tuple.from(root, "index", 0L, "group\u00000").pack(), new byte[] {42});
            }
            if (writeSecondary) {
                transaction.set(Tuple.from(root, "records", "group\u00000", 0L, 0L).pack(), new byte[] {42});
            }
            AsyncIterator<MappedKeyValue> iterator = transaction.getMappedRange(
                    KeySelector.firstGreaterOrEqual(range.begin), KeySelector.firstGreaterOrEqual(range.end),
                    mapper, 0, reverse, StreamingMode.ITERATOR).iterator();
            try {
                while (iterator.hasNext()) {
                    iterator.next();
                }
                return 0;
            } catch (RuntimeException error) {
                for (Throwable cause = error; cause != null; cause = cause.getCause()) {
                    if (cause instanceof FDBException) {
                        return ((FDBException) cause).getCode();
                    }
                }
                throw error;
            } finally {
                iterator.cancel();
            }
        }
    }
}
