package com.birdayz.conformance;

import com.apple.foundationdb.FDBException;
import com.apple.foundationdb.record.PlanHashable;
import com.apple.foundationdb.record.PlanSerializationContext;
import com.apple.foundationdb.record.planprotos.PQueryPlanConstraint;
import com.apple.foundationdb.record.provider.foundationdb.APIVersion;
import com.apple.foundationdb.record.provider.foundationdb.FDBDatabase;
import com.apple.foundationdb.record.provider.foundationdb.FDBDatabaseFactory;
import com.apple.foundationdb.record.provider.foundationdb.FDBExceptions;
import com.apple.foundationdb.record.provider.foundationdb.FormatVersion;
import com.apple.foundationdb.record.provider.foundationdb.keyspace.KeySpace;
import com.apple.foundationdb.record.query.plan.QueryPlanConstraint;
import com.apple.foundationdb.record.query.plan.cascades.predicates.ConstantPredicate;
import com.apple.foundationdb.record.query.plan.serialization.DefaultPlanSerializationRegistry;
import com.apple.foundationdb.relational.api.Continuation;
import com.apple.foundationdb.relational.api.EmbeddedRelationalDriver;
import com.apple.foundationdb.relational.api.EmbeddedRelationalEngine;
import com.apple.foundationdb.relational.api.Options;
import com.apple.foundationdb.relational.api.RelationalConnection;
import com.apple.foundationdb.relational.api.RelationalPreparedStatement;
import com.apple.foundationdb.relational.api.RelationalResultSet;
import com.apple.foundationdb.relational.api.RelationalStruct;
import com.apple.foundationdb.relational.api.StructMetaData;
import com.apple.foundationdb.relational.api.Transaction;
import com.apple.foundationdb.relational.api.catalog.StoreCatalog;
import com.apple.foundationdb.relational.continuation.CompiledStatement;
import com.apple.foundationdb.relational.continuation.ContinuationProto;
import com.apple.foundationdb.relational.recordlayer.ContinuationImpl;
import com.apple.foundationdb.relational.recordlayer.DirectFdbConnection;
import com.apple.foundationdb.relational.recordlayer.RecordLayerConfig;
import com.apple.foundationdb.relational.recordlayer.RecordLayerEngine;
import com.apple.foundationdb.relational.recordlayer.RelationalKeyspaceProvider;
import com.apple.foundationdb.relational.recordlayer.catalog.StoreCatalogProvider;
import com.apple.foundationdb.relational.recordlayer.ddl.RecordLayerMetadataOperationsFactory;
import com.codahale.metrics.MetricRegistry;

import com.google.gson.JsonArray;
import com.google.gson.JsonNull;
import com.google.gson.JsonObject;
import com.google.gson.JsonPrimitive;
import com.google.protobuf.ByteString;

import java.io.File;
import java.io.FileWriter;
import java.io.IOException;
import java.sql.DriverManager;
import java.sql.ResultSetMetaData;
import java.sql.SQLException;
import java.sql.Statement;
import java.util.Base64;
import java.util.Collections;
import java.util.UUID;
import java.util.concurrent.ThreadLocalRandom;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.concurrent.atomic.AtomicInteger;

/**
 * Conformance step exposing the Java fdb-relational planner's EXPLAIN output
 * to the Go plan-equivalence harness (RFC-022 §4.-1 Phase 2). Each call to
 * {@code planSql} creates a unique schema template + database + schema,
 * runs {@code EXPLAIN <sql>} via fdb-relational's JDBC embedded driver, and
 * returns the PLAN column as a JSON string.
 *
 * Lifecycle: the underlying {@link EmbeddedRelationalEngine} + JDBC driver
 * are initialised lazily on first {@code planSql} call (per cluster file)
 * and cached for subsequent calls — this matches the cost model of
 * {@link ConformanceBase#createDatabase}, where setup is per-server-process.
 *
 * Schema isolation: each call generates a unique {@code TEMPLATE_<uuid>} +
 * {@code /TEST/PLAN_DIFF_<uuid>} pair, so two calls with overlapping
 * schema_template strings don't collide.
 */
class SqlPlanSteps {

    /**
     * Lazy global init of the fdb-relational driver. The driver is keyed
     * by cluster-file content; a second {@code planSql} with a different
     * cluster-file string is unsupported (the harness uses one FDB
     * testcontainer per session) and would require tearing the driver
     * down — out of scope for the seed.
     */
    private static final Object SETUP_LOCK = new Object();
    private static final AtomicBoolean SETUP_DONE = new AtomicBoolean(false);
    private static String setupClusterContent = null;

