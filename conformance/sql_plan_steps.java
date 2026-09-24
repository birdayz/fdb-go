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
    /** The database and relational keyspace the registered engine uses (set once with it). */
    private static FDBDatabase sharedDatabase = null;
    private static KeySpace sharedKeySpace = null;

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
            sharedDatabase = database;
            sharedKeySpace = keySpace;
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
     * Like {@link #runWithSetup}, but the statement is a prepared statement whose parameters are
     * bound through the JDBC setters rather than written into the SQL text (see
     * {@link #bindExtended} for the parameter kinds, positional or named), with CONNECTION options
     * set on the connection before the statement is prepared ({@code connectionOptions}:
     * {@link Options.Name} to its value), and a DML mode: when {@code update} is true the statement
     * runs through executeUpdate and {@code followUpSql} then runs as a plain query in the same
     * connection; its RowSet is returned with the statement's update count in the
     * {@code updateCount} field (not a column).
     */
    @ConformanceStep("runPreparedExtended")
    public JsonObject runPreparedExtended(String clusterFile, String schemaTemplate,
                                          java.util.List<String> setupSqls, String querySql,
                                          java.util.List<java.util.Map<String, Object>> params,
                                          java.util.Map<String, Object> connectionOptions,
                                          boolean update, String followUpSql) throws Exception {
        return runWithEphemeralSchema(clusterFile, schemaTemplate, conn -> {
            try (Statement st = conn.createStatement()) {
                for (String setup : setupSqls) {
                    withFdbRetry(() -> st.executeUpdate(setup));
                }
            }
            RelationalConnection rconn = conn.unwrap(RelationalConnection.class);
            for (java.util.Map.Entry<String, Object> e : connectionOptions.entrySet()) {
                rconn.setOption(Options.Name.valueOf(e.getKey()), e.getValue());
            }
            return withFdbRetry(() -> {
                try (RelationalPreparedStatement ps = rconn.prepareStatement(querySql)) {
                    int position = 1;
                    for (java.util.Map<String, Object> p : params) {
                        String name = String.valueOf(p.getOrDefault("name", ""));
                        if (bindExtended(rconn, ps, name, position, p)) {
                            position++;
                        }
                    }
                    if (!update) {
                        try (RelationalResultSet rs = ps.executeQuery()) {
                            return resultSetToJson(rs);
                        }
                    }
                    int count = ps.executeUpdate();
                    try (Statement follow = conn.createStatement();
                         RelationalResultSet rs = follow.executeQuery(followUpSql).unwrap(RelationalResultSet.class)) {
                        JsonObject out = resultSetToJson(rs);
                        out.addProperty("updateCount", count);
                        return out;
                    }
                } catch (ReflectiveOperationException e) {
                    throw new IllegalArgumentException(e);
                }
            });
        });
    }

    /**
     * Executes one statement text once per parameter set, in order, on ONE connection of one
     * ephemeral schema. With {@code reuseStatement} the statement is prepared once and
     * re-bound; without it each execution prepares the text afresh. Each execution's answer
     * is its own element of {@code results}: a result set, or
     * {@code {sqlState, message, exceptionClass}}. This engine has NO plan cache (it is built
     * with {@code planCache = null}, see the engine's construction), so every execution is
     * planned fresh for its binding: the step measures the plan each binding gets, never a
     * cached plan's reuse, and the target's plan-constraint safety for a cached plan stays a
     * source claim.
     */
    @ConformanceStep("runPreparedSequence")
    public JsonObject runPreparedSequence(String clusterFile, String schemaTemplate, java.util.List<String> setupSqls,
                                         String querySql,
                                         java.util.List<java.util.List<java.util.Map<String, Object>>> paramSets,
                                         boolean reuseStatement) throws Exception {
        return runWithEphemeralSchema(clusterFile, schemaTemplate, conn -> {
            try (Statement st = conn.createStatement()) {
                for (String setup : setupSqls) {
                    withFdbRetry(() -> st.executeUpdate(setup));
                }
            }
            RelationalConnection rconn = conn.unwrap(RelationalConnection.class);
            JsonArray results = new JsonArray();
            RelationalPreparedStatement shared = reuseStatement ? rconn.prepareStatement(querySql) : null;
            try {
                for (java.util.List<java.util.Map<String, Object>> params : paramSets) {
                    RelationalPreparedStatement ps = shared != null ? shared : rconn.prepareStatement(querySql);
                    try {
                        int position = 1;
                        for (java.util.Map<String, Object> p : params) {
                            String name = String.valueOf(p.getOrDefault("name", ""));
                            if (bindExtended(rconn, ps, name, position, p)) {
                                position++;
                            }
                        }
                        try (RelationalResultSet rs = ps.executeQuery()) {
                            results.add(resultSetToJson(rs));
                        }
                    } catch (SQLException | RuntimeException | ReflectiveOperationException e) {
                        Throwable root = e;
                        while (root.getCause() != null) {
                            root = root.getCause();
                        }
                        JsonObject err = new JsonObject();
                        err.addProperty("sqlState", e instanceof SQLException ? ((SQLException) e).getSQLState() : "");
                        err.addProperty("message", root.getMessage() != null ? root.getMessage() : root.getClass().getName());
                        err.addProperty("exceptionClass", root.getClass().getSimpleName());
                        results.add(err);
                    } finally {
                        if (shared == null) {
                            ps.close();
                        }
                    }
                }
            } finally {
                if (shared != null) {
                    shared.close();
                }
            }
            JsonObject out = new JsonObject();
            out.add("results", results);
            return out;
        });
    }

    /** Binds one parameter; returns true when it consumed a positional ordinal. */
    private static boolean bindExtended(RelationalConnection conn, RelationalPreparedStatement ps, String name,
                                        int position, java.util.Map<String, Object> p)
            throws SQLException, ReflectiveOperationException {
        String kind = String.valueOf(p.get("kind"));
        Object value = p.get("value");
        boolean positional = name.isEmpty();
        switch (kind) {
            case "null": {
                int sqlType = java.sql.Types.class.getField(String.valueOf(p.get("sqlType"))).getInt(null);
                if (positional) { ps.setNull(position, sqlType); } else { ps.setNull(name, sqlType); }
                break;
            }
            case "long":
                if (positional) { ps.setLong(position, ((Number) value).longValue()); } else { ps.setLong(name, ((Number) value).longValue()); }
                break;
            case "int":
                if (positional) { ps.setInt(position, ((Number) value).intValue()); } else { ps.setInt(name, ((Number) value).intValue()); }
                break;
            case "string":
                if (positional) { ps.setString(position, String.valueOf(value)); } else { ps.setString(name, String.valueOf(value)); }
                break;
            case "double":
                if (positional) { ps.setDouble(position, ((Number) value).doubleValue()); } else { ps.setDouble(name, ((Number) value).doubleValue()); }
                break;
            case "float":
                if (positional) { ps.setFloat(position, ((Number) value).floatValue()); } else { ps.setFloat(name, ((Number) value).floatValue()); }
                break;
            case "boolean":
                if (positional) { ps.setBoolean(position, (Boolean) value); } else { ps.setBoolean(name, (Boolean) value); }
                break;
            case "objectNull":
                if (positional) { ps.setObject(position, null); } else { ps.setObject(name, null); }
                break;
            case "uuid": {
                java.util.UUID u = java.util.UUID.fromString(String.valueOf(value));
                if (positional) { ps.setUUID(position, u); } else { ps.setUUID(name, u); }
                break;
            }
            case "longArray": {
                java.util.List<?> items = (java.util.List<?>) value;
                Object[] elements = new Object[items.size()];
                for (int i = 0; i < elements.length; i++) {
                    elements[i] = ((Number) items.get(i)).longValue();
                }
                java.sql.Array array = conn.createArrayOf("BIGINT", elements);
                if (positional) { ps.setArray(position, array); } else { ps.setArray(name, array); }
                break;
            }
            default:
                throw new IllegalArgumentException("unknown parameter kind " + kind);
        }
        return positional;
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
    /**
     * Runs {@code setupSqls} and then {@code querySql} (EXPLAIN included) in a schema created over an
     * EXISTING schema template {@code templateName}, one another engine stored in the shared catalog.
     * The database is dropped afterwards; the template is left to its creator. This is the
     * "Go stores, Java plans" direction: the target plans over the stored key expressions exactly as
     * they were written, not over the ones it would generate itself.
     */
    @ConformanceStep("runOnExistingTemplateJava")
    public JsonObject runOnExistingTemplateJava(String clusterFile, String templateName,
                                                java.util.List<String> setupSqls, String querySql) throws Exception {
        ensureDriverRegistered(clusterFile);
        String suffix = UUID.randomUUID().toString().replace("-", "");
        String dbPath = "/TEST/EXISTING_T_" + suffix;
        String schemaName = "S_" + suffix;
        boolean dbCreated = false;
        boolean opFailed = false;
        try {
            try (java.sql.Connection sysConn = DriverManager.getConnection(SYS_CATALOG_URL);
                 Statement st = sysConn.createStatement()) {
                withFdbRetry(() -> st.executeUpdate("CREATE DATABASE \"" + dbPath + "\""));
                dbCreated = true;
                withFdbRetry(() -> st.executeUpdate("CREATE SCHEMA \"" + dbPath + "/" + schemaName + "\" WITH TEMPLATE \"" + templateName + "\""));
            }
            try (java.sql.Connection conn = DriverManager.getConnection("jdbc:embed:" + dbPath + "?schema=" + schemaName)) {
                try (Statement st = conn.createStatement()) {
                    for (String setup : setupSqls) {
                        withFdbRetry(() -> st.executeUpdate(setup));
                    }
                }
                return withFdbRetry(() -> runQuery(conn, querySql));
            }
        } catch (Exception | Error primary) {
            opFailed = true;
            teardown(dbCreated, dbPath, false, null, primary);
            throw primary;
        } finally {
            if (!opFailed) {
                teardown(dbCreated, dbPath, false, null, null);
            }
        }
    }

    /**
     * TEST-ONLY (RFC-257 WS-J F2b): creates a database and a schema bound to the existing
     * template {@code templateName} and KEEPS them, returning where the schema's record store
     * lives ({@code dbPath}, {@code schemaName}, and {@code storePrefix}, the resolved subspace
     * bytes), so a Go record layer can write into the same store. The caller drops the
     * database with {@link #wsjDropDatabaseJava}.
     */
    @ConformanceStep("wsjOpenStoreJava")
    public JsonObject wsjOpenStoreJava(String clusterFile, String templateName) throws Exception {
        ensureDriverRegistered(clusterFile);
        String suffix = UUID.randomUUID().toString().replace("-", "");
        String dbPath = "/TEST/WSJ_STORE_" + suffix;
        String schemaName = "S_" + suffix;
        try (java.sql.Connection sysConn = DriverManager.getConnection(SYS_CATALOG_URL);
             Statement st = sysConn.createStatement()) {
            withFdbRetry(() -> st.executeUpdate("CREATE DATABASE \"" + dbPath + "\""));
            withFdbRetry(() -> st.executeUpdate("CREATE SCHEMA \"" + dbPath + "/" + schemaName + "\" WITH TEMPLATE \"" + templateName + "\""));
        }
        // Open the store once through SQL so its header exists before a Go writer opens it.
        try (java.sql.Connection conn = DriverManager.getConnection("jdbc:embed:" + dbPath + "?schema=" + schemaName);
             Statement st = conn.createStatement()) {
            st.executeQuery("SELECT * FROM T").close();
        }
        byte[] prefix;
        try (com.apple.foundationdb.record.provider.foundationdb.FDBRecordContext ctx = sharedDatabase.openContext()) {
            prefix = RelationalKeyspaceProvider.toDatabasePath(java.net.URI.create(dbPath), sharedKeySpace)
                    .schemaPath(schemaName).toSubspace(ctx).getKey();
        }
        JsonObject out = new JsonObject();
        out.addProperty("dbPath", dbPath);
        out.addProperty("schemaName", schemaName);
        JsonArray bytes = new JsonArray();
        for (byte b : prefix) {
            bytes.add(b & 0xff);
        }
        out.add("storePrefix", bytes);
        return out;
    }

    /** TEST-ONLY: drops a database {@link #wsjOpenStoreJava} kept. */
    @ConformanceStep("wsjDropDatabaseJava")
    public JsonObject wsjDropDatabaseJava(String clusterFile, String dbPath) throws Exception {
        ensureDriverRegistered(clusterFile);
        try (java.sql.Connection sysConn = DriverManager.getConnection(SYS_CATALOG_URL);
             Statement st = sysConn.createStatement()) {
            withFdbRetry(() -> st.executeUpdate("DROP DATABASE IF EXISTS \"" + dbPath + "\""));
        }
        JsonObject out = new JsonObject();
        out.addProperty("dropped", true);
        return out;
    }

    /**
     * TEST-ONLY (RFC-257 WS-J F2b): inserts rows {@code (id, d, n)} into table T of a kept
     * store through a prepared INSERT with setLong/setDouble, so NaN, the infinities and
     * 2^63 reach the record as doubles ({@code rows}: [id, d, n], d as a JSON number or one
     * of the strings "NaN", "Infinity", "-Infinity").
     */
    @ConformanceStep("wsjInsertDoublesJava")
    public JsonObject wsjInsertDoublesJava(String clusterFile, String dbPath, String schemaName,
                                           java.util.List<java.util.List<Object>> rows) throws Exception {
        ensureDriverRegistered(clusterFile);
        try (java.sql.Connection conn = DriverManager.getConnection("jdbc:embed:" + dbPath + "?schema=" + schemaName);
             RelationalPreparedStatement ps = conn.unwrap(RelationalConnection.class)
                     .prepareStatement("INSERT INTO T VALUES (?, ?, ?, ?)")) {
            JsonArray outcomes = new JsonArray();
            for (java.util.List<Object> row : rows) {
                ps.setLong(1, ((Number) row.get(0)).longValue());
                Object d = row.get(1);
                ps.setDouble(2, d instanceof String ? Double.parseDouble((String) d) : ((Number) d).doubleValue());
                ps.setLong(3, ((Number) row.get(2)).longValue());
                // The FLOAT column takes the same value narrowed to float, as Go's writer does.
                Object f = row.get(3);
                ps.setFloat(4, f instanceof String ? Float.parseFloat((String) f) : ((Number) f).floatValue());
                try {
                    withFdbRetry(ps::executeUpdate);
                    outcomes.add("OK");
                } catch (SQLException e) {
                    Throwable root = e;
                    while (root.getCause() != null) {
                        root = root.getCause();
                    }
                    outcomes.add("ERROR " + e.getSQLState() + " " + root.getClass().getSimpleName() + " " + root.getMessage());
                }
            }
            JsonObject out = new JsonObject();
            out.add("outcomes", outcomes);
            return out;
        }
    }

    /**
     * TEST-ONLY (RFC-257 WS-E): runs one DML statement on a store {@link #wsjOpenStoreJava}
     * kept, returning "OK" and the update count, or "ERROR", the SQLSTATE, the root cause's
     * class and its message.
     */
    @ConformanceStep("wsjExecuteJava")
    public JsonObject wsjExecuteJava(String clusterFile, String dbPath, String schemaName, String sql) throws Exception {
        ensureDriverRegistered(clusterFile);
        JsonObject out = new JsonObject();
        try (java.sql.Connection conn = DriverManager.getConnection("jdbc:embed:" + dbPath + "?schema=" + schemaName);
             Statement st = conn.createStatement()) {
            int count = withFdbRetry(() -> st.executeUpdate(sql));
            out.addProperty("outcome", "OK " + count);
        } catch (SQLException e) {
            Throwable root = e;
            while (root.getCause() != null) {
                root = root.getCause();
            }
            out.addProperty("outcome", "ERROR " + e.getSQLState() + " " + root.getClass().getSimpleName() + " " + root.getMessage());
        }
        return out;
    }

    /** TEST-ONLY: runs one query on a store {@link #wsjOpenStoreJava} kept. */
    @ConformanceStep("wsjQueryJava")
    public JsonObject wsjQueryJava(String clusterFile, String dbPath, String schemaName, String querySql) throws Exception {
        ensureDriverRegistered(clusterFile);
        try (java.sql.Connection conn = DriverManager.getConnection("jdbc:embed:" + dbPath + "?schema=" + schemaName)) {
            return withFdbRetry(() -> runQuery(conn, querySql));
        }
    }

    /**
     * TEST-ONLY (RFC-257 WS-J F2b): every raw entry of index {@code indexName} in a kept store,
     * as the entry's key tuple (relative to the index subspace) rendered by Tuple.toString and
     * as lowercase hex (every byte, java.util.HexFormat), in key order.
     */
    @ConformanceStep("wsjIndexEntriesJava")
    public JsonObject wsjIndexEntriesJava(String clusterFile, String dbPath, String schemaName,
                                          String indexName) throws Exception {
        ensureDriverRegistered(clusterFile);
        JsonArray entries = new JsonArray();
        try (com.apple.foundationdb.record.provider.foundationdb.FDBRecordContext ctx = sharedDatabase.openContext()) {
            com.apple.foundationdb.subspace.Subspace store = RelationalKeyspaceProvider
                    .toDatabasePath(java.net.URI.create(dbPath), sharedKeySpace).schemaPath(schemaName).toSubspace(ctx);
            com.apple.foundationdb.subspace.Subspace index = store.subspace(
                    com.apple.foundationdb.tuple.Tuple.from(2L, indexName));
            for (com.apple.foundationdb.KeyValue kv : ctx.ensureActive().getRange(index.range()).asList().join()) {
                byte[] key = kv.getKey();
                byte[] rel = java.util.Arrays.copyOfRange(key, index.getKey().length, key.length);
                JsonObject e = new JsonObject();
                e.addProperty("tuple", com.apple.foundationdb.tuple.Tuple.fromBytes(rel).toString());
                e.addProperty("hex", java.util.HexFormat.of().formatHex(rel));
                entries.add(e);
            }
        }
        JsonObject out = new JsonObject();
        out.add("entries", entries);
        return out;
    }

    @ConformanceStep("createSchemaTemplatePersistentJava")
    public JsonObject createSchemaTemplatePersistentJava(String clusterFile,
                                                         String templateName,
                                                         String schemaTemplateBody) throws Exception {
        ensureDriverRegistered(clusterFile);
        try (java.sql.Connection sysConn = DriverManager.getConnection(SYS_CATALOG_URL);
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
        try (java.sql.Connection sysConn = DriverManager.getConnection(SYS_CATALOG_URL);
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

    /** A {@link ConnectionOp} that is also told the JDBC URL of its schema, to open more connections. */
    @FunctionalInterface
    private interface UrlConnectionOp<T> {
        T run(java.sql.Connection conn, String url) throws SQLException;
    }

    private <T> T runWithEphemeralSchema(String clusterFile, String schemaTemplate, ConnectionOp<T> op) throws Exception {
        return runWithEphemeralSchemaUrl(clusterFile, schemaTemplate, (conn, url) -> op.run(conn));
    }

    /** The URL every catalog DDL of the harness uses: the /__SYS database with its CATALOG schema. */
    private static final String SYS_CATALOG_URL = "jdbc:embed:/__SYS?schema=CATALOG";

    private <T> T runWithEphemeralSchemaUrl(String clusterFile, String schemaTemplate, UrlConnectionOp<T> op) throws Exception {
        return runWithEphemeralSchemaNamed(clusterFile, schemaTemplate, (conn, url, template) -> op.run(conn, url));
    }

    /** A {@link UrlConnectionOp} that is also told the name of the ephemeral schema template. */
    @FunctionalInterface
    private interface NamedConnectionOp<T> {
        T run(java.sql.Connection conn, String url, String templateName) throws SQLException;
    }

    private <T> T runWithEphemeralSchemaNamed(String clusterFile, String schemaTemplate, NamedConnectionOp<T> op) throws Exception {
        ensureDriverRegistered(clusterFile);

        String suffix = UUID.randomUUID().toString().replace("-", "");
        String templateName = "PLAN_DIFF_T_" + suffix;
        String dbPath = "/TEST/PLAN_DIFF_" + suffix;
        String schemaName = "S_" + suffix;
        boolean templateCreated = false;
        boolean dbCreated = false;
        boolean opFailed = false;

        try {
            if (schemaTemplate != null && !schemaTemplate.isEmpty()) {
                // The /__SYS database has a system "CATALOG" schema that
                // accepts CREATE SCHEMA TEMPLATE / CREATE DATABASE / CREATE
                // SCHEMA DDL. AbstractEmbeddedStatement#executeInternal
                // requires conn.getSchema() to be non-null, so we MUST set
                // the schema before executing DDL — fdb-relational tests
                // do the same (SchemaTemplateRule#beforeEach).
                try (java.sql.Connection sysConn = DriverManager.getConnection(SYS_CATALOG_URL);
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
                String url = "jdbc:embed:" + dbPath + "?schema=" + schemaName;
                try (java.sql.Connection conn = DriverManager.getConnection(url)) {
                    return op.run(conn, url, templateName);
                }
            }
            // No schema — fall back to __SYS. SELECT-without-FROM works here.
            try (java.sql.Connection conn = DriverManager.getConnection("jdbc:embed:/__SYS")) {
                return op.run(conn, "jdbc:embed:/__SYS", null);
            }
        } catch (Exception | Error primary) {
            // The operation's own failure is the one the caller sees; a teardown failure
            // rides along as suppressed instead of replacing it.
            opFailed = true;
            teardown(dbCreated, dbPath, templateCreated, templateName, primary);
            throw primary;
        } finally {
            if (!opFailed) {
                teardown(dbCreated, dbPath, templateCreated, templateName, null);
            }
        }
    }

    /**
     * Drops the ephemeral database and template through the SAME /__SYS CATALOG connection that
     * created them: a connection without the schema cannot run DDL (AbstractEmbeddedStatement
     * requires one), which is why the old teardown, on a schema-less connection with its exception
     * swallowed, left every ephemeral database and template behind. A failed drop is reported: as
     * a suppressed exception when the operation itself failed, and as the step's error when the
     * operation succeeded, so a leak can no longer pass silently.
     */
    private static void teardown(boolean dbCreated, String dbPath, boolean templateCreated, String templateName,
                                 Throwable primary) throws Exception {
        Exception failure = null;
        if (dbCreated) {
            try (java.sql.Connection sysConn = DriverManager.getConnection(SYS_CATALOG_URL);
                 Statement st = sysConn.createStatement()) {
                withFdbRetry(() -> st.executeUpdate("DROP DATABASE IF EXISTS \"" + dbPath + "\""));
            } catch (Exception e) {
                // withFdbRetry rethrows a RuntimeException as it is: every failure counts, so a
                // failed database drop never skips the template drop below.
                failure = e;
            }
        }
        if (templateCreated) {
            try (java.sql.Connection sysConn = DriverManager.getConnection(SYS_CATALOG_URL);
                 Statement st = sysConn.createStatement()) {
                withFdbRetry(() -> st.executeUpdate("DROP SCHEMA TEMPLATE IF EXISTS \"" + templateName + "\""));
            } catch (Exception e) {
                if (failure == null) {
                    failure = e;
                } else {
                    failure.addSuppressed(e);
                }
            }
        }
        if (failure == null) {
            return;
        }
        if (primary != null) {
            primary.addSuppressed(failure);
            return;
        }
        throw new IllegalStateException("ephemeral schema teardown failed for " + dbPath, failure);
    }

    /**
     * TEST-ONLY (RFC-257 WS-E section 6.4): measures what a read leaves in its transaction's
     * read-conflict set. Connection A (autocommit off) runs {@code readSql} (a SELECT, with or
     * without the statement option ISOLATION LEVEL SNAPSHOT, as written) and consumes every row;
     * connection B, on the same schema, then commits {@code concurrentSql}; A runs
     * {@code ownWriteSql}, so its commit is conflict-checked, and commits. Returns the read's
     * RowSet ({@code read}) and A's commit outcome ({@code commit}: "OK" or the SQLSTATE, class
     * and message of the failure).
     */
    @ConformanceStep("snapshotReadScopeProbe")
    public JsonObject snapshotReadScopeProbe(String clusterFile, String schemaTemplate,
                                             java.util.List<String> setupSqls, String readSql,
                                             String concurrentSql, String ownWriteSql) throws Exception {
        return runWithEphemeralSchemaUrl(clusterFile, schemaTemplate, (conn, url) -> {
            try (Statement st = conn.createStatement()) {
                for (String setup : setupSqls) {
                    withFdbRetry(() -> st.executeUpdate(setup));
                }
            }
            JsonObject out = new JsonObject();
            try (java.sql.Connection a = DriverManager.getConnection(url);
                 java.sql.Connection b = DriverManager.getConnection(url)) {
                a.setAutoCommit(false);
                try (Statement sa = a.createStatement();
                     RelationalResultSet rs = sa.executeQuery(readSql).unwrap(RelationalResultSet.class)) {
                    out.add("read", resultSetToJson(rs));
                }
                try (Statement sb = b.createStatement()) {
                    sb.executeUpdate(concurrentSql);
                }
                try (Statement sa = a.createStatement()) {
                    sa.executeUpdate(ownWriteSql);
                }
                try {
                    a.commit();
                    out.addProperty("commit", "OK");
                } catch (SQLException e) {
                    out.addProperty("commit", "ERROR " + e.getSQLState() + " " + e.getClass().getSimpleName()
                            + " " + e.getMessage());
                }
            }
            return out;
        });
    }

    /**
     * TEST-ONLY (RFC-257 WS-E): which index-state keys a SQL read adds read conflicts on. A reader
     * on connection A in an explicit transaction runs {@code readSql} (which may carry OPTIONS
     * (ISOLATION LEVEL SNAPSHOT)); then a separate FDB transaction sets the
     * index-state key of {@code indexName} in the schema's store to {@code state} (an IndexState
     * name) and commits; then A runs {@code ownWriteSql} (a write to a table the read does not
     * touch, so every conflict comes from the read) and commits. The line is the read's rows and
     * the commit outcome. The state key is written directly, (INDEX_STATE_SPACE, indexName) under
     * the store subspace, exactly the key FDBRecordStore reads and conflicts on.
     */
    @ConformanceStep("indexStateReadScopeProbe")
    public JsonObject indexStateReadScopeProbe(String clusterFile, String schemaTemplate,
                                               java.util.List<String> setupSqls, String readSql,
                                               String indexName, String state, String ownWriteSql) throws Exception {
        return runWithEphemeralSchemaUrl(clusterFile, schemaTemplate, (conn, url) -> {
            try (Statement st = conn.createStatement()) {
                for (String setup : setupSqls) {
                    withFdbRetry(() -> st.executeUpdate(setup));
                }
            }
            String path = url.substring("jdbc:embed:".length(), url.indexOf('?'));
            String schemaName = url.substring(url.indexOf("schema=") + "schema=".length());
            byte[] prefix;
            try (com.apple.foundationdb.record.provider.foundationdb.FDBRecordContext ctx = sharedDatabase.openContext()) {
                prefix = RelationalKeyspaceProvider.toDatabasePath(java.net.URI.create(path), sharedKeySpace)
                        .schemaPath(schemaName).toSubspace(ctx).getKey();
            } catch (com.apple.foundationdb.relational.api.exceptions.RelationalException e) {
                throw e.toSqlException();
            }
            byte[] stateKey = new com.apple.foundationdb.subspace.Subspace(prefix).pack(
                    com.apple.foundationdb.tuple.Tuple.from(
                            com.apple.foundationdb.record.provider.foundationdb.FDBRecordStoreKeyspace.INDEX_STATE_SPACE.key(),
                            indexName));
            byte[] stateValue = com.apple.foundationdb.tuple.Tuple.from(
                    com.apple.foundationdb.record.IndexState.valueOf(state).code()).pack();
            JsonObject out = new JsonObject();
            try (java.sql.Connection a = DriverManager.getConnection(url)) {
                a.setAutoCommit(false);
                try (Statement sa = a.createStatement();
                     RelationalResultSet rs = sa.executeQuery(readSql).unwrap(RelationalResultSet.class)) {
                    out.add("read", resultSetToJson(rs));
                }
                try (com.apple.foundationdb.record.provider.foundationdb.FDBRecordContext ctx = sharedDatabase.openContext()) {
                    ctx.ensureActive().set(stateKey, stateValue);
                    ctx.commit();
                }
                try (Statement sa = a.createStatement()) {
                    sa.executeUpdate(ownWriteSql);
                }
                try {
                    a.commit();
                    out.addProperty("commit", "OK");
                } catch (SQLException e) {
                    out.addProperty("commit", "ERROR " + e.getSQLState() + " " + e.getClass().getSimpleName()
                            + " " + e.getMessage());
                }
            }
            return out;
        });
    }

    /**
     * TEST-ONLY: what the target does with a stored index whose option list names one key
     * twice: builds the Index proto with {@code options} in the given order and reads it with
     * {@code new Index(proto)}, the constructor every stored index goes through, reporting the
     * exception class and message, or the options the Index kept.
     */
    @ConformanceStep("wsjDuplicateIndexOptionJava")
    public JsonObject wsjDuplicateIndexOptionJava(java.util.List<java.util.List<String>> options) {
        var builder = com.apple.foundationdb.record.RecordMetaDataProto.Index.newBuilder()
                .setName("IX")
                .setType("value")
                .addRecordType("T")
                .setRootExpression(com.apple.foundationdb.record.metadata.Key.Expressions.field("A").toKeyExpression());
        for (java.util.List<String> kv : options) {
            builder.addOptions(com.apple.foundationdb.record.RecordMetaDataProto.Index.Option.newBuilder()
                    .setKey(kv.get(0)).setValue(kv.get(1)));
        }
        JsonObject out = new JsonObject();
        try {
            var index = new com.apple.foundationdb.record.metadata.Index(builder.build());
            out.addProperty("outcome", "OK " + index.getOptions());
        } catch (RuntimeException e) {
            out.addProperty("outcome", "ERROR " + e.getClass().getName() + " " + e.getMessage());
        }
        return out;
    }

    /**
     * TEST-ONLY: what {@code new Index(proto)} makes of a stored Index proto: its root expression
     * (as serialized KeyExpression bytes), added and last-modified versions, type and options, or
     * the exception it raises. {@code indexProto} is the serialized RecordMetaDataProto.Index.
     */
    @ConformanceStep("wsjIndexFromProtoJava")
    public JsonObject wsjIndexFromProtoJava(java.util.List<Number> indexProto) throws Exception {
        byte[] bytes = new byte[indexProto.size()];
        for (int i = 0; i < bytes.length; i++) {
            bytes[i] = indexProto.get(i).byteValue();
        }
        JsonObject out = new JsonObject();
        try {
            var index = new com.apple.foundationdb.record.metadata.Index(
                    com.apple.foundationdb.record.RecordMetaDataProto.Index.parseFrom(bytes));
            JsonArray root = new JsonArray();
            for (byte b : index.getRootExpression().toKeyExpression().toByteArray()) {
                root.add(b & 0xff);
            }
            out.add("root", root);
            out.addProperty("addedVersion", index.getAddedVersion());
            out.addProperty("lastModifiedVersion", index.getLastModifiedVersion());
            out.addProperty("type", index.getType());
            out.addProperty("options", String.valueOf(index.getOptions()));
            out.addProperty("outcome", "OK");
        } catch (RuntimeException e) {
            out.addProperty("outcome", "ERROR " + e.getClass().getName() + " " + e.getMessage());
        }
        return out;
    }

    /**
     * TEST-ONLY: runs one ephemeral statement and then reports whether the harness's teardown
     * removed the ephemeral database and the schema template it created ({@code path},
     * {@code stillListed}; {@code template}, {@code templateStillListed}), read from SHOW
     * DATABASES and SHOW SCHEMA TEMPLATES through the catalog connection, with how many rows
     * each listing returned so an empty listing cannot pass for a removal. Pins that the
     * teardown drops what it creates.
     */
    @ConformanceStep("ephemeralTeardownProbe")
    public JsonObject ephemeralTeardownProbe(String clusterFile, String schemaTemplate) throws Exception {
        String[] names = runWithEphemeralSchemaNamed(clusterFile, schemaTemplate, (conn, u, t) -> new String[] {u, t});
        String path = names[0].substring("jdbc:embed:".length(), names[0].indexOf('?'));
        // The template name as the harness created it, not re-derived from the path.
        String template = names[1];
        boolean listed = false;
        int databases = 0;
        try (java.sql.Connection sysConn = DriverManager.getConnection(SYS_CATALOG_URL);
             Statement st = sysConn.createStatement();
             java.sql.ResultSet rs = st.executeQuery("SHOW DATABASES")) {
            while (rs.next()) {
                databases++;
                if (path.equals(rs.getString(1))) {
                    listed = true;
                }
            }
        }
        boolean templateListed = false;
        int templates = 0;
        try (java.sql.Connection sysConn = DriverManager.getConnection(SYS_CATALOG_URL);
             Statement st = sysConn.createStatement();
             java.sql.ResultSet rs = st.executeQuery("SHOW SCHEMA TEMPLATES")) {
            while (rs.next()) {
                templates++;
                if (template.equals(rs.getString(1))) {
                    templateListed = true;
                }
            }
        }
        JsonObject out = new JsonObject();
        out.addProperty("path", path);
        out.addProperty("stillListed", listed);
        out.addProperty("databasesListed", databases);
        out.addProperty("template", template);
        out.addProperty("templateStillListed", templateListed);
        out.addProperty("templatesListed", templates);
        return out;
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

    /**
     * A Cascades planner listener that tallies, per rule simple class name, the rule calls that
     * ENDED and what they yielded (final and exploratory expressions, and the simple class names of
     * the final ones). Registered only for the duration of one EXPLAIN on this thread
     * ({@code PlannerEventListeners} is thread-local).
     */
    private static final class RuleTraceListener implements
            com.apple.foundationdb.record.query.plan.cascades.events.PlannerEventListeners.EventListener {
        final java.util.Set<String> rules;
        final java.util.Map<String, int[]> counts = new java.util.TreeMap<>();
        final java.util.Map<String, java.util.TreeSet<String>> finalKinds = new java.util.TreeMap<>();
        // For the IN-union question: every final an ImplementInUnionRule call yielded, the
        // reference it went into and the planner configuration, so onDone can rank it against the
        // reference's other final members with the target's own PlanningCostModel.
        final java.util.List<Object[]> inUnionYields = new java.util.ArrayList<>();
        final java.util.List<String> comparisons = new java.util.ArrayList<>();

        // Two opt-in modes named by pseudo-rules in the rule list (no caller that names only
        // real rules sees either):
        //  - IN-UNION-PARTITIONS: at the end of each ImplementInUnionRule call, the inner
        //    reference the rule read, rolled up into the ordering partitions the rule iterates,
        //    and per partition and requested ordering the in-union ordering the rule builds and
        //    the comparison keys it enumerates. That is what the rule saw, not the survivor.
        //  - ROOT-PAIRS: the root reference's final members at its last PLANNING OptimizeGroup
        //    (before that group is pruned), each with its continuation plan hash, and the
        //    target's PlanningCostModel verdict on every pair beside the plan-hash order, so a
        //    verdict that only the hash decides shows as one that moves when names move.
        //  - REWRITING-RESULT: the query graph REWRITING handed to PLANNING, rendered at the
        //    first PLANNING event (every reference's members, which REWRITING's prune left at
        //    one), so a REWRITING survivor is measured rather than inferred from the winner.
        final boolean inUnionPartitions;
        final boolean rootPairs;
        final boolean rewritingResult;
        //  - TASK-COUNT: the number of tasks the planner executes, per phase, counted from the
        //    ExecutingTaskPlannerEvent each task begins with (CascadesPlanner counts the same
        //    tasks into QueryPlanInfoKeys.TOTAL_TASK_COUNT, per phase).
        final boolean taskCount;
        final java.util.Map<String, Integer> tasksPerPhase = new java.util.TreeMap<>();
        final java.util.Map<String, Integer> tasksPerKind = new java.util.TreeMap<>();
        String rewritingShape;
        final java.util.List<String> partitionLines = new java.util.ArrayList<>();
        com.apple.foundationdb.record.query.plan.cascades.Reference root;
        com.apple.foundationdb.record.query.plan.RecordQueryPlannerConfiguration rootConfig;
        java.util.List<com.apple.foundationdb.record.query.plan.cascades.expressions.RelationalExpression> rootMembers;

        RuleTraceListener(java.util.Collection<String> rules) {
            this.rules = new java.util.HashSet<>(rules);
            this.inUnionPartitions = this.rules.contains("IN-UNION-PARTITIONS");
            this.rootPairs = this.rules.contains("ROOT-PAIRS");
            this.rewritingResult = this.rules.contains("REWRITING-RESULT");
            this.taskCount = this.rules.contains("TASK-COUNT");
        }

        private static String shape(com.apple.foundationdb.record.query.plan.cascades.Reference ref, int depth) {
            if (depth > 40) {
                return "...";
            }
            var members = new java.util.ArrayList<>(ref.getFinalExpressions());
            if (members.isEmpty()) {
                members.addAll(ref.getExploratoryExpressions());
            }
            var rendered = new java.util.ArrayList<String>();
            for (var e : members) {
                StringBuilder sb = new StringBuilder(e.getClass().getSimpleName());
                if (e instanceof com.apple.foundationdb.record.query.plan.cascades.expressions.RelationalExpressionWithPredicates) {
                    // The predicates themselves, sorted, not only their count: two survivors
                    // that place different predicates must render differently.
                    var preds = new java.util.ArrayList<String>();
                    for (var p : ((com.apple.foundationdb.record.query.plan.cascades.expressions.RelationalExpressionWithPredicates) e).getPredicates()) {
                        preds.add(p.toString());
                    }
                    java.util.Collections.sort(preds);
                    sb.append("[preds=").append(preds.size()).append(": ").append(String.join(" AND ", preds)).append("]");
                }
                sb.append("(");
                boolean first = true;
                for (var q : e.getQuantifiers()) {
                    if (!first) {
                        sb.append(", ");
                    }
                    first = false;
                    String kind = q instanceof com.apple.foundationdb.record.query.plan.cascades.Quantifier.ForEach
                            ? (((com.apple.foundationdb.record.query.plan.cascades.Quantifier.ForEach) q).isNullOnEmpty() ? "forEachNullOnEmpty" : "forEach")
                            : q instanceof com.apple.foundationdb.record.query.plan.cascades.Quantifier.Existential ? "exists" : "physical";
                    sb.append(kind).append(":").append(shape(q.getRangesOver(), depth + 1));
                }
                sb.append(")");
                rendered.add(sb.toString());
            }
            java.util.Collections.sort(rendered);
            return rendered.size() == 1 ? rendered.get(0) : "{" + String.join(" | ", rendered) + "}";
        }

        @Override
        public void onQuery(String queryAsString, com.apple.foundationdb.record.query.plan.cascades.PlanContext planContext) {
        }

        @Override
        public void onEvent(com.apple.foundationdb.record.query.plan.cascades.events.PlannerEvent event) {
            if (taskCount && event instanceof com.apple.foundationdb.record.query.plan.cascades.events.ExecutingTaskPlannerEvent
                    && event.getLocation() == com.apple.foundationdb.record.query.plan.cascades.events.PlannerEvent.Location.BEGIN) {
                tasksPerPhase.merge(((com.apple.foundationdb.record.query.plan.cascades.events.ExecutingTaskPlannerEvent) event)
                        .getPlannerPhase().name(), 1, Integer::sum);
                tasksPerKind.merge("task " + ((com.apple.foundationdb.record.query.plan.cascades.events.ExecutingTaskPlannerEvent) event)
                        .getTask().getClass().getSimpleName(), 1, Integer::sum);
            }
            if (taskCount && event instanceof com.apple.foundationdb.record.query.plan.cascades.events.PlannerEventWithRule
                    && event.getLocation() == com.apple.foundationdb.record.query.plan.cascades.events.PlannerEvent.Location.BEGIN) {
                tasksPerKind.merge(event.getClass().getSimpleName() + " "
                        + ((com.apple.foundationdb.record.query.plan.cascades.events.PlannerEventWithRule) event).getRule().getClass().getSimpleName(),
                        1, Integer::sum);
            }
            if (rewritingResult && rewritingShape == null
                    && event instanceof com.apple.foundationdb.record.query.plan.cascades.events.PlannerEventWithState
                    && ((com.apple.foundationdb.record.query.plan.cascades.events.PlannerEventWithState) event).getPlannerPhase()
                        == com.apple.foundationdb.record.query.plan.cascades.PlannerPhase.PLANNING) {
                rewritingShape = shape(((com.apple.foundationdb.record.query.plan.cascades.events.PlannerEventWithState) event).getRootReference(), 0);
                comparisons.add("REWRITING-RESULT " + rewritingShape);
            }
            // A REWRITING prune after a simplification yielded: OptimizeGroup ranks the group's
            // final members with RewritingCostModel at exactly this point, with every child
            // reference still holding only logical members, so the criteria and the model's
            // verdict recorded here are the ones the prune uses. The group is not the reference
            // the simplification was called on (the select is re-memoized into a new reference
            // once its exploratory members are finalized), so every REWRITING group with more
            // than one final member is recorded.
            if (event instanceof com.apple.foundationdb.record.query.plan.cascades.events.OptimizeGroupPlannerEvent
                    && event.getLocation() == com.apple.foundationdb.record.query.plan.cascades.events.PlannerEvent.Location.BEGIN) {
                var og = (com.apple.foundationdb.record.query.plan.cascades.events.OptimizeGroupPlannerEvent) event;
                if (rootPairs && og.getPlannerPhase() == com.apple.foundationdb.record.query.plan.cascades.PlannerPhase.PLANNING
                        && root != null && og.getCurrentReference() == root) {
                    rootMembers = new java.util.ArrayList<>(root.getFinalExpressions());
                }
                if (og.getPlannerPhase() == com.apple.foundationdb.record.query.plan.cascades.PlannerPhase.REWRITING
                        && !simplifiedRefs.isEmpty() && og.getCurrentReference().getFinalExpressions().size() > 1) {
                    recordRewritingPrune(og.getCurrentReference(),
                            (com.apple.foundationdb.record.query.plan.RecordQueryPlannerConfiguration) simplifiedRefs.get(0)[1]);
                }
                return;
            }
            if (!(event instanceof com.apple.foundationdb.record.query.plan.cascades.events.TransformRuleCallPlannerEvent)
                    || event.getLocation() != com.apple.foundationdb.record.query.plan.cascades.events.PlannerEvent.Location.END) {
                return;
            }
            var call = (com.apple.foundationdb.record.query.plan.cascades.events.TransformRuleCallPlannerEvent) event;
            if (rootPairs && call.getRuleCall().getPlannerPhase() == com.apple.foundationdb.record.query.plan.cascades.PlannerPhase.PLANNING) {
                root = call.getRuleCall().getRoot();
                rootConfig = call.getRuleCall().getContext().getPlannerConfiguration();
            }
            String name = call.getRule().getClass().getSimpleName();
            if (!rules.contains(name)) {
                return;
            }
            int[] c = counts.computeIfAbsent(name, k -> new int[3]);
            c[0]++;
            if (inUnionPartitions && name.equals("ImplementInUnionRule")) {
                recordInUnionPartitions(call);
            }
            c[1] += call.getRuleCall().getNewFinalExpressions().size();
            c[2] += call.getRuleCall().getNewExploratoryExpressions().size();
            for (var e : call.getRuleCall().getNewFinalExpressions()) {
                finalKinds.computeIfAbsent(name, k -> new java.util.TreeSet<>()).add(e.getClass().getSimpleName());
                if (name.equals("ImplementInUnionRule")) {
                    inUnionYields.add(new Object[] {e, call.getCurrentReference(),
                            call.getRuleCall().getContext().getPlannerConfiguration()});
                }
            }
            // For the predicate-folding question: that a simplification yielded an exploratory
            // alternative (and the planner configuration), which arms the REWRITING-prune record
            // made at OptimizeGroup BEGIN above.
            if (name.equals("QueryPredicateSimplificationRule") && !call.getRuleCall().getNewExploratoryExpressions().isEmpty()) {
                simplifiedRefs.add(new Object[] {call.getCurrentReference(),
                        call.getRuleCall().getContext().getPlannerConfiguration()});
            }
        }
        final java.util.List<Object[]> simplifiedRefs = new java.util.ArrayList<>();

        private void recordRewritingPrune(com.apple.foundationdb.record.query.plan.cascades.Reference ref,
                                          com.apple.foundationdb.record.query.plan.RecordQueryPlannerConfiguration config) {
            var model = com.apple.foundationdb.record.query.plan.cascades.PlannerPhase.REWRITING.createCostModel(config);
            var members = new java.util.ArrayList<>(ref.getFinalExpressions());
            java.util.function.Function<com.apple.foundationdb.record.query.plan.cascades.expressions.RelationalExpression, String> criteria = e ->
                    e.getClass().getSimpleName()
                    + " selects=" + com.apple.foundationdb.record.query.plan.cascades.properties.ExpressionCountProperty.selectCount().evaluate(e)
                    + " conjuncts=" + com.apple.foundationdb.record.query.plan.cascades.properties.NormalizedResidualPredicateProperty.countNormalizedConjuncts(e)
                    + " predicates=" + (e instanceof com.apple.foundationdb.record.query.plan.cascades.expressions.SelectExpression
                        ? ((com.apple.foundationdb.record.query.plan.cascades.expressions.SelectExpression) e).getPredicates() : "-");
            // Order the members by their deterministic criteria so the pairwise lines are
            // stable; the semantic hash (the model's last tie-break) is reported, not ordered on.
            members.sort(java.util.Comparator.comparing(criteria::apply));
            comparisons.add("REWRITING-PRUNE members: " + members.size());
            for (var m : members) {
                comparisons.add("REWRITING-MEMBER " + criteria.apply(m) + " semanticHash=" + m.semanticHashCode());
            }
            for (int i = 0; i < members.size(); i++) {
                for (int j = i + 1; j < members.size(); j++) {
                    comparisons.add("REWRITING-COMPARE " + i + " vs " + j + " = "
                            + Integer.signum(model.compare(members.get(i), members.get(j))));
                }
            }
        }

        // Replays ImplementInUnionRule.onMatch's reading of its inner reference (4.14.2.0,
        // ImplementInUnionRule.java, from findInnerQuantifier to the enumeration of satisfying
        // comparison keys) and records it; adjustBindings is private there and is copied below.
        private void recordInUnionPartitions(com.apple.foundationdb.record.query.plan.cascades.events.TransformRuleCallPlannerEvent call) {
            Object bindable = call.getBindable();
            int callNo = counts.get("ImplementInUnionRule")[0];
            String head = "IUP call " + callNo + " phase=" + call.getRuleCall().getPlannerPhase();
            if (!(bindable instanceof com.apple.foundationdb.record.query.plan.cascades.expressions.SelectExpression)) {
                partitionLines.add(head + " bindable=" + bindable.getClass().getSimpleName());
                return;
            }
            var select = (com.apple.foundationdb.record.query.plan.cascades.expressions.SelectExpression) bindable;
            var requested = call.getRuleCall().getPlannerConstraintMaybe(
                    com.apple.foundationdb.record.query.plan.cascades.RequestedOrderingConstraint.REQUESTED_ORDERING);
            var explodeQs = new java.util.ArrayList<com.apple.foundationdb.record.query.plan.cascades.Quantifier.ForEach>();
            for (var q : select.getQuantifiers()) {
                if (q instanceof com.apple.foundationdb.record.query.plan.cascades.Quantifier.ForEach
                        && q.getRangesOver().getAllMemberExpressions().stream().anyMatch(e ->
                            e instanceof com.apple.foundationdb.record.query.plan.cascades.expressions.ExplodeExpression)) {
                    explodeQs.add((com.apple.foundationdb.record.query.plan.cascades.Quantifier.ForEach) q);
                }
            }
            var explodeAliases = com.apple.foundationdb.record.query.plan.cascades.Quantifiers.aliases(explodeQs);
            var innerOpt = com.apple.foundationdb.record.query.plan.cascades.rules.PushRequestedOrderingThroughInLikeSelectRule
                    .findInnerQuantifier(select, explodeQs, explodeAliases);
            head += " predicates=" + select.getPredicates().size() + " explodes=" + explodeQs.size()
                    + " requested=" + requested.map(Object::toString).orElse("absent");
            if (innerOpt.isEmpty()) {
                partitionLines.add(head + " inner=absent");
                return;
            }
            var innerRef = innerOpt.get().getRangesOver();
            var partitions = com.apple.foundationdb.record.query.plan.cascades.PlanPartitions.rollUpTo(
                    innerRef.toPlanPartitions(),
                    com.apple.foundationdb.record.query.plan.cascades.properties.OrderingProperty.ordering());
            partitionLines.add(head + " innerFinals=" + innerRef.getFinalExpressions().size()
                    + " innerExploratory=" + innerRef.getExploratoryExpressions().size() + " partitions=" + partitions.size());
            int pi = 0;
            for (var partition : partitions) {
                var provided = partition.getPartitionPropertyValue(
                        com.apple.foundationdb.record.query.plan.cascades.properties.OrderingProperty.ordering());
                var planNames = new java.util.TreeSet<String>();
                for (var plan : partition.getPlans()) {
                    planNames.add(com.apple.foundationdb.record.query.plan.cascades.explain.ExplainPlanVisitor.toStringForDebugging(plan));
                }
                partitionLines.add("IUP call " + callNo + " partition " + pi + " provided=" + provided + " plans=" + planNames);
                if (requested.isPresent()) {
                    for (var req : requested.get()) {
                        if (req.isPreserve()) {
                            partitionLines.add("IUP call " + callNo + " partition " + pi + " request=" + req + " PRESERVE-skipped");
                            continue;
                        }
                        var sortMap = req.getValueRequestedSortOrderMap();
                        var adjusted = com.google.common.collect.ImmutableSetMultimap.<com.apple.foundationdb.record.query.plan.cascades.values.Value,
                                com.apple.foundationdb.record.query.plan.cascades.Ordering.Binding>builder();
                        for (var e : provided.getBindingMap().asMap().entrySet()) {
                            adjusted.putAll(e.getKey(), adjustInUnionBindings(e.getValue(), explodeAliases, sortMap.get(e.getKey())));
                        }
                        var unionOrdering = com.apple.foundationdb.record.query.plan.cascades.Ordering.UNION.createOrdering(
                                adjusted.build(), provided.getOrderingSet(), provided.isDistinct());
                        var keys = new java.util.ArrayList<String>();
                        for (var k : unionOrdering.enumerateSatisfyingComparisonKeyValues(req)) {
                            keys.add(k.toString());
                        }
                        partitionLines.add("IUP call " + callNo + " partition " + pi + " request=" + req
                                + " unionOrdering=" + unionOrdering + " satisfyingKeys=" + keys);
                    }
                }
                pi++;
            }
        }

        private static Iterable<com.apple.foundationdb.record.query.plan.cascades.Ordering.Binding> adjustInUnionBindings(
                java.util.Collection<com.apple.foundationdb.record.query.plan.cascades.Ordering.Binding> bindings,
                java.util.Set<com.apple.foundationdb.record.query.plan.cascades.CorrelationIdentifier> explodeAliases,
                com.apple.foundationdb.record.query.plan.cascades.OrderingPart.RequestedSortOrder requestedSortOrder) {
            var sortOrder = com.apple.foundationdb.record.query.plan.cascades.Ordering.sortOrder(bindings);
            if (sortOrder.isDirectional()) {
                return com.google.common.collect.ImmutableList.of(com.apple.foundationdb.record.query.plan.cascades.Ordering.Binding.sorted(sortOrder));
            }
            if (com.apple.foundationdb.record.query.plan.cascades.Ordering.hasMultipleFixedBindings(bindings)) {
                return bindings;
            }
            var binding = com.apple.foundationdb.record.query.plan.cascades.Ordering.fixedBinding(bindings);
            var comparison = binding.getComparison();
            if (comparison.getType() != com.apple.foundationdb.record.query.expressions.Comparisons.Type.EQUALS) {
                return bindings;
            }
            if (comparison instanceof com.apple.foundationdb.record.query.expressions.Comparisons.ParameterComparison) {
                var pc = (com.apple.foundationdb.record.query.expressions.Comparisons.ParameterComparison) comparison;
                if (!pc.isCorrelation() || !explodeAliases.containsAll(pc.getCorrelatedTo())) {
                    return bindings;
                }
            } else if (comparison instanceof com.apple.foundationdb.record.query.expressions.Comparisons.ValueComparison) {
                var vc = (com.apple.foundationdb.record.query.expressions.Comparisons.ValueComparison) comparison;
                if (!explodeAliases.containsAll(vc.getCorrelatedTo())) {
                    return bindings;
                }
            } else {
                return bindings;
            }
            if (requestedSortOrder == null
                    || requestedSortOrder == com.apple.foundationdb.record.query.plan.cascades.OrderingPart.RequestedSortOrder.ANY) {
                return com.google.common.collect.ImmutableList.of(com.apple.foundationdb.record.query.plan.cascades.Ordering.Binding.choose());
            }
            if (!requestedSortOrder.isDirectional()) {
                return bindings;
            }
            return com.google.common.collect.ImmutableList.of(
                    com.apple.foundationdb.record.query.plan.cascades.Ordering.Binding.sorted(requestedSortOrder.toProvidedSortOrder()));
        }

        @Override
        public void onDone() {
            if (rootPairs && rootMembers != null) {
                var model = new com.apple.foundationdb.record.query.plan.cascades.PlanningCostModel(rootConfig);
                var members = new java.util.ArrayList<>(rootMembers);
                java.util.function.Function<com.apple.foundationdb.record.query.plan.cascades.expressions.RelationalExpression, String> name = e ->
                        e instanceof com.apple.foundationdb.record.query.plan.plans.RecordQueryPlan
                        ? com.apple.foundationdb.record.query.plan.cascades.explain.ExplainPlanVisitor.toStringForDebugging(
                                (com.apple.foundationdb.record.query.plan.plans.RecordQueryPlan) e)
                        : e.getClass().getSimpleName();
                members.sort(java.util.Comparator.comparing(name::apply));
                comparisons.add("ROOT members: " + members.size() + " preference=" + rootConfig.getIndexScanPreference());
                // The memo order OptimizeGroup iterates, as indices into the sorted list above,
                // and the winner its sequential pass picks over that order
                // (CascadesPlanner.java:650-658): under a cyclic relation the final plan
                // depends on the order, so the order is measured, not assumed.
                var memoOrder = new java.util.ArrayList<String>();
                com.apple.foundationdb.record.query.plan.cascades.expressions.RelationalExpression best = null;
                for (var m : rootMembers) {
                    memoOrder.add(Integer.toString(members.indexOf(m)));
                    if (best == null || model.compare(m, best) < 0) {
                        best = m;
                    }
                }
                comparisons.add("ROOT memo order: " + String.join(" ", memoOrder)
                        + " sequential winner: " + (best == null ? "-" : Integer.toString(members.indexOf(best))));
                for (int i = 0; i < members.size(); i++) {
                    var m = members.get(i);
                    String hash = m instanceof PlanHashable
                            ? Integer.toString(((PlanHashable) m).planHash(PlanHashable.CURRENT_FOR_CONTINUATION)) : "-";
                    String rungs = " conjuncts=" + com.apple.foundationdb.record.query.plan.cascades.properties.NormalizedResidualPredicateProperty.countNormalizedConjuncts(m)
                            + " typeFilters=" + com.apple.foundationdb.record.query.plan.cascades.properties.TypeFilterCountProperty.typeFilterCount().evaluate(m)
                            + " unmatchedFields=" + com.apple.foundationdb.record.query.plan.cascades.properties.UnmatchedFieldsCountProperty.unmatchedFieldsCount().evaluate(m);
                    comparisons.add("ROOT-MEMBER " + i + " " + name.apply(m) + rungs + " hash=" + hash);
                }
                for (int i = 0; i < members.size(); i++) {
                    for (int j = i + 1; j < members.size(); j++) {
                        var a = members.get(i);
                        var b = members.get(j);
                        String hashOrder = "-";
                        if (a instanceof PlanHashable && b instanceof PlanHashable) {
                            hashOrder = Integer.toString(Integer.signum(Integer.compare(
                                    ((PlanHashable) a).planHash(PlanHashable.CURRENT_FOR_CONTINUATION),
                                    ((PlanHashable) b).planHash(PlanHashable.CURRENT_FOR_CONTINUATION))));
                        }
                        comparisons.add("ROOT-COMPARE " + i + " vs " + j + " = " + Integer.signum(model.compare(a, b))
                                + " hashOrder=" + hashOrder);
                    }
                }
            }
            for (Object[] y : inUnionYields) {
                var inUnion = (com.apple.foundationdb.record.query.plan.cascades.expressions.RelationalExpression) y[0];
                var ref = (com.apple.foundationdb.record.query.plan.cascades.Reference) y[1];
                var config = (com.apple.foundationdb.record.query.plan.RecordQueryPlannerConfiguration) y[2];
                var model = new com.apple.foundationdb.record.query.plan.cascades.PlanningCostModel(config);
                boolean stillMember = false;
                for (var member : ref.getFinalExpressions()) {
                    if (member == inUnion) {
                        stillMember = true;
                        continue;
                    }
                    comparisons.add(describe(member) + " vs InUnion(" + describe(inUnion) + "): compare(InUnion, member) = "
                            + Integer.signum(model.compare(inUnion, member)));
                }
                comparisons.add("InUnion still a final member of its reference at the end: " + stillMember
                        + "; reference final members: " + ref.getFinalExpressions().size());
            }
        }

        private static String describe(com.apple.foundationdb.record.query.plan.cascades.expressions.RelationalExpression e) {
            String plan = e instanceof com.apple.foundationdb.record.query.plan.plans.RecordQueryPlan
                    ? " plan=" + com.apple.foundationdb.record.query.plan.cascades.explain.ExplainPlanVisitor.toStringForDebugging(
                            (com.apple.foundationdb.record.query.plan.plans.RecordQueryPlan) e)
                    : "";
            return e.getClass().getSimpleName() + plan
                    + " residuals=" + com.apple.foundationdb.record.query.plan.cascades.properties.NormalizedResidualPredicateProperty.countNormalizedConjuncts(e)
                    + " maxCardUnknown=" + com.apple.foundationdb.record.query.plan.cascades.properties.CardinalitiesProperty.cardinalities().evaluate(e).getMaxCardinality().isUnknown();
        }
    }

    /**
     * EXPLAIN {@code querySql} over the schema after the setup statements, with a planner listener
     * attached that reports, for each rule named in {@code rules}, how many of its calls ended, how
     * many final and exploratory expressions they yielded, and the classes of the final ones. It
     * answers "did the target's planner ever produce plan X" where EXPLAIN shows only the winner.
     */
    @ConformanceStep("planRuleTrace")
    public JsonObject planRuleTrace(String clusterFile, String schemaTemplate, java.util.List<String> setupSqls,
                                    String querySql, java.util.List<String> rules) throws Exception {
        return runWithEphemeralSchema(clusterFile, schemaTemplate, conn -> {
            try (Statement st = conn.createStatement()) {
                for (String setup : setupSqls) {
                    withFdbRetry(() -> st.executeUpdate(setup));
                }
            }
            RuleTraceListener listener = new RuleTraceListener(rules);
            com.apple.foundationdb.record.query.plan.cascades.events.PlannerEventListeners.addListener(RuleTraceListener.class, listener);
            String plan;
            try {
                plan = runExplain(conn, querySql);
            } finally {
                com.apple.foundationdb.record.query.plan.cascades.events.PlannerEventListeners.removeListener(RuleTraceListener.class);
            }
            JsonObject out = new JsonObject();
            out.addProperty("explain", plan);
            JsonObject perRule = new JsonObject();
            for (String rule : rules) {
                int[] c = listener.counts.getOrDefault(rule, new int[3]);
                JsonObject r = new JsonObject();
                r.addProperty("calls", c[0]);
                r.addProperty("finals", c[1]);
                r.addProperty("exploratory", c[2]);
                JsonArray kinds = new JsonArray();
                for (String k : listener.finalKinds.getOrDefault(rule, new java.util.TreeSet<>())) {
                    kinds.add(k);
                }
                r.add("finalKinds", kinds);
                perRule.add(rule, r);
            }
            out.add("rules", perRule);
            JsonArray cmp = new JsonArray();
            for (String c : listener.comparisons) {
                cmp.add(c);
            }
            out.add("inUnionComparisons", cmp);
            JsonArray iup = new JsonArray();
            for (String l : listener.partitionLines) {
                iup.add(l);
            }
            out.add("inUnionPartitions", iup);
            JsonObject tasks = new JsonObject();
            listener.tasksPerPhase.forEach(tasks::addProperty);
            out.add("tasksPerPhase", tasks);
            JsonObject kinds = new JsonObject();
            listener.tasksPerKind.forEach(kinds::addProperty);
            out.add("tasksPerKind", kinds);
            return out;
        });
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
        JsonArray nullability = new JsonArray(n);
        for (int i = 1; i <= n; i++) {
            JsonObject c = new JsonObject();
            c.addProperty("name", md.getColumnName(i));
            c.addProperty("type", md.getColumnTypeName(i));
            cols.add(c);
            int nullable = md.isNullable(i);
            nullability.add(nullable == ResultSetMetaData.columnNoNulls ? "NOT NULL"
                    : nullable == ResultSetMetaData.columnNullable ? "NULL" : "UNKNOWN");
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
        out.add("nullability", nullability);
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
