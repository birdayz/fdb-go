package com.birdayz.conformance;

import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.subspace.Subspace;
import com.apple.foundationdb.tuple.Tuple;

class IndexStateSteps extends ConformanceBase {
    @ConformanceStep("probeQueuedResumeValidation")
    public java.util.Map<String, Object> probeQueuedResumeValidation(String clusterFile, boolean mutual, boolean queuedPrimary) {
        var metadataBuilder = com.apple.foundationdb.record.RecordMetaData.newBuilder()
            .setRecords(com.apple.foundationdb.record.RecordLayerDemo.getDescriptor());
        metadataBuilder.getRecordType("Order").setPrimaryKey(com.apple.foundationdb.record.metadata.Key.Expressions.field("order_id"));
        metadataBuilder.getRecordType("Customer").setPrimaryKey(com.apple.foundationdb.record.metadata.Key.Expressions.field("customer_id"));
        metadataBuilder.getRecordType("TypedRecord").setPrimaryKey(com.apple.foundationdb.record.metadata.Key.Expressions.field("id"));
        metadataBuilder.addIndex("Order", new com.apple.foundationdb.record.metadata.Index("A", com.apple.foundationdb.record.metadata.Key.Expressions.field("price")));
        metadataBuilder.addIndex("Order", new com.apple.foundationdb.record.metadata.Index("Z", com.apple.foundationdb.record.metadata.Key.Expressions.field("quantity")));
        var metadata = metadataBuilder.build();
        // Indexer's runner opens independent contexts. Use its real non-tenant
        // database and a unique test-owned prefix, cleaned on every exit.
        var space = new Subspace(Tuple.from("queued-policy-probe", java.util.UUID.randomUUID().toString()));
        try {
            runInContext(clusterFile, "", context -> {
                var store = FDBRecordStore.newBuilder().setMetaDataProvider(metadata).setContext(context).setSubspace(space).create();
                store.markIndexWriteOnlyWithQueue(queuedPrimary ? "A" : "Z").join();
                store.markIndexWriteOnly(queuedPrimary ? "Z" : "A").join();
                return null;
            });
            var policy = com.apple.foundationdb.record.provider.foundationdb.OnlineIndexer.IndexingPolicy.newBuilder().setMutualIndexing(mutual).build();
            var targets = mutual ? java.util.List.of(metadata.getIndex("A")) : java.util.List.of(metadata.getIndex("A"), metadata.getIndex("Z"));
            try (var indexer = com.apple.foundationdb.record.provider.foundationdb.OnlineIndexer.newBuilder()
                    .setDatabase(createDatabase(clusterFile)).setMetaData(metadata).setSubspace(space)
                    .setTargetIndexes(targets).setIndexingPolicy(policy).build()) {
                indexer.buildIndex(false);
                return java.util.Map.of("error", "", "exception", "");
            } catch (RuntimeException ex) {
                Throwable cause = ex;
                while (cause.getCause() != null) { cause = cause.getCause(); }
                return java.util.Map.of("error", cause.getMessage(), "exception", cause.getClass().getSimpleName());
            }
        } finally {
            runInContext(clusterFile, "", context -> { context.ensureActive().clear(space.range()); return null; });
        }
    }