    private static void ensureDriverRegistered(String clusterFileContent) throws Exception {
        synchronized (SETUP_LOCK) {
            if (SETUP_DONE.get() && clusterFileContent.equals(setupClusterContent)) {
                return;
            }
            if (SETUP_DONE.get()) {
                throw new IllegalStateException(
                    "SqlPlanSteps: cluster-file content changed mid-session; only one cluster per server lifetime supported");
            }

            // Mirror EmbeddedRelationalExtension.setup() but as a non-JUnit
            // resource — register the driver once and leave it. The
            // conformance server is shared across many test suites; a
            // sibling test may have already initialised the FDB client
            // before we get here, in which case setAPIVersion would
            // throw RecordCoreException("API version cannot be changed
            // after client has already started"). Tolerate that — the
            // existing API version is fine for our purposes (we only
            // need a working FDBDatabaseFactory; the relational driver
            // doesn't depend on a specific API version).
            try {
                FDBDatabaseFactory.instance().setAPIVersion(APIVersion.API_VERSION_7_1);
            } catch (Exception e) {
                if (e.getMessage() == null || !e.getMessage().contains("API version cannot be changed")) {
                    throw e;
                }
                // already inited with some other API version — fine.
            }

            // Unique per server PROCESS — NOT a fixed /tmp path. The A3 harness
            // runs a POOL of conformance servers spawned concurrently; a shared
            // cluster-file path would let one process read another's half-written
            // file. createTempFile gives each server its own path.
            File tempFile = File.createTempFile("fdb_sql_plan_steps_", ".cluster");
            tempFile.deleteOnExit();
            try (FileWriter writer = new FileWriter(tempFile)) {
                writer.write(clusterFileContent);
            }
            String clusterFilePath = tempFile.getAbsolutePath();

            RelationalKeyspaceProvider keyspaceProvider = RelationalKeyspaceProvider.instance();
            keyspaceProvider.registerDomainIfNotExists("TEST");
            KeySpace keySpace = keyspaceProvider.getKeySpace();

            FDBDatabase database = FDBDatabaseFactory.instance().getDatabase(clusterFilePath);
            StoreCatalog storeCatalog;
            try (DirectFdbConnection connection = new DirectFdbConnection(database);
                 Transaction txn = connection.getTransactionManager().createTransaction(Options.NONE)) {
                storeCatalog = StoreCatalogProvider.getCatalog(txn, keySpace);
                txn.commit();
            }

            RecordLayerConfig config = new RecordLayerConfig.RecordLayerConfigBuilder()
                .setFormatVersion(FormatVersion.getDefaultFormatVersion())
                .build();
            RecordLayerMetadataOperationsFactory ddlFactory = RecordLayerMetadataOperationsFactory.defaultFactory()
                .setBaseKeySpace(keySpace)
                .setRlConfig(config)
                .setStoreCatalog(storeCatalog)
                .build();
            // Plan cache DISABLED (null) on purpose. The engine is built once and
            // shared across every conformance request; a live RelationalPlanCache is
            // the one piece of cross-query MUTABLE state in that shared engine. Its
            // entries are evicted on a wall-clock TTL, so whether query N hits or
            // misses the cache depends on timing — which makes Java's observable
            // behaviour (and, on a re-plan miss, error-path state) differ run-to-run
            // and machine-to-machine. makeEngine's planCache param is @Nullable and
            // AbstractEmbeddedStatement wraps it in Optional.ofNullable, so null is the
            // canonical "cache disabled" path: PlanGenerator sees Optional.empty() and
            // plans every query fresh + statelessly. Determinism > the cache's speed
            // win here — this is a conformance oracle, not a production server.
            EmbeddedRelationalEngine engine = RecordLayerEngine.makeEngine(
                config,
                Collections.singletonList(database),
                keySpace,
                storeCatalog,
                new MetricRegistry(),
                ddlFactory,
                /* planCache = */ null);

            DriverManager.registerDriver(new EmbeddedRelationalDriver(engine));
            setupClusterContent = clusterFileContent;
            SETUP_DONE.set(true);
        }
    }

    /**
     * Plan a SQL statement and return the EXPLAIN PLAN column. Creates a
     * uniquely-named schema template + database + schema for this call,
     * runs {@code EXPLAIN sql}, drops everything in {@code finally}.
     *
     * @param clusterFile     cluster-file content (string, not path)
     * @param schemaTemplate  body of CREATE SCHEMA TEMPLATE — sequence of
     *                        DDL statements (CREATE TYPE / CREATE TABLE /
     *                        CREATE INDEX). May be empty for SELECT-with-no-
     *                        FROM cases, in which case no schema is set on
     *                        the connection before EXPLAIN.
     * @param sql             the SQL to plan. EXPLAIN is prepended internally
     *                        so callers pass the bare SELECT / DML.
     * @return                the PLAN column text (one line per plan node,
     *                        indented by depth — fdb-relational's standard
     *                        EXPLAIN render).
     */
    @ConformanceStep("planSql")
    public String planSql(String clusterFile, String schemaTemplate, String sql) throws Exception {
        return runWithEphemeralSchema(clusterFile, schemaTemplate, conn -> runExplain(conn, sql));
    }

    /**
     * Run a SQL statement and return the result set as a JSON object with
     * {@code columns} and {@code rows} fields (Phase B / Track A1 of TODO.md).
     * Mirrors {@link #planSql} but executes the SQL instead of EXPLAINing it.
     *
     * <p>Result shape:
     * <pre>
     * {
     *   "columns": [{"name": "ID", "type": "BIGINT"}, ...],
     *   "rows":    [[1, "alice"], [2, null], ...]
     * }
     * </pre>
     *
     * <p>Type coverage: Number / Boolean / String values pass through as
     * native JSON; {@code byte[]} values are base64-encoded; SQL NULL maps to
     * JSON null. Anything else (java.sql.Array / Struct / vendor types) is
     * encoded as {@code {"__unsupported__": "<class>"}} so the diff harness
     * can flag it without crashing.
     *
     * @param clusterFile     cluster-file content (string, not path)
     * @param schemaTemplate  body of CREATE SCHEMA TEMPLATE — sequence of
     *                        DDL statements. Empty for SELECT-with-no-FROM.
     * @param sql             the SQL to run.
     * @return                a {@link JsonObject} (gson serialises directly).
     */
    @ConformanceStep("runSql")
    public JsonObject runSql(String clusterFile, String schemaTemplate, String sql) throws Exception {
        return runWithEphemeralSchema(clusterFile, schemaTemplate, conn -> runQuery(conn, sql));
    }

