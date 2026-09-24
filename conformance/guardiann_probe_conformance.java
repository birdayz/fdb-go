package com.birdayz.conformance;

import com.apple.foundationdb.Transaction;
import com.apple.foundationdb.async.guardiann.Config;
import com.apple.foundationdb.async.guardiann.Guardiann;
import com.apple.foundationdb.async.guardiann.GuardiannConformanceAccess;
import com.apple.foundationdb.async.guardiann.OnReadListener;
import com.apple.foundationdb.async.guardiann.OnWriteListener;
import com.apple.foundationdb.linear.DoubleRealVector;
import com.apple.foundationdb.linear.Metric;
import com.apple.foundationdb.subspace.Subspace;
import com.apple.foundationdb.tuple.Tuple;

import java.util.ArrayList;
import java.util.HexFormat;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.UUID;
import java.util.concurrent.CompletionException;
import java.util.concurrent.ForkJoinPool;

/**
 * Live-JVM oracle steps for the RFC-257 WS-D GuardiANN port. They drive the real
 * 4.14.2.0 production classes and report what the target actually does; they do
 * not reimplement any GuardiANN algorithm.
 */
class GuardiannProbeSteps extends ConformanceBase {

    /**
     * Validates the VectorId hash formula the Go port must compute, against real
     * VectorId instances on the running JVM, and reports the runtime versions the
     * formula was validated on.
     */
    @ConformanceStep("guardiannVectorIdHashProbe")
    public Map<String, Object> guardiannVectorIdHashProbe(List<String> packedPrimaryKeysHex, List<String> uuids) {
        List<Map<String, Object>> rows = new ArrayList<>();
        for (int i = 0; i < packedPrimaryKeysHex.size(); i++) {
            Tuple primaryKey = Tuple.fromBytes(HexFormat.of().parseHex(packedPrimaryKeysHex.get(i)));
            UUID uuid = UUID.fromString(uuids.get(i));
            Map<String, Object> row = new LinkedHashMap<>();
            row.put("javaHash", GuardiannConformanceAccess.vectorIdHash(primaryKey, uuid));
            row.put("tupleHash", primaryKey.hashCode());
            row.put("uuidHash", uuid.hashCode());
            rows.add(row);
        }
        Map<String, Object> result = new LinkedHashMap<>();
        result.put("rows", rows);
        result.put("javaFeatureVersion", Runtime.version().feature());
        // fdb-java's manifest carries no version; its jar name is the only version record.
        result.put("fdbJavaJar", new java.io.File(
                Tuple.class.getProtectionDomain().getCodeSource().getLocation().getPath()).getName());
        result.put("fdbExtensionsVersion", Guardiann.class.getPackage().getImplementationVersion());
        return result;
    }

    private static Guardiann smallGuardiann(byte[] subspace, int primaryClusterMin) {
        Config config = Guardiann.newConfigBuilder()
                .setMetric(Metric.EUCLIDEAN_METRIC)
                .setPrimaryClusterMin(primaryClusterMin)
                .setPrimaryClusterMax(10)
                .setPrimaryClusterHardMax(40)
                .setCollapseMinDuplicates(5)
                .setDeterministicRandomness(true)
                .build(2);
        return new Guardiann(new Subspace(subspace), ForkJoinPool.commonPool(), config,
                OnWriteListener.NOOP, OnReadListener.NOOP);
    }

    private static double[] near(int id) {
        return new double[] {0.01 * id, 0.02 * (id % 3)};
    }

    private static double[] far(int offset) {
        return new double[] {100.0 + 0.01 * offset, 100.0};
    }

    // Knob-probe geometry: no zero vector, and the two groups differ in DIRECTION
    // as well as position, so every metric (cosine included) separates them.
    private static double[] knobNear(int id) {
        return new double[] {1.0 + 0.01 * id, 1.0 + 0.02 * (id % 3)};
    }

    private static double[] knobFar(int offset) {
        return new double[] {-100.0 - 0.01 * offset, 100.0};
    }

    private static void insert(String clusterFile, String tenantName, Guardiann guardiann, long id, double[] v) {
        runInContext(clusterFile, tenantName, context -> {
            guardiann.insert(context.ensureActive(), Tuple.from(id), new DoubleRealVector(v), null, false).join();
            return null;
        });
    }

    private static void delete(String clusterFile, String tenantName, Guardiann guardiann, long id, double[] v) {
        runInContext(clusterFile, tenantName, context -> {
            guardiann.delete(context.ensureActive(), Tuple.from(id), new DoubleRealVector(v), false).join();
            return null;
        });
    }

    /** Executes queued tasks one per transaction until none remain, or three failures. */
    private static List<Map<String, Object>> drainOneByOne(String clusterFile, String tenantName, Guardiann guardiann) {
        List<Map<String, Object>> executions = new ArrayList<>();
        int failures = 0;
        for (int round = 0; round < 40 && failures < 3; round++) {
            Map<String, Object> outcome = new LinkedHashMap<>();
            try {
                int executed = runInContext(clusterFile, tenantName, context ->
                        guardiann.executeDeferredTasks(context.ensureActive(), 1, Long.MAX_VALUE).join());
                outcome.put("executed", executed);
                executions.add(outcome);
                if (executed == 0) {
                    break;
                }
            } catch (RuntimeException e) {
                Throwable cause = e;
                while ((cause instanceof CompletionException || cause.getClass() == RuntimeException.class)
                        && cause.getCause() != null) {
                    cause = cause.getCause();
                }
                outcome.put("exception", cause.getClass().getName());
                outcome.put("site", guardiannSite(cause));
                executions.add(outcome);
                failures++;
            }
        }
        return executions;
    }

    /** The innermost GuardiANN frame of a failure, e.g. "SplitMergeTask.writeClusterMetadataAndEnqueueTasks:909". */
    private static String guardiannSite(Throwable cause) {
        for (StackTraceElement frame : cause.getStackTrace()) {
            if (frame.getClassName().startsWith("com.apple.foundationdb.async.guardiann.")) {
                String simple = frame.getClassName().substring("com.apple.foundationdb.async.guardiann.".length());
                return simple + "." + frame.getMethodName() + ":" + frame.getLineNumber();
            }
        }
        return "";
    }

    /**
     * Splits a cluster while an EXISTING neighbour cluster holds zero primaries
     * (primaryClusterMin 0 keeps the empty cluster from being merge-eligible).
     * Phase 1 builds two clusters with a valid split; phase 2 deletes the far
     * cluster's two vectors; phase 3 grows the near cluster past max and drains.
     */
    @ConformanceStep("guardiannEmptyNeighbourSplitProbe")
    public Map<String, Object> guardiannEmptyNeighbourSplitProbe(String clusterFile, String tenantName,
                                                                  byte[] subspace) {
        Guardiann guardiann = smallGuardiann(subspace, 0);
        for (int i = 0; i < 10; i++) {
            insert(clusterFile, tenantName, guardiann, i, near(i));
        }
        for (int i = 0; i < 2; i++) {
            insert(clusterFile, tenantName, guardiann, 1000 + i, far(i));
        }
        Map<String, Object> result = new LinkedHashMap<>();
        result.put("buildExecutions", drainOneByOne(clusterFile, tenantName, guardiann));
        result.put("afterBuild", runInContext(clusterFile, tenantName, context ->
                GuardiannConformanceAccess.primaryCounts(guardiann, context.ensureActive())));
        for (int i = 0; i < 2; i++) {
            delete(clusterFile, tenantName, guardiann, 1000 + i, far(i));
        }
        result.put("deleteExecutions", drainOneByOne(clusterFile, tenantName, guardiann));
        result.put("afterDelete", runInContext(clusterFile, tenantName, context ->
                GuardiannConformanceAccess.primaryCounts(guardiann, context.ensureActive())));
        insert(clusterFile, tenantName, guardiann, 10, near(10));
        result.put("splitExecutions", drainOneByOne(clusterFile, tenantName, guardiann));
        result.put("afterSplit", runInContext(clusterFile, tenantName, context ->
                GuardiannConformanceAccess.primaryCounts(guardiann, context.ensureActive())));
        return result;
    }

