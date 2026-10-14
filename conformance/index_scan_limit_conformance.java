package com.birdayz.conformance;

import com.apple.foundationdb.Database;
import com.apple.foundationdb.KeySelector;
import com.apple.foundationdb.KeyValue;
import com.apple.foundationdb.Range;
import com.apple.foundationdb.ReadTransaction;
import com.apple.foundationdb.Tenant;
import com.apple.foundationdb.Transaction;
import com.apple.foundationdb.async.AsyncIterable;
import com.apple.foundationdb.async.AsyncIterator;
import com.apple.foundationdb.record.CursorStreamingMode;
import com.apple.foundationdb.record.ExecuteProperties;
import com.apple.foundationdb.record.ExecuteState;
import com.apple.foundationdb.record.FDBRecordStoreProperties;
import com.apple.foundationdb.record.PipelineOperation;
import com.apple.foundationdb.record.IndexEntry;
import com.apple.foundationdb.record.IndexScanType;
import com.apple.foundationdb.record.RecordCursor;
import com.apple.foundationdb.record.RecordCursorResult;
import com.apple.foundationdb.record.RecordCursorVisitor;
import com.apple.foundationdb.record.RecordLayerDemo;
import com.apple.foundationdb.record.RecordMetaData;
import com.apple.foundationdb.record.RecordMetaDataBuilder;
import com.apple.foundationdb.record.ScanProperties;
import com.apple.foundationdb.record.ScanLimitReachedException;
import com.apple.foundationdb.record.cursors.MapPipelinedCursor;
import com.apple.foundationdb.record.TupleRange;
import com.apple.foundationdb.record.metadata.Index;
import com.apple.foundationdb.record.metadata.IndexTypes;
import com.apple.foundationdb.record.metadata.Key;
import com.apple.foundationdb.record.provider.foundationdb.FDBDatabase;
import com.apple.foundationdb.record.provider.foundationdb.FDBIndexedRecord;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordContext;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.record.provider.foundationdb.FDBStoredRecord;
import com.apple.foundationdb.record.provider.foundationdb.IndexOrphanBehavior;
import com.apple.foundationdb.record.provider.foundationdb.KeyValueCursor;
import com.apple.foundationdb.subspace.Subspace;
import com.apple.foundationdb.tuple.Tuple;
import com.google.protobuf.Message;

import java.lang.reflect.InvocationTargetException;
import java.lang.reflect.Method;
import java.lang.reflect.Proxy;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.Base64;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.Executor;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.atomic.AtomicLong;

/** Ordinary VALUE/VERSION cursor boundaries; deliberately no remote-fetch APIs. */
class IndexScanLimitSteps extends ConformanceBase {
    public static class Request {
        public String indexName = "price";
        public String streaming = "ITERATOR";
        public boolean records;
        public boolean reverse;
        public int rowLimit;
        public int skip;
        public int scannedLimit;
        public long byteLimit;
        public long timeLimit;
        public boolean failOnScanLimit;
        public boolean holdRecordLoadsUntilSourceError;
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

    private static final class TrackingBuilder extends FDBRecordStore.Builder {
        @Override
        public FDBRecordStore build() {
            return new TrackingStore(this);
        }
    }

    private static final class TrackingStore extends FDBRecordStore {
        private final List<CompletableFuture<FDBStoredRecord<Message>>> issued = new ArrayList<>();
        private volatile ExecuteState trackedState;

        TrackingStore(FDBRecordStore.Builder builder) {
            super(builder.getContext(), builder.getSubspaceProvider(), builder.getFormatVersionEnum(),
                    builder.getMetaDataProvider(), builder.getSerializer(), builder.getIndexMaintainerRegistry(),
                    builder.getIndexMaintenanceFilter(), builder.getPipelineSizer(), builder.getStoreStateCache(),
                    builder.getStateCacheabilityOnOpen(), builder.getUserVersionChecker(),
                    builder.getBypassFullStoreLockReason(), builder.getPlanSerializationRegistry());
        }