    /**
     * Run a sequence of setup DMLs (INSERT / UPDATE / DELETE) followed by
     * a SELECT — all in the same ephemeral schema. Returns the SELECT's
     * result set in the same JSON shape as {@link #runSql}.
     *
     * <p>Used by round-trip type-coverage tests: each {@link #runSql} call
     * uses a fresh ephemeral schema, so INSERT-then-SELECT in two calls
     * doesn't share state. {@code runWithSetup} keeps the schema alive
     * for the whole sequence.
     *
     * <p>Setup statements run via {@link Statement#executeUpdate}; the
     * affected-row count is discarded. Errors during setup propagate as
     * {@link SQLException} and are returned as a typed Java exception by
     * the conformance server.
     *
     * @param clusterFile     cluster-file content (string, not path)
     * @param schemaTemplate  body of CREATE SCHEMA TEMPLATE
     * @param setupSqls       DML statements to run before the query
     * @param querySql        the SELECT to run; its RowSet is returned
     * @return                JSON RowSet of the {@code querySql} result
     */
    @ConformanceStep("runWithSetup")
    public JsonObject runWithSetup(String clusterFile, String schemaTemplate,
                                    java.util.List<String> setupSqls, String querySql) throws Exception {
        return runWithEphemeralSchema(clusterFile, schemaTemplate, conn -> {
            try (Statement st = conn.createStatement()) {
                for (String setup : setupSqls) {
                    withFdbRetry(() -> st.executeUpdate(setup));
                }
            }
            return runQuery(conn, querySql);
        });
    }

    /**
     * TEST-ONLY: like {@link #runWithSetup} but injects {@code faultCount}
     * synthetic {@link FDBException}s (code {@code faultCode}) before the query
     * actually executes, exercising the {@link #withFdbRetry} path
     * deterministically — that path is otherwise only reachable under real
     * CI-load {@code 1007 transaction_too_old} bursts. The injected exception
     * is a genuine {@code FDBException}, so the production retry predicate
     * ({@link #isRetryableNotCommitted}, which calls FDB's native classifier)
     * sees exactly what it would for a live error.
     *
     * <p>Isolation: the countdown is a method-local {@link AtomicInteger}
     * captured by the query lambda — no static/shared state, so concurrent
     * requests (or a pooled server) can't see each other's injection. The
     * setup statements run normally (so the table is populated); injection
     * applies only to the SELECT, the documented primary culprit.
     *
     * <p>Behavioural contract this pins (see fault_inject_retry_conformance_test.go):
     * a retryable-not-committed code (1007/1020/…) under the attempt budget →
     * the query recovers and returns correct rows; exhausting the budget →
     * surfaces; a maybe-committed code (1021) or a non-retryable code (1000) →
     * surfaces immediately with NO retry (proving writes are never replayed).
     */
    @ConformanceStep("runWithSetupInjectingFaults")
    public JsonObject runWithSetupInjectingFaults(String clusterFile, String schemaTemplate,
                                                  java.util.List<String> setupSqls, String querySql,
                                                  int faultCount, int faultCode) throws Exception {
        return runWithSetupInjectingFaults(
            clusterFile, schemaTemplate, setupSqls, querySql, faultCount, faultCode, false);
    }

    /**
     * TEST-ONLY raw-carrier sibling of {@link #runWithSetupInjectingFaults}.
     * Real result-set iteration can surface {@link FDBException} directly
     * (it is a {@link RuntimeException}) instead of wrapping it in
     * {@link SQLException}. Keep both carriers pinned because the retry
     * boundary must classify the FDB cause, not depend on the JDBC wrapper.
     */
    @ConformanceStep("runWithSetupInjectingRawFaults")
    public JsonObject runWithSetupInjectingRawFaults(String clusterFile, String schemaTemplate,
                                                     java.util.List<String> setupSqls, String querySql,
                                                     int faultCount, int faultCode) throws Exception {
        return runWithSetupInjectingFaults(
            clusterFile, schemaTemplate, setupSqls, querySql, faultCount, faultCode, true);
    }

    private JsonObject runWithSetupInjectingFaults(String clusterFile, String schemaTemplate,
                                                   java.util.List<String> setupSqls, String querySql,
                                                   int faultCount, int faultCode,
                                                   boolean rawFault) throws Exception {
        final AtomicInteger remaining = new AtomicInteger(faultCount);
        return runWithEphemeralSchema(clusterFile, schemaTemplate, conn -> {
            try (Statement st = conn.createStatement()) {
                for (String setup : setupSqls) {
                    withFdbRetry(() -> st.executeUpdate(setup));
                }
            }
            RelationalConnection rconn = conn.unwrap(RelationalConnection.class);
            return withFdbRetry(() -> {
                if (remaining.getAndDecrement() > 0) {
                    FDBException fault = new FDBException("injected_test_fault", faultCode);
                    if (rawFault) {
                        throw fault;
                    }
                    throw new SQLException("injected test fault", fault);
                }
                try (RelationalPreparedStatement ps = rconn.prepareStatement(querySql);
                     RelationalResultSet rs = ps.executeQuery()) {
                    return resultSetToJson(rs);
                }
            });
        });
    }

    /** Table the {@link #continuationProbe} probes read; four rows, all sharing
     *  the same {@code B} value so the bytes-literal predicate is selective on
     *  nothing and the first page always leaves a resumable continuation. */
    private static final String CONTINUATION_PROBE_SCHEMA =
            "CREATE TABLE T (ID BIGINT, B BYTES, N BIGINT, PRIMARY KEY (ID))";

    private static final String CONTINUATION_PROBE_SEED =
            "INSERT INTO T VALUES (1, x'0a0b', 1), (2, x'0a0b', 2), (3, x'0a0b', 3), (4, x'0a0b', 4)";

