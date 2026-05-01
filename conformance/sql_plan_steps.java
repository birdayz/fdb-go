package com.birdayz.conformance;

import com.apple.foundationdb.record.provider.foundationdb.APIVersion;
import com.apple.foundationdb.record.provider.foundationdb.FDBDatabase;
import com.apple.foundationdb.record.provider.foundationdb.FDBDatabaseFactory;
import com.apple.foundationdb.record.provider.foundationdb.FormatVersion;
import com.apple.foundationdb.record.provider.foundationdb.keyspace.KeySpace;
import com.apple.foundationdb.relational.api.EmbeddedRelationalDriver;
import com.apple.foundationdb.relational.api.EmbeddedRelationalEngine;
import com.apple.foundationdb.relational.api.Options;
import com.apple.foundationdb.relational.api.RelationalConnection;
import com.apple.foundationdb.relational.api.RelationalPreparedStatement;
import com.apple.foundationdb.relational.api.RelationalResultSet;
import com.apple.foundationdb.relational.api.Transaction;
import com.apple.foundationdb.relational.api.catalog.StoreCatalog;
import com.apple.foundationdb.relational.recordlayer.DirectFdbConnection;
import com.apple.foundationdb.relational.recordlayer.RecordLayerConfig;
import com.apple.foundationdb.relational.recordlayer.RecordLayerEngine;
import com.apple.foundationdb.relational.recordlayer.RelationalKeyspaceProvider;
import com.apple.foundationdb.relational.recordlayer.catalog.StoreCatalogProvider;
import com.apple.foundationdb.relational.recordlayer.ddl.RecordLayerMetadataOperationsFactory;
import com.apple.foundationdb.relational.recordlayer.query.cache.RelationalPlanCache;
import com.codahale.metrics.MetricRegistry;

import com.google.gson.JsonArray;
import com.google.gson.JsonNull;
import com.google.gson.JsonObject;
import com.google.gson.JsonPrimitive;

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
import java.util.concurrent.atomic.AtomicBoolean;

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

            File tempFile = new File("/tmp/fdb_sql_plan_steps.cluster");
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
            EmbeddedRelationalEngine engine = RecordLayerEngine.makeEngine(
                config,
                Collections.singletonList(database),
                keySpace,
                storeCatalog,
                new MetricRegistry(),
                ddlFactory,
                RelationalPlanCache.buildWithDefaults());

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
                    st.executeUpdate(setup);
                }
            }
            return runQuery(conn, querySql);
        });
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
                    st.executeUpdate("CREATE SCHEMA TEMPLATE \"" + templateName + "\" " + schemaTemplate);
                    templateCreated = true;
                    st.executeUpdate("CREATE DATABASE \"" + dbPath + "\"");
                    dbCreated = true;
                    st.executeUpdate("CREATE SCHEMA \"" + dbPath + "/" + schemaName + "\" WITH TEMPLATE \"" + templateName + "\"");
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

    private String runExplain(java.sql.Connection conn, String sql) throws SQLException {
        // fdb-relational accepts EXPLAIN as a SQL prefix; the result set has
        // a PLAN column (VARCHAR) carrying the rendered tree. Other columns
        // (PLAN_HASH, PLAN_DOT, PLAN_GML, PLAN_CONTINUATION, PLANNER_METRICS)
        // are diagnostic; the harness only diffs PLAN today.
        RelationalConnection rconn = conn.unwrap(RelationalConnection.class);
        try (RelationalPreparedStatement ps = rconn.prepareStatement("EXPLAIN " + sql);
             RelationalResultSet rs = ps.executeQuery()) {
            if (!rs.next()) {
                return "";
            }
            String plan = rs.getString("PLAN");
            return plan == null ? "" : plan;
        }
    }

    /**
     * Execute a SQL query and serialise the result set as JSON. Encoder rules
     * are documented on {@link #runSql}.
     */
    private JsonObject runQuery(java.sql.Connection conn, String sql) throws SQLException {
        RelationalConnection rconn = conn.unwrap(RelationalConnection.class);
        try (RelationalPreparedStatement ps = rconn.prepareStatement(sql);
             RelationalResultSet rs = ps.executeQuery()) {
            return resultSetToJson(rs);
        }
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
        JsonObject marker = new JsonObject();
        marker.addProperty("__unsupported__", v.getClass().getName());
        return marker;
    }
}
