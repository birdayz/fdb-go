package com.apple.foundationdb.async.guardiann;

import com.apple.foundationdb.KeyValue;
import com.apple.foundationdb.Transaction;
import com.apple.foundationdb.subspace.Subspace;
import com.apple.foundationdb.tuple.Tuple;

import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.UUID;

/**
 * Test-only accessor for package-private GuardiANN production types, used by the
 * conformance oracle. It calls the real production classes; it does not
 * reimplement any algorithm.
 */
public final class GuardiannConformanceAccess {
    private GuardiannConformanceAccess() {
    }

    /** The hash of a real VectorId, as the target's HashMaps see it. */
    public static int vectorIdHash(final Tuple primaryKey, final UUID uuid) {
        return new VectorId(primaryKey, uuid).hashCode();
    }

    /** Whether the structure's access info is trained for RaBitQ (Java AccessInfo.canUseRaBitQ). */
    public static boolean trained(final Guardiann guardiann, final Transaction tr) {
        final AccessInfo accessInfo = guardiann.getLocator().primitives().fetchAccessInfo(tr).join();
        return accessInfo != null && accessInfo.canUseRaBitQ();
    }

    /** {@link #primaryCounts(Guardiann, Transaction)} for a structure rooted at {@code root}. */
    public static List<Map<String, Object>> primaryCountsAt(final Subspace root, final Transaction tr) {
        return primaryCounts(new Guardiann(root, java.util.concurrent.ForkJoinPool.commonPool(),
                Guardiann.defaultConfig(1), OnWriteListener.NOOP, OnReadListener.NOOP), tr);
    }

    /**
     * Oracle staging: rewrites one cluster's state bits with the production metadata writer, the way a
     * concurrent state change would, so an already-queued task becomes obsolete. Returns the new code.
     */
    public static int setStates(final Guardiann guardiann, final Transaction tr, final int clusterIndex,
                                final int statesCode) {
        final Primitives primitives = guardiann.getLocator().primitives();
        final UUID clusterId = clusterIds(guardiann, tr).get(clusterIndex);
        final ClusterMetadata clusterMetadata = primitives.fetchClusterMetadata(tr, clusterId).join();
        primitives.writeClusterMetadata(tr, clusterMetadata.withNewStates(ClusterMetadata.State.ofCode(statesCode)));
        return statesCode;
    }

    /** Cluster ids in metadata key order. */
    public static List<UUID> clusterIds(final Guardiann guardiann, final Transaction tr) {
        final Subspace metadata = guardiann.getLocator().getStorageAdapter().getClusterMetadataSubspace();
        final List<UUID> result = new ArrayList<>();
        for (final KeyValue kv : tr.getRange(metadata.range()).asList().join()) {
            result.add(metadata.unpack(kv.getKey()).getUUID(0));
        }
        return result;
    }

    /** The queued tasks in key order, decoded by the production decoder: kind, priority and target count. */
    public static List<String> tasks(final Guardiann guardiann, final Transaction tr) {
        final Locator locator = guardiann.getLocator();
        final AccessInfo accessInfo = locator.primitives().fetchAccessInfo(tr).join();
        final Subspace tasks = locator.getStorageAdapter().getTasksSubspace();
        final List<String> result = new ArrayList<>();
        for (final KeyValue kv : tr.getRange(tasks.range()).asList().join()) {
            final Tuple key = tasks.unpack(kv.getKey());
            final Tuple value = Tuple.fromBytes(kv.getValue());
            final String prefix = TaskKind.fromValueTuple(value) + "/"
                    + (AbstractDeferredTask.isNormalPriority(key.getUUID(0)) ? "NORMAL" : "HIGH") + "/";
            String targets;
            try {
                targets = String.valueOf(AbstractDeferredTask.newFromTuples(locator, accessInfo, key, value)
                        .getTargetClusterIds().size());
            } catch (RuntimeException e) {
                // Decoding needs the current quantizer, which an unsupported encoding refuses.
                targets = "undecodable:" + e.getClass().getSimpleName();
            }
            result.add(prefix + targets);
        }
        return result;
    }

    /** Every cluster's persisted metadata, decoded by the production decoder, in key order. */
    public static List<Map<String, Object>> primaryCounts(final Guardiann guardiann, final Transaction tr) {
        final Subspace metadata = guardiann.getLocator().getStorageAdapter().getClusterMetadataSubspace();
        final List<Map<String, Object>> result = new ArrayList<>();
        for (final KeyValue kv : tr.getRange(metadata.range()).asList().join()) {
            final UUID clusterId = metadata.unpack(kv.getKey()).getUUID(0);
            final ClusterMetadata clusterMetadata =
                    StorageAdapter.clusterMetadataFromTuple(clusterId, Tuple.fromBytes(kv.getValue()));
            final Map<String, Object> row = new LinkedHashMap<>();
            row.put("primaries", clusterMetadata.getNumPrimaryVectors());
            row.put("states", clusterMetadata.getStatesCode());
            row.put("peak", clusterMetadata.maxEverNumPrimaryVectors());
            result.add(row);
        }
        return result;
    }
}