    private static Map<String, Object> phase(Runnable action) {
        Map<String, Object> outcome = new LinkedHashMap<>();
        try {
            action.run();
            outcome.put("ok", true);
        } catch (RuntimeException e) {
            Throwable cause = e;
            while ((cause instanceof CompletionException || cause.getClass() == RuntimeException.class)
                    && cause.getCause() != null) {
                cause = cause.getCause();
            }
            outcome.put("ok", false);
            outcome.put("exception", cause.getClass().getName());
            outcome.put("site", guardiannSite(cause));
        }
        return outcome;
    }

    /** Applies ';'-separated builder knob overrides by reflection, in order. */
    private static void applyKnobs(Config.ConfigBuilder builder, String knob, String value)
            throws ReflectiveOperationException {
        if (knob.isEmpty()) {
            return;
        }
        String[] knobs = knob.split(";");
        String[] values = value.split(";");
        for (int k = 0; k < knobs.length; k++) {
            java.lang.reflect.Method setter = null;
            for (java.lang.reflect.Method m : Config.ConfigBuilder.class.getMethods()) {
                if (m.getName().equals("set" + knobs[k]) && m.getParameterCount() == 1) {
                    setter = m;
                }
            }
            if (setter == null) {
                throw new IllegalArgumentException("no builder knob " + knobs[k]);
            }
            Class<?> type = setter.getParameterTypes()[0];
            Object arg = type == int.class ? (Object) Integer.parseInt(values[k])
                    : type == double.class ? (Object) Double.parseDouble(values[k])
                    : type == boolean.class ? (Object) Boolean.parseBoolean(values[k])
                    : type == Metric.class ? (Object) Metric.valueOf(values[k])
                    : null;
            setter.invoke(builder, arg);
        }
    }

    /**
     * Degenerate-knob consumer probe. Builds the small test configuration, then
     * overrides ONE builder knob by reflection (e.g. "DeleteConcurrency" = 0), and
     * runs insert(10 near + 2 far), drain, delete, drain and a k=3 search, each
     * phase reporting success or the target's exception class and site. It
     * records which consumer of each knob throws and which degrades silently.
     */
    @ConformanceStep("guardiannKnobProbe")
    public Map<String, Object> guardiannKnobProbe(String clusterFile, String tenantName, byte[] subspace,
                                                  String knob, String value) {
        Map<String, Object> result = new LinkedHashMap<>();
        Config.ConfigBuilder builder = Guardiann.newConfigBuilder()
                .setMetric(Metric.EUCLIDEAN_METRIC)
                .setPrimaryClusterMin(0)
                .setPrimaryClusterMax(10)
                .setPrimaryClusterHardMax(40)
                .setCollapseMinDuplicates(5)
                .setDeterministicRandomness(true);
        Config config;
        try {
            // knob/value may list several ';'-separated pairs, applied in order.
            applyKnobs(builder, knob, value);
            config = builder.build(2);
            result.put("config", Map.of("ok", true));
        } catch (ReflectiveOperationException | RuntimeException e) {
            Throwable cause = e instanceof java.lang.reflect.InvocationTargetException ? e.getCause() : e;
            result.put("config", Map.of("ok", false, "exception", cause.getClass().getName()));
            return result;
        }
        Guardiann guardiann = new Guardiann(new Subspace(subspace), ForkJoinPool.commonPool(), config,
                OnWriteListener.NOOP, OnReadListener.NOOP);
        result.put("insert", phase(() -> {
            for (int i = 0; i < 10; i++) {
                insert(clusterFile, tenantName, guardiann, i, knobNear(i));
            }
            for (int i = 0; i < 2; i++) {
                insert(clusterFile, tenantName, guardiann, 1000 + i, knobFar(i));
            }
        }));
        result.put("trainedAfterInsert", runInContext(clusterFile, tenantName, context ->
                GuardiannConformanceAccess.trained(guardiann, context.ensureActive())));
        result.put("drain", drainOneByOne(clusterFile, tenantName, guardiann));
        result.put("delete", phase(() -> delete(clusterFile, tenantName, guardiann, 3, knobNear(3))));
        result.put("drainAfterDelete", drainOneByOne(clusterFile, tenantName, guardiann));
        final int[] found = new int[1];
        result.put("search", phase(() -> found[0] = runInContext(clusterFile, tenantName, context ->
                guardiann.kNearestNeighborsSearch(context.ensureActive(), 3,
                        new com.apple.foundationdb.async.guardiann.SearchConfig.SearchConfigBuilder().build(),
                        false, new DoubleRealVector(knobNear(0))).join().size())));
        result.put("found", found[0]);
        result.put("clusters", runInContext(clusterFile, tenantName, context ->
                GuardiannConformanceAccess.primaryCounts(guardiann, context.ensureActive())));
        // Second split, now next to an existing neighbour: exercises neighbour
        // reassignment and bounce follow-ups.
        result.put("grow", phase(() -> {
            for (int i = 10; i < 19; i++) {
                insert(clusterFile, tenantName, guardiann, i, knobNear(i));
            }
        }));
        result.put("drainAfterGrow", drainOneByOne(clusterFile, tenantName, guardiann));
        result.put("clustersAfterGrow", runInContext(clusterFile, tenantName, context ->
                GuardiannConformanceAccess.primaryCounts(guardiann, context.ensureActive())));
        // Shrink the near vectors to one survivor: exercises delete-driven merges.
        result.put("shrink", phase(() -> {
            for (int i = 0; i < 18; i++) {
                if (i != 3) {
                    delete(clusterFile, tenantName, guardiann, i, knobNear(i));
                }
            }
        }));
        result.put("drainAfterShrink", drainOneByOne(clusterFile, tenantName, guardiann));
        result.put("clustersAfterShrink", runInContext(clusterFile, tenantName, context ->
                GuardiannConformanceAccess.primaryCounts(guardiann, context.ensureActive())));
        // Collapse: a separate structure of 11 identical vectors. KMeans leaves one
        // child empty, so no split candidate is usable and the duplicates (11 > 5)
        // route the split into a CollapseTask.
        byte[] dupSubspace = new Subspace(subspace).subspace(Tuple.from("dup")).pack();
        Guardiann duplicates = new Guardiann(new Subspace(dupSubspace), ForkJoinPool.commonPool(), config,
                OnWriteListener.NOOP, OnReadListener.NOOP);
        result.put("dupInsert", phase(() -> {
            for (int i = 0; i < 11; i++) {
                insert(clusterFile, tenantName, duplicates, 3000 + i, new double[] {5.0, -5.0});
            }
        }));
        result.put("drainAfterDup", drainOneByOne(clusterFile, tenantName, duplicates));
        result.put("clustersAfterDup", runInContext(clusterFile, tenantName, context ->
                GuardiannConformanceAccess.primaryCounts(duplicates, context.ensureActive())));
        final int[] dupFound = new int[1];
        result.put("dupSearch", phase(() -> dupFound[0] = runInContext(clusterFile, tenantName, context ->
                duplicates.kNearestNeighborsSearch(context.ensureActive(), 20,
                        new com.apple.foundationdb.async.guardiann.SearchConfig.SearchConfigBuilder().build(),
                        false, new DoubleRealVector(new double[] {5.0, -5.0})).join().size())));
        result.put("dupFound", dupFound[0]);
        return result;
    }

