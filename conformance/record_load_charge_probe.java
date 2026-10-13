package com.birdayz.conformance;

import com.apple.foundationdb.KeySelector;
import com.apple.foundationdb.KeyValue;
import com.apple.foundationdb.Range;
import com.apple.foundationdb.ReadTransaction;
import com.apple.foundationdb.Tenant;
import com.apple.foundationdb.Transaction;
import com.apple.foundationdb.async.AsyncIterable;
import com.apple.foundationdb.async.AsyncIterator;
import com.apple.foundationdb.record.ExecuteProperties;
import com.apple.foundationdb.record.ExecuteState;
import com.apple.foundationdb.record.FDBRecordStoreProperties;
import com.apple.foundationdb.record.IndexEntry;
import com.apple.foundationdb.record.IndexScanType;
import com.apple.foundationdb.record.RecordCursor;
import com.apple.foundationdb.record.RecordCursorResult;
import com.apple.foundationdb.record.RecordCursorVisitor;
import com.apple.foundationdb.record.RecordLayerDemo;
import com.apple.foundationdb.record.RecordMetaData;
import com.apple.foundationdb.record.RecordMetaDataBuilder;
import com.apple.foundationdb.record.ScanLimitReachedException;
import com.apple.foundationdb.record.ScanProperties;
import com.apple.foundationdb.record.TupleRange;
import com.apple.foundationdb.record.metadata.Index;
import com.apple.foundationdb.record.metadata.IndexTypes;
import com.apple.foundationdb.record.metadata.Key;
import com.apple.foundationdb.record.provider.foundationdb.FDBIndexedRecord;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordContext;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.record.provider.foundationdb.FDBStoredRecord;
import com.apple.foundationdb.record.provider.foundationdb.IndexOrphanBehavior;
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
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.atomic.AtomicLong;

/** Structural readiness probe: all data and errors come from production Java over real FDB. */
class RecordLoadChargeProbeSteps extends ConformanceBase {
    private static void require(boolean condition, String message) {
        if (!condition) throw new IllegalStateException(message);
    }