    @ConformanceStep("uncheckedJavaQueuedSetter")
    public java.util.Map<String, Object> uncheckedJavaQueuedSetter(String clusterFile, byte[] subspace, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            var store = FDBRecordStore.newBuilder().setMetaDataProvider(createIndexedMetaData())
                .setContext(context).setSubspace(new Subspace(subspace)).setFormatVersion(14).open();
            var index = store.getRecordMetaData().getIndex("Order$price");
            boolean changed = store.markIndexWriteOnlyWithQueue(index).join();
            return java.util.Map.of("changed", changed, "format", store.getFormatVersion(),
                "capable", store.getIndexMaintainer(index).isPendingWriteQueueAllowed(),
                "state", store.getIndexState(index).name());
        });
    }

    @ConformanceStep("writeOnlyBuiltCoverage")
    public java.util.Map<String, Object> writeOnlyBuiltCoverage(String clusterFile, byte[] subspace, String tenantName, boolean transition) {
        return runInContext(clusterFile, tenantName, context -> {
            var store = FDBRecordStore.newBuilder().setMetaDataProvider(createIndexedMetaData())
                .setContext(context).setSubspace(new Subspace(subspace)).open();
            var index = store.getRecordMetaData().getIndex("Order$price");
            boolean changed = transition && store.markIndexWriteOnly(index).join();
            return java.util.Map.of("changed", changed,
                "complete", store.firstUnbuiltRange(index).join().isEmpty(),
                "state", store.getIndexState(index).name());
        });
    }

    @ConformanceStep("indexingHeartbeatInterop")
    public java.util.Map<String, Object> indexingHeartbeatInterop(String clusterFile, byte[] subspace, String tenantName, String id, String action) {
        return runInContext(clusterFile, tenantName, context -> {
            var store = FDBRecordStore.newBuilder().setMetaDataProvider(createIndexedMetaData())
                .setContext(context).setSubspace(new Subspace(subspace)).open();
            var index = store.getRecordMetaData().getIndex("Order$price");
            var heartbeat = new com.apple.foundationdb.record.provider.foundationdb.indexing.IndexingHeartbeat(
                java.util.UUID.fromString(id), "JAVA_PEER", 60000, action.equals("seed"));
            switch (action) {
                case "seed":
                case "check":
                    heartbeat.checkAndUpdateHeartbeat(store, index).join();
                    break;
                case "clear":
                    heartbeat.clearHeartbeat(store, index);
                    break;
                default:
                    throw new IllegalArgumentException("unknown heartbeat action " + action);
            }
            var heartbeats = com.apple.foundationdb.record.provider.foundationdb.indexing.IndexingHeartbeat.getIndexingHeartbeats(store, index, 0).join();
            return java.util.Map.of("ids", heartbeats.keySet().stream().map(java.util.UUID::toString).sorted().toList());
        });
    }

    /**
     * Java's heartbeat administration on the heartbeats of Order$price: "read" returns
     * getIndexingHeartbeats(maxCount) and the static checkAnyOngoingOnlineIndexBuildsAsync
     * (default lease); "clear" runs clearIndexingHeartbeats(minAgeMs, maxCount) and reports the
     * count and what is left; "ongoing" runs checkAnyOngoingOnlineIndexBuildsAsync ALONE, so a
     * failure is its own and not getIndexingHeartbeats'.
     */
    @ConformanceStep("indexingHeartbeatAdmin")
    public java.util.Map<String, Object> indexingHeartbeatAdmin(String clusterFile, byte[] subspace, String tenantName,
                                                               String action, long minAgeMs, int maxCount) {
        return runInContext(clusterFile, tenantName, context -> {
            var store = FDBRecordStore.newBuilder().setMetaDataProvider(createIndexedMetaData())
                .setContext(context).setSubspace(new Subspace(subspace)).open();
            var index = store.getRecordMetaData().getIndex("Order$price");
            var result = new java.util.LinkedHashMap<String, Object>();
            if (action.equals("ongoing")) {
                result.put("ongoing", com.apple.foundationdb.record.provider.foundationdb.OnlineIndexer
                        .checkAnyOngoingOnlineIndexBuildsAsync(store, index).join());
                return result;
            }
            if (action.equals("clear")) {
                result.put("cleared", com.apple.foundationdb.record.provider.foundationdb.indexing.IndexingHeartbeat
                        .clearIndexingHeartbeats(store, index, minAgeMs, maxCount).join());
            } else if (!action.equals("read")) {
                throw new IllegalArgumentException("unknown heartbeat admin action " + action);
            }
            var heartbeats = com.apple.foundationdb.record.provider.foundationdb.indexing.IndexingHeartbeat
                    .getIndexingHeartbeats(store, index, action.equals("read") ? maxCount : 0).join();
            var rows = new java.util.TreeMap<String, Object>();
            heartbeats.forEach((id, hb) -> rows.put(id.toString(), java.util.Map.of(
                    "info", hb.getInfo(), "heartbeatTime", hb.getHeartbeatTimeMilliseconds())));
            result.put("heartbeats", rows);
            result.put("ongoing", com.apple.foundationdb.record.provider.foundationdb.OnlineIndexer
                    .checkAnyOngoingOnlineIndexBuildsAsync(store, index).join());
            return result;
        });
    }

    @ConformanceStep("probeOverlappingUniqueViolations")
    public java.util.Map<String, Object> probeOverlappingUniqueViolations(String clusterFile, byte[] subspace, String tenantName, boolean seed) {
        final var builder = com.apple.foundationdb.record.RecordMetaData.newBuilder()
                .setRecords(com.apple.foundationdb.record.RecordLayerDemo.getDescriptor());
        builder.getRecordType("Order").setPrimaryKey(com.apple.foundationdb.record.metadata.Key.Expressions.concat(
                com.apple.foundationdb.record.metadata.Key.Expressions.field("price"),
                com.apple.foundationdb.record.metadata.Key.Expressions.field("order_id")));
        builder.getRecordType("Customer").setPrimaryKey(com.apple.foundationdb.record.metadata.Key.Expressions.field("customer_id"));
        builder.getRecordType("TypedRecord").setPrimaryKey(com.apple.foundationdb.record.metadata.Key.Expressions.field("id"));
        final var index = new com.apple.foundationdb.record.metadata.Index("price",
                com.apple.foundationdb.record.metadata.Key.Expressions.field("price"),
                com.apple.foundationdb.record.metadata.IndexTypes.VALUE,
                java.util.Map.of(com.apple.foundationdb.record.metadata.IndexOptions.UNIQUE_OPTION, "true"));
        builder.addIndex("Order", index);
        final var metadata = builder.build();
        if (seed) {
            // Java's uniqueness futures finish at precommit. Inspect committed
            // violations, not a partially populated asynchronous write set.
            runInContext(clusterFile, tenantName, context -> {
                final var store = FDBRecordStore.newBuilder().setMetaDataProvider(metadata)
                        .setContext(context).setSubspace(new Subspace(subspace)).setFormatVersion(14).createOrOpen();
                store.markIndexWriteOnly("price").join();
                for (int id = 1; id <= 5; id++) {
                    store.saveRecord(com.apple.foundationdb.record.RecordLayerDemo.Order.newBuilder()
                            .setOrderId(id).setPrice(id <= 3 ? 100 : 200).build());
                }
                return null;
            });
        }
        return runInContext(clusterFile, tenantName, context -> {
            final var store = FDBRecordStore.newBuilder().setMetaDataProvider(metadata)
                    .setContext(context).setSubspace(new Subspace(subspace)).setFormatVersion(14).open();
            final var violations = store.indexUniquenessViolationsSubspace(index);
            final var rows = context.ensureActive().getRange(violations.range()).asList().join();
            final var keys = new java.util.ArrayList<String>();
            final var values = new java.util.ArrayList<String>();
            for (var row : rows) {
                keys.add(java.util.HexFormat.of().formatHex(violations.unpack(row.getKey()).pack()));
                values.add(java.util.HexFormat.of().formatHex(row.getValue()));
            }
            final var counts = new java.util.ArrayList<Integer>();
            counts.add(rows.size());
            store.deleteRecord(Tuple.from(100L, 3L));
            counts.add(context.ensureActive().getRange(violations.range()).asList().join().size());
            store.deleteRecord(Tuple.from(100L, 2L));
            counts.add(context.ensureActive().getRange(violations.range()).asList().join().size());
            return java.util.Map.of("keys", keys, "values", values, "counts", counts);
        });
    }

    @ConformanceStep("probeDeleteWithPendingReplacementRetirement")
    public java.util.Map<String, Object> probeDeleteWithPendingReplacementRetirement(String clusterFile, byte[] subspace, String tenantName) {
        final Subspace sub = new Subspace(subspace);
        final var builder = com.apple.foundationdb.record.RecordMetaData.newBuilder()
                .setRecords(com.apple.foundationdb.record.RecordLayerDemo.getDescriptor());
        builder.getRecordType("Order").setPrimaryKey(com.apple.foundationdb.record.metadata.Key.Expressions.field("order_id"));
        builder.getRecordType("Customer").setPrimaryKey(com.apple.foundationdb.record.metadata.Key.Expressions.field("customer_id"));
        builder.getRecordType("TypedRecord").setPrimaryKey(com.apple.foundationdb.record.metadata.Key.Expressions.field("id"));
        final var original = new com.apple.foundationdb.record.metadata.Index("original",
                com.apple.foundationdb.record.metadata.Key.Expressions.field("price"),
                com.apple.foundationdb.record.metadata.IndexTypes.VALUE,
                java.util.Map.of(com.apple.foundationdb.record.metadata.IndexOptions.REPLACED_BY_OPTION_PREFIX + "0", "replacement"));
        builder.addIndex("Order", original);
        builder.addIndex("Order", new com.apple.foundationdb.record.metadata.Index("replacement",
                com.apple.foundationdb.record.metadata.Key.Expressions.field("price")));
        final var metadata = builder.build();
        runInContext(clusterFile, tenantName, context -> {
            final var store = FDBRecordStore.newBuilder().setMetaDataProvider(metadata)
                    .setContext(context).setSubspace(sub).createOrOpen();
            store.markIndexWriteOnly("replacement").join();
            store.rebuildIndex(original).join();
            store.rebuildIndex(metadata.getIndex("replacement")).join();
            FDBRecordStore.deleteStoreAsync(context, sub).join();
            return null;
        });
        return runInContext(clusterFile, tenantName, context -> {
            final var rows = context.ensureActive().getRange(sub.range()).asList().join();
            final byte[] header = context.ensureActive().get(sub.pack(Tuple.from(0L))).join();
            final byte[] state = context.ensureActive().get(sub.pack(Tuple.from(5L, "original"))).join();
            return java.util.Map.of("remainingRows", rows.size(), "headerPresent", header != null,
                    "originalState", state == null ? -1L : Tuple.fromBytes(state).getLong(0));
        });
    }

    @ConformanceStep("markIndexWriteOnly")
    public void markIndexWriteOnly(String clusterFile, byte[] subspace, String indexName, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createIndexedMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();
            store.markIndexWriteOnly(indexName).join();
            return null;
        });
    }

    @ConformanceStep("markIndexDisabled")
    public void markIndexDisabled(String clusterFile, byte[] subspace, String indexName, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createIndexedMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();
            store.markIndexDisabled(indexName).join();
            return null;
        });
    }

    @ConformanceStep("markIndexReadable")
    public void markIndexReadable(String clusterFile, byte[] subspace, String indexName, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createIndexedMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();
            store.markIndexReadable(indexName).join();
            return null;
        });
    }

    @ConformanceStep("getIndexStateRaw")
    public String getIndexStateRaw(String clusterFile, byte[] subspace, String indexName, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            Subspace sub = new Subspace(subspace);
            Subspace isSubspace = sub.get(5L);
            byte[] stateKey = isSubspace.pack(Tuple.from(indexName));
            byte[] stateBytes = context.ensureActive().get(stateKey).join();
            if (stateBytes == null) {
                return "READABLE";
            }
            long code = Tuple.fromBytes(stateBytes).getLong(0);
            switch ((int)code) {
                case 0: return "READABLE";
                case 1: return "WRITE_ONLY";
                case 2: return "DISABLED";
                case 3: return "READABLE_UNIQUE_PENDING";
                default: return "UNKNOWN(" + code + ")";
            }
        });
    }
}
