package com.birdayz.conformance;

import com.apple.foundationdb.record.ExecuteProperties;
import com.apple.foundationdb.record.IndexEntry;
import com.apple.foundationdb.record.IndexScanType;
import com.apple.foundationdb.record.RecordCursor;
import com.apple.foundationdb.record.RecordCursorResult;
import com.apple.foundationdb.record.RecordLayerDemo;
import com.apple.foundationdb.record.RecordMetaData;
import com.apple.foundationdb.record.RecordMetaDataBuilder;
import com.apple.foundationdb.record.ScanProperties;
import com.apple.foundationdb.record.TupleRange;
import com.apple.foundationdb.record.metadata.Index;
import com.apple.foundationdb.record.metadata.IndexOptions;
import com.apple.foundationdb.record.metadata.IndexTypes;
import com.apple.foundationdb.record.metadata.Key;
import com.apple.foundationdb.record.metadata.expressions.GroupingKeyExpression;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.subspace.Subspace;
import com.apple.foundationdb.tuple.Tuple;

import java.util.ArrayList;
import java.util.Base64;
import java.util.Collections;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

/**
 * Continuations over narrowed ranges, whose prefix length is the common prefix of the
 * packed bounds (KeyValueCursorBase.calculatePrefixLength) rather than the subspace.
 */
class IndexContinuationRangeSteps extends ConformanceBase {

    static RecordMetaData createRangeContinuationMetaData() {
        RecordMetaDataBuilder metaDataBuilder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        metaDataBuilder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("order_id"));
        metaDataBuilder.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.field("customer_id"));
        metaDataBuilder.getRecordType("TypedRecord")
            .setPrimaryKey(Key.Expressions.field("id"));
        metaDataBuilder.addIndex("Order", new Index("Order$qty_price",
            Key.Expressions.concatenateFields("quantity", "price"), IndexTypes.VALUE));
        metaDataBuilder.addIndex("Order", new Index("Order$cnt_qty_price",
            new GroupingKeyExpression(Key.Expressions.concatenateFields("quantity", "price"), 0),
            IndexTypes.COUNT));
        metaDataBuilder.addIndex("Order", new Index("Order$bm_qty",
            new GroupingKeyExpression(Key.Expressions.concatenateFields("quantity", "order_id"), 1),
            IndexTypes.BITMAP_VALUE,
            Collections.singletonMap(IndexOptions.BITMAP_VALUE_ENTRY_SIZE_OPTION, "4")));
        metaDataBuilder.addIndex("Customer", new Index("Customer$name",
            Key.Expressions.field("name"), IndexTypes.VALUE));
        return metaDataBuilder.build();
    }

    private static TupleRange rangeFor(String range) {
        switch (range) {
            case "allOf11":
                return TupleRange.allOf(Tuple.from(11L));
            case "between11":
                return TupleRange.betweenInclusive(Tuple.from(11L, 2L), Tuple.from(11L, 8L));
            case "prefixAb":
                return TupleRange.prefixedBy("ab");
            case "pk5to9":
                return TupleRange.betweenInclusive(Tuple.from(5L), Tuple.from(9L));
            default:
                throw new IllegalArgumentException("unknown range " + range);
        }
    }

    /**
     * One page of a narrowed scan. indexName "" scans records by primary key; scanType is
     * BY_VALUE or BY_GROUP. Returns entry keys (primary keys for records), the continuation
     * (base64) and whether the source is exhausted.
     */
    @ConformanceStep("scanRangeWithContinuation")
    public Map<String, Object> scanRangeWithContinuation(String clusterFile, byte[] subspace, String indexName,
                                                         String scanType, String range, boolean reverse, int limit,
                                                         String continuation, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            RecordMetaData metadata = createRangeContinuationMetaData();
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metadata)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();
            byte[] contBytes = continuation == null || continuation.isEmpty() ? null : Base64.getDecoder().decode(continuation);
            ScanProperties props = new ScanProperties(ExecuteProperties.newBuilder().setReturnedRowLimit(limit).build(), reverse);

            List<List<Object>> keys = new ArrayList<>();
            RecordCursorResult<?> result;
            if (indexName == null || indexName.isEmpty()) {
                RecordCursor<com.apple.foundationdb.record.provider.foundationdb.FDBStoredRecord<com.google.protobuf.Message>> cursor =
                    store.scanRecords(rangeFor(range), contBytes, props);
                RecordCursorResult<com.apple.foundationdb.record.provider.foundationdb.FDBStoredRecord<com.google.protobuf.Message>> r;
                while ((r = cursor.getNext()).hasNext()) {
                    keys.add(new ArrayList<>(r.get().getPrimaryKey().getItems()));
                }
                result = r;
            } else {
                RecordCursor<IndexEntry> cursor = store.scanIndex(metadata.getIndex(indexName),
                    "BY_GROUP".equals(scanType) ? IndexScanType.BY_GROUP : IndexScanType.BY_VALUE, rangeFor(range), contBytes, props);
                RecordCursorResult<IndexEntry> r;
                while ((r = cursor.getNext()).hasNext()) {
                    keys.add(new ArrayList<>(r.get().getKey().getItems()));
                }
                result = r;
            }
            Map<String, Object> response = new HashMap<>();
            response.put("keys", keys);
            byte[] next = result.getContinuation().toBytes();
            if (next != null) {
                response.put("continuation", Base64.getEncoder().encodeToString(next));
            }
            response.put("sourceExhausted", result.getNoNextReason().isSourceExhausted());
            return response;
        });
    }
}
