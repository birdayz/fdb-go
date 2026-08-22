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
import com.apple.foundationdb.record.RecordLayerDemo;
import com.apple.foundationdb.record.RecordLayerDemo.Order;
import com.apple.foundationdb.subspace.Subspace;

import java.util.ArrayList;
import java.util.Arrays;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

/**
 * Multi-type and universal index entry conformance.
 *
 * <p>The sibling CompositeIndexSteps covers a SINGLE-TYPE index whose key
 * overlaps the primary key, where Java trims the redundant components. That is
 * one of three registration shapes, and it is the only one Java trims:
 * setPrimaryKeyComponentPositions is called from a single site
 * (RecordMetaDataBuilder:1466) iterating recordTypeBuilder.getIndexes(), and
 * addMultiTypeIndex routes zero names to universalIndexes and two-or-more to
 * getMultiTypeIndexes(), neither of which that loop visits.
 *
 * <p>So a multi-type or universal index writes its primary key UNTRIMMED, and
 * the entry for a record whose index key IS its primary key repeats the value.
 * Go trimmed it away, producing a shorter entry than Java for identical
 * metadata; nothing in this suite could see that, because it had no multi-type
 * index at all and its one universal index used a key that could not overlap a
 * primary key. This class supplies both shapes.
 *
 * <p>Every record type is keyed on `price` because it is the only field the demo
 * proto declares on all three, which is what lets a universal index key overlap
 * every primary key.
 */
class MultiTypeIndexSteps extends ConformanceBase {
    private static RecordMetaDataBuilder priceKeyedBuilder() {
        RecordMetaDataBuilder metaDataBuilder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        metaDataBuilder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("price"));
        metaDataBuilder.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.field("price"));
        metaDataBuilder.getRecordType("TypedRecord")
            .setPrimaryKey(Key.Expressions.field("price"));
        return metaDataBuilder;
    }

    private static RecordMetaData createMultiTypeIndexedMetaData() {
        RecordMetaDataBuilder metaDataBuilder = priceKeyedBuilder();
        metaDataBuilder.addMultiTypeIndex(
            Arrays.asList(metaDataBuilder.getRecordType("Order"),
                          metaDataBuilder.getRecordType("Customer")),
            new Index("MT$price", Key.Expressions.field("price"), IndexTypes.VALUE));
        return metaDataBuilder.build();
    }

    private static RecordMetaData createUniversalIndexedMetaData() {
        RecordMetaDataBuilder metaDataBuilder = priceKeyedBuilder();
        metaDataBuilder.addUniversalIndex(
            new Index("UNI$price", Key.Expressions.field("price"), IndexTypes.VALUE));
        return metaDataBuilder.build();
    }

    private static RecordMetaData metaDataFor(String kind) {
        if ("universal".equals(kind)) {
            return createUniversalIndexedMetaData();
        }
        if ("multiType".equals(kind)) {
            return createMultiTypeIndexedMetaData();
        }
        throw new IllegalArgumentException("unknown index kind: " + kind);
    }

    private static String indexNameFor(String kind) {
        return "universal".equals(kind) ? "UNI$price" : "MT$price";
    }

    private static FDBRecordStore openStore(FDBRecordContext context, byte[] subspace, String kind) {
        return FDBRecordStore.newBuilder()
            .setMetaDataProvider(metaDataFor(kind))
            .setContext(context)
            .setSubspace(new Subspace(subspace))
            .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
            .createOrOpen();
    }

    @ConformanceStep("saveOrderWithSharedIndex")
    public void saveOrderWithSharedIndex(String clusterFile, byte[] subspace, Order order,
                                         String indexKind, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openStore(context, subspace, indexKind);
            store.saveRecord(order);
            return null;
        });
    }

    @ConformanceStep("scanSharedIndex")
    public List<Map<String, Object>> scanSharedIndex(String clusterFile, byte[] subspace,
                                                     String indexKind, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            RecordMetaData metadata = metaDataFor(indexKind);
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metadata)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();

            Index index = metadata.getIndex(indexNameFor(indexKind));
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

    /**
     * Reports whether Java assigned primaryKeyComponentPositions to the index,
     * which is the metadata-level cause of the entry shape above. Asserting the
     * cause as well as the effect is what distinguishes "Go matches Java's
     * bytes" from "Go matches Java's bytes for a different reason".
     */
    @ConformanceStep("sharedIndexHasPrimaryKeyComponentPositions")
    public boolean sharedIndexHasPrimaryKeyComponentPositions(String indexKind) {
        RecordMetaData metadata = metaDataFor(indexKind);
        // getPrimaryKeyComponentPositions(), not hasPrimaryKeyComponentPositions():
        // the latter is package-private in Index, which is itself a small piece of
        // evidence that positions are an internal detail Java does not expect
        // callers to reason about. The public getter returns null when unset, and
        // hasPrimaryKeyComponentPositions() is documented as agreeing with exactly
        // that (Index.java:519-525).
        return metadata.getIndex(indexNameFor(indexKind)).getPrimaryKeyComponentPositions() != null;
    }
}
