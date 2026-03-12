package com.birdayz.conformance;

import com.apple.foundationdb.record.IndexScanType;
import com.apple.foundationdb.record.RecordMetaData;
import com.apple.foundationdb.record.RecordMetaDataProto;
import com.apple.foundationdb.record.ScanProperties;
import com.apple.foundationdb.record.TupleRange;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStoreBase;
import com.apple.foundationdb.record.RecordLayerDemo.Order;
import com.apple.foundationdb.subspace.Subspace;

/**
 * Conformance steps that intentionally trigger Record Layer exceptions.
 * Used by Go error_conformance_test.go to verify that Go error types match Java exception classes.
 */
class ErrorSteps extends ConformanceBase {

    @ConformanceStep("insertDuplicateOrder")
    public void insertDuplicateOrder(String clusterFile, byte[] subspace, Order order, String tenantName) {
        // Save the order, then try to insert it again with ERROR_IF_EXISTS
        // Should throw RecordAlreadyExistsException
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();
            store.saveRecord(order);
            store.saveRecord(order, FDBRecordStoreBase.RecordExistenceCheck.ERROR_IF_EXISTS);
            return null;
        });
    }

    @ConformanceStep("updateNonExistentOrder")
    public void updateNonExistentOrder(String clusterFile, byte[] subspace, Order order, String tenantName) {
        // Try to update a record that doesn't exist with ERROR_IF_NOT_EXISTS
        // Should throw RecordDoesNotExistException
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();
            store.saveRecord(order, FDBRecordStoreBase.RecordExistenceCheck.ERROR_IF_NOT_EXISTS);
            return null;
        });
    }

    @ConformanceStep("openNonExistentStore")
    public void openNonExistentStore(String clusterFile, byte[] subspace, String tenantName) {
        // Try to open a store that doesn't exist (using open(), not createOrOpen())
        // Should throw RecordStoreDoesNotExistException
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .open();
            return null;
        });
    }

    @ConformanceStep("createExistingStore")
    public void createExistingStore(String clusterFile, byte[] subspace, String tenantName) {
        // Create a store, then try to create it again
        // Should throw RecordStoreAlreadyExistsException
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore.newBuilder()
                .setMetaDataProvider(createMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .create();
            return null;
        });
        // Second create in new transaction
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore.newBuilder()
                .setMetaDataProvider(createMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .create();
            return null;
        });
    }

    @ConformanceStep("scanNonReadableIndex")
    public void scanNonReadableIndex(String clusterFile, byte[] subspace, String tenantName) {
        // Create store with index, mark index write-only, then try to scan it
        // Should throw ScanNonReadableIndexException
        RecordMetaData metaData = createIndexedMetaData();

        // First: create store and mark index write-only
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metaData)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();
            store.markIndexWriteOnly("Order$price").join();
            return null;
        });

        // Second: try to scan the write-only index
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metaData)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();
            // ScanIndex should throw ScanNonReadableIndexException
            store.scanIndex(
                metaData.getIndex("Order$price"),
                IndexScanType.BY_VALUE,
                TupleRange.ALL,
                null,
                ScanProperties.FORWARD_SCAN
            ).asList().join();
            return null;
        });
    }

    @ConformanceStep("saveLocked")
    public void saveLocked(String clusterFile, byte[] subspace, Order order, String tenantName) {
        // Set store lock to FORBID_RECORD_UPDATE, then try to save
        // Should throw RecordCoreException with lock-related message
        RecordMetaData metaData = createMetaData();

        // First: create store and lock it
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metaData)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();
            store.setStoreLockStateAsync(
                RecordMetaDataProto.DataStoreInfo.StoreLockState.State.FORBID_RECORD_UPDATE,
                "conformance test lock"
            ).join();
            return null;
        });

        // Second: try to save in locked store
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metaData)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();
            store.saveRecord(order);
            return null;
        });
    }
}