    /**
     * Measures three EXECUTE CONTINUATION behaviours of the live Java engine
     * that a Go continuation envelope has to be designed against. Every probe
     * is independently guarded, so one probe blowing up still yields the other
     * two — the point is a measurement, not a pass/fail.
     *
     * <p>Probe A — an unknown {@code plan_serialization_mode}. Java resolves it
     * with {@code PlanHashMode.valueOf(...)} inside
     * {@code PlanValidator.validateSerializedPlanSerializationMode} BEFORE the
     * {@code validPlanHashModes.contains} membership test, so the failure mode
     * for a foreign mode string is whatever {@code Enum.valueOf} does, not the
     * typed INVALID_CONTINUATION the membership test would raise. Reports the
     * thrown class, message, SQLSTATE and full cause chain.
     *
     * <p>Probe B — the plan constraint on the EXECUTE CONTINUATION path.
     * {@code ContinuedPhysicalQueryPlan#validatePlanAgainstEnvironment} calls
     * {@code PlanValidator.validateContinuationConstraint} inside a try that
     * catches {@code PlanValidationException}, counts CONTINUATION_REJECTED,
     * and then falls through to CONTINUATION_ACCEPTED. The probe takes a real
     * continuation, swaps ONLY its {@code plan_constraint} for a constant-false
     * predicate, and reports whether the query still returns rows. Two control
     * runs bracket it: the untouched continuation bytes (isolating the swap as
     * the only variable) and the same doctored bytes with {@code plan_hash}
     * additionally perturbed by one, which Java rejects unconditionally in
     * {@code PlanGenerator#generatePhysicalPlanForCompiledStatementContinuation}.
     * The second control is what makes the constraint result a measurement
     * rather than a blind spot: the doctored bytes provably reach validation.
     *
     * <p>Probe C — binding-hash stability across two executions of the same
     * query text. {@code AstNormalizer#processLiteral} folds each literal into
     * the parameter hash with {@code Objects.hash(canonicalName, literal)};
     * for a {@code x'..'} literal that literal is a {@code byte[]}, whose
     * {@code hashCode()} is identity. An integer literal is the control.
     *
     * @param clusterFile cluster-file content (string, not path)
     * @return a {@link JsonObject} with one group of fields per probe
     */
    @ConformanceStep("continuationProbe")
    public JsonObject continuationProbe(String clusterFile) throws Exception {
        return runWithEphemeralSchema(clusterFile, CONTINUATION_PROBE_SCHEMA, conn -> {
            final JsonObject out = new JsonObject();
            try (Statement st = conn.createStatement()) {
                withFdbRetry(() -> st.executeUpdate(CONTINUATION_PROBE_SEED));
                out.addProperty("seed_ok", true);
            } catch (SQLException | RuntimeException e) {
                recordThrowable(out, "seed", e);
                out.addProperty("seed_ok", false);
                return out;
            }
            final RelationalConnection rconn = conn.unwrap(RelationalConnection.class);
            probeGoV0Mode(rconn, out);
            probeFalsePlanConstraint(rconn, out);
            probeBindingHashStability(rconn, out);
            return out;
        });
    }

    /**
     * Probe A: hand-build a continuation whose compiled statement claims a
     * {@code plan_serialization_mode} the Java enum has never heard of, and
     * carries no plan at all, then feed it to EXECUTE CONTINUATION.
     */
    private static void probeGoV0Mode(RelationalConnection rconn, JsonObject out) {
        final byte[] bytes = ContinuationProto.newBuilder()
                .setVersion(ContinuationImpl.CURRENT_VERSION)
                .setExecutionState(ByteString.copyFrom(new byte[] {0x01, 0x02, 0x03, 0x04}))
                .setBindingHash(1)
                .setPlanHash(1)
                .setCompiledStatement(CompiledStatement.newBuilder()
                        .setPlanSerializationMode("GO_V0")
                        .build())
                .build()
                .toByteArray();
        out.addProperty("probeA_continuationBytesLen", bytes.length);
        try (RelationalPreparedStatement ps = rconn.prepareStatement("EXECUTE CONTINUATION ?continuation")) {
            ps.setBytes("continuation", bytes);
            ps.execute();
            out.addProperty("probeA_threw", false);
        } catch (Throwable t) {
            recordThrowable(out, "probeA", t);
        }
    }