    /**
     * Inline maintenance mode (maintainInTransaction=true on every insert). Builds
     * {@code preload} vectors in DEFERRED mode first (so a backlog of unexecuted
     * tasks can exist), then inserts {@code inline} more vectors with inline
     * maintenance, one per transaction. Reports each inline insert's outcome and
     * the final cluster metadata. Optional knob overrides as in guardiannKnobProbe.
     */
    @ConformanceStep("guardiannInlineProbe")
    public Map<String, Object> guardiannInlineProbe(String clusterFile, String tenantName, byte[] subspace,
                                                    long max, long hardMax, long preload, long inline,
                                                    long farCount, long deletes, String knob, String value) {
        Map<String, Object> result = new LinkedHashMap<>();
        Config.ConfigBuilder builder = Guardiann.newConfigBuilder()
                .setMetric(Metric.EUCLIDEAN_METRIC)
                .setPrimaryClusterMin(0)
                .setPrimaryClusterMax((int) max)
                .setPrimaryClusterHardMax((int) hardMax)
                .setCollapseMinDuplicates(5)
                .setDeterministicRandomness(true);
        try {
            applyKnobs(builder, knob, value);
        } catch (ReflectiveOperationException e) {
            throw new IllegalStateException(e);
        }
        Config config = builder.build(2);
        Guardiann guardiann = new Guardiann(new Subspace(subspace), ForkJoinPool.commonPool(), config,
                OnWriteListener.NOOP, OnReadListener.NOOP);
        result.put("preload", phase(() -> {
            for (int i = 0; i < preload; i++) {
                insert(clusterFile, tenantName, guardiann, i, knobNear(i));
            }
            for (int i = 0; i < farCount; i++) {
                insert(clusterFile, tenantName, guardiann, 1000 + i, knobFar(i));
            }
        }));
        List<Map<String, Object>> inserts = new ArrayList<>();
        for (int i = 0; i < inline; i++) {
            final long id = preload + i;
            inserts.add(phase(() -> runInContext(clusterFile, tenantName, context -> {
                guardiann.insert(context.ensureActive(), Tuple.from(id), new DoubleRealVector(knobNear((int) id)),
                        null, true).join();
                return null;
            })));
        }
        result.put("inserts", inserts);
        // Inline deletes of ids 0..deletes-1 (each runs one queued task first).
        List<Map<String, Object>> deleteOutcomes = new ArrayList<>();
        for (int i = 0; i < deletes; i++) {
            final long id = i;
            deleteOutcomes.add(phase(() -> runInContext(clusterFile, tenantName, context -> {
                guardiann.delete(context.ensureActive(), Tuple.from(id), new DoubleRealVector(knobNear((int) id)),
                        true).join();
                return null;
            })));
        }
        result.put("deletes", deleteOutcomes);
        result.put("clusters", runInContext(clusterFile, tenantName, context ->
                GuardiannConformanceAccess.primaryCounts(guardiann, context.ensureActive())));
        final int[] found = new int[1];
        result.put("search", phase(() -> found[0] = runInContext(clusterFile, tenantName, context ->
                guardiann.kNearestNeighborsSearch(context.ensureActive(), 100,
                        new com.apple.foundationdb.async.guardiann.SearchConfig.SearchConfigBuilder().build(),
                        false, new DoubleRealVector(knobNear(0))).join().size())));
        result.put("found", found[0]);
        return result;
    }

    /**
     * Pure-Java measurement of a size-balanced split (KMeans lambda > 0 with the
     * production overflowQuadraticPenalty) on the split-probe population: nearCount
     * vectors near the origin plus farCount far away. Uses the real KMeans.fit and
     * PartitionEvaluator with GuardiANN's default split parameters, then computes
     * FINAL nearest-centroid ownership the way SplitMergeTask re-homes primaries.
     */
    @ConformanceStep("guardiannBalancedSplitProbe")
    public List<Map<String, Object>> guardiannBalancedSplitProbe(long nearCount, long farCount, List<Number> lambdaValues) {
        List<com.apple.foundationdb.linear.RealVector> vectors = new ArrayList<>();
        for (int i = 0; i < Math.abs(nearCount); i++) {
            // Negative nearCount: an IDENTICAL-coordinate mass (every near vector equal).
            vectors.add(new DoubleRealVector(nearCount < 0 ? near(0) : near(i)));
        }
        for (int i = 0; i < farCount; i++) {
            vectors.add(new DoubleRealVector(far(i)));
        }
        com.apple.foundationdb.linear.DistanceEstimator estimator =
                com.apple.foundationdb.linear.DistanceEstimator.ofMetric(Metric.EUCLIDEAN_METRIC);
        com.apple.foundationdb.util.Lens<com.apple.foundationdb.linear.RealVector, com.apple.foundationdb.linear.RealVector> identity =
                new com.apple.foundationdb.util.Lens<>() {
                    @Override
                    public com.apple.foundationdb.linear.RealVector get(com.apple.foundationdb.linear.RealVector c) {
                        return c;
                    }

                    @Override
                    public com.apple.foundationdb.linear.RealVector set(com.apple.foundationdb.linear.RealVector c,
                                                                        com.apple.foundationdb.linear.RealVector a) {
                        return a;
                    }
                };
        Config defaults = Guardiann.defaultConfig(2);
        com.apple.foundationdb.kmeans.PartitionEvaluator.Parameters base =
                new com.apple.foundationdb.kmeans.PartitionEvaluator.Parameters(estimator);
        com.apple.foundationdb.kmeans.PartitionEvaluator.Parameters parameters =
                new com.apple.foundationdb.kmeans.PartitionEvaluator.Parameters(estimator,
                        base.minRelativeSseGain(), base.minSeparation(), base.maxLowMarginRate(),
                        defaults.minChildFraction(), defaults.maxRelativeImbalance(), base.lowMarginThreshold(),
                        base.alphaSseGain(), base.betaSeparationGain(), defaults.splitImbalancePenalty(),
                        base.deltaLowMarginPenalty(), base.minScoreGain());
        com.apple.foundationdb.kmeans.PartitionEvaluator.Partition<com.apple.foundationdb.linear.RealVector> current =
                new com.apple.foundationdb.kmeans.PartitionEvaluator.Partition<>(List.of(vectors.get(0)), identity,
                        new int[vectors.size()]);
        List<Map<String, Object>> rows = new ArrayList<>();
        for (Number lambdaValue : lambdaValues) {
            double lambda = lambdaValue.doubleValue();
            com.apple.foundationdb.kmeans.KMeans.Result<com.apple.foundationdb.linear.RealVector> fit =
                    com.apple.foundationdb.kmeans.KMeans.fit(new java.util.SplittableRandom(42), estimator, identity,
                            identity, vectors, 2, defaults.kMeansMaxIterations(), defaults.kMeansMaxRestarts(),
                            lambda, com.apple.foundationdb.kmeans.KMeans.overflowQuadraticPenalty());
            com.apple.foundationdb.kmeans.PartitionEvaluator.EvaluationResult evaluation =
                    com.apple.foundationdb.kmeans.PartitionEvaluator.evaluate(vectors, current, vectors,
                            new com.apple.foundationdb.kmeans.PartitionEvaluator.Partition<>(fit.clusterCentroids(),
                                    identity, fit.assignment()), identity, parameters);
            int[] owned = new int[2];
            for (com.apple.foundationdb.linear.RealVector v : vectors) {
                double d0 = estimator.distance(v, fit.clusterCentroids().get(0));
                double d1 = estimator.distance(v, fit.clusterCentroids().get(1));
                owned[d1 < d0 ? 1 : 0]++;
            }
            Map<String, Object> row = new LinkedHashMap<>();
            row.put("lambda", lambda);
            row.put("sizes", List.of(fit.clusterSizes()[0], fit.clusterSizes()[1]));
            row.put("decision", evaluation.decision().name());
            row.put("finalOwnership", List.of(owned[0], owned[1]));
            if (lambda == 0.0d) {
                // Outlier-excluded refit: drop the members of children below
                // minChildFraction, refit k=2 on the remaining mass with the same
                // KMeans, re-home EVERY vector to its nearest new centroid (Java's
                // final-ownership rule), and score that final partition.
                int n = vectors.size();
                List<com.apple.foundationdb.linear.RealVector> mass = new ArrayList<>();
                for (int i = 0; i < n; i++) {
                    int child = fit.assignment()[i];
                    if ((double) fit.clusterSizes()[child] / n >= defaults.minChildFraction()) {
                        mass.add(vectors.get(i));
                    }
                }
                if (mass.size() >= 2) {
                    com.apple.foundationdb.kmeans.KMeans.Result<com.apple.foundationdb.linear.RealVector> refit =
                            com.apple.foundationdb.kmeans.KMeans.fit(new java.util.SplittableRandom(43), estimator,
                                    identity, identity, mass, 2, defaults.kMeansMaxIterations(),
                                    defaults.kMeansMaxRestarts(), 0.0d,
                                    com.apple.foundationdb.kmeans.KMeans.overflowQuadraticPenalty());
                    int[] finalAssignment = new int[n];
                    int[] finalSizes = new int[2];
                    for (int i = 0; i < n; i++) {
                        double d0 = estimator.distance(vectors.get(i), refit.clusterCentroids().get(0));
                        double d1 = estimator.distance(vectors.get(i), refit.clusterCentroids().get(1));
                        finalAssignment[i] = d1 < d0 ? 1 : 0;
                        finalSizes[finalAssignment[i]]++;
                    }
                    com.apple.foundationdb.kmeans.PartitionEvaluator.EvaluationResult refitEvaluation =
                            com.apple.foundationdb.kmeans.PartitionEvaluator.evaluate(vectors, current, vectors,
                                    new com.apple.foundationdb.kmeans.PartitionEvaluator.Partition<>(
                                            refit.clusterCentroids(), identity, finalAssignment), identity, parameters);
                    row.put("refitMass", mass.size());
                    row.put("refitFinalOwnership", List.of(finalSizes[0], finalSizes[1]));
                    row.put("refitDecision", refitEvaluation.decision().name());
                    row.put("refitReason", String.valueOf(refitEvaluation.reason()));
                }
            }
            rows.add(row);
        }
        return rows;
    }

