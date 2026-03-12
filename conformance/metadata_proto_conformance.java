package com.birdayz.conformance;

import com.apple.foundationdb.record.RecordMetaData;
import com.apple.foundationdb.record.RecordMetaDataBuilder;
import com.apple.foundationdb.record.RecordMetaDataOptionsProto;
import com.apple.foundationdb.record.RecordMetaDataProto;
import com.apple.foundationdb.record.RecordLayerDemo;
import com.apple.foundationdb.record.metadata.Index;
import com.apple.foundationdb.record.metadata.IndexTypes;
import com.apple.foundationdb.record.metadata.Key;
import com.apple.foundationdb.record.metadata.RecordType;
import com.apple.foundationdb.record.metadata.expressions.KeyExpression;
import com.google.protobuf.ExtensionRegistry;
import com.google.protobuf.InvalidProtocolBufferException;
import com.google.protobuf.util.JsonFormat;

import java.util.ArrayList;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

class MetaDataProtoSteps extends ConformanceBase {

    private static final ExtensionRegistry EXTENSION_REGISTRY;
    static {
        EXTENSION_REGISTRY = ExtensionRegistry.newInstance();
        RecordMetaDataOptionsProto.registerAllExtensions(EXTENSION_REGISTRY);
    }

    /**
     * Deserialize Go-produced metadata proto bytes and return a detailed summary.
     * This validates that Java can parse what Go serializes.
     */
    @ConformanceStep("deserializeMetaData")
    public Map<String, Object> deserializeMetaData(byte[] protoBytes) {
        try {
            RecordMetaDataProto.MetaData metaDataProto = RecordMetaDataProto.MetaData.parseFrom(protoBytes, EXTENSION_REGISTRY);
            RecordMetaData metaData = RecordMetaData.build(metaDataProto);
            return extractMetaDataSummary(metaData);
        } catch (InvalidProtocolBufferException e) {
            throw new RuntimeException("Failed to parse metadata proto: " + e.getMessage(), e);
        }
    }

    /**
     * Build metadata with a specific configuration and serialize to proto bytes.
     * Returns the raw bytes as int array for Go to deserialize.
     */
    @ConformanceStep("serializeMetaData")
    public Map<String, Object> serializeMetaData(String config) {
        RecordMetaData metaData = buildMetaData(config);
        RecordMetaDataProto.MetaData proto = metaData.toProto();
        byte[] bytes = proto.toByteArray();

        // Return as int array (JSON-safe) + summary for validation
        int[] intArray = new int[bytes.length];
        for (int i = 0; i < bytes.length; i++) {
            intArray[i] = bytes[i] & 0xFF;
        }

        Map<String, Object> result = new HashMap<>();
        result.put("protoBytes", intArray);
        result.put("summary", extractMetaDataSummary(metaData));
        return result;
    }

    /**
     * Accept proto bytes, deserialize, re-serialize, return new bytes.
     * Tests Go -> Java -> Go roundtrip.
     */
    @ConformanceStep("reserializeMetaData")
    public Map<String, Object> reserializeMetaData(byte[] protoBytes) {
        try {
            RecordMetaDataProto.MetaData metaDataProto = RecordMetaDataProto.MetaData.parseFrom(protoBytes, EXTENSION_REGISTRY);
            RecordMetaData metaData = RecordMetaData.build(metaDataProto);

            // Re-serialize
            RecordMetaDataProto.MetaData reProto = metaData.toProto();
            byte[] reBytes = reProto.toByteArray();

            int[] intArray = new int[reBytes.length];
            for (int i = 0; i < reBytes.length; i++) {
                intArray[i] = reBytes[i] & 0xFF;
            }

            Map<String, Object> result = new HashMap<>();
            result.put("protoBytes", intArray);
            result.put("summary", extractMetaDataSummary(metaData));
            return result;
        } catch (InvalidProtocolBufferException e) {
            throw new RuntimeException("Failed to parse metadata proto: " + e.getMessage(), e);
        }
    }

