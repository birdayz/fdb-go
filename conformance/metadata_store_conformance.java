package com.birdayz.conformance;

import com.apple.foundationdb.record.RecordMetaData;
import com.apple.foundationdb.record.RecordMetaDataBuilder;
import com.apple.foundationdb.record.RecordMetaDataProto;
import com.apple.foundationdb.record.metadata.Key;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordContext;
import com.apple.foundationdb.record.provider.foundationdb.SplitHelper;
import com.apple.foundationdb.record.RecordLayerDemo;
import com.apple.foundationdb.subspace.Subspace;
import com.apple.foundationdb.tuple.Tuple;

import com.google.protobuf.InvalidProtocolBufferException;

import java.util.HashMap;
import java.util.Map;

/**
 * Conformance steps for FDBMetaDataStore cross-language validation.
 * Uses non-tenant mode (null tenantName → db.run()) with unique subspace
 * prefixes for isolation. This avoids tenant-related issues with direct
 * SplitHelper calls.
 */
class MetaDataStoreSteps extends ConformanceBase {

    private static RecordMetaData createTestMetaData() {
        RecordMetaDataBuilder b = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        b.getRecordType("Order").setPrimaryKey(Key.Expressions.field("order_id"));
        b.getRecordType("Customer").setPrimaryKey(Key.Expressions.field("customer_id"));
        b.getRecordType("TypedRecord").setPrimaryKey(Key.Expressions.field("id"));
        return b.build();
    }

    /**
     * Save metadata proto using Java's SplitHelper (matching FDBMetaDataStore wire format).
     * Uses non-tenant mode for reliable cross-language testing.
     */
    @ConformanceStep("saveMetaDataJava")
    public Map<String, Object> saveMetaDataJava(String clusterFile, byte[] subspace, int version) {
        return runInContext(clusterFile, null, context -> {
            Subspace ss = new Subspace(subspace);
            Tuple currentKey = Tuple.from((Object) null);
            RecordMetaDataProto.MetaData.Builder proto = createTestMetaData().toProto().toBuilder();
            proto.setVersion(version);
            byte[] serialized = proto.build().toByteArray();
            SplitHelper.saveWithSplit(context, ss, currentKey, serialized, null);
            Map<String, Object> result = new HashMap<>();
            result.put("savedBytes", serialized.length);
            return result;
        });
    }

    /**
     * Load metadata proto using raw FDB read at unsplit key and return version.
     * Reads at subspace.pack(null, 0) — the unsplit suffix key.
     */
    @ConformanceStep("loadMetaDataJava")
    public Map<String, Object> loadMetaDataJava(String clusterFile, byte[] subspace) {
        return runInContext(clusterFile, null, context -> {
            Subspace ss = new Subspace(subspace);
            // Read at unsplit key: subspace.pack(null, 0L)
            byte[] unsplitKey = ss.pack(Tuple.from((Object) null, 0L));
            byte[] data = context.ensureActive().get(unsplitKey).join();
            Map<String, Object> result = new HashMap<>();
            if (data != null && data.length > 0) {
                RecordMetaDataProto.MetaData proto;
                try {
                    proto = RecordMetaDataProto.MetaData.parseFrom(data);
                } catch (InvalidProtocolBufferException e) {
                    throw new RuntimeException("Failed to parse metadata proto", e);
                }
                result.put("version", proto.getVersion());
                result.put("found", true);
            } else {
                result.put("found", false);
            }
            return result;
        });
    }

    /**
     * Save historical metadata version using Java's SplitHelper.
     */
    @ConformanceStep("saveMetaDataHistoryJava")
    public void saveMetaDataHistoryJava(String clusterFile, byte[] subspace, int version) {
        runInContext(clusterFile, null, context -> {
            Subspace ss = new Subspace(subspace);
            Tuple historyKey = Tuple.from("H", (long) version);
            RecordMetaDataProto.MetaData.Builder proto = createTestMetaData().toProto().toBuilder();
            proto.setVersion(version);
            byte[] serialized = proto.build().toByteArray();
            SplitHelper.saveWithSplit(context, ss, historyKey, serialized, null);
            return null;
        });
    }

    /**
     * Load historical metadata version using raw FDB read at unsplit key.
     */
    @ConformanceStep("loadMetaDataHistoryJava")
    public Map<String, Object> loadMetaDataHistoryJava(String clusterFile, byte[] subspace, int version) {
        return runInContext(clusterFile, null, context -> {
            Subspace ss = new Subspace(subspace);
            // Read at unsplit key: subspace.pack("H", version, 0L)
            byte[] unsplitKey = ss.pack(Tuple.from("H", (long) version, 0L));
            byte[] data = context.ensureActive().get(unsplitKey).join();
            Map<String, Object> result = new HashMap<>();
            if (data != null && data.length > 0) {
                RecordMetaDataProto.MetaData proto;
                try {
                    proto = RecordMetaDataProto.MetaData.parseFrom(data);
                } catch (InvalidProtocolBufferException e) {
                    throw new RuntimeException("Failed to parse metadata proto", e);
                }
                result.put("version", proto.getVersion());
                result.put("found", true);
            } else {
                result.put("found", false);
            }
            return result;
        });
    }
}
