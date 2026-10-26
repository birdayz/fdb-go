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