    /**
     * Probe B: take a genuine mid-stream continuation, replace ONLY its
     * {@code plan_constraint} with {@code ConstantPredicate.FALSE}, and resume.
     * Every other field — plan, plan hash, binding hash, execution state,
     * literals — is byte-identical to the continuation Java itself emitted.
     */
    private static void probeFalsePlanConstraint(RelationalConnection rconn, JsonObject out) {
        final byte[] realBytes;
        try (RelationalPreparedStatement ps = rconn.prepareStatement("SELECT * FROM T")) {
            ps.setMaxRows(1);
            try (RelationalResultSet rs = ps.executeQuery()) {
                int n = 0;
                while (rs.next()) {
                    n++;
                }
                out.addProperty("probeB_firstPageRows", n);
                realBytes = rs.getContinuation().serialize();
            }
        } catch (Throwable t) {
            recordThrowable(out, "probeB_setup", t);
            return;
        }

        final byte[] doctoredBytes;
        final byte[] hashPerturbedBytes;
        try {
            final ContinuationProto orig = ContinuationProto.parseFrom(realBytes);
            out.addProperty("probeB_origHasCompiledStatement", orig.hasCompiledStatement());
            final CompiledStatement origStmt = orig.getCompiledStatement();
            final String mode = origStmt.getPlanSerializationMode();
            out.addProperty("probeB_planSerializationMode", mode);
            out.addProperty("probeB_origHasPlanConstraint", origStmt.hasPlanConstraint());
            out.addProperty("probeB_origPlanConstraintProto",
                    truncate(origStmt.getPlanConstraint().toString(), 400));

            // The false constraint carries no type-dictionary references
            // (ConstantPredicate serialises to a bare PAbstractQueryPredicate +
            // a bool), so building it in a fresh serialization context cannot
            // desynchronise the plan/arguments/constraints dictionary order the
            // deserialiser relies on.
            final PlanSerializationContext sctx = new PlanSerializationContext(
                    DefaultPlanSerializationRegistry.INSTANCE, PlanHashable.PlanHashMode.valueOf(mode));
            final PQueryPlanConstraint falseConstraint =
                    QueryPlanConstraint.ofPredicate(ConstantPredicate.FALSE).toProto(sctx);
            out.addProperty("probeB_falsePlanConstraintProto",
                    truncate(falseConstraint.toString(), 400));

            doctoredBytes = orig.toBuilder()
                    .setCompiledStatement(origStmt.toBuilder().setPlanConstraint(falseConstraint).build())
                    .build()
                    .toByteArray();

            // Same doctored bytes, plus a perturbed plan_hash. The ONLY delta
            // from `doctoredBytes` is the int32 plan_hash field, so if this run
            // is rejected and the other is not, the doctored continuation
            // demonstrably reaches Java's validation chain — the constraint
            // fail-open is a property of the constraint gate, not an artifact
            // of the probe feeding Java bytes it silently ignores.
            out.addProperty("probeB_origPlanHash", orig.getPlanHash());
            out.addProperty("probeB_perturbedPlanHash", orig.getPlanHash() + 1);
            hashPerturbedBytes = orig.toBuilder()
                    .setCompiledStatement(origStmt.toBuilder().setPlanConstraint(falseConstraint).build())
                    .setPlanHash(orig.getPlanHash() + 1)
                    .build()
                    .toByteArray();
        } catch (Throwable t) {
            recordThrowable(out, "probeB_doctor", t);
            return;
        }

        // Control: the untouched continuation, so the swap is the only variable.
        try (RelationalPreparedStatement ps = rconn.prepareStatement("EXECUTE CONTINUATION ?continuation")) {
            ps.setBytes("continuation", realBytes);
            try (RelationalResultSet rs = ps.executeQuery()) {
                int rows = 0;
                while (rs.next()) {
                    rows++;
                }
                out.addProperty("probeB_control_threw", false);
                out.addProperty("probeB_control_rowsReturned", rows);
            }
        } catch (Throwable t) {
            recordThrowable(out, "probeB_control", t);
        }

        try (RelationalPreparedStatement ps = rconn.prepareStatement("EXECUTE CONTINUATION ?continuation")) {
            ps.setBytes("continuation", doctoredBytes);
            try (RelationalResultSet rs = ps.executeQuery()) {
                int rows = 0;
                while (rs.next()) {
                    rows++;
                }
                out.addProperty("probeB_threw", false);
                out.addProperty("probeB_rowsReturned", rows);
            }
        } catch (Throwable t) {
            recordThrowable(out, "probeB", t);
        }

        // Contrast arm: identical to the doctored run except for plan_hash.
        // PlanGenerator#generatePhysicalPlanForCompiledStatementContinuation
        // compares continuation.getPlanHash() against the DESERIALISED plan's
        // planHash and raises PlanValidationException -> INVALID_CONTINUATION
        // (24F00) on mismatch, unguarded by any catch. This arm therefore
        // establishes that the probe can see Java refuse a doctored
        // continuation at all, which is what makes the constraint arm's
        // acceptance a measurement rather than a blind spot.
        try (RelationalPreparedStatement ps = rconn.prepareStatement("EXECUTE CONTINUATION ?continuation")) {
            ps.setBytes("continuation", hashPerturbedBytes);
            try (RelationalResultSet rs = ps.executeQuery()) {
                int rows = 0;
                while (rs.next()) {
                    rows++;
                }
                out.addProperty("probeB_hashPerturbed_threw", false);
                out.addProperty("probeB_hashPerturbed_rowsReturned", rows);
            }
        } catch (Throwable t) {
            recordThrowable(out, "probeB_hashPerturbed", t);
        }
    }

    /**
     * Probe C: two executions of the same query text, same connection, same
     * JVM — do they agree on the binding hash Java stamps into the
     * continuation? Bytes literal versus integer-literal control.
     */
    private static void probeBindingHashStability(RelationalConnection rconn, JsonObject out) {
        final String bytesQuery = "SELECT * FROM T WHERE B = x'0a0b'";
        final String intQuery = "SELECT * FROM T WHERE N = 3";
        try {
            final Integer b1 = bindingHashOf(rconn, bytesQuery);
            final Integer b2 = bindingHashOf(rconn, bytesQuery);
            addNullableInt(out, "probeC_bytes_hash1", b1);
            addNullableInt(out, "probeC_bytes_hash2", b2);
            out.addProperty("probeC_bytes_equal", java.util.Objects.equals(b1, b2));

            final Integer i1 = bindingHashOf(rconn, intQuery);
            final Integer i2 = bindingHashOf(rconn, intQuery);
            addNullableInt(out, "probeC_int_hash1", i1);
            addNullableInt(out, "probeC_int_hash2", i2);
            out.addProperty("probeC_int_equal", java.util.Objects.equals(i1, i2));
        } catch (Throwable t) {
            recordThrowable(out, "probeC", t);
        }
    }

    /**
     * Run {@code sql} with a one-row page, drain it, and return the binding
     * hash Java stamped into the resulting continuation. A fresh
     * {@code prepareStatement} per call is deliberate: the whole question is
     * whether two independent parses of identical text agree.
     */
    private static Integer bindingHashOf(RelationalConnection rconn, String sql) throws Exception {
        try (RelationalPreparedStatement ps = rconn.prepareStatement(sql)) {
            ps.setMaxRows(1);
            try (RelationalResultSet rs = ps.executeQuery()) {
                while (rs.next()) {
                    // drain: getContinuation() rejects a non-exhausted result set
                }
                final Continuation continuation = rs.getContinuation();
                return ContinuationImpl.parseContinuation(continuation.serialize()).getBindingHash();
            }
        }
    }

    private static void addNullableInt(JsonObject out, String name, Integer value) {
        if (value == null) {
            out.add(name, JsonNull.INSTANCE);
        } else {
            out.addProperty(name, value);
        }
    }

    private static String truncate(String s, int max) {
        final String oneLine = s.replace('\n', ' ').trim();
        return oneLine.length() <= max ? oneLine : oneLine.substring(0, max) + "...";
    }

