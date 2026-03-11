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
import com.apple.foundationdb.record.provider.foundationdb.FDBStoredRecord;
import com.apple.foundationdb.record.RecordLayerDemo;
import com.apple.foundationdb.record.RecordLayerDemo.TypedRecord;
import com.apple.foundationdb.subspace.Subspace;
import com.apple.foundationdb.tuple.Tuple;
import com.google.protobuf.ByteString;
import com.google.protobuf.Message;

import java.util.ArrayList;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

/**
 * Conformance steps for TypedRecord — tests FDB tuple encoding of all proto field types.
 * Each field type gets its own VALUE index so we can verify the tuple encoding matches
 * between Go and Java by scanning the index and comparing entries.
 */
class TypedRecordSteps extends ConformanceBase {

    private static RecordMetaData createTypedRecordMetaData() {
        RecordMetaDataBuilder builder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        builder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("order_id"));
        builder.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.field("customer_id"));
        builder.getRecordType("TypedRecord")
            .setPrimaryKey(Key.Expressions.field("id"));

        // One index per field type — each exercises a different tuple encoding path
        // NOTE: unsigned types (uint32, uint64, fixed32, fixed64) rejected by Java
        builder.addIndex("TypedRecord", new Index("idx_int32", Key.Expressions.field("val_int32"), IndexTypes.VALUE));
        builder.addIndex("TypedRecord", new Index("idx_int64", Key.Expressions.field("val_int64"), IndexTypes.VALUE));
        builder.addIndex("TypedRecord", new Index("idx_sint32", Key.Expressions.field("val_sint32"), IndexTypes.VALUE));
        builder.addIndex("TypedRecord", new Index("idx_sint64", Key.Expressions.field("val_sint64"), IndexTypes.VALUE));
        builder.addIndex("TypedRecord", new Index("idx_sfixed32", Key.Expressions.field("val_sfixed32"), IndexTypes.VALUE));
        builder.addIndex("TypedRecord", new Index("idx_sfixed64", Key.Expressions.field("val_sfixed64"), IndexTypes.VALUE));
        builder.addIndex("TypedRecord", new Index("idx_float", Key.Expressions.field("val_float"), IndexTypes.VALUE));
        builder.addIndex("TypedRecord", new Index("idx_double", Key.Expressions.field("val_double"), IndexTypes.VALUE));
        builder.addIndex("TypedRecord", new Index("idx_bool", Key.Expressions.field("val_bool"), IndexTypes.VALUE));
        builder.addIndex("TypedRecord", new Index("idx_string", Key.Expressions.field("val_string"), IndexTypes.VALUE));
        builder.addIndex("TypedRecord", new Index("idx_bytes", Key.Expressions.field("val_bytes"), IndexTypes.VALUE));
        builder.addIndex("TypedRecord", new Index("idx_enum", Key.Expressions.field("val_enum"), IndexTypes.VALUE));

        return builder.build();
    }

    private static FDBRecordStore openTypedRecordStore(FDBRecordContext context, byte[] subspace) {
        return FDBRecordStore.newBuilder()
            .setMetaDataProvider(createTypedRecordMetaData())
            .setContext(context)
            .setSubspace(new Subspace(subspace))
            .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
            .createOrOpen();
    }

    @ConformanceStep("saveTypedRecord")
    public void saveTypedRecord(String clusterFile, byte[] subspace, TypedRecord record, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openTypedRecordStore(context, subspace);
            store.saveRecord(record);
            return null;
        });
    }

    @ConformanceStep("scanTypedIndex")
    public List<Map<String, Object>> scanTypedIndex(String clusterFile, byte[] subspace, String indexName, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            RecordMetaData metadata = createTypedRecordMetaData();
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metadata)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();

            Index index = metadata.getIndex(indexName);
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

    @ConformanceStep("loadTypedRecord")
    public TypedRecord loadTypedRecord(String clusterFile, byte[] subspace, long id, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openTypedRecordStore(context, subspace);
            FDBStoredRecord<Message> record = store.loadRecord(Tuple.from(id));
            if (record == null) {
                throw new RuntimeException("TypedRecord not found: " + id);
            }
            return TypedRecord.newBuilder().mergeFrom(record.getRecord()).build();
        });
    }
}
