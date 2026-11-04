package com.birdayz.conformance;

import com.apple.foundationdb.KeyValue;
import com.apple.foundationdb.record.RecordMetaData;
import com.apple.foundationdb.record.RecordMetaDataOptionsProto;
import com.apple.foundationdb.record.RecordMetaDataProto;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStoreBase;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordVersion;
import com.apple.foundationdb.subspace.Subspace;
import com.apple.foundationdb.tuple.Tuple;
import com.google.protobuf.Descriptors;
import com.google.protobuf.DynamicMessage;
import com.google.protobuf.ExtensionRegistry;
import com.google.protobuf.InvalidProtocolBufferException;

import java.util.ArrayList;
import java.util.Arrays;
import java.util.HashMap;
import java.util.HexFormat;
import java.util.List;
import java.util.Map;

class VersionSaveSteps extends ConformanceBase {
    private static final ExtensionRegistry EXTENSION_REGISTRY;
    static {
        EXTENSION_REGISTRY = ExtensionRegistry.newInstance();
        RecordMetaDataOptionsProto.registerAllExtensions(EXTENSION_REGISTRY);
    }

    @ConformanceStep("legacyExplicitVersionDelete")
    public Map<String, Object> legacyExplicitVersionDelete(String clusterFile, byte[] subspace, byte[] metaData,
                                                           byte[] record, String deleteKind, boolean seed)
            throws InvalidProtocolBufferException {
        final RecordMetaData md = RecordMetaData.build(RecordMetaDataProto.MetaData.parseFrom(metaData, EXTENSION_REGISTRY));
        final DynamicMessage message = DynamicMessage.parseFrom(md.getRecordType("Order").getDescriptor(), record);
        final Subspace ss = new Subspace(subspace);
        final byte[] oldVersion = new byte[12];
        oldVersion[0] = 1;
        oldVersion[11] = 9;
        if (seed) {
            runInContext(clusterFile, null, context -> {
                FDBRecordStore store = FDBRecordStore.newBuilder().setMetaDataProvider(md).setContext(context)
                        .setSubspace(ss).setFormatVersion(5).setUserVersionChecker(ALWAYS_READABLE_CHECKER).createOrOpen();
                store.saveRecord(message, FDBRecordStoreBase.RecordExistenceCheck.NONE, FDBRecordVersion.complete(oldVersion),
                        FDBRecordStoreBase.VersionstampSaveBehavior.IF_PRESENT);
                return null;
            });
        }
        runInContext(clusterFile, null, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder().setMetaDataProvider(md).setContext(context)
                    .setSubspace(ss).setFormatVersion(5).setUserVersionChecker(ALWAYS_READABLE_CHECKER).createOrOpen();
            store.saveRecord(message, FDBRecordStoreBase.RecordExistenceCheck.NONE, FDBRecordVersion.incomplete(1),
                    FDBRecordStoreBase.VersionstampSaveBehavior.IF_PRESENT);
            if ("point".equals(deleteKind)) {
                if (!store.deleteRecord(Tuple.from(md.getRecordType("Order").getRecordTypeKey(), 1L, 7L))) { throw new IllegalStateException("saved record absent"); }
            } else if ("where".equals(deleteKind)) {
                store.deleteRecordsWhere("Order", com.apple.foundationdb.record.query.expressions.Query.field("order_id").equalsValue(1L));
            } else { throw new IllegalArgumentException(deleteKind); }
            return null;
        });
        return runInContext(clusterFile, null, context -> {
            Map<String, Object> result = new HashMap<>();
            result.put("recordRows", context.ensureActive().getRange(ss.subspace(Tuple.from(1L)).range()).asList().join().size());
            List<String> versions = new ArrayList<>();
            List<String> versionKeys = new ArrayList<>();
            for (KeyValue kv : context.ensureActive().getRange(ss.subspace(Tuple.from(8L)).range()).asList().join()) {
                versionKeys.add(HexFormat.of().formatHex(kv.getKey()));
                versions.add(HexFormat.of().formatHex(kv.getValue()));
            }
            FDBRecordStore store = FDBRecordStore.newBuilder().setMetaDataProvider(md).setContext(context)
                    .setSubspace(ss).setFormatVersion(5).setUserVersionChecker(ALWAYS_READABLE_CHECKER).open();
            result.put("formatVersion", store.getFormatVersion());
            result.put("storeRecordVersions", md.isStoreRecordVersions());
            result.put("versionKeys", versionKeys);
            result.put("versions", versions);
            return result;
        });
    }

    /** Saves each record, one transaction each, with its version (empty: none) and behavior; dumps spaces 1, 2, 4 and 8. */
    @ConformanceStep("saveWithVersionsJava")
    public Map<String, Object> saveWithVersionsJava(String clusterFile, byte[] subspace, byte[] metaData, String recordTypeName,
                                                    byte[][] records, byte[][] versions, String[] behaviors, int formatVersion)
            throws InvalidProtocolBufferException {
        final RecordMetaData md = RecordMetaData.build(RecordMetaDataProto.MetaData.parseFrom(metaData, EXTENSION_REGISTRY));
        final Descriptors.Descriptor descriptor = md.getRecordType(recordTypeName).getDescriptor();
        final Subspace ss = new Subspace(subspace);
        final List<String> verdicts = new ArrayList<>();
        for (int i = 0; i < records.length; i++) {
            final DynamicMessage record = DynamicMessage.parseFrom(descriptor, records[i]);
            final FDBRecordVersion version = versions[i] == null || versions[i].length == 0 ? null : FDBRecordVersion.complete(versions[i]);
            final FDBRecordStoreBase.VersionstampSaveBehavior behavior = FDBRecordStoreBase.VersionstampSaveBehavior.valueOf(behaviors[i]);
            try {
                runInContext(clusterFile, null, context -> {
                    final FDBRecordStore.Builder builder = FDBRecordStore.newBuilder().setMetaDataProvider(md)
                            .setContext(context).setSubspace(ss).setUserVersionChecker(ALWAYS_READABLE_CHECKER);
                    if (formatVersion > 0) {
                        builder.setFormatVersion(formatVersion);
                    }
                    builder.createOrOpen().saveRecord(record, FDBRecordStoreBase.RecordExistenceCheck.NONE, version, behavior);
                    return null;
                });
                verdicts.add("ok");
            } catch (RuntimeException ex) {
                Throwable cause = ex;
                while (cause.getCause() != null && cause.getCause() != cause) {
                    cause = cause.getCause();
                }
                verdicts.add(cause.getClass().getName());
            }
        }
        final Map<String, Object> result = new HashMap<>();
        result.put("verdicts", verdicts);
        result.put("kvs", runInContext(clusterFile, null, context -> {
            final byte[] prefix = ss.getKey();
            final List<List<String>> out = new ArrayList<>();
            for (long space : new long[] {1L, 2L, 4L, 8L}) {
                for (KeyValue kv : context.ensureActive().getRange(ss.range(Tuple.from(space))).asList().join()) {
                    out.add(List.of(HexFormat.of().formatHex(Arrays.copyOfRange(kv.getKey(), prefix.length, kv.getKey().length)),
                            HexFormat.of().formatHex(kv.getValue())));
                }
            }
            return out;
        }));
        return result;
    }
}
