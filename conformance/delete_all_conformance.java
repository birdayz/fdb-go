package com.birdayz.conformance;

import com.apple.foundationdb.record.ScanProperties;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.subspace.Subspace;

class DeleteAllSteps extends ConformanceBase {
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
}