        @Override
        public CompletableFuture<FDBStoredRecord<Message>> loadRecordInternal(Tuple primaryKey, ExecuteState state, boolean snapshot) {
            CompletableFuture<FDBStoredRecord<Message>> future = super.loadRecordInternal(primaryKey, state, snapshot);
            if (trackedState != null && state == trackedState) {
                synchronized (issued) {
                    issued.add(future);
                }
            }
            return future;
        }

        boolean hasOnePendingLoad() {
            synchronized (issued) {
                return issued.size() == 1 && !issued.get(0).isDone();
            }
        }

        void quiesceIssued() {
            CompletableFuture<?>[] settled;
            synchronized (issued) {
                // Observe settlement only; queued load failures must not replace the cursor's error or stop reason.
                settled = issued.stream().map(future -> future.handle((record, error) -> null))
                        .toArray(CompletableFuture[]::new);
            }
            CompletableFuture.allOf(settled).join();
        }
    }

    private static TrackingStore openTracking(FDBRecordContext context, byte[] subspace) {
        return (TrackingStore) new TrackingBuilder().setContext(context).setSubspace(new Subspace(subspace))
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

    private static Map<String, Object> terminal(RecordCursorResult<?> next, ExecuteProperties properties, TrackingStore store) {
        store.quiesceIssued();
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

    private static void proofRequire(boolean condition, String message) {
        if (!condition) throw new IllegalStateException(message);
    }

    private static <T> T proofAwait(CompletableFuture<T> future) {
        try {
            return future.get(5, TimeUnit.SECONDS);
        } catch (InterruptedException error) {
            Thread.currentThread().interrupt();
            throw new IllegalStateException("proof wait interrupted", error);
        } catch (Exception error) {
            throw new IllegalStateException("bounded proof wait failed", error);
        }
    }

    private static final class HeadControl {
        final boolean readyAtOffer;
        final CompletableFuture<Void> release = new CompletableFuture<>();
        final CompletableFuture<Boolean> nativeReady = new CompletableFuture<>();
        final AtomicInteger sourceCalls = new AtomicInteger();
        final AtomicInteger rawRanges = new AtomicInteger();
        final AtomicInteger mappings = new AtomicInteger();
        final AtomicLong indexBytes = new AtomicLong();
        final AtomicLong recordBytes = new AtomicLong();
        final List<CompletableFuture<FDBStoredRecord<Message>>> loads = new ArrayList<>();
        final List<Tuple> primaryKeys = new ArrayList<>();
        volatile boolean enabled;
        volatile boolean mappedReadyWhenOffered;
        volatile boolean nativeReadyWhenOffered;
        volatile boolean gateClosedWhenOffered;
        byte[] recordPrefix;
        byte[] indexPrefix;
        ExecuteState state;

        HeadControl(boolean readyAtOffer) {
            this.readyAtOffer = readyAtOffer;
        }

        synchronized void capture(Tuple primaryKey, CompletableFuture<FDBStoredRecord<Message>> future) {
            primaryKeys.add(primaryKey);
            loads.add(future);
        }

        synchronized List<CompletableFuture<FDBStoredRecord<Message>>> issued() {
            return new ArrayList<>(loads);
        }
    }

    private static final class HeadBuilder extends FDBRecordStore.Builder {
        private final HeadControl control;

        HeadBuilder(HeadControl control) {
            this.control = control;
        }

        @Override public FDBRecordStore build() { return new HeadStore(this, control); }
    }

    private static final class HeadStore extends FDBRecordStore {
        private final HeadControl control;

        HeadStore(FDBRecordStore.Builder builder, HeadControl control) {
            super(builder.getContext(), builder.getSubspaceProvider(), builder.getFormatVersionEnum(),
                    builder.getMetaDataProvider(), builder.getSerializer(), builder.getIndexMaintainerRegistry(),
                    builder.getIndexMaintenanceFilter(), builder.getPipelineSizer(), builder.getStoreStateCache(),
                    builder.getStateCacheabilityOnOpen(), builder.getUserVersionChecker(),
                    builder.getBypassFullStoreLockReason(), builder.getPlanSerializationRegistry());
            this.control = control;
        }

        @Override
        public CompletableFuture<FDBStoredRecord<Message>> loadRecordInternal(Tuple primaryKey, ExecuteState state, boolean snapshot) {
            CompletableFuture<FDBStoredRecord<Message>> actual = super.loadRecordInternal(primaryKey, state, snapshot);
            if (!control.enabled) return actual;
            proofRequire(state == control.state, "load must use the actual statement ExecuteState");
            control.capture(primaryKey, actual);
            proofRequire(proofAwait(control.nativeReady), "real record range must be native-ready and nonempty");
            if (control.readyAtOffer) {
                control.release.complete(null);
                // Return the same production future, but only after its full load has completed.
                proofAwait(actual);
            }
            return actual;
        }

        @Override
        public CompletableFuture<FDBIndexedRecord<Message>> loadIndexEntryRecord(IndexEntry entry, IndexOrphanBehavior orphan, ExecuteState state) {
            CompletableFuture<FDBIndexedRecord<Message>> actual = super.loadIndexEntryRecord(entry, orphan, state);
            if (control.enabled) {
                control.mappings.incrementAndGet();
                control.mappedReadyWhenOffered = actual.isDone();
                control.nativeReadyWhenOffered = control.nativeReady.isDone()
                        && !control.nativeReady.isCompletedExceptionally() && Boolean.TRUE.equals(control.nativeReady.getNow(false));
                control.gateClosedWhenOffered = !control.release.isDone();
                proofRequire(control.mappedReadyWhenOffered == control.readyAtOffer,
                        "full production mapping future must match its controlled offer schedule");
                proofRequire(control.nativeReadyWhenOffered, "native readiness must precede either mapping offer");
            }
            return actual;
        }

        @Override
        public RecordCursor<IndexEntry> scanIndex(Index index, IndexScanType type, TupleRange range,
                                                  byte[] continuation, ScanProperties properties) {
            RecordCursor<IndexEntry> actual = super.scanIndex(index, type, range, continuation, properties);
            if (!control.enabled) return actual;
            return new RecordCursor<IndexEntry>() {
                @Override
                public CompletableFuture<RecordCursorResult<IndexEntry>> onNext() {
                    control.sourceCalls.incrementAndGet();
                    // The real KeyValueCursor / CursorLimitManager produces the scan error.
                    return actual.onNext();
                }
                @Override public void close() { actual.close(); }
                @Override public boolean isClosed() { return actual.isClosed(); }
                @Override public Executor getExecutor() { return actual.getExecutor(); }
                @Override public boolean accept(RecordCursorVisitor visitor) { return actual.accept(visitor); }
            };
        }
    }

    private static Object headInvoke(Object delegate, Method method, Object[] arguments) throws Throwable {
        try {
            return method.invoke(delegate, arguments);
        } catch (InvocationTargetException error) {
            throw error.getCause();
        }
    }

    private static boolean headStartsWith(byte[] key, byte[] prefix) {
        if (prefix == null || key.length < prefix.length) return false;
        for (int i = 0; i < prefix.length; i++) if (key[i] != prefix[i]) return false;
        return true;
    }

    private static byte[] headBegin(Object[] arguments) {
        Object first = arguments[0];
        if (first instanceof KeySelector) return ((KeySelector) first).getKey();
        if (first instanceof Range) return ((Range) first).begin;
        if (first instanceof byte[]) return (byte[]) first;
        throw new IllegalStateException("unexpected real range overload");
    }

    @SuppressWarnings("unchecked")
    private static <T extends ReadTransaction> T headTransaction(T actual, Class<T> api, HeadControl control) {
        return (T) Proxy.newProxyInstance(api.getClassLoader(), new Class<?>[] {api}, (proxy, method, arguments) -> {
            Object result = headInvoke(actual, method, arguments);
            if (method.getName().equals("snapshot")) return headTransaction((ReadTransaction) result, ReadTransaction.class, control);
            if (!control.enabled || !method.getName().equals("getRange")) return result;
            byte[] begin = headBegin(arguments);
            boolean record = headStartsWith(begin, control.recordPrefix);
            boolean index = headStartsWith(begin, control.indexPrefix);
            if (!record && !index) return result;
            if (record) control.rawRanges.incrementAndGet();
            AsyncIterable<KeyValue> real = (AsyncIterable<KeyValue>) result;
            return new AsyncIterable<KeyValue>() {
                @Override
                public AsyncIterator<KeyValue> iterator() {
                    AsyncIterator<KeyValue> delegate = real.iterator();
                    return new AsyncIterator<KeyValue>() {
                        @Override
                        public CompletableFuture<Boolean> onHasNext() {
                            CompletableFuture<Boolean> ready = delegate.onHasNext();
                            if (!record) return ready;
                            ready.whenComplete((hasNext, error) -> {
                                if (error == null) control.nativeReady.complete(hasNext);
                                else control.nativeReady.completeExceptionally(error);
                            });
                            return ready.thenCombine(control.release, (hasNext, ignore) -> hasNext);
                        }
                        @Override public boolean hasNext() { return onHasNext().join(); }
                        @Override
                        public KeyValue next() {
                            KeyValue value = delegate.next();
                            (record ? control.recordBytes : control.indexBytes).addAndGet(value.getKey().length + value.getValue().length);
                            return value;
                        }
                        @Override public void remove() { delegate.remove(); }
                        @Override public void cancel() { delegate.cancel(); }
                    };
                }
                @Override
                public CompletableFuture<List<KeyValue>> asList() {
                    CompletableFuture<List<KeyValue>> list = real.asList();
                    if (record) {
                        list.whenComplete((values, error) -> {
                            if (error == null) control.nativeReady.complete(!values.isEmpty());
                            else control.nativeReady.completeExceptionally(error);
                        });
                        list = list.thenCombine(control.release, (values, ignore) -> values);
                    }
                    return list.thenApply(values -> {
                        for (KeyValue value : values) {
                            (record ? control.recordBytes : control.indexBytes).addAndGet(value.getKey().length + value.getValue().length);
                        }
                        return values;
                    });
                }
            };
        });
    }

    @ConformanceStep("probeIndexPipelinedReadyHead")
    public Map<String, Object> probeReadyHead(String clusterFile, String tenantName, byte[] subspace, int repeats) {
        proofRequire(repeats >= 10 && repeats <= 100, "proof requires 10..100 repeats per path and arm");
        proofRequire(Arrays.equals(subspace, Tuple.from(Tuple.from(), "limits").pack()), "proof uses the exact tenant-root limits fixture subspace");
        seed(clusterFile, tenantName, subspace, 6);
        Map<String, Object> calibration = runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = open(context, subspace);
            Index index = store.getRecordMetaData().getIndex("version");
            List<KeyValue> keys = context.ensureActive().getRange(store.indexSubspace(index).range(), 1, true).asList().join();
            proofRequire(keys.size() == 1, "calibration needs a real reverse VERSION row");
            long indexBytes = keys.get(0).getKey().length + keys.get(0).getValue().length;
            List<KeyValue> parts = context.ensureActive().getRange(store.recordsSubspace().subspace(Tuple.from(5L)).range()).asList().join();
            proofRequire(!parts.isEmpty(), "calibration needs real record 5 parts");
            long recordBytes = parts.stream().mapToLong(part -> part.getKey().length + part.getValue().length).sum();
            proofRequire(indexBytes + recordBytes == 91, "independent raw calibration must total 91 bytes");
            ExecuteProperties properties = ExecuteProperties.newBuilder().setReturnedRowLimit(1).build();
            try (RecordCursor<FDBIndexedRecord<Message>> cursor = store.scanIndexRecords("version", IndexScanType.BY_VALUE,
                    TupleRange.ALL, null, IndexOrphanBehavior.ERROR, new ScanProperties(properties, true))) {
                RecordCursorResult<FDBIndexedRecord<Message>> first = cursor.onNext().join();
                proofRequire(first.hasNext() && first.get().getIndexEntry().getPrimaryKey().equals(Tuple.from(5L)), "baseline must load PK5");
                return Map.of("indexBytes", indexBytes, "recordBytes", recordBytes, "baselineRow", row(first));
            }
        });
        long expectedIndex = ((Number) calibration.get("indexBytes")).longValue();
        long expectedRecord = ((Number) calibration.get("recordBytes")).longValue();
        List<Map<String, Object>> observations = new ArrayList<>();
        ExecutorService callbacks = Executors.newFixedThreadPool(4);
        try {
            for (String path : List.of("DIRECT_PIPELINE", "ORDINARY_SCAN")) {
                for (boolean readyAtOffer : new boolean[] {true, false}) {
                    for (int repeat = 0; repeat < repeats; repeat++) {
                        HeadControl control = new HeadControl(readyAtOffer);
                        try (Tenant tenant = createDatabase(clusterFile).database().openTenant(tenantName.getBytes(StandardCharsets.UTF_8));
                             Transaction nativeTransaction = tenant.createTransaction(callbacks)) {
                            nativeTransaction.options().setTimeout(4000);
                            Transaction proxy = headTransaction(nativeTransaction, Transaction.class, control);
                            try (FDBRecordContext context = createContextFromTransaction(createDatabase(clusterFile), proxy)) {
                                proofRequire(!Boolean.TRUE.equals(context.getPropertyStorage().getPropertyValue(FDBRecordStoreProperties.LOAD_RECORDS_VIA_GETS)),
                                        "proof requires the real record range-read path");
                                HeadBuilder builder = new HeadBuilder(control);
                                builder.setContext(context).setSubspace(new Subspace(subspace)).setMetaDataProvider(metadata())
                                        .setUserVersionChecker(ALWAYS_READABLE_CHECKER).setFormatVersion(14);
                                FDBRecordStore store = builder.createOrOpen();
                                ExecuteProperties properties = ExecuteProperties.newBuilder().setScannedRecordsLimit(1).setFailOnScanLimitReached(true).build();
                                ScanProperties scanProperties = new ScanProperties(properties, true);
                                control.recordPrefix = store.recordsSubspace().pack();
                                control.indexPrefix = store.indexSubspace(store.getRecordMetaData().getIndex("version")).pack();
                                control.state = properties.getState();
                                control.enabled = true;
                                RecordCursor<FDBIndexedRecord<Message>> cursor;
                                if (path.equals("DIRECT_PIPELINE")) {
                                    RecordCursor<IndexEntry> source = store.scanIndex(store.getRecordMetaData().getIndex("version"),
                                            IndexScanType.BY_VALUE, TupleRange.ALL, null, scanProperties);
                                    cursor = new MapPipelinedCursor<>(source,
                                            entry -> store.loadIndexEntryRecord(entry, IndexOrphanBehavior.ERROR, properties.getState()),
                                            store.getPipelineSize(PipelineOperation.INDEX_TO_RECORD));
                                } else {
                                    cursor = store.scanIndexRecords("version", IndexScanType.BY_VALUE, TupleRange.ALL,
                                            null, IndexOrphanBehavior.ERROR, scanProperties);
                                }
                                try (RecordCursor<FDBIndexedRecord<Message>> closing = cursor) {
                                    List<Map<String, Object>> details = new ArrayList<>();
                                    int firstCalls = -1;
                                    int firstScanned = -1;
                                    Throwable failure;
                                    try {
                                        while (true) {
                                            RecordCursorResult<FDBIndexedRecord<Message>> next = proofAwait(cursor.onNext());
                                            if (firstCalls < 0) {
                                                firstCalls = control.sourceCalls.get();
                                                firstScanned = properties.getState().getRecordsScanned();
                                            }
                                            proofRequire(next.hasNext(), "proof must reach the actual scan exception, not a terminal result");
                                            details.add(row(next));
                                        }
                                    } catch (RuntimeException error) {
                                        if (firstCalls < 0) {
                                            firstCalls = control.sourceCalls.get();
                                            firstScanned = properties.getState().getRecordsScanned();
                                        }
                                        failure = error;
                                        while (failure.getCause() != null) failure = failure.getCause();
                                    }
                                    proofRequire(failure instanceof ScanLimitReachedException, "proof needs the real source ScanLimitReachedException");
                                    proofRequire(details.size() == (readyAtOffer ? 1 : 0), "controlled mapping offer must produce exact ready/held row count");
                                    proofRequire(firstCalls == (readyAtOffer ? 1 : 2) && control.sourceCalls.get() == 2,
                                            "ready head must return before pull2; held head must fail on pull2");
                                    proofRequire(firstScanned == (readyAtOffer ? 1 : 2) && properties.getState().getRecordsScanned() == 2,
                                            "actual source attempt counts must match the offer schedule");
                                    if (readyAtOffer) proofRequire(details.get(0).equals(calibration.get("baselineRow")), "ready row must equal real ungated native baseline");
                                    List<CompletableFuture<FDBStoredRecord<Message>>> issued = control.issued();
                                    int rawAtFailure = control.rawRanges.get();
                                    long completedAtFailure = issued.stream().filter(CompletableFuture::isDone).count();
                                    long failureBytes = properties.getState().getBytesScanned();
                                    proofRequire(issued.size() == 1 && control.mappings.get() == 1 && rawAtFailure == 1,
                                            "both paths must issue exactly one real record load and full mapping");
                                    proofRequire(control.primaryKeys.get(0).equals(Tuple.from(5L)), "the real offered load must target PK5");
                                    proofRequire(completedAtFailure == (readyAtOffer ? 1 : 0)
                                            && failureBytes == (readyAtOffer ? expectedIndex + expectedRecord : expectedIndex),
                                            "failure-time charges must reflect the structurally controlled real load");
                                    control.release.complete(null);
                                    proofAwait(CompletableFuture.allOf(issued.toArray(CompletableFuture[]::new)));
                                    proofRequire(control.issued().size() == 1 && control.rawRanges.get() == rawAtFailure,
                                            "quiescence must not issue another load or range");
                                    proofRequire(control.indexBytes.get() == expectedIndex && control.recordBytes.get() == expectedRecord
                                            && properties.getState().getBytesScanned() == expectedIndex + expectedRecord,
                                            "real consumed key/value bytes must equal independent native calibration");
                                    Map<String, Object> observation = new HashMap<>();
                                    observation.put("path", path);
                                    observation.put("arm", readyAtOffer ? "READY_WHEN_OFFERED" : "HELD_WHEN_OFFERED");
                                    observation.put("repeat", repeat);
                                    observation.put("rows", details.size());
                                    observation.put("rowDetails", details);
                                    observation.put("baselineRow", calibration.get("baselineRow"));
                                    observation.put("exceptionClass", failure.getClass().getSimpleName());
                                    observation.put("sourceCallsAtFirstResult", firstCalls);
                                    observation.put("sourceCallsAtFailure", control.sourceCalls.get());
                                    observation.put("firstCallScannedRecords", firstScanned);
                                    observation.put("mappedReadyWhenOffered", control.mappedReadyWhenOffered);
                                    observation.put("nativeReadyWhenOffered", control.nativeReadyWhenOffered);
                                    observation.put("gateClosedWhenOffered", control.gateClosedWhenOffered);
                                    observation.put("issuedLoads", issued.size());
                                    observation.put("issuedMappings", control.mappings.get());
                                    observation.put("rawRangesIssued", rawAtFailure);
                                    observation.put("completedLoadsAtFailure", completedAtFailure);
                                    observation.put("quiescedLoads", issued.stream().filter(future -> future.isDone() && !future.isCompletedExceptionally()).count());
                                    observation.put("issuedLoadsAfterQuiescence", control.issued().size());
                                    observation.put("rawRangesAfterQuiescence", control.rawRanges.get());
                                    observation.put("recordsScanned", properties.getState().getRecordsScanned());
                                    observation.put("indexBytes", control.indexBytes.get());
                                    observation.put("recordBytes", control.recordBytes.get());
                                    observation.put("expectedIndexBytes", expectedIndex);
                                    observation.put("expectedRecordBytes", expectedRecord);
                                    observation.put("failureBytes", failureBytes);
                                    observation.put("quiescedBytes", properties.getState().getBytesScanned());
                                    observation.put("recordPrimaryKeys", List.of(bytes(control.primaryKeys.get(0).pack())));
                                    observation.put("rowPrimaryKeys", details.stream().map(detail -> detail.get("primaryKey")).toArray());
                                    observation.put("rowContinuations", details.stream().map(detail -> detail.get("continuation")).toArray());
                                    observations.add(observation);
                                } finally {
                                    control.release.complete(null);
                                }
                            }
                        }
                    }
                }
            }
        } finally {
            callbacks.shutdownNow();
        }
        return Map.of("observations", observations);
    }

