package com.birdayz.conformance;

import com.apple.foundationdb.record.RecordMetaData;
import com.apple.foundationdb.record.RecordMetaDataBuilder;
import com.apple.foundationdb.record.metadata.Index;
import com.apple.foundationdb.record.metadata.IndexTypes;
import com.apple.foundationdb.record.metadata.Key;
import com.apple.foundationdb.record.provider.foundationdb.FDBDatabase;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordContext;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.record.provider.foundationdb.FDBStoredRecord;
import com.apple.foundationdb.record.ScanProperties;
import com.apple.foundationdb.record.TupleRange;
import com.apple.foundationdb.record.RecordLayerDemo;
import com.apple.foundationdb.record.RecordLayerDemo.Order;
import com.apple.foundationdb.subspace.Subspace;
import com.apple.foundationdb.tuple.Tuple;
import com.google.protobuf.Message;

import java.util.HashMap;
import java.util.List;
import java.util.Map;

/**
 * Benchmark steps that measure Java Record Layer performance internally.
 * Each step runs warmup + N measured iterations, eliminating HTTP overhead
 * and JVM JIT compilation from the measurement.
 */
class BenchmarkSteps extends ConformanceBase {

    // JIT warmup iterations (discarded from measurement).
    private static final int WARMUP = 20;

    private static RecordMetaData benchMetaData() {
        RecordMetaDataBuilder b = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        b.getRecordType("Order").setPrimaryKey(Key.Expressions.field("order_id"));
        b.getRecordType("Customer").setPrimaryKey(Key.Expressions.field("customer_id"));
        b.getRecordType("TypedRecord").setPrimaryKey(Key.Expressions.field("id"));
        return b.build();
    }

    private static RecordMetaData benchMetaDataWithIndex() {
        RecordMetaDataBuilder b = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        b.getRecordType("Order").setPrimaryKey(Key.Expressions.field("order_id"));
        b.getRecordType("Customer").setPrimaryKey(Key.Expressions.field("customer_id"));
        b.getRecordType("TypedRecord").setPrimaryKey(Key.Expressions.field("id"));
        b.addIndex("Order", new Index("bench_price", Key.Expressions.field("price"), IndexTypes.VALUE));
        return b.build();
    }

    private static Order makeOrder(long id, int price) {
        return Order.newBuilder()
            .setOrderId(id)
            .setPrice(price)
            .setFlower(RecordLayerDemo.Flower.newBuilder()
                .setType("Rose")
                .setColor(RecordLayerDemo.Color.RED))
            .build();
    }

    @ConformanceStep("benchmarkSaveRecord")
    public Map<String, Object> benchmarkSaveRecord(String clusterFile, byte[] subspace, long iterations) {
        RecordMetaData md = benchMetaData();
        FDBDatabase db = createDatabase(clusterFile);
        Subspace ss = new Subspace(subspace);

        db.run(context -> {
            FDBRecordStore.newBuilder().setMetaDataProvider(md).setContext(context).setSubspace(ss).createOrOpen();
            return null;
        });

        // Warmup.
        for (int w = 0; w < WARMUP; w++) {
            final long id = -(w + 1);
            db.run(context -> {
                FDBRecordStore store = FDBRecordStore.newBuilder().setMetaDataProvider(md).setContext(context).setSubspace(ss).open();
                store.saveRecord(makeOrder(id, 100));
                return null;
            });
        }

        long start = System.nanoTime();
        for (long i = 0; i < iterations; i++) {
            final long id = i;
            db.run(context -> {
                FDBRecordStore store = FDBRecordStore.newBuilder().setMetaDataProvider(md).setContext(context).setSubspace(ss).open();
                store.saveRecord(makeOrder(id, 100));
                return null;
            });
        }
        return timingResult(iterations, System.nanoTime() - start);
    }

    @ConformanceStep("benchmarkLoadRecord")
    public Map<String, Object> benchmarkLoadRecord(String clusterFile, byte[] subspace, long iterations) {
        RecordMetaData md = benchMetaData();
        FDBDatabase db = createDatabase(clusterFile);
        Subspace ss = new Subspace(subspace);

        db.run(context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder().setMetaDataProvider(md).setContext(context).setSubspace(ss).createOrOpen();
            store.saveRecord(makeOrder(1, 100));
            return null;
        });

        // Warmup.
        for (int w = 0; w < WARMUP; w++) {
            db.run(context -> {
                FDBRecordStore store = FDBRecordStore.newBuilder().setMetaDataProvider(md).setContext(context).setSubspace(ss).open();
                store.loadRecord(Tuple.from(1L));
                return null;
            });
        }

