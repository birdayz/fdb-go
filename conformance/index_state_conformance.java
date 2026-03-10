package com.birdayz.conformance;

import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.subspace.Subspace;
import com.apple.foundationdb.tuple.Tuple;

class IndexStateSteps extends ConformanceBase {
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

    @ConformanceStep("getIndexStateRaw")
    public String getIndexStateRaw(String clusterFile, byte[] subspace, String indexName, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            Subspace sub = new Subspace(subspace);
            Subspace isSubspace = sub.get(5L);
            byte[] stateKey = isSubspace.pack(Tuple.from(indexName));
            byte[] stateBytes = context.ensureActive().get(stateKey).join();
            if (stateBytes == null) {
                return "READABLE";
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
}