    @ConformanceStep("scanIndexScanLimits")
    public Map<String, Object> scan(String clusterFile, String tenantName, byte[] subspace, Request request) {
        if (!request.holdRecordLoadsUntilSourceError) {
            return runInContext(clusterFile, tenantName, context -> scanInContext(context, subspace, request, null));
        }
        boolean scanned = request.scannedLimit == 1 && request.byteLimit == 0 && request.timeLimit == 0;
        boolean bytes = request.scannedLimit == 0 && request.byteLimit == 1 && request.timeLimit == 0;
        boolean time = request.scannedLimit == 0 && request.byteLimit == 0 && request.timeLimit == 200;
        proofRequire(request.records && request.failOnScanLimit && request.rowLimit == 0
                        && (request.continuation == null || request.continuation.isEmpty()) && (scanned || bytes || time),
                "held matrix schedule requires records, fail-on-limit, no row limit/continuation, and exactly scanned1, byte1, or time200");
        HeadControl hold = new HeadControl(false);
        ExecutorService callbacks = Executors.newFixedThreadPool(4);
        FDBDatabase database = createDatabase(clusterFile);
        Database nativeDatabase = database.database();
        try (Tenant tenant = tenantName == null || tenantName.isEmpty() ? null
                    : nativeDatabase.openTenant(tenantName.getBytes(StandardCharsets.UTF_8));
             Transaction transaction = tenant == null ? nativeDatabase.createTransaction(callbacks) : tenant.createTransaction(callbacks);
             FDBRecordContext context = createContextFromTransaction(database, headTransaction(transaction, Transaction.class, hold))) {
            Map<String, Object> result = scanInContext(context, subspace, request, hold);
            context.commitAsync().join();
            return result;
        } finally {
            hold.release.complete(null);
            callbacks.shutdownNow();
        }
    }