    private static com.apple.foundationdb.record.RecordMetaData guardiannQueueMetaData() {
        return guardiannRecordMetaData("12");
    }

    private static com.apple.foundationdb.record.RecordMetaData guardiannRecordMetaData(String hardMax) {
        var builder = com.apple.foundationdb.record.RecordMetaData.newBuilder()
                .setRecords(com.apple.foundationdb.record.RecordLayerDemo.getDescriptor());
        builder.getRecordType("Order").setPrimaryKey(com.apple.foundationdb.record.metadata.Key.Expressions.field("order_id"));
        builder.getRecordType("Customer").setPrimaryKey(com.apple.foundationdb.record.metadata.Key.Expressions.field("customer_id"));
        builder.getRecordType("TypedRecord").setPrimaryKey(com.apple.foundationdb.record.metadata.Key.Expressions.field("id"));
        builder.addIndex("Order", new com.apple.foundationdb.record.metadata.Index("gv",
                new com.apple.foundationdb.record.metadata.expressions.KeyWithValueExpression(
                        com.apple.foundationdb.record.metadata.Key.Expressions.field("vector_data"), 0),
                com.apple.foundationdb.record.metadata.IndexTypes.VECTOR,
                Map.of(com.apple.foundationdb.record.metadata.IndexOptions.VECTOR_ENGINE, "GUARDIANN",
                        com.apple.foundationdb.record.metadata.IndexOptions.HNSW_NUM_DIMENSIONS, "2",
                        com.apple.foundationdb.record.metadata.IndexOptions.HNSW_METRIC, "EUCLIDEAN_METRIC",
                        com.apple.foundationdb.record.metadata.IndexOptions.GUARDIANN_PRIMARY_CLUSTER_MIN, "0",
                        com.apple.foundationdb.record.metadata.IndexOptions.GUARDIANN_PRIMARY_CLUSTER_MAX, "10",
                        com.apple.foundationdb.record.metadata.IndexOptions.GUARDIANN_PRIMARY_CLUSTER_HARD_MAX, hardMax,
                        com.apple.foundationdb.record.metadata.IndexOptions.GUARDIANN_COLLAPSE_MIN_DUPLICATES, "5",
                        com.apple.foundationdb.record.metadata.IndexOptions.GUARDIANN_DETERMINISTIC_RANDOMNESS, "true")));
        return builder.build();
    }

