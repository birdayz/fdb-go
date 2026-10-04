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
import com.apple.foundationdb.record.metadata.expressions.GroupingKeyExpression;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordContext;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
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
import java.util.HashMap;
import java.util.List;
import java.util.Map;

class RankIndexSteps extends ConformanceBase {
    private static RecordMetaData createRankIndexedMetaData() {
        RecordMetaDataBuilder metaDataBuilder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        metaDataBuilder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("order_id"));
        metaDataBuilder.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.field("customer_id"));
        metaDataBuilder.getRecordType("TypedRecord")
            .setPrimaryKey(Key.Expressions.field("id"));
        metaDataBuilder.addIndex("Order", new Index("rank_by_price",
            new GroupingKeyExpression(Key.Expressions.field("price"), 1),
            IndexTypes.RANK));
        return metaDataBuilder.build();
    }

    private static FDBRecordStore openRankIndexedStore(FDBRecordContext context, byte[] subspace) {
        return FDBRecordStore.newBuilder()
            .setMetaDataProvider(createRankIndexedMetaData())
            .setContext(context)
            .setSubspace(new Subspace(subspace))
            .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
            .createOrOpen();
    }

    @ConformanceStep("saveOrderWithRankIndex")
    public void saveOrderWithRankIndex(String clusterFile, byte[] subspace, Order order, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openRankIndexedStore(context, subspace);
            store.saveRecord(order);
            return null;
        });
    }

    @ConformanceStep("deleteOrderWithRankIndex")
    public boolean deleteOrderWithRankIndex(String clusterFile, byte[] subspace, long orderID, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openRankIndexedStore(context, subspace);
            return store.deleteRecord(Tuple.from(orderID));
        });
    }

    @ConformanceStep("rankForRecord")
    public Long rankForRecord(String clusterFile, byte[] subspace, long orderID, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            RecordMetaData metadata = createRankIndexedMetaData();
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
                new GroupingKeyExpression(Key.Expressions.field("price"), 1),
                null);

            return store.evaluateRecordFunction(rankFunction, record).join();
        });
    }

    @ConformanceStep("scanRankIndex")
    public List<Map<String, Object>> scanRankIndex(String clusterFile, byte[] subspace, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            RecordMetaData metadata = createRankIndexedMetaData();
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metadata)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();

            Index index = metadata.getIndex("rank_by_price");
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

    @ConformanceStep("scanRankIndexByRank")
    public List<Map<String, Object>> scanRankIndexByRank(String clusterFile, byte[] subspace,
            long lowRank, long highRank, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            RecordMetaData metadata = createRankIndexedMetaData();
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metadata)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();

            Index index = metadata.getIndex("rank_by_price");
            TupleRange rankRange = new TupleRange(
                Tuple.from(lowRank), Tuple.from(highRank),
                com.apple.foundationdb.record.EndpointType.RANGE_INCLUSIVE,
                com.apple.foundationdb.record.EndpointType.RANGE_EXCLUSIVE);

            List<IndexEntry> entries = store.scanIndex(
                index, IndexScanType.BY_RANK, rankRange, null, ScanProperties.FORWARD_SCAN)
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
    private static RecordMetaData rankMetadata(byte[] protoBytes) throws com.google.protobuf.InvalidProtocolBufferException {
        if (protoBytes.length == 0) {
            return createRankIndexedMetaData();
        }
        var registry = com.google.protobuf.ExtensionRegistry.newInstance();
        com.apple.foundationdb.record.RecordMetaDataOptionsProto.registerAllExtensions(registry);
        return RecordMetaData.build(com.apple.foundationdb.record.RecordMetaDataProto.MetaData.parseFrom(protoBytes, registry));
    }

    @ConformanceStep("saveOrderWithRankMetadata")
    public void saveOrderWithRankMetadata(String clusterFile, byte[] subspace, String tenantName,
            byte[] protoBytes, Order order) throws com.google.protobuf.InvalidProtocolBufferException {
        var metadata = rankMetadata(protoBytes);
        var record = com.google.protobuf.DynamicMessage.parseFrom(metadata.getRecordType("Order").getDescriptor(), order.toByteArray());
        runInContext(clusterFile, tenantName, context -> {
            var store = FDBRecordStore.newBuilder().setMetaDataProvider(metadata).setContext(context)
                    .setSubspace(new Subspace(subspace)).setFormatVersion(14)
                    .setUserVersionChecker(ALWAYS_READABLE_CHECKER).createOrOpen();
            store.saveRecord(record);
            return null;
        });
    }

    @ConformanceStep("scanRankIndexValues")
    public List<Map<String, Object>> scanRankIndexValues(String clusterFile, byte[] subspace,
            boolean byRank, boolean includeRank, boolean reverse, String tenantName,
            byte[] protoBytes, long group) throws com.google.protobuf.InvalidProtocolBufferException {
        var metadata = rankMetadata(protoBytes);
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder().setMetaDataProvider(metadata).setContext(context)
                    .setSubspace(new Subspace(subspace)).setFormatVersion(14).open();
            Index index = store.getRecordMetaData().getIndex("rank_by_price");
            TupleRange range = TupleRange.ALL;
            if (group >= 0) {
                range = byRank ? new TupleRange(Tuple.from(group, 0L), Tuple.from(group, 10L),
                        com.apple.foundationdb.record.EndpointType.RANGE_INCLUSIVE,
                        com.apple.foundationdb.record.EndpointType.RANGE_EXCLUSIVE)
                        : TupleRange.allOf(Tuple.from(group));
            }
            var bounds = new com.apple.foundationdb.record.provider.foundationdb.RankScanBounds(
                    byRank ? IndexScanType.BY_RANK : IndexScanType.BY_VALUE, range, includeRank);
            List<IndexEntry> entries = store.scanIndex(index, bounds, null,
                    reverse ? ScanProperties.REVERSE_SCAN : ScanProperties.FORWARD_SCAN).asList().join();
            List<Map<String, Object>> result = new ArrayList<>();
            for (IndexEntry entry : entries) {
                Map<String, Object> row = new HashMap<>();
                row.put("key", unsignedBytes(entry.getKey().pack()));
                row.put("primaryKey", unsignedBytes(entry.getPrimaryKey().pack()));
                row.put("value", unsignedBytes(entry.getValue().pack()));
                result.add(row);
            }
            return result;
        });
    }

    private static List<Integer> unsignedBytes(byte[] bytes) {
        List<Integer> result = new ArrayList<>(bytes.length);
        for (byte value : bytes) {
            result.add(Byte.toUnsignedInt(value));
        }
        return result;
    }

}
