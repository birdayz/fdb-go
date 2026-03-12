package com.birdayz.conformance;

import com.apple.foundationdb.record.IndexBuildProto;
import com.apple.foundationdb.record.RecordMetaData;
import com.apple.foundationdb.record.RecordMetaDataBuilder;
import com.apple.foundationdb.record.metadata.Index;
import com.apple.foundationdb.record.metadata.IndexTypes;
import com.apple.foundationdb.record.metadata.Key;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.record.RecordLayerDemo;
import com.apple.foundationdb.subspace.Subspace;

import java.util.HashMap;
import java.util.Map;

/**
 * Conformance steps for index build state (IndexBuildIndexingStamp) wire format validation.
 * Tests that Go and Java can read each other's BY_RECORDS stamps at subspace [9][indexSubspaceKey][2].
 */
class IndexBuildStateSteps extends ConformanceBase {

    /**
     * Load the indexing type stamp for the Order$price index.
     * Returns the method name (e.g. "BY_RECORDS") or null if no stamp exists.
     */
    @ConformanceStep("loadIndexingTypeStamp")
    public Map<String, Object> loadIndexingTypeStamp(String clusterFile, byte[] subspace, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            RecordMetaData metadata = createIndexedMetaData();
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metadata)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();

            Index index = metadata.getIndex("Order$price");
            IndexBuildProto.IndexBuildIndexingStamp stamp =
                store.loadIndexingTypeStampAsync(index).join();

            Map<String, Object> result = new HashMap<>();
            if (stamp == null) {
                result.put("exists", false);
            } else {
                result.put("exists", true);
                result.put("method", stamp.getMethod().name());
                result.put("methodNumber", stamp.getMethod().getNumber());
                if (stamp.hasBlock()) {
                    result.put("block", stamp.getBlock());
                }
                if (stamp.hasBlockID()) {
                    result.put("blockID", stamp.getBlockID());
                }
                if (stamp.getTargetIndexCount() > 0) {
                    result.put("targetIndexCount", stamp.getTargetIndexCount());
                }
            }
            return result;
        });
    }

    /**
     * Save a BY_RECORDS indexing type stamp for the Order$price index.
     * Matches Java's compileSingleTargetLegacyIndexingTypeStamp().
     */
    @ConformanceStep("saveIndexingTypeStampByRecords")
    public void saveIndexingTypeStampByRecords(String clusterFile, byte[] subspace, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            RecordMetaData metadata = createIndexedMetaData();
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metadata)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();

            Index index = metadata.getIndex("Order$price");
            IndexBuildProto.IndexBuildIndexingStamp stamp =
                IndexBuildProto.IndexBuildIndexingStamp.newBuilder()
                    .setMethod(IndexBuildProto.IndexBuildIndexingStamp.Method.BY_RECORDS)
                    .build();
            store.saveIndexingTypeStamp(index, stamp);
            return null;
        });
    }
}