    /**
     * Queue-replay shape of the capacity stranding the design declares. A
     * GuardiANN index is built empty with a pending write queue (so its range set
     * is complete and it stays WRITE_ONLY_WITH_QUEUE), {@code records} vectors of
     * one cluster are then saved into the queue, and a second build drains the
     * queue before any merge. Reports the failure chain and what the drain left.
     */
    @ConformanceStep("guardiannQueueCapacityProbe")
    public Map<String, Object> guardiannQueueCapacityProbe(String clusterFile, long records, boolean markQueue) {
        var metadata = guardiannQueueMetaData();
        var index = metadata.getIndex("gv");
        var space = new Subspace(Tuple.from("guardiann-queue-probe", UUID.randomUUID().toString()));
        var policy = com.apple.foundationdb.record.provider.foundationdb.OnlineIndexer.IndexingPolicy.newBuilder()
                .setUsePendingWriteQueue(List.of(index)).build();
        Map<String, Object> result = new LinkedHashMap<>();
        try {
            runInContext(clusterFile, "", context -> {
                com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore.newBuilder()
                        .setMetaDataProvider(metadata).setContext(context).setSubspace(space)
                        .setFormatVersion(com.apple.foundationdb.record.provider.foundationdb.FormatVersion.WRITE_ONLY_WITH_QUEUE)
                        .create();
                return null;
            });
            if (markQueue) {
                // A new index is READABLE at store creation; put it into the
                // pending-queue build state so later saves are queued.
                runInContext(clusterFile, "", context -> {
                    com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore.newBuilder()
                            .setMetaDataProvider(metadata).setContext(context).setSubspace(space).open()
                            .clearAndMarkIndexWriteOnlyWithQueue(index).join();
                    return null;
                });
            }
            try (var indexer = com.apple.foundationdb.record.provider.foundationdb.OnlineIndexer.newBuilder()
                    .setDatabase(createDatabase(clusterFile)).setMetaData(metadata).setSubspace(space)
                    .setIndex(index).setIndexingPolicy(policy).build()) {
                indexer.buildIndex(false);
            }
            result.put("stateAfterEmptyBuild", runInContext(clusterFile, "", context ->
                    com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore.newBuilder()
                            .setMetaDataProvider(metadata).setContext(context).setSubspace(space).open()
                            .getIndexState(index).name()));
            int saved = 0;
            String saveFailure = "";
            for (int i = 0; i < records && saveFailure.isEmpty(); i++) {
                final int id = i;
                try {
                runInContext(clusterFile, "", context -> {
                    var store = com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore.newBuilder()
                            .setMetaDataProvider(metadata).setContext(context).setSubspace(space).open();
                    var descriptor = metadata.getRecordType("Order").getDescriptor();
                    store.saveRecord(com.google.protobuf.DynamicMessage.newBuilder(descriptor)
                            .setField(descriptor.findFieldByName("order_id"), (long) id)
                            .setField(descriptor.findFieldByName("vector_data"),
                                    com.google.protobuf.ByteString.copyFrom(new DoubleRealVector(near(id)).getRawData()))
                            .build());
                    return null;
                });
                } catch (RuntimeException e) {
                    Throwable cause = e;
                    while (cause.getCause() != null) {
                        cause = cause.getCause();
                    }
                    saveFailure = cause.getClass().getName() + " at record " + i;
                    break;
                }
                saved++;
            }
            result.put("saved", saved);
            result.put("saveFailure", saveFailure);
            result.put("stateBeforeDrain", runInContext(clusterFile, "", context ->
                    com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore.newBuilder()
                            .setMetaDataProvider(metadata).setContext(context).setSubspace(space).open()
                            .getIndexState(index).name()));
            List<String> chain = new ArrayList<>();
            String site = "";
            try (var indexer = com.apple.foundationdb.record.provider.foundationdb.OnlineIndexer.newBuilder()
                    .setDatabase(createDatabase(clusterFile)).setMetaData(metadata).setSubspace(space)
                    .setIndex(index).setIndexingPolicy(policy).build()) {
                indexer.buildIndex(true);
            } catch (RuntimeException e) {
                for (Throwable t = e; t != null; t = t.getCause()) {
                    chain.add(t.getClass().getName());
                    if (site.isEmpty()) {
                        site = guardiannSite(t);
                    }
                }
            }
            result.put("failureChain", chain);
            result.put("failureSite", site);
            runInContext(clusterFile, "", context -> {
                var store = com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore.newBuilder()
                        .setMetaDataProvider(metadata).setContext(context).setSubspace(space).open();
                result.put("stateAfterDrain", store.getIndexState(index).name());
                result.put("clusters", GuardiannConformanceAccess.primaryCountsAt(
                        store.indexSubspace(index), context.ensureActive()));
                result.put("queueEntries", context.ensureActive().getRange(
                        space.subspace(Tuple.from(9L, index.getSubspaceTupleKey(), 8L)).range()).asList().join().size());
                return null;
            });
        } finally {
            runInContext(clusterFile, "", context -> {
                context.ensureActive().clear(space.range());
                return null;
            });
        }
        return result;
    }

    /**
     * Builds a single-cluster GuardiANN structure (primaryClusterMax 10) from
     * {@code nearCount} vectors near the origin and {@code farCount} vectors far
     * away, with deferred maintenance, then executes the queued tasks one
     * transaction at a time. Reports each task execution outcome, including the
     * target's exception class when a task fails.
     */
    @ConformanceStep("guardiannSplitCandidateProbe")
    public Map<String, Object> guardiannSplitCandidateProbe(String clusterFile, String tenantName,
                                                             byte[] subspace, long nearCount, long farCount) {
        Config config = Guardiann.newConfigBuilder()
                .setMetric(Metric.EUCLIDEAN_METRIC)
                .setPrimaryClusterMin(2)
                .setPrimaryClusterMax(10)
                .setPrimaryClusterHardMax(40)
                .setCollapseMinDuplicates(5)
                .setDeterministicRandomness(true)
                .build(2);
        Guardiann guardiann = new Guardiann(new Subspace(subspace), ForkJoinPool.commonPool(), config,
                OnWriteListener.NOOP, OnReadListener.NOOP);
        for (int i = 0; i < nearCount; i++) {
            final int id = i;
            runInContext(clusterFile, tenantName, context -> {
                Transaction tr = context.ensureActive();
                guardiann.insert(tr, Tuple.from((long) id),
                        new DoubleRealVector(new double[] {0.01 * id, 0.02 * (id % 3)}), null, false).join();
                return null;
            });
        }
        for (int i = 0; i < farCount; i++) {
            final int id = 1000 + i;
            final int offset = i;
            runInContext(clusterFile, tenantName, context -> {
                Transaction tr = context.ensureActive();
                guardiann.insert(tr, Tuple.from((long) id),
                        new DoubleRealVector(new double[] {100.0 + 0.01 * offset, 100.0}), null, false).join();
                return null;
            });
        }
        List<Map<String, Object>> executions = drainOneByOne(clusterFile, tenantName, guardiann);
        Map<String, Object> result = new LinkedHashMap<>();
        result.put("executions", executions);
        result.put("clusters", runInContext(clusterFile, tenantName, context ->
                GuardiannConformanceAccess.primaryCounts(guardiann, context.ensureActive())));
        return result;
    }

    private static List<com.apple.foundationdb.linear.RealVector> toVectors(List<List<Number>> coordinates) {
        List<com.apple.foundationdb.linear.RealVector> vectors = new ArrayList<>();
        for (List<Number> c : coordinates) {
            double[] v = new double[c.size()];
            for (int i = 0; i < v.length; i++) {
                v[i] = c.get(i).doubleValue();
            }
            vectors.add(new DoubleRealVector(v));
        }
        return vectors;
    }

    /**
     * Target behaviour of one split population in both maintenance modes. The vectors are inserted
     * in list order with deferred maintenance into a lone cluster (primaryClusterMax 10, hard max 400,
     * so the first split evaluates the whole population), the queue is drained one task per
     * transaction, and then {@code inlineTail} further vectors next to the first one are inserted with
     * inline maintenance. Reports every task execution, every inline insert and the final metadata.
     */
    @ConformanceStep("guardiannSplitShapeProbe")
    public Map<String, Object> guardiannSplitShapeProbe(String clusterFile, String tenantName, byte[] subspace,
                                                        List<List<Number>> vectors, double minChildFraction,
                                                        long inlineTail) {
        Config config = Guardiann.newConfigBuilder()
                .setMetric(Metric.EUCLIDEAN_METRIC)
                .setPrimaryClusterMin(2)
                .setPrimaryClusterMax(10)
                .setPrimaryClusterHardMax(400)
                .setCollapseMinDuplicates(5)
                .setMinChildFraction(minChildFraction)
                .setDeterministicRandomness(true)
                .build(2);
        Guardiann guardiann = new Guardiann(new Subspace(subspace), ForkJoinPool.commonPool(), config,
                OnWriteListener.NOOP, OnReadListener.NOOP);
        List<com.apple.foundationdb.linear.RealVector> population = toVectors(vectors);
        for (int i = 0; i < population.size(); i++) {
            final int id = i;
            runInContext(clusterFile, tenantName, context -> {
                guardiann.insert(context.ensureActive(), Tuple.from((long) id), population.get(id), null, false).join();
                return null;
            });
        }
        Map<String, Object> result = new LinkedHashMap<>();
        result.put("executions", drainOneByOne(clusterFile, tenantName, guardiann));
        result.put("clusters", runInContext(clusterFile, tenantName, context ->
                GuardiannConformanceAccess.primaryCounts(guardiann, context.ensureActive())));
        List<Map<String, Object>> inline = new ArrayList<>();
        double[] first = population.get(0).getData();
        for (int k = 0; k < inlineTail; k++) {
            final long id = 10_000L + k;
            final double[] v = first.clone();
            v[0] += 0.001 * (k + 1);
            inline.add(phase(() -> runInContext(clusterFile, tenantName, context -> {
                guardiann.insert(context.ensureActive(), Tuple.from(id), new DoubleRealVector(v), null, true).join();
                return null;
            })));
        }
        result.put("inline", inline);
        result.put("clustersAfterInline", runInContext(clusterFile, tenantName, context ->
                GuardiannConformanceAccess.primaryCounts(guardiann, context.ensureActive())));
        return result;
    }