    private RecordMetaData buildMetaData(String config) {
        RecordMetaDataBuilder builder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        builder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("order_id"));
        builder.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.field("customer_id"));
        builder.getRecordType("TypedRecord")
            .setPrimaryKey(Key.Expressions.field("id"));

        switch (config) {
            case "basic":
                // Just primary keys, no indexes
                break;
            case "with_indexes":
                builder.addIndex("Order", new Index("Order$price", Key.Expressions.field("price"), IndexTypes.VALUE));
                builder.addIndex("Order", new Index("Order$quantity_price",
                    Key.Expressions.concatenateFields("quantity", "price"), IndexTypes.VALUE));
                builder.addIndex("Customer", new Index("Customer$name", Key.Expressions.field("name"), IndexTypes.VALUE));
                break;
            case "with_former_indexes":
                builder.addIndex("Order", new Index("temp_idx", Key.Expressions.field("price"), IndexTypes.VALUE));
                builder.removeIndex("temp_idx");
                builder.addIndex("Order", new Index("Order$price", Key.Expressions.field("price"), IndexTypes.VALUE));
                break;
            case "full":
                // Indexes
                builder.addIndex("Order", new Index("Order$price", Key.Expressions.field("price"), IndexTypes.VALUE));
                builder.addIndex("Order", new Index("Order$quantity_price",
                    Key.Expressions.concatenateFields("quantity", "price"), IndexTypes.VALUE));
                builder.addIndex("Customer", new Index("Customer$name", Key.Expressions.field("name"), IndexTypes.VALUE));
                // Former index
                builder.addIndex("Order", new Index("temp_idx", Key.Expressions.field("quantity"), IndexTypes.VALUE));
                builder.removeIndex("temp_idx");
                // Flags
                builder.setSplitLongRecords(true);
                builder.setStoreRecordVersions(true);
                break;
            case "with_universal_index":
                builder.addUniversalIndex(new Index("global_price", Key.Expressions.field("price"), IndexTypes.VALUE));
                break;
            case "with_record_count":
                builder.setRecordCountKey(Key.Expressions.empty());
                break;
            default:
                throw new RuntimeException("Unknown config: " + config);
        }

        builder.setVersion(5);
        return builder.build();
    }

    private Map<String, Object> extractMetaDataSummary(RecordMetaData metaData) {
        Map<String, Object> summary = new HashMap<>();

        // Version
        summary.put("version", metaData.getVersion());

        // Flags
        summary.put("splitLongRecords", metaData.isSplitLongRecords());
        summary.put("storeRecordVersions", metaData.isStoreRecordVersions());

        // Record types
        List<Map<String, Object>> recordTypes = new ArrayList<>();
        for (RecordType rt : metaData.getRecordTypes().values()) {
            Map<String, Object> rtMap = new HashMap<>();
            rtMap.put("name", rt.getName());
            if (rt.getSinceVersion() != null) {
                rtMap.put("sinceVersion", rt.getSinceVersion());
            }
            if (rt.getExplicitRecordTypeKey() != null) {
                rtMap.put("explicitTypeKey", rt.getExplicitRecordTypeKey());
            }
            recordTypes.add(rtMap);
        }
        summary.put("recordTypes", recordTypes);

        // Indexes
        List<Map<String, Object>> indexes = new ArrayList<>();
        for (Index idx : metaData.getAllIndexes()) {
            Map<String, Object> idxMap = new HashMap<>();
            idxMap.put("name", idx.getName());
            idxMap.put("type", idx.getType());
            idxMap.put("subspaceKey", idx.getSubspaceKey().toString());
            try {
                idxMap.put("rootExpression", JsonFormat.printer()
                    .omittingInsignificantWhitespace()
                    .print(idx.getRootExpression().toKeyExpression()));
            } catch (com.google.protobuf.InvalidProtocolBufferException e) {
                idxMap.put("rootExpression", idx.getRootExpression().toString());
            }
            idxMap.put("addedVersion", idx.getAddedVersion());
            idxMap.put("lastModifiedVersion", idx.getLastModifiedVersion());
            indexes.add(idxMap);
        }
        summary.put("indexes", indexes);

        // Former indexes
        List<Map<String, Object>> formerIndexes = new ArrayList<>();
        for (com.apple.foundationdb.record.metadata.FormerIndex fi : metaData.getFormerIndexes()) {
            Map<String, Object> fiMap = new HashMap<>();
            fiMap.put("formerName", fi.getFormerName());
            fiMap.put("subspaceKey", fi.getSubspaceKey().toString());
            fiMap.put("addedVersion", fi.getAddedVersion());
            fiMap.put("removedVersion", fi.getRemovedVersion());
            formerIndexes.add(fiMap);
        }
        summary.put("formerIndexes", formerIndexes);

        if (metaData.getRecordCountKey() != null) {
            try {
                summary.put("recordCountKey", JsonFormat.printer()
                    .omittingInsignificantWhitespace()
                    .print(metaData.getRecordCountKey().toKeyExpression()));
            } catch (com.google.protobuf.InvalidProtocolBufferException e) {
                summary.put("recordCountKey", metaData.getRecordCountKey().toString());
            }
        }

        return summary;
    }
}
