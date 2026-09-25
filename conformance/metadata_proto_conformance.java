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

    @ConformanceStep("evolveUnionRecord")
    public Map<String, Object> evolveUnionRecord(String clusterFile, byte[] subspace, String tenantName,
                                                 byte[] protoBytes) throws InvalidProtocolBufferException {
        final var metadata = RecordMetaData.build(RecordMetaDataProto.MetaData.parseFrom(protoBytes, EXTENSION_REGISTRY));
        return runInContext(clusterFile, tenantName, context -> {
            final var store = com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore.newBuilder()
                    .setMetaDataProvider(metadata).setContext(context)
                    .setSubspace(new com.apple.foundationdb.subspace.Subspace(subspace)).setFormatVersion(14).open();
            final var loaded = store.loadRecord(com.apple.foundationdb.tuple.Tuple.from(10L));
            if (loaded == null) {
                throw new IllegalStateException("missing old union record");
            }
            final var record = loaded.getRecord();
            final var descriptor = record.getDescriptorForType();
            final var updated = record.toBuilder().setField(descriptor.findFieldByName("id"), 11L).build();
            store.saveRecord(updated);
            return Map.of("recordName", loaded.getRecordType().getName(),
                    "payload", record.getField(descriptor.findFieldByName("payload")),
                    "typeKey", loaded.getRecordType().getRecordTypeKey(),
                    "tag", metadata.getUnionFieldForRecordType(loaded.getRecordType()).getNumber());
        });
    }

    @ConformanceStep("validateMetaDataEvolutionOptions")
    public Map<String, Object> validateMetaDataEvolutionOptions(byte[] oldProtoBytes, byte[] newProtoBytes,
                                                               List<String> ignoredOptions) throws InvalidProtocolBufferException {
        final var oldMetadata = RecordMetaData.build(RecordMetaDataProto.MetaData.parseFrom(oldProtoBytes, EXTENSION_REGISTRY));
        final var newMetadata = RecordMetaData.build(RecordMetaDataProto.MetaData.parseFrom(newProtoBytes, EXTENSION_REGISTRY));
        final var validator = com.apple.foundationdb.record.metadata.MetaDataEvolutionValidator.newBuilder()
                .setIgnoredIndexOptions(ignoredOptions).build();
        try {
            validator.validate(oldMetadata, newMetadata);
            return Map.of("valid", true, "error", "");
        } catch (com.apple.foundationdb.record.metadata.MetaDataException ex) {
            return Map.of("valid", false, "error", ex.getMessage());
        }
    }

    /**
     * Validate an evolution with the validator flags that govern former indexes and
     * index rebuilds. MetaDataException is the verdict; any other exception fails the
     * step.
     */
    @ConformanceStep("validateMetaDataEvolutionFlags")
    public Map<String, Object> validateMetaDataEvolutionFlags(byte[] oldProtoBytes, byte[] newProtoBytes,
                                                             boolean allowMissingFormerIndexNames,
                                                             boolean allowOlderFormerIndexAddedVersions,
                                                             boolean allowIndexRebuilds) throws InvalidProtocolBufferException {
        final var oldMetadata = RecordMetaData.build(RecordMetaDataProto.MetaData.parseFrom(oldProtoBytes, EXTENSION_REGISTRY));
        final var newMetadata = RecordMetaData.build(RecordMetaDataProto.MetaData.parseFrom(newProtoBytes, EXTENSION_REGISTRY));
        final var validator = com.apple.foundationdb.record.metadata.MetaDataEvolutionValidator.newBuilder()
                .setAllowMissingFormerIndexNames(allowMissingFormerIndexNames)
                .setAllowOlderFormerIndexAddedVerions(allowOlderFormerIndexAddedVersions)
                .setAllowIndexRebuilds(allowIndexRebuilds)
                .build();
        try {
            validator.validate(oldMetadata, newMetadata);
            return Map.of("valid", true, "error", "");
        } catch (com.apple.foundationdb.record.metadata.MetaDataException ex) {
            return Map.of("valid", false, "error", ex.getMessage());
        }
    }

    /**
     * Java's MetaDataEvolutionValidator with its default options over two
     * serialized meta-data, reporting ANY runtime exception as the verdict: its
     * class's simple name and message. The index option checks parse options
     * with the JDK and the RankedSet/RTree config builders, so a refusal there
     * is a RecordCoreArgumentException, a NumberFormatException or an
     * IllegalArgumentException, not a MetaDataException.
     */
    @ConformanceStep("validateMetaDataEvolutionAnyVerdict")
    public Map<String, Object> validateMetaDataEvolutionAnyVerdict(byte[] oldProtoBytes, byte[] newProtoBytes)
            throws InvalidProtocolBufferException {
        final var oldMetadata = RecordMetaData.build(RecordMetaDataProto.MetaData.parseFrom(oldProtoBytes, EXTENSION_REGISTRY));
        final var newMetadata = RecordMetaData.build(RecordMetaDataProto.MetaData.parseFrom(newProtoBytes, EXTENSION_REGISTRY));
        try {
            com.apple.foundationdb.record.metadata.MetaDataEvolutionValidator.getDefaultInstance().validate(oldMetadata, newMetadata);
            return Map.of("valid", true, "error", "", "class", "");
        } catch (RuntimeException ex) {
            return Map.of("valid", false, "error", String.valueOf(ex.getMessage()), "class", ex.getClass().getSimpleName());
        }
    }

    /**
     * Build meta-data from proto bytes, which runs Java's MetaDataValidator,
     * reporting ANY runtime exception as the verdict with its FULL class name:
     * KeyExpression.InvalidExpressionException and Query.InvalidExpressionException
     * share a simple name, and key validation throws both.
     */
    /**
     * Java's in-code build over a records file: RecordMetaData.build(FileDescriptor),
     * which runs setRecords with processExtensionOptions true, so the file's
     * schema, record type and field options are read. The file depends only on
     * record_metadata_options.proto. Reports the built meta-data's proto, or any
     * runtime exception with its full class name.
     */
    @ConformanceStep("buildInCodeMetaData")
    public Map<String, Object> buildInCodeMetaData(byte[] fileDescriptorProto) throws Exception {
        final var fdp = com.google.protobuf.DescriptorProtos.FileDescriptorProto.parseFrom(fileDescriptorProto, EXTENSION_REGISTRY);
        final var fd = com.google.protobuf.Descriptors.FileDescriptor.buildFrom(fdp,
                new com.google.protobuf.Descriptors.FileDescriptor[] {RecordMetaDataOptionsProto.getDescriptor()});
        try {
            final RecordMetaData md = RecordMetaData.build(fd);
            final List<Integer> bytes = new ArrayList<>();
            for (byte b : md.toProto().toByteArray()) {
                bytes.add((int) b);
            }
            return Map.of("valid", true, "error", "", "class", "", "metaData", bytes);
        } catch (RuntimeException ex) {
            return Map.of("valid", false, "error", String.valueOf(ex.getMessage()), "class", ex.getClass().getName(), "metaData", List.of());
        }
    }

    /**
     * Java's query-side read of a record's field,
     * MessageHelpers.getFieldOnMessage, over a message of a records file that
     * depends on nothing: whether the field reads as null, and otherwise its
     * value's string form.
     */
    @ConformanceStep("getFieldOnMessageJava")
    public Map<String, Object> getFieldOnMessageJava(byte[] fileDescriptorProto, String messageName, byte[] message, String fieldName)
            throws Exception {
        final var fdp = com.google.protobuf.DescriptorProtos.FileDescriptorProto.parseFrom(fileDescriptorProto);
        final var fd = com.google.protobuf.Descriptors.FileDescriptor.buildFrom(fdp, new com.google.protobuf.Descriptors.FileDescriptor[0]);
        final var msg = com.google.protobuf.DynamicMessage.parseFrom(fd.findMessageTypeByName(messageName), message);
        final Object value = com.apple.foundationdb.record.query.plan.cascades.values.MessageHelpers.getFieldOnMessage(msg, fieldName);
        return Map.of("isNull", value == null, "value", String.valueOf(value));
    }

    /**
     * Every key function the JVM's registry holds (FunctionKeyExpression.Registry
     * reads the same service loader): its name, the factory that registers it,
     * the bounds and column size of an instance, and what it returns for an
     * argument of nulls ("NULL" for Key.Evaluated.NULL, "null" for a plain null,
     * "value" for a non-null, "error: ..." when it throws).
     */
    @ConformanceStep("keyFunctionRegistryJava")
    public List<Map<String, Object>> keyFunctionRegistryJava() {
        final List<Map<String, Object>> out = new ArrayList<>();
        for (final var factory : java.util.ServiceLoader.load(com.apple.foundationdb.record.metadata.expressions.FunctionKeyExpression.Factory.class)) {
            for (final var builder : factory.getBuilders()) {
                final var fn = builder.build(com.apple.foundationdb.record.metadata.expressions.EmptyKeyExpression.EMPTY);
                String nullResult;
                try {
                    final Object[] nulls = new Object[fn.getMinArguments()];
                    final var result = fn.evaluateFunction(null, null,
                            com.apple.foundationdb.record.metadata.Key.Evaluated.concatenate(java.util.Arrays.asList(nulls)));
                    final Object first = result.get(0).values().get(0);
                    nullResult = first == com.apple.foundationdb.record.metadata.Key.Evaluated.NullStandin.NULL ? "NULL"
                            : first == null ? "null" : "value";
                } catch (RuntimeException ex) {
                    nullResult = "error: " + ex.getClass().getName();
                }
                final Map<String, Object> row = new HashMap<>();
                row.put("name", builder.getName());
                row.put("factory", factory.getClass().getName());
                row.put("min", fn.getMinArguments());
                row.put("max", fn.getMaxArguments());
                row.put("columnSize", fn.getColumnSize());
                row.put("nullResult", nullResult);
                out.add(row);
            }
        }
        return out;
    }

    /**
     * Java's FunctionKeyExpression.create (the in-code path) and fromProto (the
     * load path) over a serialized Function: each one's verdict, with the
     * exception's full class name and message.
     */
    @ConformanceStep("functionKeyExpressionVerdictsJava")
    public Map<String, Object> functionKeyExpressionVerdictsJava(byte[] functionProto) throws InvalidProtocolBufferException {
        final var function = com.apple.foundationdb.record.expressions.RecordKeyExpressionProto.Function.parseFrom(functionProto);
        final Map<String, Object> out = new HashMap<>();
        try {
            final var args = com.apple.foundationdb.record.metadata.expressions.KeyExpression.fromProto(function.getArguments());
            com.apple.foundationdb.record.metadata.expressions.FunctionKeyExpression.create(function.getName(), args);
            out.put("createClass", "");
            out.put("createError", "");
        } catch (RuntimeException ex) {
            out.put("createClass", ex.getClass().getName());
            out.put("createError", String.valueOf(ex.getMessage()));
        }
        try {
            com.apple.foundationdb.record.metadata.expressions.FunctionKeyExpression.fromProto(function);
            out.put("loadClass", "");
            out.put("loadError", "");
        } catch (RuntimeException ex) {
            out.put("loadClass", ex.getClass().getName());
            out.put("loadError", String.valueOf(ex.getMessage()));
        }
        return out;
    }

    /**
     * Java's KeyExpression.fromProto over a serialized KeyExpression parsed
     * PARTIALLY, so a proto2 required child may be absent as it can be in
     * memory: the verdict with the exception's full class name and message.
     */
    @ConformanceStep("keyExpressionFromPartialProtoJava")
    public Map<String, Object> keyExpressionFromPartialProtoJava(byte[] expression) throws InvalidProtocolBufferException {
        final var proto = com.apple.foundationdb.record.expressions.RecordKeyExpressionProto.KeyExpression.parser().parsePartialFrom(expression);
        try {
            KeyExpression.fromProto(proto);
            return Map.of("class", "", "error", "");
        } catch (RuntimeException ex) {
            return Map.of("class", ex.getClass().getName(), "error", String.valueOf(ex.getMessage()));
        }
    }

    @ConformanceStep("buildMetaDataAnyVerdict")
    public Map<String, Object> buildMetaDataAnyVerdict(byte[] protoBytes) throws InvalidProtocolBufferException {
        final var proto = RecordMetaDataProto.MetaData.parseFrom(protoBytes, EXTENSION_REGISTRY);
        try {
            RecordMetaData.build(proto);
            return Map.of("valid", true, "error", "", "class", "", "causeClass", "", "causeError", "");
        } catch (RuntimeException ex) {
            // The exception's direct cause, when it has one (MetaDataException("incorrect
            // index options", cause) carries the parse failure there).
            final Throwable cause = ex.getCause();
            return Map.of("valid", false, "error", String.valueOf(ex.getMessage()), "class", ex.getClass().getName(),
                    "causeClass", cause == null ? "" : cause.getClass().getName(),
                    "causeError", cause == null ? "" : String.valueOf(cause.getMessage()));
        }
    }

    /**
     * Builds meta-data from proto bytes (Java's MetaDataValidator runs) and returns the HNSW
     * configuration Java reads from the named VECTOR index's options.
     */
    @ConformanceStep("vectorIndexConfigJava")
    public Map<String, Object> vectorIndexConfigJava(byte[] protoBytes, String indexName) throws InvalidProtocolBufferException {
        final var proto = RecordMetaDataProto.MetaData.parseFrom(protoBytes, EXTENSION_REGISTRY);
        final RecordMetaData md = RecordMetaData.build(proto);
        return com.apple.foundationdb.record.provider.foundationdb.indexes.HnswConformanceAccess.config(md.getIndex(indexName));
    }

    /**
     * Build meta-data from proto bytes, which runs Java's MetaDataValidator.
     * MetaDataException is the verdict; any other exception fails the step.
     */
    @ConformanceStep("buildMetaDataVerdict")
    public Map<String, Object> buildMetaDataVerdict(byte[] protoBytes) throws InvalidProtocolBufferException {
        final var proto = RecordMetaDataProto.MetaData.parseFrom(protoBytes, EXTENSION_REGISTRY);
        try {
            RecordMetaData.build(proto);
            return Map.of("valid", true, "error", "");
        } catch (com.apple.foundationdb.record.metadata.MetaDataException ex) {
            return Map.of("valid", false, "error", ex.getMessage());
        }
    }

    /**
     * Read one serialized RecordMetaDataProto.FormerIndex as Java's FormerIndex(proto)
     * does: "OK" and the subspace key, or "ERROR", the exception class and its message.
     */
    @ConformanceStep("formerIndexFromProtoVerdict")
    public Map<String, Object> formerIndexFromProtoVerdict(byte[] formerIndexProto) throws InvalidProtocolBufferException {
        final var proto = RecordMetaDataProto.FormerIndex.parseFrom(formerIndexProto, EXTENSION_REGISTRY);
        try {
            final var formerIndex = new com.apple.foundationdb.record.metadata.FormerIndex(proto);
            return Map.of("outcome", "OK " + formerIndex.getSubspaceKey());
        } catch (RuntimeException e) {
            return Map.of("outcome", "ERROR " + e.getClass().getName() + " " + e.getMessage());
        }
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
            case "with_explicit_type_key":
                builder.getRecordType("Order").setRecordTypeKey(42L);
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
            // Options and predicate ride the comparison too: UNIQUE lives in
            // the options map (IndexOptions.UNIQUE_OPTION), and a sparse
            // index's WHERE lives in the predicate — omitting either lets an
            // index that silently dropped them compare equal.
            idxMap.put("options", new java.util.TreeMap<>(idx.getOptions()));
            if (idx.hasPredicate()) {
                try {
                    idxMap.put("predicate", JsonFormat.printer()
                        .omittingInsignificantWhitespace()
                        .print(java.util.Objects.requireNonNull(idx.getPredicate()).toProto()));
                } catch (com.google.protobuf.InvalidProtocolBufferException e) {
                    idxMap.put("predicate", String.valueOf(idx.getPredicate()));
                }
            }
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
