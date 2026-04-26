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

import java.io.File;
import java.io.FileWriter;
import java.io.IOException;
import java.sql.DriverManager;
import java.sql.ResultSetMetaData;
import java.sql.SQLException;
import java.sql.Statement;
import java.sql.Types;
import java.util.ArrayList;
import java.util.Collections;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
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
            // resource — register the driver once and leave it.
            FDBDatabaseFactory.instance().setAPIVersion(APIVersion.API_VERSION_7_1);

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
        ensureDriverRegistered(clusterFile);

        String suffix = UUID.randomUUID().toString().replace("-", "");
        String templateName = "PLAN_DIFF_T_" + suffix;
        String dbPath = "/TEST/PLAN_DIFF_" + suffix;
        String schemaName = "S_" + suffix;
        boolean templateCreated = false;
        boolean dbCreated = false;

        try {
            if (schemaTemplate != null && !schemaTemplate.isEmpty()) {
                try (java.sql.Connection sysConn = DriverManager.getConnection("jdbc:embed:/__SYS");
                     Statement st = sysConn.createStatement()) {
                    st.executeUpdate("CREATE SCHEMA TEMPLATE \"" + templateName + "\" " + schemaTemplate);
                    templateCreated = true;
                    st.executeUpdate("CREATE DATABASE \"" + dbPath + "\"");
                    dbCreated = true;
                    st.executeUpdate("CREATE SCHEMA \"" + dbPath + "/" + schemaName + "\" WITH TEMPLATE \"" + templateName + "\"");
                }

                try (java.sql.Connection conn = DriverManager.getConnection("jdbc:embed:" + dbPath)) {
                    conn.setSchema(schemaName);
                    return runExplain(conn, sql);
                }
            }
            // No schema — connect to __SYS and run EXPLAIN against whatever
            // the SQL refers to. SELECT-without-FROM works here.
            try (java.sql.Connection conn = DriverManager.getConnection("jdbc:embed:/__SYS")) {
                return runExplain(conn, sql);
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
     * Run a SQL statement against fdb-relational's embedded driver and return
     * the result rows as a structured JSON-shape Map. Mirrors {@link #planSql}'s
     * setup/teardown — fresh schema-template + database + schema per call,
     * dropped in {@code finally}.
     *
     * Track A1 (TODO.md): the Go-side {@code plandiff} harness today only
     * diffs plan trees ({@link #planSql}); cross-language SQL conformance
     * needs {@code runSql} so Go and Java can also diff actual result sets.
     *
     * <p>Result format (Gson-serialised):
     * <pre>{@code
     * {
     *   "columns":     ["c1", "c2"],         // result-set column names, in order
     *   "columnTypes": ["BIGINT", "STRING"], // SQL type names from ResultSetMetaData
     *   "rows":        [[1, "a"], [2, null]] // values per row, JSON-typed
     * }
     * }</pre>
     *
     * Value encoding (mirrors what the Go-side decoder must accept):
     * <ul>
     *   <li>NULL → JSON null</li>
     *   <li>BIGINT / INTEGER / SMALLINT / TINYINT → JSON number</li>
     *   <li>DOUBLE / FLOAT / REAL → JSON number</li>
     *   <li>BOOLEAN → JSON boolean</li>
     *   <li>VARCHAR / CHAR / OTHER text → JSON string</li>
     *   <li>VARBINARY / BINARY → "0x" + hex string (lossless, JSON-safe)</li>
     *   <li>Anything else → toString() fallback (JSON string)</li>
     * </ul>
     *
     * Non-SELECT statements (INSERT / UPDATE / DELETE) are accepted: the
     * statement is executed via {@code execute()} and the response carries
     * the affected-row count instead of a row-set, in this shape:
     * <pre>{@code
     * { "updateCount": 3 }
     * }</pre>
     *
     * @param clusterFile     cluster-file content (string, not path)
     * @param schemaTemplate  CREATE SCHEMA TEMPLATE body. Empty → run against /__SYS.
     * @param sql             SELECT / INSERT / UPDATE / DELETE — bare SQL, no EXPLAIN prefix.
     * @return                Map with {columns, columnTypes, rows} for SELECT, or
     *                        {updateCount} for DML.
     */
    @ConformanceStep("runSql")
    public Map<String, Object> runSql(String clusterFile, String schemaTemplate, String sql) throws Exception {
        ensureDriverRegistered(clusterFile);

        String suffix = UUID.randomUUID().toString().replace("-", "");
        String templateName = "RUN_SQL_T_" + suffix;
        String dbPath = "/TEST/RUN_SQL_" + suffix;
        String schemaName = "S_" + suffix;
        boolean templateCreated = false;
        boolean dbCreated = false;

        try {
            if (schemaTemplate != null && !schemaTemplate.isEmpty()) {
                try (java.sql.Connection sysConn = DriverManager.getConnection("jdbc:embed:/__SYS");
                     Statement st = sysConn.createStatement()) {
                    st.executeUpdate("CREATE SCHEMA TEMPLATE \"" + templateName + "\" " + schemaTemplate);
                    templateCreated = true;
                    st.executeUpdate("CREATE DATABASE \"" + dbPath + "\"");
                    dbCreated = true;
                    st.executeUpdate("CREATE SCHEMA \"" + dbPath + "/" + schemaName + "\" WITH TEMPLATE \"" + templateName + "\"");
                }

                try (java.sql.Connection conn = DriverManager.getConnection("jdbc:embed:" + dbPath)) {
                    conn.setSchema(schemaName);
                    return executeAndCapture(conn, sql, schemaName);
                }
            }
            try (java.sql.Connection conn = DriverManager.getConnection("jdbc:embed:/__SYS")) {
                return executeAndCapture(conn, sql, null);
            }
        } finally {
            if (dbCreated) {
                try (java.sql.Connection sysConn = DriverManager.getConnection("jdbc:embed:/__SYS");
                     Statement st = sysConn.createStatement()) {
                    st.executeUpdate("DROP DATABASE IF EXISTS \"" + dbPath + "\"");
                } catch (SQLException ignored) {
                    // teardown best-effort — preserve primary exception.
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

    private Map<String, Object> executeAndCapture(java.sql.Connection conn, String sql, String schemaName) throws SQLException {
        // FORCED diagnostic — throw on entry to confirm we got here. If we
        // never see "runSql.entered" round-tripped through the response
        // envelope, the failure is upstream (DDL setup or conn.setSchema).
        if (schemaName != null) {
            throw new SQLException("[runSql.entered] schemaName=" + schemaName + " sql=" + sql);
        }
        RelationalConnection rconn = conn.unwrap(RelationalConnection.class);
        try (RelationalPreparedStatement ps = rconn.prepareStatement(sql)) {
            boolean hasResultSet = ps.execute();
            if (!hasResultSet) {
                Map<String, Object> out = new LinkedHashMap<>();
                out.put("updateCount", ps.getUpdateCount());
                return out;
            }
            try (RelationalResultSet rs = (RelationalResultSet) ps.getResultSet()) {
                return captureRows(rs);
            }
        }
    }

    private Map<String, Object> captureRows(java.sql.ResultSet rs) throws SQLException {
        ResultSetMetaData md = rs.getMetaData();
        int n = md.getColumnCount();

        List<String> cols = new ArrayList<>(n);
        List<String> types = new ArrayList<>(n);
        for (int i = 1; i <= n; i++) {
            // getColumnLabel returns the alias if set, else the column name —
            // matches what a SELECT-with-alias caller actually sees.
            cols.add(md.getColumnLabel(i));
            types.add(md.getColumnTypeName(i));
        }

        List<List<Object>> rows = new ArrayList<>();
        while (rs.next()) {
            List<Object> row = new ArrayList<>(n);
            for (int i = 1; i <= n; i++) {
                row.add(extractValue(rs, i, md.getColumnType(i)));
            }
            rows.add(row);
        }

        Map<String, Object> out = new LinkedHashMap<>();
        out.put("columns", cols);
        out.put("columnTypes", types);
        out.put("rows", rows);
        return out;
    }

    /**
     * Extract a column value into a Gson-serialisable Object. Centralised
     * so the Go-side decoder can mirror the same type matrix.
     */
    private Object extractValue(java.sql.ResultSet rs, int idx, int sqlType) throws SQLException {
        switch (sqlType) {
            case Types.NULL:
                return null;

            case Types.BIGINT: {
                long v = rs.getLong(idx);
                return rs.wasNull() ? null : v;
            }
            case Types.INTEGER:
            case Types.SMALLINT:
            case Types.TINYINT: {
                int v = rs.getInt(idx);
                return rs.wasNull() ? null : v;
            }

            case Types.DOUBLE:
            case Types.FLOAT:
            case Types.REAL: {
                double v = rs.getDouble(idx);
                return rs.wasNull() ? null : v;
            }

            case Types.BOOLEAN:
            case Types.BIT: {
                boolean v = rs.getBoolean(idx);
                return rs.wasNull() ? null : v;
            }

            case Types.VARCHAR:
            case Types.CHAR:
            case Types.LONGVARCHAR:
            case Types.NVARCHAR:
            case Types.NCHAR:
            case Types.LONGNVARCHAR:
                return rs.getString(idx);

            case Types.VARBINARY:
            case Types.BINARY:
            case Types.LONGVARBINARY: {
                byte[] b = rs.getBytes(idx);
                if (b == null) {
                    return null;
                }
                StringBuilder sb = new StringBuilder(2 + 2 * b.length);
                sb.append("0x");
                for (byte x : b) {
                    sb.append(String.format("%02x", x & 0xff));
                }
                return sb.toString();
            }

            default: {
                Object v = rs.getObject(idx);
                if (v == null) {
                    return null;
                }
                return v.toString();
            }
        }
    }
}
