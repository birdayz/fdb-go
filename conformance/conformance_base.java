package com.birdayz.conformance;

import com.apple.foundationdb.record.IndexState;
import com.apple.foundationdb.record.RecordMetaData;
import com.apple.foundationdb.record.RecordMetaDataBuilder;
import com.apple.foundationdb.record.RecordMetaDataProvider;
import com.apple.foundationdb.record.metadata.Index;
import com.apple.foundationdb.record.metadata.IndexTypes;
import com.apple.foundationdb.record.metadata.Key;
import com.apple.foundationdb.record.provider.foundationdb.FDBDatabase;
import com.apple.foundationdb.record.provider.foundationdb.FDBDatabaseFactory;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordContext;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordContextConfig;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStoreBase;
import com.apple.foundationdb.record.RecordLayerDemo;
import com.apple.foundationdb.Database;
import com.apple.foundationdb.Tenant;
import com.apple.foundationdb.Transaction;

import java.io.File;
import java.io.FileWriter;
import java.io.IOException;
import java.lang.reflect.Constructor;
import java.nio.charset.StandardCharsets;
import java.util.concurrent.CompletableFuture;

/**
 * Shared infrastructure for all conformance step classes.
 * Contains database caching, context creation, metadata builders, and common constants.
 */
class ConformanceBase {

    private static String cachedClusterContent = null;
    private static FDBDatabase cachedDatabase = null;

    /**
     * UserVersionChecker that always marks indexes as READABLE.
     * Needed for conformance tests where Go creates the store and index entries,
     * but Java doesn't know the index was already built.
     */
    static final FDBRecordStoreBase.UserVersionChecker ALWAYS_READABLE_CHECKER = new FDBRecordStoreBase.UserVersionChecker() {
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
    interface ContextAction<T> {
        T execute(FDBRecordContext context);
    }

    /**
     * Run an action within an FDBRecordContext, handling tenant vs non-tenant branching.
     */
    static <T> T runInContext(String clusterFile, String tenantName, ContextAction<T> action) {
        FDBDatabase db = createDatabase(clusterFile);
        if (tenantName != null && !tenantName.isEmpty()) {
            Database nativeDb = db.database();
            Tenant tenant = nativeDb.openTenant(tenantName.getBytes(StandardCharsets.UTF_8));
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
     */
    static synchronized FDBDatabase createDatabase(String clusterFileContent) {
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
     */
    static FDBRecordContext createContextFromTransaction(FDBDatabase db, Transaction transaction) {
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

    /** Basic metadata: Order(order_id), Customer(customer_id), TypedRecord(id). */
    static RecordMetaData createMetaData() {
        RecordMetaDataBuilder metaDataBuilder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        metaDataBuilder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("order_id"));
        metaDataBuilder.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.field("customer_id"));
        metaDataBuilder.getRecordType("TypedRecord")
            .setPrimaryKey(Key.Expressions.field("id"));
        return metaDataBuilder.build();
    }

    /** Indexed metadata: basic + Order$price VALUE index. */
    static RecordMetaData createIndexedMetaData() {
        RecordMetaDataBuilder metaDataBuilder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        metaDataBuilder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("order_id"));
        metaDataBuilder.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.field("customer_id"));
        metaDataBuilder.getRecordType("TypedRecord")
            .setPrimaryKey(Key.Expressions.field("id"));
        metaDataBuilder.addIndex("Order", new Index("Order$price", Key.Expressions.field("price"), IndexTypes.VALUE));
        return metaDataBuilder.build();
    }

    /** Open store with indexed metadata + ALWAYS_READABLE_CHECKER. */
    static FDBRecordStore openIndexedStore(FDBRecordContext context, byte[] subspace) {
        return FDBRecordStore.newBuilder()
            .setMetaDataProvider(createIndexedMetaData())
            .setContext(context)
            .setSubspace(new com.apple.foundationdb.subspace.Subspace(subspace))
            .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
            .createOrOpen();
    }
}