    /**
     * Record a probe outcome as {@code <prefix>_threw / _exceptionClass /
     * _message / _sqlState / _causeChain}. The cause walk is bounded so a
     * self-referential or cyclic chain cannot spin.
     */
    private static void recordThrowable(JsonObject out, String prefix, Throwable t) {
        out.addProperty(prefix + "_threw", true);
        out.addProperty(prefix + "_exceptionClass", t.getClass().getName());
        out.addProperty(prefix + "_message", String.valueOf(t.getMessage()));
        if (t instanceof SQLException) {
            out.addProperty(prefix + "_sqlState", ((SQLException) t).getSQLState());
        } else {
            out.add(prefix + "_sqlState", JsonNull.INSTANCE);
        }
        final StringBuilder chain = new StringBuilder();
        Throwable c = t;
        for (int depth = 0; c != null && depth < 32; depth++) {
            if (chain.length() > 0) {
                chain.append(" <- ");
            }
            chain.append(c.getClass().getName());
            if (c.getCause() == c) {
                break;
            }
            c = c.getCause();
        }
        out.addProperty(prefix + "_causeChain", chain.toString());
    }

    /**
     * Persistently create a SchemaTemplate via JDBC's
     * {@code CREATE SCHEMA TEMPLATE} (without auto-drop). The template lives
     * in the system catalog at the standard subspace
     * {@code (NULL, NULL, int64(0))}. Used by Track A2 cross-language
     * SchemaTemplateCatalog round-trip: Go's
     * {@code catalog.OpenRecordLayerStoreCatalog()} (also at the
     * {@code (NULL, NULL, int64(0))} subspace via
     * {@code DefaultCatalogSubspace}) can then read this template.
     *
     * Caller is responsible for cleanup via
     * {@link #dropSchemaTemplatePersistentJava}.
     */
    @ConformanceStep("createSchemaTemplatePersistentJava")
    public JsonObject createSchemaTemplatePersistentJava(String clusterFile,
                                                         String templateName,
                                                         String schemaTemplateBody) throws Exception {
        ensureDriverRegistered(clusterFile);
        try (java.sql.Connection sysConn = DriverManager.getConnection(
                "jdbc:embed:/__SYS?schema=CATALOG");
             Statement st = sysConn.createStatement()) {
            st.executeUpdate("CREATE SCHEMA TEMPLATE \"" + templateName + "\" " + schemaTemplateBody);
        }
        JsonObject result = new JsonObject();
        result.addProperty("created", true);
        result.addProperty("templateName", templateName);
        return result;
    }

    /**
     * Best-effort drop of a persistently-created SchemaTemplate. Used as
     * cleanup after {@link #createSchemaTemplatePersistentJava}.
     */
    @ConformanceStep("dropSchemaTemplatePersistentJava")
    public JsonObject dropSchemaTemplatePersistentJava(String clusterFile,
                                                       String templateName) throws Exception {
        ensureDriverRegistered(clusterFile);
        boolean dropped = false;
        try (java.sql.Connection sysConn = DriverManager.getConnection(
                "jdbc:embed:/__SYS?schema=CATALOG");
             Statement st = sysConn.createStatement()) {
            st.executeUpdate("DROP SCHEMA TEMPLATE IF EXISTS \"" + templateName + "\"");
            dropped = true;
        } catch (SQLException e) {
            // fdb-relational 4.11.1.0 ignores `IF EXISTS` on DROP
            // SCHEMA TEMPLATE — throws on absent template anyway.
            // Tolerate the specific "not found" path for idempotency.
            if (e.getMessage() == null || !e.getMessage().toLowerCase().contains("not found")) {
                throw e;
            }
        }
        JsonObject result = new JsonObject();
        result.addProperty("dropped", dropped);
        return result;
    }

    /**
     * Wraps a JDBC operation in the ephemeral schema-template / database /
     * schema lifecycle. Both {@link #planSql} and {@link #runSql} drive
     * fdb-relational the same way: create a uniquely-named template + db
     * + schema if a {@code schemaTemplate} is supplied, run the operation,
     * tear everything down in {@code finally}. Empty template falls back to
     * the {@code /__SYS} connection.
     */
    @FunctionalInterface
    private interface ConnectionOp<T> {
        T run(java.sql.Connection conn) throws SQLException;
    }

    private <T> T runWithEphemeralSchema(String clusterFile, String schemaTemplate, ConnectionOp<T> op) throws Exception {
        ensureDriverRegistered(clusterFile);

        String suffix = UUID.randomUUID().toString().replace("-", "");
        String templateName = "PLAN_DIFF_T_" + suffix;
        String dbPath = "/TEST/PLAN_DIFF_" + suffix;
        String schemaName = "S_" + suffix;
        boolean templateCreated = false;
        boolean dbCreated = false;

        try {
            if (schemaTemplate != null && !schemaTemplate.isEmpty()) {
                // The /__SYS database has a system "CATALOG" schema that
                // accepts CREATE SCHEMA TEMPLATE / CREATE DATABASE / CREATE
                // SCHEMA DDL. AbstractEmbeddedStatement#executeInternal
                // requires conn.getSchema() to be non-null, so we MUST set
                // the schema before executing DDL — fdb-relational tests
                // do the same (SchemaTemplateRule#beforeEach).
                try (java.sql.Connection sysConn = DriverManager.getConnection("jdbc:embed:/__SYS?schema=CATALOG");
                     Statement st = sysConn.createStatement()) {
                    withFdbRetry(() -> st.executeUpdate("CREATE SCHEMA TEMPLATE \"" + templateName + "\" " + schemaTemplate));
                    templateCreated = true;
                    withFdbRetry(() -> st.executeUpdate("CREATE DATABASE \"" + dbPath + "\""));
                    dbCreated = true;
                    withFdbRetry(() -> st.executeUpdate("CREATE SCHEMA \"" + dbPath + "/" + schemaName + "\" WITH TEMPLATE \"" + templateName + "\""));
                }

                // fdb-relational reads the active schema from the
                // JDBC URL's query string (`?schema=NAME`, case-
                // insensitive — RecordLayerStorageCluster#parseConnectionQueryString).
                // Calling Connection.setSchema() on the JDBC wrapper
                // does NOT propagate to EmbeddedRelationalConnection's
                // currentSchemaLabel — every executeQuery / executeUpdate
                // would fail with "No Schema specified".
                try (java.sql.Connection conn = DriverManager.getConnection(
                        "jdbc:embed:" + dbPath + "?schema=" + schemaName)) {
                    return op.run(conn);
                }
            }
            // No schema — fall back to __SYS. SELECT-without-FROM works here.
            try (java.sql.Connection conn = DriverManager.getConnection("jdbc:embed:/__SYS")) {
                return op.run(conn);
            }
        } finally {
            if (dbCreated) {
                try (java.sql.Connection sysConn = DriverManager.getConnection("jdbc:embed:/__SYS");
                     Statement st = sysConn.createStatement()) {
                    // Teardown is best-effort and the path is a unique UUID, so a
                    // transient 1007 here just leaks one ephemeral DB — not worth a
                    // retry loop holding the cleanup path. catch+ignore as before.
                    st.executeUpdate("DROP DATABASE IF EXISTS \"" + dbPath + "\"");
                } catch (SQLException ignored) {
                    // teardown best-effort — a stuck DB is preferable to swallowing the
                    // primary exception from the caller's try block.
                }
            }
            if (templateCreated) {
                try (java.sql.Connection sysConn = DriverManager.getConnection("jdbc:embed:/__SYS");
                     Statement st = sysConn.createStatement()) {
                    st.executeUpdate("DROP SCHEMA TEMPLATE IF EXISTS \"" + templateName + "\"");
                } catch (SQLException ignored) {
                    // ditto
                }
            }
        }
    }

