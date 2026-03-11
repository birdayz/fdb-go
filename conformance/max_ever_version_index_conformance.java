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
import com.apple.foundationdb.tuple.Versionstamp;

import java.util.ArrayList;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

class MaxEverVersionIndexSteps extends ConformanceBase {
    private static final char[] HEX_CHARS = "0123456789abcdef".toCharArray();

    private static String bytesToHex(byte[] bytes) {
        char[] hex = new char[bytes.length * 2];
        for (int i = 0; i < bytes.length; i++) {
            int v = bytes[i] & 0xFF;
            hex[i * 2] = HEX_CHARS[v >>> 4];
            hex[i * 2 + 1] = HEX_CHARS[v & 0x0F];
        }
        return new String(hex);
    }

    private static RecordMetaData createMaxEverVersionIndexedMetaData() {
        RecordMetaDataBuilder builder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        builder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("order_id"));
        builder.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.field("customer_id"));
        builder.getRecordType("TypedRecord")
            .setPrimaryKey(Key.Expressions.field("id"));
        builder.setStoreRecordVersions(true);
        builder.addIndex("Order", new Index("Order$maxVersion",
            new GroupingKeyExpression(Key.Expressions.version(), 1),
            IndexTypes.MAX_EVER_VERSION));
        return builder.build();
    }

    private static FDBRecordStore openMaxEverVersionIndexedStore(FDBRecordContext context, byte[] subspace) {
        return FDBRecordStore.newBuilder()
            .setMetaDataProvider(createMaxEverVersionIndexedMetaData())
            .setContext(context)
            .setSubspace(new Subspace(subspace))
            .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
            .createOrOpen();
    }

    @ConformanceStep("saveOrderWithMaxEverVersionIndex")
    public void saveOrderWithMaxEverVersionIndex(String clusterFile, byte[] subspace, Order order, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openMaxEverVersionIndexedStore(context, subspace);
            store.saveRecord(order);
            return null;
        });
    }

    @ConformanceStep("deleteOrderWithMaxEverVersionIndex")
    public boolean deleteOrderWithMaxEverVersionIndex(String clusterFile, byte[] subspace, long orderID, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openMaxEverVersionIndexedStore(context, subspace);
            return store.deleteRecord(Tuple.from(orderID));
        });
    }

    @ConformanceStep("scanMaxEverVersionIndex")
    public List<Map<String, Object>> scanMaxEverVersionIndex(String clusterFile, byte[] subspace, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            RecordMetaData metadata = createMaxEverVersionIndexedMetaData();
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metadata)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();

            Index index = metadata.getIndex("Order$maxVersion");
            List<IndexEntry> entries = store.scanIndex(
                index, IndexScanType.BY_GROUP, TupleRange.ALL, null, ScanProperties.FORWARD_SCAN)
                .asList()
                .join();

            List<Map<String, Object>> result = new ArrayList<>();
            for (IndexEntry entry : entries) {
                Map<String, Object> map = new HashMap<>();
                // Key is the grouping key (empty tuple for ungrouped)
                List<Object> keyValues = new ArrayList<>();
                for (Object item : entry.getKey()) {
                    keyValues.add(item);
                }
                map.put("key", keyValues);

                // Value is tuple containing the versionstamp
                Tuple valueTuple = entry.getValue();
                List<Object> valueItems = new ArrayList<>();
                for (int i = 0; i < valueTuple.size(); i++) {
                    Object item = valueTuple.get(i);
                    if (item instanceof Versionstamp) {
                        valueItems.add(bytesToHex(((Versionstamp) item).getBytes()));
                    } else {
                        valueItems.add(item);
                    }
                }
                map.put("value", valueItems);

                result.add(map);
            }
            return result;
        });
    }
}
