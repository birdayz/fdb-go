package com.birdayz.conformance;

import com.apple.foundationdb.record.IndexScanType;
import com.apple.foundationdb.record.IndexState;
import com.apple.foundationdb.record.RecordIndexUniquenessViolation;
import com.apple.foundationdb.record.RecordMetaData;
import com.apple.foundationdb.record.RecordMetaDataBuilder;
import com.apple.foundationdb.record.ScanProperties;
import com.apple.foundationdb.record.TupleRange;
import com.apple.foundationdb.record.IndexEntry;
import com.apple.foundationdb.record.metadata.Index;
import com.apple.foundationdb.record.metadata.IndexOptions;
import com.apple.foundationdb.record.metadata.IndexTypes;
import com.apple.foundationdb.record.metadata.Key;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStoreBase;
import com.apple.foundationdb.record.RecordLayerDemo;
import com.apple.foundationdb.record.RecordLayerDemo.Order;
import com.apple.foundationdb.subspace.Subspace;
import com.apple.foundationdb.tuple.Tuple;
import com.google.protobuf.Message;

import java.util.ArrayList;
import java.util.Collections;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

class UniqueViolationSteps extends ConformanceBase {

    private static final String UNIQUE_INDEX_NAME = "Order$price_unique";

    private static RecordMetaData createUniqueIndexMetaData() {
        RecordMetaDataBuilder metaDataBuilder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        metaDataBuilder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("order_id"));
        metaDataBuilder.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.field("customer_id"));
        metaDataBuilder.getRecordType("TypedRecord")
            .setPrimaryKey(Key.Expressions.field("id"));
        metaDataBuilder.addIndex("Order", new Index(UNIQUE_INDEX_NAME,
            Key.Expressions.field("price"), IndexTypes.VALUE,
            Collections.singletonMap(IndexOptions.UNIQUE_OPTION, "true")));
        return metaDataBuilder.build();
    }

    private static FDBRecordStore openUniqueIndexStore(com.apple.foundationdb.record.provider.foundationdb.FDBRecordContext context, byte[] subspace) {
        return FDBRecordStore.newBuilder()
            .setMetaDataProvider(createUniqueIndexMetaData())
            .setContext(context)
            .setSubspace(new Subspace(subspace))
            .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
            .createOrOpen();
    }

    @ConformanceStep("saveWithUniqueIndex")
    public void saveWithUniqueIndex(String clusterFile, byte[] subspace, Order order, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openUniqueIndexStore(context, subspace);
            store.saveRecord(order);
            return null;
        });
    }

    @ConformanceStep("deleteWithUniqueIndex")
    public void deleteWithUniqueIndex(String clusterFile, byte[] subspace, long orderID, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openUniqueIndexStore(context, subspace);
            store.deleteRecord(Tuple.from(orderID));
            return null;
        });
    }

    @ConformanceStep("scanUniqueIndex")
    public List<Map<String, Object>> scanUniqueIndex(String clusterFile, byte[] subspace, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            RecordMetaData metadata = createUniqueIndexMetaData();
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metadata)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();

            Index index = metadata.getIndex(UNIQUE_INDEX_NAME);
            List<IndexEntry> entries = store.scanIndex(
                index, IndexScanType.BY_VALUE, TupleRange.ALL, null, ScanProperties.FORWARD_SCAN)
                .asList()
                .join();

            List<Map<String, Object>> result = new ArrayList<>();
            for (IndexEntry entry : entries) {
                Map<String, Object> map = new HashMap<>();
                List<Object> keyValues = new ArrayList<>();
                for (Object item : entry.getKey()) {
                    keyValues.add(item);
                }
                map.put("key", keyValues);
                List<Object> pkValues = new ArrayList<>();
                for (Object item : entry.getPrimaryKey()) {
                    pkValues.add(item);
                }
                map.put("primaryKey", pkValues);
                result.add(map);
            }
            return result;
        });
    }

    @ConformanceStep("saveDuplicateWithUniqueIndex")
    public void saveDuplicateWithUniqueIndex(String clusterFile, byte[] subspace, Order order1, Order order2, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openUniqueIndexStore(context, subspace);
            store.saveRecord(order1);
            store.saveRecord(order2);
            return null;
        });
    }

    @ConformanceStep("markUniqueIndexWriteOnly")
    public void markUniqueIndexWriteOnly(String clusterFile, byte[] subspace, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openUniqueIndexStore(context, subspace);
            store.markIndexWriteOnly(UNIQUE_INDEX_NAME).join();
            return null;
        });
    }

    @ConformanceStep("saveWithUniqueIndexDuringWriteOnly")
    public void saveWithUniqueIndexDuringWriteOnly(String clusterFile, byte[] subspace, Order order, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openUniqueIndexStore(context, subspace);
            store.saveRecord(order);
            return null;
        });
    }

    @ConformanceStep("scanUniquenessViolations")
    public List<Map<String, Object>> scanUniquenessViolations(String clusterFile, byte[] subspace, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            RecordMetaData metadata = createUniqueIndexMetaData();
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metadata)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();

            Index index = metadata.getIndex(UNIQUE_INDEX_NAME);
            List<RecordIndexUniquenessViolation> violations = store.scanUniquenessViolations(index)
                .asList()
                .join();

            List<Map<String, Object>> result = new ArrayList<>();
            for (RecordIndexUniquenessViolation violation : violations) {
                Map<String, Object> map = new HashMap<>();
                List<Object> indexKeyList = new ArrayList<>();
                for (Object item : violation.getIndexEntry().getKey()) {
                    indexKeyList.add(item);
                }
                map.put("indexKey", indexKeyList);
                List<Object> pkList = new ArrayList<>();
                for (Object item : violation.getPrimaryKey()) {
                    pkList.add(item);
                }
                map.put("primaryKey", pkList);
                if (violation.getExistingKey() != null) {
                    List<Object> existingList = new ArrayList<>();
                    for (Object item : violation.getExistingKey()) {
                        existingList.add(item);
                    }
                    map.put("existingKey", existingList);
                } else {
                    map.put("existingKey", null);
                }
                result.add(map);
            }
            return result;
        });
    }

    @ConformanceStep("getUniqueIndexState")
    public String getUniqueIndexState(String clusterFile, byte[] subspace, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openUniqueIndexStore(context, subspace);
            IndexState state = store.getRecordStoreState().getState(UNIQUE_INDEX_NAME);
            return state.name();
        });
    }
}