    /** Max attempts for a single auto-commit statement that hits a
     *  retryable-and-not-committed FDB error (e.g. 1007 transaction_too_old
     *  under heavy parallel A3 load). Six tries with jittered backoff (five
     *  sleeps, base 50→800ms; with full jitter up to ~3s total) ride out a
     *  transient contention spike; a genuinely wedged box still fails loudly. */
    private static final int MAX_FDB_RETRIES = 6;

    @FunctionalInterface
    private interface SqlSupplier<T> {
        T get() throws SQLException;
    }

    /**
     * True iff {@code t}'s cause chain carries an FDB error that is RETRYABLE
     * and DEFINITELY NOT COMMITTED — the class {@link FDBException#isRetryableNotCommitted()}
     * defines: 1007 transaction_too_old, 1020 not_committed, 1009 future_version,
     * 1037 process_behind, … but EXCLUDING 1021 commit_unknown_result.
     *
     * <p>The exclusion is load-bearing: a maybe-committed write (1021) must
     * never be blindly replayed — re-running a setup INSERT / CREATE DDL that
     * actually committed would duplicate a row (spurious 23505) or silently
     * double-insert and corrupt the SELECT. Restricting to the not-committed
     * class keeps the retry idempotent for the write paths as well as reads.
     */
    private static boolean isRetryableNotCommitted(Throwable t) {
        FDBException fdb = FDBExceptions.getFDBCause(t);
        if (fdb != null) {
            return fdb.isRetryableNotCommitted();
        }
        // No raw FDBException in the chain. isRetriable() still catches the
        // record layer's RecordCoreRetriableTransactionException wrappers
        // (conflict / lock-taken) — those are not-committed by construction,
        // so they're safe to retry.
        return FDBExceptions.isRetriable(t);
    }

    /**
     * Execute a single auto-commit JDBC statement, retrying on a
     * retryable-and-not-committed FDB error. Under heavy parallel A3 load the
     * shared JVM thread can be starved long enough that fdb-relational's
     * (plan-cache-disabled, so always-fresh) Cascades plan + execute overruns
     * FDB's 5s transaction window, surfacing {@code 1007 transaction_too_old}.
     * That error is precisely what FDB's {@code FDBDatabase#run()} retry loop
     * exists to absorb; the conformance server bypasses that loop (raw JDBC
     * {@code executeQuery}), so we add it here.
     *
     * <p>Safe because every conformance connection is auto-commit
     * ({@code EmbeddedRelationalConnection} defaults {@code autoCommit=true} and
     * we never disable it), so each statement is its own transaction — and we
     * retry only the {@link #isRetryableNotCommitted} class, which by
     * definition did NOT commit. Re-running it on a fresh transaction is
     * therefore idempotent for both reads and the setup writes.
     *
     * <p>This cannot mask a real Go-vs-Java divergence: the predicate is true
     * ONLY for genuine infra/timing errors. A semantic divergence (wrong plan,
     * type mismatch, parse error) surfaces as a non-retryable
     * {@code RelationalException} with a SQLState — the predicate returns false,
     * it's rethrown immediately, and the spec still fails loudly.
     */
    private static <T> T withFdbRetry(SqlSupplier<T> op) throws SQLException {
        for (int attempt = 0; attempt < MAX_FDB_RETRIES; attempt++) {
            try {
                return op.get();
            } catch (SQLException | RuntimeException e) {
                if (!isRetryableNotCommitted(e)) {
                    throw e;
                }
                if (attempt == MAX_FDB_RETRIES - 1) {
                    throw e; // out of budget — don't back off just to give up
                }
                // Exponential backoff (base 50ms..800ms) with additive jitter:
                // sleep in [base, 2*base), so there's always a backoff floor and
                // the jitter decorrelates the wakeups of pool threads that all
                // hit 1007 at the same instant, so the retries don't thunder back
                // in lockstep.
                long base = Math.min(50L << attempt, 800L);
                try {
                    Thread.sleep(base + ThreadLocalRandom.current().nextLong(base));
                } catch (InterruptedException ie) {
                    Thread.currentThread().interrupt();
                    throw e;
                }
            }
        }
        throw new AssertionError("positive retry budget exhausted without returning or throwing");
    }

