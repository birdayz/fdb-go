package com.birdayz.conformance;

import com.apple.foundationdb.record.IndexScanType;
import com.apple.foundationdb.record.RecordMetaData;
import com.apple.foundationdb.record.RecordMetaDataBuilder;
import com.apple.foundationdb.record.ScanProperties;
import com.apple.foundationdb.record.TupleRange;
import com.apple.foundationdb.record.IndexEntry;
import com.apple.foundationdb.record.metadata.Index;
import com.apple.foundationdb.record.metadata.IndexTypes;
import com.apple.foundationdb.record.metadata.Key;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordContext;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.record.provider.foundationdb.leaderboard.TimeWindowLeaderboardWindowUpdate;
import com.apple.foundationdb.record.RecordLayerDemo;
import com.apple.foundationdb.record.RecordLayerDemo.Order;
import com.apple.foundationdb.subspace.Subspace;
import com.apple.foundationdb.tuple.Tuple;

import com.apple.foundationdb.record.EvaluationContext;
import com.apple.foundationdb.record.FunctionNames;
import com.apple.foundationdb.record.metadata.IndexRecordFunction;
import com.apple.foundationdb.record.metadata.expressions.GroupingKeyExpression;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecord;
import com.google.protobuf.Message;

import java.util.ArrayList;
import java.util.Collections;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

class TimeWindowLeaderboardSteps extends ConformanceBase {

    private static final String INDEX_NAME = "leaderboard_score";

    private static RecordMetaData createLeaderboardMetaData() {
        RecordMetaDataBuilder metaDataBuilder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        metaDataBuilder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("order_id"));
        metaDataBuilder.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.field("customer_id"));
        metaDataBuilder.getRecordType("TypedRecord")
            .setPrimaryKey(Key.Expressions.field("id"));
        metaDataBuilder.addIndex("Order", new Index(INDEX_NAME,
            Key.Expressions.concat(Key.Expressions.field("price"), Key.Expressions.field("quantity")).ungrouped(),
            IndexTypes.TIME_WINDOW_LEADERBOARD));
        return metaDataBuilder.build();
    }

    private static FDBRecordStore openLeaderboardStore(FDBRecordContext context, byte[] subspace) {
        return FDBRecordStore.newBuilder()
            .setMetaDataProvider(createLeaderboardMetaData())
            .setContext(context)
            .setSubspace(new Subspace(subspace))
            .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
            .createOrOpen();
    }

    @ConformanceStep("setupLeaderboardWindows")
    public void setupLeaderboardWindows(String clusterFile, byte[] subspace, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openLeaderboardStore(context, subspace);
            TimeWindowLeaderboardWindowUpdate update = new TimeWindowLeaderboardWindowUpdate(
                System.currentTimeMillis(),
                false,
                0,
                true,
                Collections.emptyList(),
                TimeWindowLeaderboardWindowUpdate.Rebuild.IF_OVERLAPPING_CHANGED);
            store.performIndexOperation(INDEX_NAME, update);
            return null;
        });
    }

    @ConformanceStep("saveOrderWithLeaderboard")
    public void saveOrderWithLeaderboard(String clusterFile, byte[] subspace, Order order, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openLeaderboardStore(context, subspace);
            store.saveRecord(order);
            return null;
        });
    }

    @ConformanceStep("deleteOrderWithLeaderboard")
    public boolean deleteOrderWithLeaderboard(String clusterFile, byte[] subspace, long orderID, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openLeaderboardStore(context, subspace);
            return store.deleteRecord(Tuple.from(orderID));
        });
    }

    @ConformanceStep("scanLeaderboardIndex")
    public List<Map<String, Object>> scanLeaderboardIndex(String clusterFile, byte[] subspace, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            RecordMetaData metadata = createLeaderboardMetaData();
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metadata)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();

            Index index = metadata.getIndex(INDEX_NAME);
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

    @ConformanceStep("leaderboardRankForRecord")
    public Long leaderboardRankForRecord(String clusterFile, byte[] subspace, long orderID, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            RecordMetaData metadata = createLeaderboardMetaData();
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metadata)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();

            FDBRecord<Message> record = store.loadRecord(Tuple.from(orderID));
            if (record == null) {
                return null;
            }

            IndexRecordFunction<Long> rankFunction = new IndexRecordFunction<>(
                FunctionNames.RANK,
                Key.Expressions.concat(Key.Expressions.field("price"), Key.Expressions.field("quantity")).ungrouped(),
                INDEX_NAME);

            return store.evaluateRecordFunction(rankFunction, record).join();
        });
    }
}