    /**
     * Pure-algorithm measurement of the iterated outlier peel the Go port adds at the split
     * no-usable-candidate site, computed with the real KMeans and PartitionEvaluator. The current
     * partition is the lone bootstrapped cluster: its centroid is the first inserted vector
     * (Insert.initialAccessInfoAndFirstCluster writes newVector as the first centroid), which is
     * vectors[0] here. Round 0 is the plain 1-to-2 KMeans (the target's own candidate). While the
     * latest partition is INVALID, its undersized child's members are removed from the mass, KMeans
     * k=2 is refitted on the remaining mass, every vector is re-homed to its nearest refit centroid
     * (strict less-than, ties to the lower index) and that final partition is evaluated. Stops on the
     * first usable partition, when the undersized child holds no mass member, when fewer than two
     * mass members remain, or after maxRefits refits.
     */
    @ConformanceStep("guardiannPeelProbe")
    public List<Map<String, Object>> guardiannPeelProbe(List<List<Number>> vectors, double minChildFraction,
                                                        long maxRefits, List<Number> seeds) {
        return guardiannPeelModeProbe(vectors, minChildFraction, maxRefits, seeds, "child");
    }

    /**
     * {@link #guardiannPeelProbe} with the removal rule as a parameter. "child" removes the undersized
     * child's mass members (the design's peel); "tail" additionally removes every mass member at least as
     * far from the other child's centroid as the undersized child's NEAREST mass member, so a round never
     * removes a member closer than the group it isolated; "floor" adds the geometric removal floor;
     * "floor-sample:S" is "floor" with every refit's KMeans run on a deterministic stride sample of at
     * most S mass members (every ceil(|M|/S)-th member of M in primary index order, from the first),
     * while the final partition still assigns EVERY vector and is evaluated in full.
     */
    @ConformanceStep("guardiannPeelModeProbe")
    public List<Map<String, Object>> guardiannPeelModeProbe(List<List<Number>> vectors, double minChildFraction,
                                                            long maxRefits, List<Number> seeds, String peelMode) {
        return peelModeOverPopulation(toVectors(vectors), minChildFraction, maxRefits, seeds, peelMode);
    }

    private static List<Map<String, Object>> peelModeOverPopulation(
            List<com.apple.foundationdb.linear.RealVector> population, double minChildFraction, long maxRefits,
            List<Number> seeds, String peelMode) {
        List<Map<String, Object>> perSeed = new ArrayList<>();
        final int sampleSize = peelMode.startsWith("floor-sample:")
                ? Integer.parseInt(peelMode.substring("floor-sample:".length())) : Integer.MAX_VALUE;
        for (Number seed : seeds) {
            Map<String, Object> row = peelWithSeed(population, minChildFraction, maxRefits, seed.longValue(),
                    "tail".equals(peelMode), "floor".equals(peelMode) || peelMode.startsWith("floor-sample:"),
                    sampleSize);
            row.put("seed", seed.longValue());
            perSeed.add(row);
        }
        return perSeed;
    }

    /**
     * {@link #guardiannPeelModeProbe} over a shape generated inside the JVM, so that shapes at
     * d in the thousands need not cross the invoker as JSON. The shape is n - m core vectors with
     * independent N(0, sigma^2) coordinates followed by m outliers in uniformly random directions at
     * radius radiusFactor * sqrt(d), all drawn from java.util.Random(genSeed) in that order. Vector 0
     * (the lone cluster's bootstrapped centroid) is therefore a core vector. The pinned result is the
     * target's own KMeans and PartitionEvaluator over this data, as for the JSON form.
     */
    @ConformanceStep("guardiannPeelGeneratedProbe")
    public List<Map<String, Object>> guardiannPeelGeneratedProbe(long n, long d, long m, double sigma,
                                                                 double radiusFactor, long genSeed,
                                                                 double minChildFraction, long maxRefits,
                                                                 List<Number> seeds, String peelMode) {
        java.util.Random random = new java.util.Random(genSeed);
        List<com.apple.foundationdb.linear.RealVector> population = new ArrayList<>();
        for (long i = 0; i < n - m; i++) {
            double[] v = new double[(int) d];
            for (int j = 0; j < d; j++) {
                v[j] = random.nextGaussian() * sigma;
            }
            population.add(new DoubleRealVector(v));
        }
        final double radius = radiusFactor * Math.sqrt(d);
        for (long i = 0; i < m; i++) {
            double[] v = new double[(int) d];
            double norm = 0;
            for (int j = 0; j < d; j++) {
                v[j] = random.nextGaussian();
                norm += v[j] * v[j];
            }
            norm = Math.sqrt(norm);
            for (int j = 0; j < d; j++) {
                v[j] = v[j] / norm * radius;
            }
            population.add(new DoubleRealVector(v));
        }
        return peelModeOverPopulation(population, minChildFraction, maxRefits, seeds, peelMode);
    }