        long start = System.nanoTime();
        for (long i = 0; i < iterations; i++) {
            db.run(context -> {
                FDBRecordStore store = FDBRecordStore.newBuilder().setMetaDataProvider(md).setContext(context).setSubspace(ss).open();
                FDBStoredRecord<Message> rec = store.loadRecord(Tuple.from(1L));
                if (rec == null) throw new RuntimeException("record not found");
                return null;
            });
        }
        return timingResult(iterations, System.nanoTime() - start);
    }

    @ConformanceStep("benchmarkScanRecords")
    public Map<String, Object> benchmarkScanRecords(String clusterFile, byte[] subspace, long iterations) {
        RecordMetaData md = benchMetaData();
        FDBDatabase db = createDatabase(clusterFile);
        Subspace ss = new Subspace(subspace);

        db.run(context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder().setMetaDataProvider(md).setContext(context).setSubspace(ss).createOrOpen();
            for (long j = 1; j <= 100; j++) store.saveRecord(makeOrder(j, (int)(j * 10)));
            return null;
        });

        // Warmup.
        for (int w = 0; w < WARMUP; w++) {
            db.run(context -> {
                FDBRecordStore store = FDBRecordStore.newBuilder().setMetaDataProvider(md).setContext(context).setSubspace(ss).open();
                store.scanRecords(TupleRange.ALL, null, ScanProperties.FORWARD_SCAN).asList().join();
                return null;
            });
        }

        long start = System.nanoTime();
        for (long i = 0; i < iterations; i++) {
            db.run(context -> {
                FDBRecordStore store = FDBRecordStore.newBuilder().setMetaDataProvider(md).setContext(context).setSubspace(ss).open();
                List<FDBStoredRecord<Message>> records = store.scanRecords(TupleRange.ALL, null, ScanProperties.FORWARD_SCAN).asList().join();
                if (records.size() != 100) throw new RuntimeException("expected 100, got " + records.size());
                return null;
            });
        }
        return timingResult(iterations, System.nanoTime() - start);
    }

    @ConformanceStep("benchmarkSaveRecordWithIndex")
    public Map<String, Object> benchmarkSaveRecordWithIndex(String clusterFile, byte[] subspace, long iterations) {
        RecordMetaData md = benchMetaDataWithIndex();
        FDBDatabase db = createDatabase(clusterFile);
        Subspace ss = new Subspace(subspace);

        db.run(context -> {
            FDBRecordStore.newBuilder().setMetaDataProvider(md).setContext(context).setSubspace(ss).createOrOpen();
            return null;
        });

        for (int w = 0; w < WARMUP; w++) {
            final long id = -(w + 1);
            db.run(context -> {
                FDBRecordStore store = FDBRecordStore.newBuilder().setMetaDataProvider(md).setContext(context).setSubspace(ss).open();
                store.saveRecord(makeOrder(id, 100));
                return null;
            });
        }

        long start = System.nanoTime();
        for (long i = 0; i < iterations; i++) {
            final long id = i;
            db.run(context -> {
                FDBRecordStore store = FDBRecordStore.newBuilder().setMetaDataProvider(md).setContext(context).setSubspace(ss).open();
                store.saveRecord(makeOrder(id, 100));
                return null;
            });
        }
        return timingResult(iterations, System.nanoTime() - start);
    }

    @ConformanceStep("benchmarkScanIndex")
    public Map<String, Object> benchmarkScanIndex(String clusterFile, byte[] subspace, long iterations) {
        RecordMetaData md = benchMetaDataWithIndex();
        FDBDatabase db = createDatabase(clusterFile);
        Subspace ss = new Subspace(subspace);

        db.run(context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder().setMetaDataProvider(md).setContext(context).setSubspace(ss).createOrOpen();
            for (long j = 1; j <= 100; j++) store.saveRecord(makeOrder(j, (int)(j * 10)));
            return null;
        });

        for (int w = 0; w < WARMUP; w++) {
            db.run(context -> {
                FDBRecordStore store = FDBRecordStore.newBuilder().setMetaDataProvider(md).setContext(context).setSubspace(ss).open();
                store.scanIndex(store.getRecordMetaData().getIndex("bench_price"),
                    com.apple.foundationdb.record.IndexScanType.BY_VALUE,
                    TupleRange.ALL, null, ScanProperties.FORWARD_SCAN).asList().join();
                return null;
            });
        }

        long start = System.nanoTime();
        for (long i = 0; i < iterations; i++) {
            db.run(context -> {
                FDBRecordStore store = FDBRecordStore.newBuilder().setMetaDataProvider(md).setContext(context).setSubspace(ss).open();
                store.scanIndex(store.getRecordMetaData().getIndex("bench_price"),
                    com.apple.foundationdb.record.IndexScanType.BY_VALUE,
                    TupleRange.ALL, null, ScanProperties.FORWARD_SCAN).asList().join();
                return null;
            });
        }
        return timingResult(iterations, System.nanoTime() - start);
    }

    @ConformanceStep("benchmarkDeleteRecord")
    public Map<String, Object> benchmarkDeleteRecord(String clusterFile, byte[] subspace, long iterations) {
        RecordMetaData md = benchMetaData();
        FDBDatabase db = createDatabase(clusterFile);
        Subspace ss = new Subspace(subspace);

        // Pre-populate warmup + measured records.
        db.run(context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder().setMetaDataProvider(md).setContext(context).setSubspace(ss).createOrOpen();
            for (long j = -(WARMUP); j < iterations; j++) store.saveRecord(makeOrder(j, 100));
            return null;
        });

        for (int w = 0; w < WARMUP; w++) {
            final long id = -(WARMUP) + w;
            db.run(context -> {
                FDBRecordStore store = FDBRecordStore.newBuilder().setMetaDataProvider(md).setContext(context).setSubspace(ss).open();
                store.deleteRecord(Tuple.from(id));
                return null;
            });
        }

        long start = System.nanoTime();
        for (long i = 0; i < iterations; i++) {
            final long id = i;
            db.run(context -> {
                FDBRecordStore store = FDBRecordStore.newBuilder().setMetaDataProvider(md).setContext(context).setSubspace(ss).open();
                store.deleteRecord(Tuple.from(id));
                return null;
            });
        }
        return timingResult(iterations, System.nanoTime() - start);
    }

    @ConformanceStep("benchmarkStoreOpen")
    public Map<String, Object> benchmarkStoreOpen(String clusterFile, byte[] subspace, long iterations) {
        RecordMetaData md = benchMetaData();
        FDBDatabase db = createDatabase(clusterFile);
        Subspace ss = new Subspace(subspace);

        db.run(context -> {
            FDBRecordStore.newBuilder().setMetaDataProvider(md).setContext(context).setSubspace(ss).createOrOpen();
            return null;
        });

        for (int w = 0; w < WARMUP; w++) {
            db.run(context -> {
                FDBRecordStore.newBuilder().setMetaDataProvider(md).setContext(context).setSubspace(ss).open();
                return null;
            });
        }

        long start = System.nanoTime();
        for (long i = 0; i < iterations; i++) {
            db.run(context -> {
                FDBRecordStore.newBuilder().setMetaDataProvider(md).setContext(context).setSubspace(ss).open();
                return null;
            });
        }
        return timingResult(iterations, System.nanoTime() - start);
    }

    @ConformanceStep("benchmarkSaveRecordBatch")
    public Map<String, Object> benchmarkSaveRecordBatch(String clusterFile, byte[] subspace, long iterations) {
        RecordMetaData md = benchMetaDataWithIndex();
        FDBDatabase db = createDatabase(clusterFile);
        Subspace ss = new Subspace(subspace);

        db.run(context -> {
            FDBRecordStore.newBuilder().setMetaDataProvider(md).setContext(context).setSubspace(ss).createOrOpen();
            return null;
        });

        for (int w = 0; w < WARMUP; w++) {
            final long batch = -(w + 1);
            db.run(context -> {
                FDBRecordStore store = FDBRecordStore.newBuilder().setMetaDataProvider(md).setContext(context).setSubspace(ss).open();
                for (int j = 0; j < 10; j++) store.saveRecord(makeOrder(batch * 10 + j, 100 + j));
                return null;
            });
        }

        long start = System.nanoTime();
        for (long i = 0; i < iterations; i++) {
            final long batch = i;
            db.run(context -> {
                FDBRecordStore store = FDBRecordStore.newBuilder().setMetaDataProvider(md).setContext(context).setSubspace(ss).open();
                for (int j = 0; j < 10; j++) store.saveRecord(makeOrder(batch * 10 + j, 100 + j));
                return null;
            });
        }
        return timingResult(iterations, System.nanoTime() - start);
    }

    private static Map<String, Object> timingResult(long iterations, long totalNanos) {
        Map<String, Object> result = new HashMap<>();
        result.put("iterations", iterations);
        result.put("totalNanos", totalNanos);
        result.put("avgNanos", totalNanos / iterations);
        return result;
    }
}