    private String runExplain(java.sql.Connection conn, String sql) throws SQLException {
        // fdb-relational accepts EXPLAIN as a SQL prefix; the result set has
        // a PLAN column (VARCHAR) carrying the rendered tree. Other columns
        // (PLAN_HASH, PLAN_DOT, PLAN_GML, PLAN_CONTINUATION, PLANNER_METRICS)
        // are diagnostic; the harness only diffs PLAN today.
        RelationalConnection rconn = conn.unwrap(RelationalConnection.class);
        return withFdbRetry(() -> {
            try (RelationalPreparedStatement ps = rconn.prepareStatement("EXPLAIN " + sql);
                 RelationalResultSet rs = ps.executeQuery()) {
                if (!rs.next()) {
                    return "";
                }
                String plan = rs.getString("PLAN");
                return plan == null ? "" : plan;
            }
        });
    }

    /**
     * Execute a SQL query and serialise the result set as JSON. Encoder rules
     * are documented on {@link #runSql}.
     */
    private JsonObject runQuery(java.sql.Connection conn, String sql) throws SQLException {
        RelationalConnection rconn = conn.unwrap(RelationalConnection.class);
        return withFdbRetry(() -> {
            try (RelationalPreparedStatement ps = rconn.prepareStatement(sql);
                 RelationalResultSet rs = ps.executeQuery()) {
                return resultSetToJson(rs);
            }
        });
    }

    /**
     * Encode a {@link RelationalResultSet} as a {@link JsonObject} with
     * {@code columns} (name + JDBC type-name) and {@code rows} (array of
     * value arrays). Visible for tests via reflection.
     */
    static JsonObject resultSetToJson(RelationalResultSet rs) throws SQLException {
        ResultSetMetaData md = rs.getMetaData();
        int n = md.getColumnCount();

        JsonArray cols = new JsonArray(n);
        for (int i = 1; i <= n; i++) {
            JsonObject c = new JsonObject();
            c.addProperty("name", md.getColumnName(i));
            c.addProperty("type", md.getColumnTypeName(i));
            cols.add(c);
        }

        JsonArray rows = new JsonArray();
        while (rs.next()) {
            JsonArray row = new JsonArray(n);
            for (int i = 1; i <= n; i++) {
                row.add(encodeValue(rs.getObject(i)));
            }
            rows.add(row);
        }

        JsonObject out = new JsonObject();
        out.add("columns", cols);
        out.add("rows", rows);
        return out;
    }

    /**
     * Encode a single column value. Numbers, booleans, and strings pass
     * through as native JSON. {@code byte[]} is base64-encoded as a string.
     * SQL NULL → JSON null. Unknown types render as
     * {@code {"__unsupported__": "<class>"}} so the diff harness can flag
     * them rather than crash.
     */
    private static com.google.gson.JsonElement encodeValue(Object v) {
        if (v == null) {
            return JsonNull.INSTANCE;
        }
        if (v instanceof Number) {
            // Gson's JsonPrimitive((Number)) emits a bare token for the
            // numeric value, which produces invalid JSON when the value
            // is +Infinity / -Infinity / NaN (Gson writes 'Infinity',
            // '-Infinity', 'NaN' literals — not valid JSON, the Go HTTP
            // client unmarshal rejects them). For floats / doubles we
            // detect the IEEE-754 specials and encode them as strings
            // ("Infinity", "-Infinity", "NaN") so the JSON stays valid;
            // the harness on either side decodes the string back to the
            // appropriate float when comparing.
            if (v instanceof Double) {
                double d = (Double) v;
                if (Double.isInfinite(d)) {
                    return new JsonPrimitive(d > 0 ? "Infinity" : "-Infinity");
                }
                if (Double.isNaN(d)) {
                    return new JsonPrimitive("NaN");
                }
            } else if (v instanceof Float) {
                float f = (Float) v;
                if (Float.isInfinite(f)) {
                    return new JsonPrimitive(f > 0 ? "Infinity" : "-Infinity");
                }
                if (Float.isNaN(f)) {
                    return new JsonPrimitive("NaN");
                }
            }
            return new JsonPrimitive((Number) v);
        }
        if (v instanceof Boolean) {
            return new JsonPrimitive((Boolean) v);
        }
        if (v instanceof String) {
            return new JsonPrimitive((String) v);
        }
        if (v instanceof byte[]) {
            return new JsonPrimitive(Base64.getEncoder().encodeToString((byte[]) v));
        }
        if (v instanceof java.util.UUID) {
            return new JsonPrimitive(v.toString());
        }
        // A STRUCT column renders as its ATTRIBUTES, keyed by the struct
        // metadata's column names. Without this a struct was indistinguishable
        // from any other unsupported class, so a probe could see that a column
        // was typed STRUCT but never what it CONTAINED — which is exactly the
        // question when deciding whether Java wraps a scalar into a one-field
        // record or returns the scalar itself.
        if (v instanceof RelationalStruct) {
            RelationalStruct s = (RelationalStruct) v;
            JsonObject obj = new JsonObject();
            try {
                StructMetaData md = s.getMetaData();
                int count = md.getColumnCount();
                for (int i = 1; i <= count; i++) {
                    obj.add(md.getColumnName(i), encodeValue(s.getObject(i)));
                }
            } catch (SQLException e) {
                obj.addProperty("__struct_error__", e.getMessage());
            }
            return obj;
        }
        // An ARRAY renders as a JSON array of its materialized elements, each
        // through the same encoder — an array OF structs is the composition,
        // not a second mechanism.
        if (v instanceof java.sql.Array) {
            JsonArray arr = new JsonArray();
            try {
                Object raw = ((java.sql.Array) v).getArray();
                int len = java.lang.reflect.Array.getLength(raw);
                for (int i = 0; i < len; i++) {
                    arr.add(encodeValue(java.lang.reflect.Array.get(raw, i)));
                }
            } catch (SQLException | IllegalArgumentException e) {
                JsonObject err = new JsonObject();
                err.addProperty("__array_error__", String.valueOf(e.getMessage()));
                arr.add(err);
            }
            return arr;
        }
        JsonObject marker = new JsonObject();
        marker.addProperty("__unsupported__", v.getClass().getName());
        return marker;
    }
}