    /**
     * One peel run: the plain 1-to-2 KMeans uses SplittableRandom(seed), refit round r uses
     * SplittableRandom(seed + 1 + r). The target draws its candidate KMeans from the task RNG, so a
     * fixed seed is one sample of the target's own candidate, not a replay of a particular task.
     */
    private static Map<String, Object> peelWithSeed(List<com.apple.foundationdb.linear.RealVector> population,
                                                    double minChildFraction, long maxRefits, long seed,
                                                    boolean tailCut, boolean geometricFloor, int sampleSize) {
        int n = population.size();
        com.apple.foundationdb.linear.DistanceEstimator estimator =
                com.apple.foundationdb.linear.DistanceEstimator.ofMetric(Metric.EUCLIDEAN_METRIC);
        com.apple.foundationdb.util.Lens<com.apple.foundationdb.linear.RealVector, com.apple.foundationdb.linear.RealVector> identity =
                new com.apple.foundationdb.util.Lens<>() {
                    @Override
                    public com.apple.foundationdb.linear.RealVector get(com.apple.foundationdb.linear.RealVector c) {
                        return c;
                    }

                    @Override
                    public com.apple.foundationdb.linear.RealVector set(com.apple.foundationdb.linear.RealVector c,
                                                                        com.apple.foundationdb.linear.RealVector a) {
                        return a;
                    }
                };
        Config defaults = Guardiann.defaultConfig(2);
        com.apple.foundationdb.kmeans.PartitionEvaluator.Parameters base =
                new com.apple.foundationdb.kmeans.PartitionEvaluator.Parameters(estimator);
        com.apple.foundationdb.kmeans.PartitionEvaluator.Parameters parameters =
                new com.apple.foundationdb.kmeans.PartitionEvaluator.Parameters(estimator,
                        base.minRelativeSseGain(), base.minSeparation(), base.maxLowMarginRate(),
                        minChildFraction, defaults.maxRelativeImbalance(), base.lowMarginThreshold(),
                        base.alphaSseGain(), base.betaSeparationGain(), defaults.splitImbalancePenalty(),
                        base.deltaLowMarginPenalty(), base.minScoreGain());
        com.apple.foundationdb.kmeans.PartitionEvaluator.Partition<com.apple.foundationdb.linear.RealVector> current =
                new com.apple.foundationdb.kmeans.PartitionEvaluator.Partition<>(List.of(population.get(0)), identity,
                        new int[n]);
        final long fitStart = System.nanoTime();
        com.apple.foundationdb.kmeans.KMeans.Result<com.apple.foundationdb.linear.RealVector> fit =
                com.apple.foundationdb.kmeans.KMeans.fit(new java.util.SplittableRandom(seed), estimator, identity,
                        identity, population, 2, defaults.kMeansMaxIterations(), defaults.kMeansMaxRestarts(),
                        0.0d, com.apple.foundationdb.kmeans.KMeans.overflowQuadraticPenalty());
        final long fitNanos = System.nanoTime() - fitStart;
        com.apple.foundationdb.kmeans.PartitionEvaluator.EvaluationResult evaluation =
                com.apple.foundationdb.kmeans.PartitionEvaluator.evaluate(population, current, population,
                        new com.apple.foundationdb.kmeans.PartitionEvaluator.Partition<>(fit.clusterCentroids(),
                                identity, fit.assignment()), identity, parameters);
        Map<String, Object> result = new LinkedHashMap<>();
        // Wall-clock cost of the target's own candidate fit and of each refit: a measurement of
        // the machine, never part of a pinned line.
        result.put("fitNanos", fitNanos);
        result.put("initialSizes", List.of(fit.clusterSizes()[0], fit.clusterSizes()[1]));
        result.put("initialDecision", evaluation.decision().name());
        int[] assignment = fit.assignment().clone();
        int[] sizes = fit.clusterSizes().clone();
        List<com.apple.foundationdb.linear.RealVector> centroids = fit.clusterCentroids();
        List<Integer> mass = new ArrayList<>();
        for (int i = 0; i < n; i++) {
            mass.add(i);
        }
        List<Map<String, Object>> rounds = new ArrayList<>();
        String outcome = "initialUsable";
        if (evaluation.decision() == com.apple.foundationdb.kmeans.PartitionEvaluator.Decision.INVALID_CANDIDATE) {
            outcome = "roundBound";
            for (int round = 0; round < maxRefits; round++) {
                final int undersized = sizes[0] <= sizes[1] ? 0 : 1;
                final int[] latest = assignment;
                double threshold = Double.POSITIVE_INFINITY;
                if (tailCut) {
                    com.apple.foundationdb.linear.RealVector big = centroids.get(1 - undersized);
                    for (int i : mass) {
                        if (latest[i] == undersized) {
                            threshold = Math.min(threshold, estimator.distance(population.get(i), big));
                        }
                    }
                }
                List<Integer> next = new ArrayList<>();
                for (int i : mass) {
                    if (latest[i] == undersized) {
                        continue;
                    }
                    if (tailCut && estimator.distance(population.get(i), centroids.get(1 - undersized)) >= threshold) {
                        continue;
                    }
                    next.add(i);
                }
                if (next.size() == mass.size()) {
                    outcome = "undersizedChildHoldsNoMass";
                    break;
                }
                if (geometricFloor) {
                    // Applied only once the undersized child held a mass member (KMeans separated
                    // something). After round r at least 2^(r+1) - 1 members have left the mass; a shortfall is taken
                    // from the members farthest from the other child's centroid (ties: lower index kept).
                    final long floor = (1L << Math.min(62, round + 1)) - 1;
                    final long shortfall = floor - (n - next.size());
                    if (shortfall > 0 && next.size() > 0) {
                        final com.apple.foundationdb.linear.RealVector big = centroids.get(1 - undersized);
                        List<Integer> byDistance = new ArrayList<>(next);
                        byDistance.sort((a, b) -> {
                            int c = Double.compare(estimator.distance(population.get(b), big),
                                    estimator.distance(population.get(a), big));
                            return c != 0 ? c : Integer.compare(b, a);
                        });
                        java.util.Set<Integer> drop = new java.util.HashSet<>(
                                byDistance.subList(0, (int)Math.min(shortfall, byDistance.size())));
                        List<Integer> kept = new ArrayList<>();
                        for (int i : next) {
                            if (!drop.contains(i)) {
                                kept.add(i);
                            }
                        }
                        next = kept;
                    }
                }
                if (next.size() < 2) {
                    outcome = "massBelowTwo";
                    break;
                }
                mass = next;
                List<com.apple.foundationdb.linear.RealVector> massVectors = new ArrayList<>();
                final int stride = (int) ((mass.size() + (long) sampleSize - 1) / sampleSize);
                for (int k = 0; k < mass.size() && massVectors.size() < sampleSize; k += stride) {
                    massVectors.add(population.get(mass.get(k)));
                }
                final long refitStart = System.nanoTime();
                com.apple.foundationdb.kmeans.KMeans.Result<com.apple.foundationdb.linear.RealVector> refit =
                        com.apple.foundationdb.kmeans.KMeans.fit(new java.util.SplittableRandom(seed + 1 + round), estimator,
                                identity, identity, massVectors, 2, defaults.kMeansMaxIterations(),
                                defaults.kMeansMaxRestarts(), 0.0d,
                                com.apple.foundationdb.kmeans.KMeans.overflowQuadraticPenalty());
                int[] finalAssignment = new int[n];
                int[] finalSizes = new int[2];
                for (int i = 0; i < n; i++) {
                    double d0 = estimator.distance(population.get(i), refit.clusterCentroids().get(0));
                    double d1 = estimator.distance(population.get(i), refit.clusterCentroids().get(1));
                    finalAssignment[i] = d1 < d0 ? 1 : 0;
                    finalSizes[finalAssignment[i]]++;
                }
                com.apple.foundationdb.kmeans.PartitionEvaluator.EvaluationResult refitEvaluation =
                        com.apple.foundationdb.kmeans.PartitionEvaluator.evaluate(population, current, population,
                                new com.apple.foundationdb.kmeans.PartitionEvaluator.Partition<>(
                                        refit.clusterCentroids(), identity, finalAssignment), identity, parameters);
                final long refitNanos = System.nanoTime() - refitStart;
                Map<String, Object> row = new LinkedHashMap<>();
                row.put("refitNanos", refitNanos);
                row.put("mass", mass.size());
                row.put("fitted", massVectors.size());
                row.put("finalSizes", List.of(finalSizes[0], finalSizes[1]));
                row.put("decision", refitEvaluation.decision().name());
                rounds.add(row);
                if (refitEvaluation.decision() != com.apple.foundationdb.kmeans.PartitionEvaluator.Decision.INVALID_CANDIDATE) {
                    outcome = "selected";
                    break;
                }
                assignment = finalAssignment;
                sizes = finalSizes;
                centroids = refit.clusterCentroids();
            }
        }
        result.put("rounds", rounds);
        result.put("outcome", outcome);
        return result;
    }

