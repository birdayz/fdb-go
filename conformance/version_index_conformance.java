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

class VersionIndexSteps extends ConformanceBase {
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

    private static RecordMetaData createVersionIndexedMetaData() {
        RecordMetaDataBuilder builder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        builder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("order_id"));
        builder.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.field("customer_id"));
        builder.setStoreRecordVersions(true);
        builder.addIndex("Order", new Index("Order$version",
            Key.Expressions.version(),
            IndexTypes.VERSION));
        return builder.build();
    }

    private static FDBRecordStore openVersionIndexedStore(com.apple.foundationdb.record.provider.foundationdb.FDBRecordContext context, byte[] subspace) {
        return FDBRecordStore.newBuilder()
            .setMetaDataProvider(createVersionIndexedMetaData())
            .setContext(context)
            .setSubspace(new Subspace(subspace))
            .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
            .createOrOpen();
    }

    @ConformanceStep("saveOrderWithVersionIndex")
    public void saveOrderWithVersionIndex(String clusterFile, byte[] subspace, Order order, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openVersionIndexedStore(context, subspace);
            store.saveRecord(order);
            return null;
        });
    }

    @ConformanceStep("deleteOrderWithVersionIndex")
    public boolean deleteOrderWithVersionIndex(String clusterFile, byte[] subspace, long orderID, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openVersionIndexedStore(context, subspace);
            return store.deleteRecord(Tuple.from(orderID));
        });
    }

    @ConformanceStep("scanVersionIndex")
    public List<Map<String, Object>> scanVersionIndex(String clusterFile, byte[] subspace, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            RecordMetaData metadata = createVersionIndexedMetaData();
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metadata)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();

            Index index = metadata.getIndex("Order$version");
            List<IndexEntry> entries = store.scanIndex(
                index, IndexScanType.BY_VALUE, TupleRange.ALL, null, ScanProperties.FORWARD_SCAN)
                .asList()
                .join();

            List<Map<String, Object>> result = new ArrayList<>();
            for (IndexEntry entry : entries) {
                Map<String, Object> map = new HashMap<>();
                // Key contains [versionstamp, primaryKey...]
                // Serialize versionstamp as hex for JSON transport (hex preserves byte ordering)
                List<Object> keyValues = new ArrayList<>();
                for (Object item : entry.getKey()) {
                    if (item instanceof Versionstamp) {
                        Versionstamp vs = (Versionstamp) item;
                        keyValues.add(bytesToHex(vs.getBytes()));
                    } else {
                        keyValues.add(item);
                    }
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
}
