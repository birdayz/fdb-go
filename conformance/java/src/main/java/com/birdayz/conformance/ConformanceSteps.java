package com.birdayz.conformance;

import com.apple.foundationdb.record.IndexScanType;
import com.apple.foundationdb.record.IndexState;
import com.apple.foundationdb.record.RecordMetaData;
import com.apple.foundationdb.record.RecordMetaDataBuilder;
import com.apple.foundationdb.record.RecordMetaDataProvider;
import com.apple.foundationdb.record.ScanProperties;
import com.apple.foundationdb.record.TupleRange;
import com.apple.foundationdb.record.IndexEntry;
import com.apple.foundationdb.record.provider.foundationdb.FDBDatabase;
import com.apple.foundationdb.record.provider.foundationdb.FDBDatabaseFactory;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStoreBase;
import com.apple.foundationdb.record.provider.foundationdb.FDBStoredRecord;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordContext;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordContextConfig;
import com.apple.foundationdb.record.metadata.Index;
import com.apple.foundationdb.record.metadata.IndexTypes;
import com.apple.foundationdb.record.metadata.Key;
import com.apple.foundationdb.record.metadata.expressions.GroupingKeyExpression;
import com.apple.foundationdb.record.metadata.expressions.KeyExpression;
import com.apple.foundationdb.subspace.Subspace;
import com.apple.foundationdb.tuple.Tuple;
import com.apple.foundationdb.Database;
import com.apple.foundationdb.Tenant;
import com.apple.foundationdb.Transaction;
import com.google.protobuf.Message;

import com.apple.foundationdb.record.ExecuteProperties;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordVersion;
import com.apple.foundationdb.record.RecordLayerDemo;
import com.apple.foundationdb.record.RecordLayerDemo.Order;
import com.apple.foundationdb.record.RecordLayerDemo.Customer;

import java.io.File;
import java.io.FileWriter;
import java.io.IOException;
import java.lang.reflect.Constructor;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.Base64;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.CompletableFuture;

/**
 * Conformance test steps that can be invoked from Go via the generic invokeJava() function.
 * Each method is annotated with @ConformanceStep and can be called by name with JSON args.
 */
public class ConformanceSteps {

    private static String cachedClusterContent = null;
    private static FDBDatabase cachedDatabase = null;

    /**
     * UserVersionChecker that always marks indexes as READABLE.
     * Needed for conformance tests where Go creates the store and index entries,
     * but Java doesn't know the index was already built.
     */
    private static final FDBRecordStoreBase.UserVersionChecker ALWAYS_READABLE_CHECKER = new FDBRecordStoreBase.UserVersionChecker() {
        @Override
        public CompletableFuture<Integer> checkUserVersion(int oldUserVersion, int oldMetaDataVersion,
                                                            RecordMetaDataProvider metaData) {
            return CompletableFuture.completedFuture(0);
        }

        @Override
        public IndexState needRebuildIndex(Index index, long recordCount, boolean indexOnNewRecordTypes) {
            return IndexState.READABLE;
        }
    };

    @FunctionalInterface
    private interface ContextAction<T> {
        T execute(FDBRecordContext context);
    }

    /**
     * Run an action within an FDBRecordContext, handling tenant vs non-tenant branching.
     *
     * @param clusterFile The FDB cluster file content
     * @param tenantName Optional tenant name for isolation (null or empty for no tenant)
     * @param action The action to execute with the context
     * @return The result of the action
     */
    private <T> T runInContext(String clusterFile, String tenantName, ContextAction<T> action) {
        FDBDatabase db = createDatabase(clusterFile);
        if (tenantName != null && !tenantName.isEmpty()) {
            Database nativeDb = db.database();
            Tenant tenant = nativeDb.openTenant(tenantName.getBytes(StandardCharsets.UTF_8));
            // Use createTransaction + FDBRecordContext.commitAsync() instead of tenant.run()
            // to ensure pre-commit hooks (like version mutation flush) fire correctly.
            Transaction tx = tenant.createTransaction();
            try {
                FDBRecordContext context = createContextFromTransaction(db, tx);
                T result = action.execute(context);
                context.commitAsync().join();
                return result;
            } catch (Exception e) {
                tx.cancel();
                throw e;
            }
        } else {
            return db.run(context -> action.execute(context));
        }
    }

    /**
     * Create an FDBDatabase instance using the provided cluster file content.
     * Caches the database and cluster file to avoid leaking connections and temp files.
     *
     * @param clusterFileContent The cluster file content as a string
     * @return FDBDatabase configured with the cluster file
     * @throws RuntimeException if cluster file cannot be created
     */
    private static synchronized FDBDatabase createDatabase(String clusterFileContent) {
        if (cachedDatabase != null && clusterFileContent.equals(cachedClusterContent)) {
            return cachedDatabase;
        }
        try {
            File tempFile = new File("/tmp/fdb_conformance.cluster");
            try (FileWriter writer = new FileWriter(tempFile)) {
                writer.write(clusterFileContent);
            }
            cachedClusterContent = clusterFileContent;
            cachedDatabase = FDBDatabaseFactory.instance().getDatabase(tempFile.getAbsolutePath());
            return cachedDatabase;
        } catch (IOException e) {
            throw new RuntimeException("Failed to create cluster file: " + e.getMessage(), e);
        }
    }

    /**
     * Create an FDBRecordContext from a tenant transaction using reflection.
     * This is needed because FDBRecordContext constructor is protected and doesn't
     * have built-in tenant support.
     */
    private static FDBRecordContext createContextFromTransaction(FDBDatabase db, Transaction transaction) {
        try {
            Constructor<FDBRecordContext> constructor = FDBRecordContext.class.getDeclaredConstructor(
                FDBDatabase.class,
                Transaction.class,
                FDBRecordContextConfig.class,
                com.apple.foundationdb.record.provider.foundationdb.FDBStoreTimer.class
            );
            constructor.setAccessible(true);
            FDBRecordContextConfig config = FDBRecordContextConfig.newBuilder().build();
            return constructor.newInstance(db, transaction, config, null);
        } catch (Exception e) {
            throw new RuntimeException("Failed to create FDBRecordContext from transaction: " + e.getMessage(), e);
        }
    }