    private static <T> T await(CompletableFuture<T> future) throws Exception {
        return future.get(5, TimeUnit.SECONDS);
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

    private static boolean startsWith(byte[] bytes, byte[] prefix) {
        if (prefix == null || bytes.length < prefix.length) return false;
        for (int i = 0; i < prefix.length; i++) {
            if (bytes[i] != prefix[i]) return false;
        }
        return true;
    }

    private static final class IssuedLoad {
        final Tuple primaryKey;
        final CompletableFuture<FDBStoredRecord<Message>> charged;

        IssuedLoad(Tuple primaryKey, CompletableFuture<FDBStoredRecord<Message>> charged) {
            this.primaryKey = primaryKey;
            this.charged = charged;
        }
    }

    private static final class Control {
        final boolean releaseBeforeError;
        final CompletableFuture<Void> release = new CompletableFuture<>();
        final CompletableFuture<Void> loadIssued = new CompletableFuture<>();
        final CompletableFuture<Boolean> nativeReady = new CompletableFuture<>();
        final AtomicInteger rawRanges = new AtomicInteger();
        final AtomicLong indexBytes = new AtomicLong();
        final AtomicLong recordBytes = new AtomicLong();
        final List<IssuedLoad> loads = new ArrayList<>();
        volatile boolean enabled;
        byte[] recordPrefix;
        byte[] indexPrefix;
        ExecuteState state;

        Control(boolean releaseBeforeError) {
            this.releaseBeforeError = releaseBeforeError;
        }

        synchronized void capture(Tuple primaryKey, CompletableFuture<FDBStoredRecord<Message>> future) {
            loads.add(new IssuedLoad(primaryKey, future));
            loadIssued.complete(null);
        }

        synchronized List<IssuedLoad> issued() {
            return new ArrayList<>(loads);
        }

        CompletableFuture<Void> alreadyIssued() {
            List<IssuedLoad> snapshot = issued();
            require(snapshot.size() == 1, "expected exactly one issued load, got " + snapshot.size());
            return CompletableFuture.allOf(snapshot.stream().map(load -> load.charged).toArray(CompletableFuture[]::new));
        }

        CompletableFuture<Void> beforeRealError() {
            return loadIssued.thenCompose(ignore -> nativeReady).thenCompose(ready -> {
                require(ready, "the real record range must contain data");
                if (releaseBeforeError) {
                    release.complete(null);
                    return alreadyIssued();
                }
                // Native I/O is ready, but the raw/mapping future stays held through the error.
                return CompletableFuture.completedFuture(null);
            });
        }
    }

    private static final class CapturingBuilder extends FDBRecordStore.Builder {
        private final Control control;

        CapturingBuilder(Control control) {
            this.control = control;
        }

        @Override
        public FDBRecordStore build() {
            return new CapturingStore(this, control);
        }
    }

    private static final class CapturingStore extends FDBRecordStore {
        private final Control control;

        CapturingStore(FDBRecordStore.Builder builder, Control control) {
            super(builder.getContext(), builder.getSubspaceProvider(), builder.getFormatVersionEnum(),
                    builder.getMetaDataProvider(), builder.getSerializer(), builder.getIndexMaintainerRegistry(),
                    builder.getIndexMaintenanceFilter(), builder.getPipelineSizer(), builder.getStoreStateCache(),
                    builder.getStateCacheabilityOnOpen(), builder.getUserVersionChecker(),
                    builder.getBypassFullStoreLockReason(), builder.getPlanSerializationRegistry());
            this.control = control;
        }

        @Override
        public CompletableFuture<FDBStoredRecord<Message>> loadRecordInternal(Tuple primaryKey, ExecuteState state, boolean snapshot) {
            CompletableFuture<FDBStoredRecord<Message>> future = super.loadRecordInternal(primaryKey, state, snapshot);
            if (control.enabled) {
                require(state == control.state, "captured load must charge the scan's actual ExecuteState");
                control.capture(primaryKey, future);
            }
            return future;
        }

        @Override
        public RecordCursor<IndexEntry> scanIndex(Index index, IndexScanType type, TupleRange range,
                                                  byte[] continuation, ScanProperties properties) {
            RecordCursor<IndexEntry> real = super.scanIndex(index, type, range, continuation, properties);
            if (!control.enabled) return real;
            require(index.getName().equals("version"), "probe must use the VERSION index");
            return new RecordCursor<IndexEntry>() {
                private int calls;

                @Override
                public CompletableFuture<RecordCursorResult<IndexEntry>> onNext() {
                    calls++;
                    if (calls == 2) {
                        // Both arms observe native readiness before the REAL limit manager fails.
                        return control.beforeRealError().thenCompose(ignore -> real.onNext());
                    }
                    return real.onNext();
                }

                @Override public void close() { real.close(); }
                @Override public boolean isClosed() { return real.isClosed(); }
                @Override public Executor getExecutor() { return real.getExecutor(); }
                @Override public boolean accept(RecordCursorVisitor visitor) { return real.accept(visitor); }
            };
        }
    }

    private static Object invoke(Object delegate, Method method, Object[] arguments) throws Throwable {
        try {
            return method.invoke(delegate, arguments);
        } catch (InvocationTargetException error) {
            throw error.getCause();
        }
    }

    private static byte[] begin(Object[] arguments) {
        Object first = arguments[0];
        if (first instanceof byte[]) return (byte[]) first;
        if (first instanceof KeySelector) return ((KeySelector) first).getKey();
        if (first instanceof Range) return ((Range) first).begin;
        throw new IllegalStateException("unexpected real getRange overload: " + first.getClass());
    }

    @SuppressWarnings("unchecked")
    private static <T extends ReadTransaction> T transactionProxy(T real, Class<T> api, Control control) {
        return (T) Proxy.newProxyInstance(api.getClassLoader(), new Class<?>[] {api}, (proxy, method, arguments) -> {
            Object result = invoke(real, method, arguments);
            if (method.getName().equals("snapshot")) {
                return transactionProxy((ReadTransaction) result, ReadTransaction.class, control);
            }
            if (!control.enabled || !method.getName().equals("getRange")) return result;
            byte[] begin = begin(arguments);
            boolean record = startsWith(begin, control.recordPrefix);
            boolean index = startsWith(begin, control.indexPrefix);
            if (!record && !index) return result;
            if (record) control.rawRanges.incrementAndGet();
            AsyncIterable<KeyValue> iterable = (AsyncIterable<KeyValue>) result;
            return new AsyncIterable<KeyValue>() {
                @Override
                public AsyncIterator<KeyValue> iterator() {
                    AsyncIterator<KeyValue> delegate = iterable.iterator();
                    return new AsyncIterator<KeyValue>() {
                        @Override
                        public CompletableFuture<Boolean> onHasNext() {
                            CompletableFuture<Boolean> ready = delegate.onHasNext();
                            if (!record) return ready;
                            ready.whenComplete((hasNext, error) -> {
                                if (error == null) control.nativeReady.complete(hasNext);
                                else control.nativeReady.completeExceptionally(error);
                            });
                            // Delay only readiness. The iterator still returns its real native rows.
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
                    CompletableFuture<List<KeyValue>> realList = iterable.asList();
                    if (record) {
                        realList.whenComplete((values, error) -> {
                            if (error == null) control.nativeReady.complete(!values.isEmpty());
                            else control.nativeReady.completeExceptionally(error);
                        });
                        realList = realList.thenCombine(control.release, (values, ignore) -> values);
                    }
                    return realList.thenApply(values -> {
                        for (KeyValue value : values) {
                            (record ? control.recordBytes : control.indexBytes).addAndGet(value.getKey().length + value.getValue().length);
                        }
                        return values;
                    });
                }
            };
        });
    }

    @ConformanceStep("probeRecordLoadCharges")
    public Map<String, Object> probe(String clusterFile, String tenantName, byte[] subspace, int repeats) throws Exception {
        require(repeats >= 10 && repeats <= 100, "readiness study requires 10..100 repeats per arm");
        // The Go fixture's Sub(tuple.Tuple{}) contributes one nested empty tuple.
        require(Arrays.equals(subspace, Tuple.from(Tuple.from(), "limits").pack()), "89-byte fixture requires the exact limits subspace");
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder().setContext(context).setSubspace(new Subspace(subspace))
                    .setMetaDataProvider(metadata()).setUserVersionChecker(ALWAYS_READABLE_CHECKER).setFormatVersion(14).createOrOpen();
            for (int id = 0; id < 6; id++) {
                store.saveRecord(RecordLayerDemo.Order.newBuilder().setOrderId(id).setPrice(100 - id)
                        .addTags(id % 2 == 0 ? "even" : "odd").build());
            }
            return null;
        });
        List<Map<String, Object>> observations = new ArrayList<>();
        for (boolean releaseBeforeError : new boolean[] {false, true}) {
            for (int repeat = 0; repeat < repeats; repeat++) {
                Control control = new Control(releaseBeforeError);
                try (Tenant tenant = createDatabase(clusterFile).database().openTenant(tenantName.getBytes(StandardCharsets.UTF_8));
                     Transaction nativeTransaction = tenant.createTransaction()) {
                    nativeTransaction.options().setTimeout(4000);
                    Transaction proxy = transactionProxy(nativeTransaction, Transaction.class, control);
                    try (FDBRecordContext context = createContextFromTransaction(createDatabase(clusterFile), proxy)) {
                        require(!Boolean.TRUE.equals(context.getPropertyStorage().getPropertyValue(FDBRecordStoreProperties.LOAD_RECORDS_VIA_GETS)),
                                "probe requires the default real range-read path");
                        CapturingBuilder builder = new CapturingBuilder(control);
                        builder.setContext(context).setSubspace(new Subspace(subspace)).setMetaDataProvider(metadata())
                                .setUserVersionChecker(ALWAYS_READABLE_CHECKER).setFormatVersion(14);
                        FDBRecordStore store = builder.createOrOpen();
                        ExecuteProperties properties = ExecuteProperties.newBuilder().setScannedRecordsLimit(1)
                                .setFailOnScanLimitReached(true).build();
                        control.recordPrefix = store.recordsSubspace().pack();
                        control.indexPrefix = store.indexSubspace(store.getRecordMetaData().getIndex("version")).pack();
                        control.state = properties.getState();
                        control.enabled = true;
                        RecordCursor<FDBIndexedRecord<Message>> cursor = store.scanIndexRecords("version", IndexScanType.BY_VALUE,
                                TupleRange.ALL, null, IndexOrphanBehavior.ERROR, new ScanProperties(properties, false));
                        try (RecordCursor<FDBIndexedRecord<Message>> closing = cursor) {
                            int rows = 0;
                            Throwable failure;
                            try {
                                while (await(cursor.onNext()).hasNext()) rows++;
                                throw new IllegalStateException("real scan unexpectedly exhausted instead of failing");
                            } catch (Exception error) {
                                failure = error;
                                while (failure.getCause() != null) failure = failure.getCause();
                            }
                            require(failure instanceof ScanLimitReachedException, "expected real ScanLimitReachedException, got " + failure);
                            List<IssuedLoad> issued = control.issued();
                            int issuedAtFailure = issued.size();
                            long failureBytes = properties.getState().getBytesScanned();
                            long completedAtFailure = issued.stream().filter(load -> load.charged.isDone()).count();
                            int rawAtFailure = control.rawRanges.get();
                            boolean gateClosedAtFailure = !control.release.isDone();
                            boolean nativeReadyAtFailure = control.nativeReady.isDone()
                                    && !control.nativeReady.isCompletedExceptionally()
                                    && Boolean.TRUE.equals(control.nativeReady.getNow(false));
                            require(nativeReadyAtFailure, "the real native record read must be ready before either arm's error");
                            require(issuedAtFailure == 1 && rawAtFailure == 1, "failure must leave exactly one already-issued real load/range");
                            require(issued.get(0).primaryKey.equals(Tuple.from(0L)), "the issued record must be VERSION's first primary key 0");
                            if (releaseBeforeError) {
                                require(!gateClosedAtFailure && completedAtFailure == 1 && failureBytes == 89,
                                        "before-error arm must charge 89 before the real error, got " + failureBytes);
                            } else {
                                require(gateClosedAtFailure && completedAtFailure == 0 && rows == 0 && failureBytes == 35,
                                        "held arm must fail with one pending load and 35 bytes, got " + failureBytes);
                            }
                            require(await(control.nativeReady), "the issued real FDB range must contain data");
                            control.release.complete(null);
                            // Quiescence awaits the failure-time list, never asks the store for a new load.
                            await(CompletableFuture.allOf(issued.stream().map(load -> load.charged).toArray(CompletableFuture[]::new)));
                            long quiescedBytes = properties.getState().getBytesScanned();
                            int issuedAfter = control.issued().size();
                            int rawAfter = control.rawRanges.get();
                            long quiescedLoads = issued.stream().filter(load -> load.charged.isDone() && !load.charged.isCompletedExceptionally()).count();
                            require(issuedAfter == issuedAtFailure && rawAfter == rawAtFailure && quiescedLoads == 1,
                                    "quiescence must complete only the one already-issued real load");
                            require(issued.get(0).charged.join() != null, "the real loaded record must exist");
                            require(control.indexBytes.get() == 35 && control.recordBytes.get() == 54 && quiescedBytes == 89,
                                    "quiesced real-read charges must be index35 + record54 = 89, got " + quiescedBytes);
                            require(properties.getState().getRecordsScanned() == 2, "real scanned limiter must have two attempts");
                            Map<String, Object> observation = new HashMap<>();
                            observation.put("arm", releaseBeforeError ? "RELEASED_BEFORE_ERROR" : "HELD_UNTIL_ERROR");
                            observation.put("repeat", repeat);
                            observation.put("issuedLoads", issuedAfter);
                            observation.put("issuedLoadsAtFailure", issuedAtFailure);
                            observation.put("completedLoadsAtFailure", completedAtFailure);
                            observation.put("rawRangesIssued", rawAtFailure);
                            observation.put("gateClosedAtFailure", gateClosedAtFailure);
                            observation.put("nativeReadyAtFailure", nativeReadyAtFailure);
                            observation.put("indexBytes", control.indexBytes.get());
                            observation.put("recordBytes", control.recordBytes.get());
                            observation.put("failureBytes", failureBytes);
                            observation.put("quiescedBytes", quiescedBytes);
                            observation.put("recordsScanned", properties.getState().getRecordsScanned());
                            observation.put("rows", rows);
                            observation.put("exceptionClass", failure.getClass().getSimpleName());
                            observation.put("quiescedLoads", quiescedLoads);
                            observation.put("issuedLoadsAfterQuiescence", issuedAfter);
                            observation.put("rawRangesAfterQuiescence", rawAfter);
                            observation.put("recordPrimaryKeys", List.of(Base64.getEncoder().encodeToString(issued.get(0).primaryKey.pack())));
                            observations.add(observation);
                        } finally {
                            control.release.complete(null);
                        }
                    }
                }
            }
        }
        return Map.of("observations", observations);
    }
}
