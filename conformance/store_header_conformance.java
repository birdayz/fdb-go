package com.birdayz.conformance;

import com.apple.foundationdb.record.IndexState;
import com.apple.foundationdb.record.RecordMetaDataProvider;
import com.apple.foundationdb.record.metadata.Index;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStoreBase;
import com.apple.foundationdb.subspace.Subspace;
import com.apple.foundationdb.tuple.Tuple;

import java.util.HashMap;
import java.util.Map;
import java.util.concurrent.CompletableFuture;

class StoreHeaderSteps extends ConformanceBase {
    @ConformanceStep("getStoreHeaderRaw")
    public Map<String, Object> getStoreHeaderRaw(String clusterFile, byte[] subspace, String tenantName) {
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
            Map<String, Object> result = new HashMap<>();
            result.put("formatVersion", storeInfo.getFormatVersion());
            result.put("metaDataVersion", storeInfo.getMetaDataversion());
            result.put("userVersion", storeInfo.getUserVersion());
            return result;
        });
    }

    @ConformanceStep("createStoreWithUserVersion")
    public void createStoreWithUserVersion(String clusterFile, byte[] subspace, int userVersion, String tenantName) {
        final int targetVersion = userVersion;
        FDBRecordStoreBase.UserVersionChecker checker = new FDBRecordStoreBase.UserVersionChecker() {
            @Override
            public CompletableFuture<Integer> checkUserVersion(
                    int oldUserVersion, int oldMetaDataVersion, RecordMetaDataProvider metaData) {
                return CompletableFuture.completedFuture(targetVersion);
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
}
