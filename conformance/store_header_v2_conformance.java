package com.birdayz.conformance;

import com.apple.foundationdb.record.RecordMetaDataProto;
import com.apple.foundationdb.record.RecordLayerDemo;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.subspace.Subspace;
import com.apple.foundationdb.tuple.Tuple;
import com.google.protobuf.ByteString;

import java.util.ArrayList;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

class StoreHeaderV2Steps extends ConformanceBase {

    @ConformanceStep("getStoreHeaderV2Raw")
    public Map<String, Object> getStoreHeaderV2Raw(String clusterFile, byte[] subspace, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            Subspace sub = new Subspace(subspace);
            byte[] headerKey = sub.pack(Tuple.from(0L));
            byte[] headerBytes = context.ensureActive().get(headerKey).join();
            if (headerBytes == null) {
                throw new RuntimeException("Store header not found at subspace key 0");
            }
            RecordMetaDataProto.DataStoreInfo storeInfo;
            try {
                storeInfo = RecordMetaDataProto.DataStoreInfo.parseFrom(headerBytes);
            } catch (com.google.protobuf.InvalidProtocolBufferException e) {
                throw new RuntimeException("Failed to parse store header proto: " + e.getMessage(), e);
            }

            Map<String, Object> result = new HashMap<>();
            result.put("formatVersion", storeInfo.getFormatVersion());
            result.put("incarnation", storeInfo.hasIncarnation() ? storeInfo.getIncarnation() : 0);

            List<Map<String, Object>> userFields = new ArrayList<>();
            for (RecordMetaDataProto.DataStoreInfo.UserFieldEntry entry : storeInfo.getUserFieldList()) {
                Map<String, Object> field = new HashMap<>();
                field.put("key", entry.getKey());
                byte[] bytes = entry.getValue().toByteArray();
                int[] intArray = new int[bytes.length];
                for (int i = 0; i < bytes.length; i++) {
                    intArray[i] = bytes[i] & 0xFF;
                }
                field.put("value", intArray);
                userFields.add(field);
            }
            result.put("userFields", userFields);

            result.put("hasLockState", storeInfo.hasStoreLockState());
            if (storeInfo.hasStoreLockState()) {
                RecordMetaDataProto.DataStoreInfo.StoreLockState lockState = storeInfo.getStoreLockState();
                if (lockState.hasLockState()) {
                    result.put("lockState", lockState.getLockState().name());
                }
                if (lockState.hasReason()) {
                    result.put("lockReason", lockState.getReason());
                }
            }

            return result;
        });
    }

    @ConformanceStep("setHeaderUserFieldJava")
    public void setHeaderUserFieldJava(String clusterFile, byte[] subspace, String fieldKey, byte[] fieldValue, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();
            store.setHeaderUserField(fieldKey, fieldValue);
            return null;
        });
    }

    @ConformanceStep("setIncarnationJava")
    public void setIncarnationJava(String clusterFile, byte[] subspace, int incarnation, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();
            store.updateIncarnation(current -> incarnation).join();
            return null;
        });
    }

    @ConformanceStep("setStoreLockStateJava")
    public void setStoreLockStateJava(String clusterFile, byte[] subspace, String lockState, String reason, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();
            RecordMetaDataProto.DataStoreInfo.StoreLockState.State state =
                RecordMetaDataProto.DataStoreInfo.StoreLockState.State.valueOf(lockState);
            store.setStoreLockStateAsync(state, reason).join();
            return null;
        });
    }

    @ConformanceStep("clearStoreLockStateJava")
    public void clearStoreLockStateJava(String clusterFile, byte[] subspace, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();
            store.clearStoreLockStateAsync().join();
            return null;
        });
    }

    @ConformanceStep("openLockedStoreJava")
    public Map<String, Object> openLockedStoreJava(String clusterFile, byte[] subspace, String bypassReason, String tenantName) {
        try {
            runInContext(clusterFile, tenantName, context -> {
                FDBRecordStore.Builder builder = FDBRecordStore.newBuilder()
                    .setMetaDataProvider(createMetaData())
                    .setContext(context)
                    .setSubspace(new Subspace(subspace))
                    .setUserVersionChecker(ALWAYS_READABLE_CHECKER);
                if (bypassReason != null && !bypassReason.isEmpty()) {
                    builder.setBypassFullStoreLockReason(bypassReason);
                }
                builder.createOrOpen();
                return null;
            });
            Map<String, Object> result = new HashMap<>();
            result.put("success", true);
            result.put("error", "");
            return result;
        } catch (Exception e) {
            Map<String, Object> result = new HashMap<>();
            result.put("success", false);
            result.put("error", e.getMessage() != null ? e.getMessage() : e.getClass().getName());
            return result;
        }
    }

    @ConformanceStep("saveOrderLockedJava")
    public Map<String, Object> saveOrderLockedJava(String clusterFile, byte[] subspace, RecordLayerDemo.Order order, String tenantName) {
        try {
            runInContext(clusterFile, tenantName, context -> {
                FDBRecordStore store = FDBRecordStore.newBuilder()
                    .setMetaDataProvider(createMetaData())
                    .setContext(context)
                    .setSubspace(new Subspace(subspace))
                    .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                    .createOrOpen();
                store.saveRecord(order);
                return null;
            });
            Map<String, Object> result = new HashMap<>();
            result.put("success", true);
            result.put("error", "");
            return result;
        } catch (Exception e) {
            Map<String, Object> result = new HashMap<>();
            result.put("success", false);
            result.put("error", e.getMessage() != null ? e.getMessage() : e.getClass().getName());
            return result;
        }
    }
}