    private static Map<String, Object> scanInContext(FDBRecordContext context, byte[] subspace, Request request, HeadControl hold) {
        TrackingStore store = openTracking(context, subspace);
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
                .setSkip(request.skip).setDefaultCursorStreamingMode(CursorStreamingMode.valueOf(request.streaming))
                .setFailOnScanLimitReached(request.failOnScanLimit);
        if (request.scannedLimit > 0) builder.setScannedRecordsLimit(request.scannedLimit);
        if (request.byteLimit > 0) builder.setScannedBytesLimit(request.byteLimit);
        if (request.timeLimit > 0) builder.setTimeLimit(request.timeLimit);
        ExecuteProperties properties = builder.build();
        store.trackedState = properties.getState();
        ScanProperties scanProperties = new ScanProperties(properties, request.reverse);
        Index index = store.getRecordMetaData().getIndex(request.indexName);
        byte[] continuation = request.continuation == null || request.continuation.isEmpty()
                ? null : Base64.getDecoder().decode(request.continuation);
        KeyValueCursor.Builder leafBuilder = KeyValueCursor.Builder.newBuilder(store.indexSubspace(index))
                .setContext(context).setRange(TupleRange.ALL).setContinuation(continuation).setScanProperties(scanProperties);
        List<Map<String, Object>> rangeOptions;
        List<Map<String, Object>> selectors;
        try (KeyValueCursor leaf = leafBuilder.build()) {
            // Native Java and Go streaming enum values differ by one.
            rangeOptions = List.of(Map.of("mode", leafBuilder.getStreamingMode().code() + 1,
                    "limit", leafBuilder.getLimit(), "reverse", request.reverse));
            // The getRange selectors after continuation and skip (KeyValueCursorBase.java:327-352).
            KeySelector begin = leafBuilder.getBegin();
            KeySelector end = leafBuilder.getEnd();
            selectors = List.of(Map.of("beginKey", bytes(begin.getKey()), "beginOrEqual", begin.orEqual(),
                    "beginOffset", begin.getOffset(), "endKey", bytes(end.getKey()), "endOrEqual", end.orEqual(),
                    "endOffset", end.getOffset()));
        }
        if (hold != null) {
            proofRequire(store.getPipelineSize(PipelineOperation.INDEX_TO_RECORD) > 1,
                    "held matrix schedule requires room to pull the real source error before awaiting a mapping");
            proofRequire(!Boolean.TRUE.equals(context.getPropertyStorage().getPropertyValue(FDBRecordStoreProperties.LOAD_RECORDS_VIA_GETS)),
                    "held matrix schedule requires real record range reads");
            // Only record ranges are held; metadata opening and index reads retain their ordinary path.
            hold.recordPrefix = store.recordsSubspace().pack();
            hold.enabled = true;
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
        result.put("selectors", selectors);
        result.put("reason", "");
        result.put("continuation", "");
        result.put("end", false);
        result.put("exceptionClass", "");
        try (RecordCursor<?> closing = cursor) {
            try {
                while (true) {
                    RecordCursorResult<?> next = cursor.onNext().join();
                    if (next.hasNext()) {
                        rows.add(row(next));
                    } else {
                        proofRequire(hold == null, "held matrix schedule must reach the real source exception, not a terminal result");
                        result.putAll(terminal(next, properties, store));
                        for (int repeat = 0; repeat < 2; repeat++) {
                            repeats.add(terminal(cursor.onNext().join(), properties, store));
                        }
                        break;
                    }
                }
            } finally {
                try {
                    if (hold != null) {
                        proofRequire(!hold.release.isDone() && hold.rawRanges.get() == 1 && store.hasOnePendingLoad(),
                                "held matrix schedule must hold exactly one real original record load through the source error");
                    }
                } finally {
                    if (hold != null) hold.release.complete(null);
                    store.quiesceIssued();
                }
            }
        } catch (RuntimeException error) {
            Throwable root = error;
            while (root.getCause() != null) root = root.getCause();
            result.put("exceptionClass", root.getClass().getSimpleName());
        }
        if (hold != null) {
            proofRequire(result.get("exceptionClass").equals("ScanLimitReachedException") && rows.isEmpty(),
                    "held matrix schedule must preserve the real source exception with no offered rows");
        }
        result.put("scannedRecords", properties.getState().getRecordsScanned());
        result.put("scannedBytes", properties.getState().getBytesScanned());
        return result;
    }
}
