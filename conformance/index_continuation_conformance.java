package com.birdayz.conformance;

import com.apple.foundationdb.record.ExecuteProperties;
import com.apple.foundationdb.record.IndexScanType;
import com.apple.foundationdb.record.RecordMetaData;
import com.apple.foundationdb.record.ScanProperties;
import com.apple.foundationdb.record.TupleRange;
import com.apple.foundationdb.record.IndexEntry;
import com.apple.foundationdb.record.RecordCursorResult;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.record.RecordLayerDemo.Order;
import com.apple.foundationdb.record.metadata.Index;
import com.apple.foundationdb.subspace.Subspace;

import java.util.ArrayList;
import java.util.Base64;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

class IndexContinuationSteps extends ConformanceBase {

    @ConformanceStep("saveOrderForIndexContinuation")
    public void saveOrderForIndexContinuation(String clusterFile, byte[] subspace, Order order, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openIndexedStore(context, subspace);
            store.saveRecord(order);
            return null;
        });
    }

    @ConformanceStep("scanIndexWithContinuation")
    public Map<String, Object> scanIndexWithContinuation(String clusterFile, byte[] subspace, int limit, String continuation, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            RecordMetaData metadata = createIndexedMetaData();
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metadata)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();

            byte[] contBytes = null;
            if (continuation != null && !continuation.isEmpty()) {
                contBytes = Base64.getDecoder().decode(continuation);
            }

            Index index = metadata.getIndex("Order$price");
            ScanProperties scanProps = new ScanProperties(ExecuteProperties.newBuilder()
                .setReturnedRowLimit(limit)
                .build());

            com.apple.foundationdb.record.RecordCursor<IndexEntry> cursor = store.scanIndex(
                index, IndexScanType.BY_VALUE, TupleRange.ALL, contBytes, scanProps);

            List<Map<String, Object>> entries = new ArrayList<>();
            byte[] nextContinuation = null;
            boolean sourceExhausted = false;

            RecordCursorResult<IndexEntry> result;
            while ((result = cursor.getNext()) != null && result.hasNext()) {
                IndexEntry entry = result.get();
                Map<String, Object> entryMap = new HashMap<>();
                List<Object> keyValues = new ArrayList<>();
                for (Object item : entry.getKey()) {
                    keyValues.add(item);
                }
                entryMap.put("key", keyValues);
                entries.add(entryMap);
            }
            if (result != null) {
                nextContinuation = result.getContinuation().toBytes();
                sourceExhausted = result.getNoNextReason().isSourceExhausted();
            }

            Map<String, Object> response = new HashMap<>();
            response.put("entries", entries);
            if (nextContinuation != null) {
                response.put("continuation", Base64.getEncoder().encodeToString(nextContinuation));
            }
            response.put("sourceExhausted", sourceExhausted);
            return response;
        });
    }
}