    @ConformanceStep("saveOrder")
    public void saveOrder(String clusterFile, byte[] subspace, Order order, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .createOrOpen();
            store.saveRecord(order);
            return null;
        });
    }

    @ConformanceStep("loadOrder")
    public Order loadOrder(String clusterFile, byte[] subspace, long orderID, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .open();

            FDBStoredRecord<Message> record = store.loadRecord(Tuple.from(orderID));
            if (record == null) {
                throw new RuntimeException("Record not found: " + orderID);
            }

            return Order.newBuilder()
                .mergeFrom(record.getRecord())
                .setOrderId(orderID)
                .build();
        });
    }

    @ConformanceStep("deleteOrder")
    public boolean deleteOrder(String clusterFile, byte[] subspace, long orderID, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .open();
            return store.deleteRecord(Tuple.from(orderID));
        });
    }

    @ConformanceStep("recordExists")
    public boolean recordExists(String clusterFile, byte[] subspace, long orderID, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .open();
            return store.loadRecord(Tuple.from(orderID)) != null;
        });
    }

    private static RecordMetaData createMetaData() {
        RecordMetaDataBuilder metaDataBuilder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        metaDataBuilder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("order_id"));
        metaDataBuilder.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.field("customer_id"));
        return metaDataBuilder.build();
    }

    private static RecordMetaData createSplitMetaData() {
        RecordMetaDataBuilder metaDataBuilder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        metaDataBuilder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("order_id"));
        metaDataBuilder.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.field("customer_id"));
        metaDataBuilder.setSplitLongRecords(true);
        return metaDataBuilder.build();
    }

    @ConformanceStep("saveSplitOrder")
    public void saveSplitOrder(String clusterFile, byte[] subspace, Order order, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createSplitMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .createOrOpen();
            store.saveRecord(order);
            return null;
        });
    }

    @ConformanceStep("loadSplitOrder")
    public Order loadSplitOrder(String clusterFile, byte[] subspace, long orderID, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createSplitMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .open();

            FDBStoredRecord<Message> record = store.loadRecord(Tuple.from(orderID));
            if (record == null) {
                throw new RuntimeException("Record not found: " + orderID);
            }

            return Order.newBuilder()
                .mergeFrom(record.getRecord())
                .setOrderId(orderID)
                .build();
        });
    }

    // --- Index conformance steps ---

    private static RecordMetaData createIndexedMetaData() {
        RecordMetaDataBuilder metaDataBuilder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        metaDataBuilder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("order_id"));
        metaDataBuilder.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.field("customer_id"));
        metaDataBuilder.addIndex("Order", new Index("Order$price", Key.Expressions.field("price"), IndexTypes.VALUE));
        return metaDataBuilder.build();
    }

    private static FDBRecordStore openIndexedStore(FDBRecordContext context, byte[] subspace) {
        return FDBRecordStore.newBuilder()
            .setMetaDataProvider(createIndexedMetaData())
            .setContext(context)
            .setSubspace(new Subspace(subspace))
            .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
            .createOrOpen();
    }

    @ConformanceStep("saveOrderWithIndex")
    public void saveOrderWithIndex(String clusterFile, byte[] subspace, Order order, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openIndexedStore(context, subspace);
            store.saveRecord(order);
            return null;
        });
    }

    @ConformanceStep("loadOrderWithIndex")
    public Order loadOrderWithIndex(String clusterFile, byte[] subspace, long orderID, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openIndexedStore(context, subspace);

            FDBStoredRecord<Message> record = store.loadRecord(Tuple.from(orderID));
            if (record == null) {
                throw new RuntimeException("Record not found: " + orderID);
            }

            return Order.newBuilder()
                .mergeFrom(record.getRecord())
                .setOrderId(orderID)
                .build();
        });
    }

    @ConformanceStep("deleteOrderWithIndex")
    public boolean deleteOrderWithIndex(String clusterFile, byte[] subspace, long orderID, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openIndexedStore(context, subspace);
            return store.deleteRecord(Tuple.from(orderID));
        });
    }

    @ConformanceStep("scanIndex")
    public java.util.List<java.util.Map<String, Object>> scanIndex(String clusterFile, byte[] subspace, String indexName, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            RecordMetaData metadata = createIndexedMetaData();
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metadata)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();

            Index index = metadata.getIndex(indexName);
            java.util.List<IndexEntry> entries = store.scanIndex(
                index, IndexScanType.BY_VALUE, TupleRange.ALL, null, ScanProperties.FORWARD_SCAN)
                .asList()
                .join();

            java.util.List<java.util.Map<String, Object>> result = new java.util.ArrayList<>();
            for (IndexEntry entry : entries) {
                java.util.Map<String, Object> map = new java.util.HashMap<>();

                java.util.List<Object> keyValues = new java.util.ArrayList<>();
                for (Object item : entry.getKey()) {
                    keyValues.add(item);
                }
                map.put("key", keyValues);

                java.util.List<Object> pkValues = new java.util.ArrayList<>();
                for (Object item : entry.getPrimaryKey()) {
                    pkValues.add(item);
                }
                map.put("primaryKey", pkValues);

                result.add(map);
            }
            return result;
        });
    }

    // --- Composite index conformance steps (PK dedup) ---

    private static RecordMetaData createCompositeIndexedMetaData() {
        RecordMetaDataBuilder metaDataBuilder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        metaDataBuilder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("order_id"));
        metaDataBuilder.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.field("customer_id"));
        metaDataBuilder.addIndex("Order", new Index("Order$price_id",
            Key.Expressions.concatenateFields("price", "order_id"), IndexTypes.VALUE));
        return metaDataBuilder.build();
    }

    private static FDBRecordStore openCompositeIndexedStore(FDBRecordContext context, byte[] subspace) {
        return FDBRecordStore.newBuilder()
            .setMetaDataProvider(createCompositeIndexedMetaData())
            .setContext(context)
            .setSubspace(new Subspace(subspace))
            .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
            .createOrOpen();
    }

    @ConformanceStep("saveOrderWithCompositeIndex")
    public void saveOrderWithCompositeIndex(String clusterFile, byte[] subspace, Order order, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openCompositeIndexedStore(context, subspace);
            store.saveRecord(order);
            return null;
        });
    }

    @ConformanceStep("scanCompositeIndex")
    public java.util.List<java.util.Map<String, Object>> scanCompositeIndex(String clusterFile, byte[] subspace, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            RecordMetaData metadata = createCompositeIndexedMetaData();
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metadata)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();

            Index index = metadata.getIndex("Order$price_id");
            java.util.List<IndexEntry> entries = store.scanIndex(
                index, IndexScanType.BY_VALUE, TupleRange.ALL, null, ScanProperties.FORWARD_SCAN)
                .asList()
                .join();

            java.util.List<java.util.Map<String, Object>> result = new java.util.ArrayList<>();
            for (IndexEntry entry : entries) {
                java.util.Map<String, Object> map = new java.util.HashMap<>();

                java.util.List<Object> keyValues = new java.util.ArrayList<>();
                for (Object item : entry.getKey()) {
                    keyValues.add(item);
                }
                map.put("key", keyValues);

                java.util.List<Object> pkValues = new java.util.ArrayList<>();
                for (Object item : entry.getPrimaryKey()) {
                    pkValues.add(item);
                }
                map.put("primaryKey", pkValues);

                result.add(map);
            }
            return result;
        });
    }

    @ConformanceStep("deleteOrderWithCompositeIndex")
    public boolean deleteOrderWithCompositeIndex(String clusterFile, byte[] subspace, long orderID, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openCompositeIndexedStore(context, subspace);
            return store.deleteRecord(Tuple.from(orderID));
        });
    }

    // --- Scan conformance steps ---

    @ConformanceStep("scanOrders")
    public List<Map<String, Object>> scanOrders(String clusterFile, byte[] subspace, int limit, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .createOrOpen();

            ScanProperties scanProps;
            if (limit > 0) {
                scanProps = new ScanProperties(ExecuteProperties.newBuilder()
                    .setReturnedRowLimit(limit)
                    .build());
            } else {
                scanProps = ScanProperties.FORWARD_SCAN;
            }

            List<FDBStoredRecord<Message>> records = store.scanRecords(null, scanProps)
                .asList().join();

            List<Map<String, Object>> result = new ArrayList<>();
            for (FDBStoredRecord<Message> record : records) {
                Order order = Order.newBuilder().mergeFrom(record.getRecord()).build();
                Map<String, Object> orderMap = new HashMap<>();
                orderMap.put("orderId", order.getOrderId());
                if (order.hasPrice()) {
                    orderMap.put("price", order.getPrice());
                }
                if (order.hasFlower()) {
                    Map<String, Object> flowerMap = new HashMap<>();
                    flowerMap.put("type", order.getFlower().getType());
                    flowerMap.put("color", order.getFlower().getColor().name());
                    orderMap.put("flower", flowerMap);
                }
                result.add(orderMap);
            }
            return result;
        });
    }

    // --- Record count conformance steps ---

    private static RecordMetaData createCountingMetaData() {
        RecordMetaDataBuilder builder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        builder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("order_id"));
        builder.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.field("customer_id"));
        builder.setRecordCountKey(Key.Expressions.empty());
        return builder.build();
    }

    @ConformanceStep("saveOrderCounting")
    public void saveOrderCounting(String clusterFile, byte[] subspace, Order order, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createCountingMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .createOrOpen();
            store.saveRecord(order);
            return null;
        });
    }

    @ConformanceStep("deleteOrderCounting")
    public boolean deleteOrderCounting(String clusterFile, byte[] subspace, long orderID, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createCountingMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .createOrOpen();
            return store.deleteRecord(Tuple.from(orderID));
        });
    }

    @ConformanceStep("getRecordCount")
    public long getRecordCount(String clusterFile, byte[] subspace, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createCountingMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .createOrOpen();
            return store.getSnapshotRecordCount().join();
        });
    }

    // --- Record version conformance steps ---

    private static RecordMetaData createVersionedMetaData() {
        RecordMetaDataBuilder builder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        builder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("order_id"));
        builder.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.field("customer_id"));
        builder.setStoreRecordVersions(true);
        return builder.build();
    }

    @ConformanceStep("saveOrderVersioned")
    public void saveOrderVersioned(String clusterFile, byte[] subspace, Order order, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createVersionedMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .createOrOpen();
            store.saveRecord(order);
            return null;
        });
    }

    @ConformanceStep("loadOrderWithVersion")
    public Map<String, Object> loadOrderWithVersion(String clusterFile, byte[] subspace, long orderID, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createVersionedMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .open();

            FDBStoredRecord<Message> record = store.loadRecord(Tuple.from(orderID));
            if (record == null) {
                throw new RuntimeException("Record not found: " + orderID);
            }

            Order order = Order.newBuilder().mergeFrom(record.getRecord()).build();

            Map<String, Object> result = new HashMap<>();
            result.put("orderId", order.getOrderId());
            if (order.hasPrice()) {
                result.put("price", order.getPrice());
            }

            if (record.hasVersion()) {
                FDBRecordVersion version = record.getVersion();
                result.put("hasVersion", true);
                result.put("globalVersion", Base64.getEncoder().encodeToString(version.getGlobalVersion()));
                result.put("localVersion", version.getLocalVersion());
                result.put("isComplete", version.isComplete());
            } else {
                result.put("hasVersion", false);
            }

            return result;
        });
    }

    // --- Continuation token conformance steps ---

    @ConformanceStep("scanOrdersWithContinuation")
    public Map<String, Object> scanOrdersWithContinuation(String clusterFile, byte[] subspace, int limit, String continuation, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .createOrOpen();

            byte[] contBytes = null;
            if (continuation != null && !continuation.isEmpty()) {
                contBytes = Base64.getDecoder().decode(continuation);
            }

            ScanProperties scanProps = new ScanProperties(ExecuteProperties.newBuilder()
                .setReturnedRowLimit(limit)
                .build());

            com.apple.foundationdb.record.RecordCursor<FDBStoredRecord<Message>> cursor =
                store.scanRecords(contBytes, scanProps);

            List<Map<String, Object>> orders = new ArrayList<>();
            byte[] nextContinuation = null;

            com.apple.foundationdb.record.RecordCursorResult<FDBStoredRecord<Message>> result;
            while ((result = cursor.getNext()) != null && result.hasNext()) {
                FDBStoredRecord<Message> record = result.get();
                Order order = Order.newBuilder().mergeFrom(record.getRecord()).build();
                Map<String, Object> orderMap = new HashMap<>();
                orderMap.put("orderId", order.getOrderId());
                if (order.hasPrice()) {
                    orderMap.put("price", order.getPrice());
                }
                orders.add(orderMap);
            }
            // After loop, result is the "no next" result — its continuation is the resume point
            if (result != null) {
                nextContinuation = result.getContinuation().toBytes();
            }

            Map<String, Object> response = new HashMap<>();
            response.put("orders", orders);
            if (nextContinuation != null) {
                response.put("continuation", Base64.getEncoder().encodeToString(nextContinuation));
            }
            // Track whether source was exhausted
            if (result != null) {
                response.put("sourceExhausted", result.getNoNextReason().isSourceExhausted());
            }
            return response;
        });
    }

    // --- Reverse scan conformance steps ---

    @ConformanceStep("scanOrdersReverse")
    public List<Map<String, Object>> scanOrdersReverse(String clusterFile, byte[] subspace, int limit, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .createOrOpen();

            ScanProperties scanProps;
            if (limit > 0) {
                scanProps = new ScanProperties(ExecuteProperties.newBuilder()
                    .setReturnedRowLimit(limit)
                    .build(), true); // reverse=true
            } else {
                scanProps = ScanProperties.REVERSE_SCAN;
            }

            List<FDBStoredRecord<Message>> records = store.scanRecords(null, scanProps)
                .asList().join();

            List<Map<String, Object>> result = new ArrayList<>();
            for (FDBStoredRecord<Message> record : records) {
                Order order = Order.newBuilder().mergeFrom(record.getRecord()).build();
                Map<String, Object> orderMap = new HashMap<>();
                orderMap.put("orderId", order.getOrderId());
                if (order.hasPrice()) {
                    orderMap.put("price", order.getPrice());
                }
                result.add(orderMap);
            }
            return result;
        });
    }

    @ConformanceStep("scanOrdersReverseWithContinuation")
    public Map<String, Object> scanOrdersReverseWithContinuation(String clusterFile, byte[] subspace, int limit, String continuation, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .createOrOpen();

            byte[] contBytes = null;
            if (continuation != null && !continuation.isEmpty()) {
                contBytes = Base64.getDecoder().decode(continuation);
            }

            ScanProperties scanProps = new ScanProperties(ExecuteProperties.newBuilder()
                .setReturnedRowLimit(limit)
                .build(), true); // reverse=true

            com.apple.foundationdb.record.RecordCursor<FDBStoredRecord<Message>> cursor =
                store.scanRecords(contBytes, scanProps);

            List<Map<String, Object>> orders = new ArrayList<>();
            byte[] nextContinuation = null;

            com.apple.foundationdb.record.RecordCursorResult<FDBStoredRecord<Message>> result;
            while ((result = cursor.getNext()) != null && result.hasNext()) {
                FDBStoredRecord<Message> record = result.get();
                Order order = Order.newBuilder().mergeFrom(record.getRecord()).build();
                Map<String, Object> orderMap = new HashMap<>();
                orderMap.put("orderId", order.getOrderId());
                if (order.hasPrice()) {
                    orderMap.put("price", order.getPrice());
                }
                orders.add(orderMap);
            }
            if (result != null) {
                nextContinuation = result.getContinuation().toBytes();
            }

            Map<String, Object> response = new HashMap<>();
            response.put("orders", orders);
            if (nextContinuation != null) {
                response.put("continuation", Base64.getEncoder().encodeToString(nextContinuation));
            }
            if (result != null) {
                response.put("sourceExhausted", result.getNoNextReason().isSourceExhausted());
            }
            return response;
        });
    }

    // --- Fan-out index conformance steps ---

    private static RecordMetaData createFanOutIndexedMetaData() {
        RecordMetaDataBuilder metaDataBuilder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        metaDataBuilder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("order_id"));
        metaDataBuilder.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.field("customer_id"));
        metaDataBuilder.addIndex("Order", new Index("Order$tags",
            Key.Expressions.field("tags", KeyExpression.FanType.FanOut), IndexTypes.VALUE));
        return metaDataBuilder.build();
    }

    private static FDBRecordStore openFanOutIndexedStore(FDBRecordContext context, byte[] subspace) {
        return FDBRecordStore.newBuilder()
            .setMetaDataProvider(createFanOutIndexedMetaData())
            .setContext(context)
            .setSubspace(new Subspace(subspace))
            .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
            .createOrOpen();
    }

    @ConformanceStep("saveOrderWithFanOutIndex")
    public void saveOrderWithFanOutIndex(String clusterFile, byte[] subspace, Order order, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openFanOutIndexedStore(context, subspace);
            store.saveRecord(order);
            return null;
        });
    }

    @ConformanceStep("scanFanOutIndex")
    public java.util.List<java.util.Map<String, Object>> scanFanOutIndex(String clusterFile, byte[] subspace, String indexName, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            RecordMetaData metadata = createFanOutIndexedMetaData();
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metadata)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();

            Index index = metadata.getIndex(indexName);
            java.util.List<IndexEntry> entries = store.scanIndex(
                index, IndexScanType.BY_VALUE, TupleRange.ALL, null, ScanProperties.FORWARD_SCAN)
                .asList()
                .join();

            java.util.List<java.util.Map<String, Object>> result = new java.util.ArrayList<>();
            for (IndexEntry entry : entries) {
                java.util.Map<String, Object> map = new java.util.HashMap<>();

                java.util.List<Object> keyValues = new java.util.ArrayList<>();
                for (Object item : entry.getKey()) {
                    keyValues.add(item);
                }
                map.put("key", keyValues);

                java.util.List<Object> pkValues = new java.util.ArrayList<>();
                for (Object item : entry.getPrimaryKey()) {
                    pkValues.add(item);
                }
                map.put("primaryKey", pkValues);

                result.add(map);
            }
            return result;
        });
    }

    @ConformanceStep("deleteOrderWithFanOutIndex")
    public boolean deleteOrderWithFanOutIndex(String clusterFile, byte[] subspace, long orderID, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openFanOutIndexedStore(context, subspace);
            return store.deleteRecord(Tuple.from(orderID));
        });
    }

    // --- COUNT index conformance steps ---

    private static RecordMetaData createCountIndexedMetaData() {
        RecordMetaDataBuilder metaDataBuilder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        metaDataBuilder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("order_id"));
        metaDataBuilder.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.field("customer_id"));
        metaDataBuilder.addIndex("Order", new Index("count_by_price",
            new GroupingKeyExpression(Key.Expressions.field("price"), 0),
            IndexTypes.COUNT));
        return metaDataBuilder.build();
    }

    private static FDBRecordStore openCountIndexedStore(FDBRecordContext context, byte[] subspace) {
        return FDBRecordStore.newBuilder()
            .setMetaDataProvider(createCountIndexedMetaData())
            .setContext(context)
            .setSubspace(new Subspace(subspace))
            .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
            .createOrOpen();
    }

    @ConformanceStep("saveOrderWithCountIndex")
    public void saveOrderWithCountIndex(String clusterFile, byte[] subspace, Order order, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openCountIndexedStore(context, subspace);
            store.saveRecord(order);
            return null;
        });
    }

    @ConformanceStep("deleteOrderWithCountIndex")
    public boolean deleteOrderWithCountIndex(String clusterFile, byte[] subspace, long orderID, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openCountIndexedStore(context, subspace);
            return store.deleteRecord(Tuple.from(orderID));
        });
    }

    @ConformanceStep("scanCountIndex")
    public java.util.List<java.util.Map<String, Object>> scanCountIndex(String clusterFile, byte[] subspace, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            RecordMetaData metadata = createCountIndexedMetaData();
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metadata)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();

            Index index = metadata.getIndex("count_by_price");
            java.util.List<IndexEntry> entries = store.scanIndex(
                index, IndexScanType.BY_GROUP, TupleRange.ALL, null, ScanProperties.FORWARD_SCAN)
                .asList()
                .join();

            java.util.List<java.util.Map<String, Object>> result = new java.util.ArrayList<>();
            for (IndexEntry entry : entries) {
                java.util.Map<String, Object> map = new java.util.HashMap<>();

                java.util.List<Object> keyValues = new java.util.ArrayList<>();
                for (Object item : entry.getKey()) {
                    keyValues.add(item);
                }
                map.put("key", keyValues);
                map.put("count", entry.getValue().getLong(0));

                result.add(map);
            }
            return result;
        });
    }

    // --- Customer conformance steps ---

    @ConformanceStep("saveCustomer")
    public void saveCustomer(String clusterFile, byte[] subspace, Customer customer, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .createOrOpen();
            store.saveRecord(customer);
            return null;
        });
    }

    @ConformanceStep("loadCustomer")
    public Customer loadCustomer(String clusterFile, byte[] subspace, long customerID, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .open();
            FDBStoredRecord<Message> record = store.loadRecord(Tuple.from(customerID));
            if (record == null) {
                throw new RuntimeException("Customer not found: " + customerID);
            }
            return Customer.newBuilder()
                .mergeFrom(record.getRecord())
                .setCustomerId(customerID)
                .build();
        });
    }

    @ConformanceStep("deleteCustomer")
    public boolean deleteCustomer(String clusterFile, byte[] subspace, long customerID, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .open();
            return store.deleteRecord(Tuple.from(customerID));
        });
    }

    @ConformanceStep("customerExists")
    public boolean customerExists(String clusterFile, byte[] subspace, long customerID, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .open();
            return store.loadRecord(Tuple.from(customerID)) != null;
        });
    }

    /**
     * Rebuild a VALUE index on existing data within a single transaction.
     * Opens the store with indexed metadata and calls rebuildIndex().
     * Matches Go's FDBRecordStore.RebuildIndex().
     */
    @ConformanceStep("rebuildIndex")
    public void rebuildIndex(String clusterFile, byte[] subspace, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            RecordMetaData metadata = createIndexedMetaData();
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metadata)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();

            Index index = metadata.getIndex("Order$price");
            store.rebuildIndex(index).join();
            return null;
        });
    }

    // --- DeleteAllRecords conformance steps ---

    @ConformanceStep("deleteAllRecordsWithIndex")
    public void deleteAllRecordsWithIndex(String clusterFile, byte[] subspace, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openIndexedStore(context, subspace);
            store.deleteAllRecords();
            return null;
        });
    }

    @ConformanceStep("countRecordsWithIndex")
    public long countRecordsWithIndex(String clusterFile, byte[] subspace, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openIndexedStore(context, subspace);
            return store.scanRecords(null, ScanProperties.FORWARD_SCAN)
                .getCount().join();
        });
    }

    // --- RangeSet wire format conformance steps ---

    @ConformanceStep("rangeSetInsert")
    public boolean rangeSetInsert(String clusterFile, byte[] rsSubspace, byte[] begin, byte[] end, String tenantName) {
        FDBDatabase db = createDatabase(clusterFile);
        Database nativeDb = db.database();
        com.apple.foundationdb.async.RangeSet rs = new com.apple.foundationdb.async.RangeSet(new Subspace(rsSubspace));
        if (tenantName != null && !tenantName.isEmpty()) {
            Tenant tenant = nativeDb.openTenant(tenantName.getBytes(StandardCharsets.UTF_8));
            return rs.insertRange(tenant, begin, end).join();
        } else {
            return rs.insertRange(nativeDb, begin, end).join();
        }
    }

    @ConformanceStep("rangeSetContains")
    public boolean rangeSetContains(String clusterFile, byte[] rsSubspace, byte[] key, String tenantName) {
        FDBDatabase db = createDatabase(clusterFile);
        Database nativeDb = db.database();
        com.apple.foundationdb.async.RangeSet rs = new com.apple.foundationdb.async.RangeSet(new Subspace(rsSubspace));
        if (tenantName != null && !tenantName.isEmpty()) {
            Tenant tenant = nativeDb.openTenant(tenantName.getBytes(StandardCharsets.UTF_8));
            return rs.contains(tenant, key).join();
        } else {
            return rs.contains(nativeDb, key).join();
        }
    }

    @ConformanceStep("rangeSetMissingRanges")
    public java.util.List<java.util.Map<String, Object>> rangeSetMissingRanges(String clusterFile, byte[] rsSubspace, String tenantName) {
        FDBDatabase db = createDatabase(clusterFile);
        Database nativeDb = db.database();
        com.apple.foundationdb.async.RangeSet rs = new com.apple.foundationdb.async.RangeSet(new Subspace(rsSubspace));
        java.util.List<com.apple.foundationdb.Range> missing;
        if (tenantName != null && !tenantName.isEmpty()) {
            Tenant tenant = nativeDb.openTenant(tenantName.getBytes(StandardCharsets.UTF_8));
            missing = rs.missingRanges(tenant).join();
        } else {
            missing = rs.missingRanges(nativeDb).join();
        }

        java.util.List<java.util.Map<String, Object>> result = new java.util.ArrayList<>();
        for (com.apple.foundationdb.Range range : missing) {
            java.util.Map<String, Object> map = new java.util.HashMap<>();
            java.util.List<Integer> beginInts = new java.util.ArrayList<>();
            for (byte b : range.begin) {
                beginInts.add(b & 0xFF);
            }
            java.util.List<Integer> endInts = new java.util.ArrayList<>();
            for (byte b : range.end) {
                endInts.add(b & 0xFF);
            }
            map.put("begin", beginInts);
            map.put("end", endInts);
            result.add(map);
        }
        return result;
    }

    // --- Store header conformance steps ---

    /**
     * Read the store header raw bytes from FDB and return parsed fields.
     * Uses raw FDB read (no store open) to avoid any format version upgrade side effects.
     */
    @ConformanceStep("getStoreHeaderRaw")
    public java.util.Map<String, Object> getStoreHeaderRaw(String clusterFile, byte[] subspace, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            Subspace sub = new Subspace(subspace);
            byte[] headerKey = sub.pack(Tuple.from(0L));
            byte[] headerBytes = context.ensureActive().get(headerKey).join();
            if (headerBytes == null) {
                throw new RuntimeException("Store header not found at subspace key 0");
            }
            com.apple.foundationdb.record.RecordMetaDataProto.DataStoreInfo storeInfo;
            try {
                storeInfo = com.apple.foundationdb.record.RecordMetaDataProto.DataStoreInfo.parseFrom(headerBytes);
            } catch (com.google.protobuf.InvalidProtocolBufferException e) {
                throw new RuntimeException("Failed to parse store header proto: " + e.getMessage(), e);
            }
            java.util.Map<String, Object> result = new java.util.HashMap<>();
            result.put("formatVersion", storeInfo.getFormatVersion());
            result.put("metaDataVersion", storeInfo.getMetaDataversion());
            result.put("userVersion", storeInfo.getUserVersion());
            return result;
        });
    }

    /**
     * Create a store with a specific user version via a custom UserVersionChecker.
     * Java sets user version during checkVersion() via UserVersionChecker callback.
     */
    @ConformanceStep("createStoreWithUserVersion")
    public void createStoreWithUserVersion(String clusterFile, byte[] subspace, int userVersion, String tenantName) {
        final int targetVersion = userVersion;
        FDBRecordStoreBase.UserVersionChecker checker = new FDBRecordStoreBase.UserVersionChecker() {
            @Override
            public java.util.concurrent.CompletableFuture<Integer> checkUserVersion(
                    int oldUserVersion, int oldMetaDataVersion, RecordMetaDataProvider metaData) {
                return java.util.concurrent.CompletableFuture.completedFuture(targetVersion);
            }
            @Override
            public IndexState needRebuildIndex(Index index, long recordCount, boolean indexOnNewRecordTypes) {
                return IndexState.READABLE;
            }
        };
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore.newBuilder()
                .setMetaDataProvider(createMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(checker)
                .createOrOpen();
            return null;
        });
    }

    // --- Index state persistence conformance steps ---

    /**
     * Mark an index as WRITE_ONLY via Java's store.markIndexWriteOnly().
     * Uses the indexed metadata (Order$price VALUE index).
     */
    @ConformanceStep("markIndexWriteOnly")
    public void markIndexWriteOnly(String clusterFile, byte[] subspace, String indexName, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createIndexedMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();
            store.markIndexWriteOnly(indexName).join();
            return null;
        });
    }

    /**
     * Mark an index as DISABLED via Java's store.markIndexDisabled().
     */
    @ConformanceStep("markIndexDisabled")
    public void markIndexDisabled(String clusterFile, byte[] subspace, String indexName, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createIndexedMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();
            store.markIndexDisabled(indexName).join();
            return null;
        });
    }

    /**
     * Mark an index as READABLE via Java's store.markIndexReadable().
     */
    @ConformanceStep("markIndexReadable")
    public void markIndexReadable(String clusterFile, byte[] subspace, String indexName, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createIndexedMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();
            store.markIndexReadable(indexName).join();
            return null;
        });
    }

    /**
     * Read the raw index state from FDB (no store open).
     * Returns the state as a string: "READABLE", "WRITE_ONLY", "DISABLED", "READABLE_UNIQUE_PENDING".
     * If no state key exists, returns "READABLE" (default).
     */
    @ConformanceStep("getIndexStateRaw")
    public String getIndexStateRaw(String clusterFile, byte[] subspace, String indexName, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            Subspace sub = new Subspace(subspace);
            // Index state key: subspace[5][indexName]
            Subspace isSubspace = sub.get(5L);
            byte[] stateKey = isSubspace.pack(Tuple.from(indexName));
            byte[] stateBytes = context.ensureActive().get(stateKey).join();
            if (stateBytes == null) {
                return "READABLE"; // Default
            }
            long code = Tuple.fromBytes(stateBytes).getLong(0);
            switch ((int)code) {
                case 0: return "READABLE";
                case 1: return "WRITE_ONLY";
                case 2: return "DISABLED";
                case 3: return "READABLE_UNIQUE_PENDING";
                default: return "UNKNOWN(" + code + ")";
            }
        });
    }

    // --- SUM index conformance steps ---

    private static RecordMetaData createSumIndexedMetaData() {
        RecordMetaDataBuilder metaDataBuilder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        metaDataBuilder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("order_id"));
        metaDataBuilder.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.field("customer_id"));
        // Ungrouped SUM of price: field("price").ungrouped()
        // = new GroupingKeyExpression(field("price"), 1)
        // groupingCount = 1-1 = 0 → no grouping key, price is the summed value
        metaDataBuilder.addIndex("Order", new Index("sum_price",
            new GroupingKeyExpression(Key.Expressions.field("price"), 1),
            IndexTypes.SUM));
        return metaDataBuilder.build();
    }

    private static FDBRecordStore openSumIndexedStore(FDBRecordContext context, byte[] subspace) {
        return FDBRecordStore.newBuilder()
            .setMetaDataProvider(createSumIndexedMetaData())
            .setContext(context)
            .setSubspace(new Subspace(subspace))
            .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
            .createOrOpen();
    }

    @ConformanceStep("saveOrderWithSumIndex")
    public void saveOrderWithSumIndex(String clusterFile, byte[] subspace, Order order, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openSumIndexedStore(context, subspace);
            store.saveRecord(order);
            return null;
        });
    }

    @ConformanceStep("deleteOrderWithSumIndex")
    public boolean deleteOrderWithSumIndex(String clusterFile, byte[] subspace, long orderID, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openSumIndexedStore(context, subspace);
            return store.deleteRecord(Tuple.from(orderID));
        });
    }

    @ConformanceStep("scanSumIndex")
    public java.util.List<java.util.Map<String, Object>> scanSumIndex(String clusterFile, byte[] subspace, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            RecordMetaData metadata = createSumIndexedMetaData();
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metadata)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();

            Index index = metadata.getIndex("sum_price");
            java.util.List<IndexEntry> entries = store.scanIndex(
                index, IndexScanType.BY_GROUP, TupleRange.ALL, null, ScanProperties.FORWARD_SCAN)
                .asList()
                .join();

            java.util.List<java.util.Map<String, Object>> result = new java.util.ArrayList<>();
            for (IndexEntry entry : entries) {
                java.util.Map<String, Object> map = new java.util.HashMap<>();

                java.util.List<Object> keyValues = new java.util.ArrayList<>();
                for (Object item : entry.getKey()) {
                    keyValues.add(item);
                }
                map.put("key", keyValues);
                map.put("sum", entry.getValue().getLong(0));

                result.add(map);
            }
            return result;
        });
    }

    // ========== MAX_EVER_LONG index ==========

    private static RecordMetaData createMaxEverLongIndexedMetaData() {
        RecordMetaDataBuilder metaDataBuilder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        metaDataBuilder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("order_id"));
        metaDataBuilder.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.field("customer_id"));
        // Ungrouped MAX_EVER_LONG of price: field("price").ungrouped()
        // = new GroupingKeyExpression(field("price"), 1)
        metaDataBuilder.addIndex("Order", new Index("max_ever_price",
            new GroupingKeyExpression(Key.Expressions.field("price"), 1),
            IndexTypes.MAX_EVER_LONG));
        return metaDataBuilder.build();
    }

    private static FDBRecordStore openMaxEverLongIndexedStore(FDBRecordContext context, byte[] subspace) {
        return FDBRecordStore.newBuilder()
            .setMetaDataProvider(createMaxEverLongIndexedMetaData())
            .setContext(context)
            .setSubspace(new Subspace(subspace))
            .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
            .createOrOpen();
    }

    @ConformanceStep("saveOrderWithMaxEverLongIndex")
    public void saveOrderWithMaxEverLongIndex(String clusterFile, byte[] subspace, Order order, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openMaxEverLongIndexedStore(context, subspace);
            store.saveRecord(order);
            return null;
        });
    }

    @ConformanceStep("deleteOrderWithMaxEverLongIndex")
    public boolean deleteOrderWithMaxEverLongIndex(String clusterFile, byte[] subspace, long orderID, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openMaxEverLongIndexedStore(context, subspace);
            return store.deleteRecord(Tuple.from(orderID));
        });
    }

    @ConformanceStep("scanMaxEverLongIndex")
    public java.util.List<java.util.Map<String, Object>> scanMaxEverLongIndex(String clusterFile, byte[] subspace, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            RecordMetaData metadata = createMaxEverLongIndexedMetaData();
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metadata)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();

            Index index = metadata.getIndex("max_ever_price");
            java.util.List<IndexEntry> entries = store.scanIndex(
                index, IndexScanType.BY_GROUP, TupleRange.ALL, null, ScanProperties.FORWARD_SCAN)
                .asList()
                .join();

            java.util.List<java.util.Map<String, Object>> result = new java.util.ArrayList<>();
            for (IndexEntry entry : entries) {
                java.util.Map<String, Object> map = new java.util.HashMap<>();

                java.util.List<Object> keyValues = new java.util.ArrayList<>();
                for (Object item : entry.getKey()) {
                    keyValues.add(item);
                }
                map.put("key", keyValues);
                map.put("value", entry.getValue().getLong(0));

                result.add(map);
            }
            return result;
        });
    }

    // ========== MIN_EVER_LONG index ==========

    private static RecordMetaData createMinEverLongIndexedMetaData() {
        RecordMetaDataBuilder metaDataBuilder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        metaDataBuilder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("order_id"));
        metaDataBuilder.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.field("customer_id"));
        // Ungrouped MIN_EVER_LONG of price: field("price").ungrouped()
        // = new GroupingKeyExpression(field("price"), 1)
        metaDataBuilder.addIndex("Order", new Index("min_ever_price",
            new GroupingKeyExpression(Key.Expressions.field("price"), 1),
            IndexTypes.MIN_EVER_LONG));
        return metaDataBuilder.build();
    }

    private static FDBRecordStore openMinEverLongIndexedStore(FDBRecordContext context, byte[] subspace) {
        return FDBRecordStore.newBuilder()
            .setMetaDataProvider(createMinEverLongIndexedMetaData())
            .setContext(context)
            .setSubspace(new Subspace(subspace))
            .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
            .createOrOpen();
    }

    @ConformanceStep("saveOrderWithMinEverLongIndex")
    public void saveOrderWithMinEverLongIndex(String clusterFile, byte[] subspace, Order order, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openMinEverLongIndexedStore(context, subspace);
            store.saveRecord(order);
            return null;
        });
    }

    @ConformanceStep("deleteOrderWithMinEverLongIndex")
    public boolean deleteOrderWithMinEverLongIndex(String clusterFile, byte[] subspace, long orderID, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openMinEverLongIndexedStore(context, subspace);
            return store.deleteRecord(Tuple.from(orderID));
        });
    }

    @ConformanceStep("scanMinEverLongIndex")
    public java.util.List<java.util.Map<String, Object>> scanMinEverLongIndex(String clusterFile, byte[] subspace, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            RecordMetaData metadata = createMinEverLongIndexedMetaData();
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metadata)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();

            Index index = metadata.getIndex("min_ever_price");
            java.util.List<IndexEntry> entries = store.scanIndex(
                index, IndexScanType.BY_GROUP, TupleRange.ALL, null, ScanProperties.FORWARD_SCAN)
                .asList()
                .join();

            java.util.List<java.util.Map<String, Object>> result = new java.util.ArrayList<>();
            for (IndexEntry entry : entries) {
                java.util.Map<String, Object> map = new java.util.HashMap<>();

                java.util.List<Object> keyValues = new java.util.ArrayList<>();
                for (Object item : entry.getKey()) {
                    keyValues.add(item);
                }
                map.put("key", keyValues);
                map.put("value", entry.getValue().getLong(0));

                result.add(map);
            }
            return result;
        });
    }

    // --- COUNT CLEAR_WHEN_ZERO conformance steps ---

    private static RecordMetaData createCountClearWhenZeroMetaData() {
        RecordMetaDataBuilder metaDataBuilder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        metaDataBuilder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("order_id"));
        metaDataBuilder.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.field("customer_id"));
        Map<String, String> options = new HashMap<>();
        options.put("clearWhenZero", "true");
        metaDataBuilder.addIndex("Order", new Index("count_cwz_price",
            new GroupingKeyExpression(Key.Expressions.field("price"), 0),
            IndexTypes.COUNT, options));
        return metaDataBuilder.build();
    }

    private static FDBRecordStore openCountClearWhenZeroStore(FDBRecordContext context, byte[] subspace) {
        return FDBRecordStore.newBuilder()
            .setMetaDataProvider(createCountClearWhenZeroMetaData())
            .setContext(context)
            .setSubspace(new Subspace(subspace))
            .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
            .createOrOpen();
    }

    @ConformanceStep("saveOrderWithCountCWZ")
    public void saveOrderWithCountCWZ(String clusterFile, byte[] subspace, Order order, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openCountClearWhenZeroStore(context, subspace);
            store.saveRecord(order);
            return null;
        });
    }

    @ConformanceStep("deleteOrderWithCountCWZ")
    public boolean deleteOrderWithCountCWZ(String clusterFile, byte[] subspace, long orderID, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openCountClearWhenZeroStore(context, subspace);
            return store.deleteRecord(Tuple.from(orderID));
        });
    }

    @ConformanceStep("scanCountCWZIndex")
    public java.util.List<java.util.Map<String, Object>> scanCountCWZIndex(String clusterFile, byte[] subspace, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            RecordMetaData metadata = createCountClearWhenZeroMetaData();
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metadata)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();

            Index index = metadata.getIndex("count_cwz_price");
            java.util.List<IndexEntry> entries = store.scanIndex(
                index, IndexScanType.BY_GROUP, TupleRange.ALL, null, ScanProperties.FORWARD_SCAN)
                .asList()
                .join();

            java.util.List<java.util.Map<String, Object>> result = new java.util.ArrayList<>();
            for (IndexEntry entry : entries) {
                java.util.Map<String, Object> map = new java.util.HashMap<>();

                java.util.List<Object> keyValues = new java.util.ArrayList<>();
                for (Object item : entry.getKey()) {
                    keyValues.add(item);
                }
                map.put("key", keyValues);
                map.put("count", entry.getValue().getLong(0));

                result.add(map);
            }
            return result;
        });
    }

    // --- COUNT_NOT_NULL index conformance steps ---

    private static RecordMetaData createCountNotNullIndexedMetaData() {
        RecordMetaDataBuilder metaDataBuilder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        metaDataBuilder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("order_id"));
        metaDataBuilder.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.field("customer_id"));
        metaDataBuilder.addIndex("Order", new Index("count_not_null_price",
            new GroupingKeyExpression(Key.Expressions.field("price"), 1),
            IndexTypes.COUNT_NOT_NULL));
        return metaDataBuilder.build();
    }

    private static FDBRecordStore openCountNotNullIndexedStore(FDBRecordContext context, byte[] subspace) {
        return FDBRecordStore.newBuilder()
            .setMetaDataProvider(createCountNotNullIndexedMetaData())
            .setContext(context)
            .setSubspace(new Subspace(subspace))
            .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
            .createOrOpen();
    }

    @ConformanceStep("saveOrderWithCountNotNullIndex")
    public void saveOrderWithCountNotNullIndex(String clusterFile, byte[] subspace, Order order, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openCountNotNullIndexedStore(context, subspace);
            store.saveRecord(order);
            return null;
        });
    }

    @ConformanceStep("deleteOrderWithCountNotNullIndex")
    public boolean deleteOrderWithCountNotNullIndex(String clusterFile, byte[] subspace, long orderID, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openCountNotNullIndexedStore(context, subspace);
            return store.deleteRecord(Tuple.from(orderID));
        });
    }

    @ConformanceStep("scanCountNotNullIndex")
    public java.util.List<java.util.Map<String, Object>> scanCountNotNullIndex(String clusterFile, byte[] subspace, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            RecordMetaData metadata = createCountNotNullIndexedMetaData();
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metadata)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();

            Index index = metadata.getIndex("count_not_null_price");
            java.util.List<IndexEntry> entries = store.scanIndex(
                index, IndexScanType.BY_GROUP, TupleRange.ALL, null, ScanProperties.FORWARD_SCAN)
                .asList()
                .join();

            java.util.List<java.util.Map<String, Object>> result = new java.util.ArrayList<>();
            for (IndexEntry entry : entries) {
                java.util.Map<String, Object> map = new java.util.HashMap<>();

                java.util.List<Object> keyValues = new java.util.ArrayList<>();
                for (Object item : entry.getKey()) {
                    keyValues.add(item);
                }
                map.put("key", keyValues);
                map.put("count", entry.getValue().getLong(0));

                result.add(map);
            }
            return result;
        });
    }

    // --- COUNT_UPDATES index conformance steps ---

    private static RecordMetaData createCountUpdatesIndexedMetaData() {
        RecordMetaDataBuilder metaDataBuilder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        metaDataBuilder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("order_id"));
        metaDataBuilder.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.field("customer_id"));
        metaDataBuilder.addIndex("Order", new Index("count_updates_price",
            new GroupingKeyExpression(Key.Expressions.field("price"), 0),
            IndexTypes.COUNT_UPDATES));
        return metaDataBuilder.build();
    }

    private static FDBRecordStore openCountUpdatesIndexedStore(FDBRecordContext context, byte[] subspace) {
        return FDBRecordStore.newBuilder()
            .setMetaDataProvider(createCountUpdatesIndexedMetaData())
            .setContext(context)
            .setSubspace(new Subspace(subspace))
            .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
            .createOrOpen();
    }

    @ConformanceStep("saveOrderWithCountUpdatesIndex")
    public void saveOrderWithCountUpdatesIndex(String clusterFile, byte[] subspace, Order order, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openCountUpdatesIndexedStore(context, subspace);
            store.saveRecord(order);
            return null;
        });
    }

    @ConformanceStep("deleteOrderWithCountUpdatesIndex")
    public boolean deleteOrderWithCountUpdatesIndex(String clusterFile, byte[] subspace, long orderID, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openCountUpdatesIndexedStore(context, subspace);
            return store.deleteRecord(Tuple.from(orderID));
        });
    }

    @ConformanceStep("scanCountUpdatesIndex")
    public java.util.List<java.util.Map<String, Object>> scanCountUpdatesIndex(String clusterFile, byte[] subspace, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            RecordMetaData metadata = createCountUpdatesIndexedMetaData();
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metadata)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();

            Index index = metadata.getIndex("count_updates_price");
            java.util.List<IndexEntry> entries = store.scanIndex(
                index, IndexScanType.BY_GROUP, TupleRange.ALL, null, ScanProperties.FORWARD_SCAN)
                .asList()
                .join();

            java.util.List<java.util.Map<String, Object>> result = new java.util.ArrayList<>();
            for (IndexEntry entry : entries) {
                java.util.Map<String, Object> map = new java.util.HashMap<>();

                java.util.List<Object> keyValues = new java.util.ArrayList<>();
                for (Object item : entry.getKey()) {
                    keyValues.add(item);
                }
                map.put("key", keyValues);
                map.put("count", entry.getValue().getLong(0));

                result.add(map);
            }
            return result;
        });
    }
}