    /**
     * Whether the target consumes an OBSOLETE task as a no-op while the knob that task would consume
     * is degenerate. Each scenario first drives a real task into its failing or livelocking state,
     * then stages obsolescence by rewriting the target clusters' state bits with the production
     * metadata writer (as a concurrent state change would), then drains again one task per transaction.
     */
    @ConformanceStep("guardiannObsoleteTaskProbe")
    public Map<String, Object> guardiannObsoleteTaskProbe(String clusterFile, String tenantName, byte[] subspace,
                                                         String scenario) {
        Config.ConfigBuilder builder = Guardiann.newConfigBuilder()
                .setMetric(Metric.EUCLIDEAN_METRIC)
                .setPrimaryClusterMin(0)
                .setPrimaryClusterMax(10)
                .setPrimaryClusterHardMax(40)
                .setCollapseMinDuplicates(5)
                .setDeterministicRandomness(true);
        final int clearBits;
        final int setBits;
        final int selectBit;
        switch (scenario) {
            case "collapse":
                builder.setCollapseConcurrency(0);
                selectBit = 4;
                clearBits = 4;
                setBits = 0;
                break;
            case "reassign":
                builder.setReassignConcurrency(0);
                selectBit = 2;
                clearBits = 0;
                setBits = 1;
                break;
            case "merge":
                builder.setMergeNumNearestClusters(0);
                selectBit = 1;
                clearBits = 1;
                setBits = 0;
                break;
            case "bits9Live":
            case "bits9Obsolete":
                // RaBitQ with unsupported extra bits 9, trained by the 11th insert, which
                // also arms the split: the task is queued before training.
                builder.setUseRaBitQ(true).setRaBitQNumExBits(9).setStatsThreshold(11)
                        .setSampleVectorStatsProbability(1.0).setMaintainStatsProbability(1.0);
                selectBit = scenario.equals("bits9Obsolete") ? 1 : 0;
                clearBits = 1;
                setBits = 0;
                break;
            default:
                throw new IllegalArgumentException("unknown scenario " + scenario);
        }
        Guardiann guardiann = new Guardiann(new Subspace(subspace), ForkJoinPool.commonPool(), builder.build(2),
                OnWriteListener.NOOP, OnReadListener.NOOP);
        Map<String, Object> result = new LinkedHashMap<>();
        List<Map<String, Object>> setup = new ArrayList<>();
        if (scenario.equals("collapse")) {
            for (int i = 0; i < 11; i++) {
                insert(clusterFile, tenantName, guardiann, 3000 + i, new double[] {5.0, -5.0});
            }
            setup.addAll(drainOneByOne(clusterFile, tenantName, guardiann));
        } else if (scenario.startsWith("bits9")) {
            for (int i = 0; i < 11; i++) {
                insert(clusterFile, tenantName, guardiann, i, knobNear(i));
            }
            result.put("trained", runInContext(clusterFile, tenantName, context ->
                    GuardiannConformanceAccess.trained(guardiann, context.ensureActive())));
        } else {
            for (int i = 0; i < 10; i++) {
                insert(clusterFile, tenantName, guardiann, i, knobNear(i));
            }
            for (int i = 0; i < 2; i++) {
                insert(clusterFile, tenantName, guardiann, 1000 + i, knobFar(i));
            }
            setup.addAll(drainOneByOne(clusterFile, tenantName, guardiann));
            for (int i = 10; i < 19; i++) {
                insert(clusterFile, tenantName, guardiann, i, knobNear(i));
            }
            setup.addAll(drainOneByOne(clusterFile, tenantName, guardiann));
            if (scenario.equals("merge")) {
                for (int i = 0; i < 18; i++) {
                    if (i != 3) {
                        delete(clusterFile, tenantName, guardiann, i, knobNear(i));
                    }
                }
                setup.addAll(drainOneByOne(clusterFile, tenantName, guardiann));
            }
        }
        result.put("setup", setup);
        result.put("clustersBefore", runInContext(clusterFile, tenantName, context ->
                GuardiannConformanceAccess.primaryCounts(guardiann, context.ensureActive())));
        result.put("tasksBefore", runInContext(clusterFile, tenantName, context ->
                GuardiannConformanceAccess.tasks(guardiann, context.ensureActive())));
        result.put("staged", runInContext(clusterFile, tenantName, context -> {
            List<Integer> staged = new ArrayList<>();
            List<Map<String, Object>> clusters = GuardiannConformanceAccess.primaryCounts(guardiann, context.ensureActive());
            for (int c = 0; c < clusters.size(); c++) {
                int states = (Integer) clusters.get(c).get("states");
                if ((states & selectBit) != 0) {
                    staged.add(GuardiannConformanceAccess.setStates(guardiann, context.ensureActive(), c,
                            (states & ~clearBits) | setBits));
                }
            }
            return staged;
        }));
        result.put("drain", drainOneByOne(clusterFile, tenantName, guardiann));
        result.put("tasksAfter", runInContext(clusterFile, tenantName, context ->
                GuardiannConformanceAccess.tasks(guardiann, context.ensureActive())));
        result.put("clustersAfter", runInContext(clusterFile, tenantName, context ->
                GuardiannConformanceAccess.primaryCounts(guardiann, context.ensureActive())));
        return result;
    }

    /**
     * Inline maintenance reached through the RECORD LAYER: every save runs with
     * IndexDeferredMaintenanceControl.autoMergeDuringCommit, which VectorIndexMaintainer passes to
     * the engine as maintainInTransaction. Saves nearCount near vectors, then farCount far vectors,
     * then tail more near vectors, one record per transaction, and reports every save's outcome.
     */
    @ConformanceStep("guardiannRecordInlineProbe")
    public Map<String, Object> guardiannRecordInlineProbe(String clusterFile, long nearCount, long farCount, long tail,
                                                          String hardMax, boolean autoMerge) {
        var metadata = guardiannRecordMetaData(hardMax);
        var index = metadata.getIndex("gv");
        var space = new Subspace(Tuple.from("guardiann-record-inline-probe", UUID.randomUUID().toString()));
        Map<String, Object> result = new LinkedHashMap<>();
        try {
            runInContext(clusterFile, "", context -> {
                com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore.newBuilder()
                        .setMetaDataProvider(metadata).setContext(context).setSubspace(space).create();
                return null;
            });
            List<double[]> vectors = new ArrayList<>();
            for (int i = 0; i < nearCount; i++) {
                vectors.add(near(i));
            }
            for (int i = 0; i < farCount; i++) {
                vectors.add(far(i));
            }
            for (int i = 0; i < tail; i++) {
                vectors.add(near((int) nearCount + i));
            }
            List<Map<String, Object>> saves = new ArrayList<>();
            for (int i = 0; i < vectors.size(); i++) {
                final int id = i;
                saves.add(phase(() -> runInContext(clusterFile, "", context -> {
                    var store = com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore.newBuilder()
                            .setMetaDataProvider(metadata).setContext(context).setSubspace(space).open();
                    store.getIndexDeferredMaintenanceControl().setAutoMergeDuringCommit(autoMerge);
                    var descriptor = metadata.getRecordType("Order").getDescriptor();
                    store.saveRecord(com.google.protobuf.DynamicMessage.newBuilder(descriptor)
                            .setField(descriptor.findFieldByName("order_id"), (long) id)
                            .setField(descriptor.findFieldByName("vector_data"),
                                    com.google.protobuf.ByteString.copyFrom(new DoubleRealVector(vectors.get(id)).getRawData()))
                            .build());
                    return null;
                })));
            }
            result.put("saves", saves);
            runInContext(clusterFile, "", context -> {
                var store = com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore.newBuilder()
                        .setMetaDataProvider(metadata).setContext(context).setSubspace(space).open();
                result.put("clusters", GuardiannConformanceAccess.primaryCountsAt(
                        store.indexSubspace(index), context.ensureActive()));
                return null;
            });
        } finally {
            runInContext(clusterFile, "", context -> {
                context.ensureActive().clear(space.range());
                return null;
            });
        }
        return result;
    }
}
