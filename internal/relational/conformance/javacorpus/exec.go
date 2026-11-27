// Portions derived from FoundationDB Record Layer (YamlExecutionContext.java,
// PreambleBlock.java, Command.java, EmbeddedConfig.java),
// Copyright 2015-2025 Apple Inc. and the FoundationDB project authors
// Copyright 2021-2024 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package javacorpus

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"fdb.dev/internal/relational/conformance/javayamsql"
	"fdb.dev/internal/relational/conformance/yamsql"
	"fdb.dev/internal/relational/relapi"
	"fdb.dev/pkg/relational/sqldriver"
)

// execute walks the block list of one resource, expanding includes in place.
//
// Java parses the whole include graph first and executes the flattened list
// afterwards, with each resource's teardown blocks appended after that
// resource's own blocks. Expanding recursively here produces the same order
// because the recursion returns before the parent's next block runs.
func (r *runner) execute(ctx context.Context, file *javayamsql.File) error {
	r.resourceOrder = append(r.resourceOrder, file.Path)
	defer func() { r.resourceOrder = r.resourceOrder[:len(r.resourceOrder)-1] }()

	for _, blk := range file.Blocks {
		if err := r.executeBlock(ctx, file.Path, blk); err != nil {
			return err
		}
	}
	return nil
}

// skipFileError aborts a file with a counted class rather than a failure. It is
// the Go stand-in for Java's Assumptions.assumeTrue, which throws
// TestAbortedException out of the parse and takes the whole file with it.
type skipFileError struct {
	class  SkipClass
	detail string
}

func (e *skipFileError) Error() string { return string(e.class) + ": " + e.detail }

func (r *runner) executeBlock(ctx context.Context, resource string, blk *javayamsql.Block) error {
	switch blk.Kind {
	case javayamsql.BlockOptions:
		return r.executeOptions(blk)
	case javayamsql.BlockSchemaTemplate:
		return r.executeSchemaTemplate(ctx, resource, blk)
	case javayamsql.BlockSetup:
		return r.executeSetup(ctx, resource, blk)
	case javayamsql.BlockTest:
		return r.executeTestBlock(ctx, resource, blk)
	case javayamsql.BlockInclude:
		if blk.Include.Resolved == nil {
			return fmt.Errorf("include %s was not resolved", blk.Include.Resource)
		}
		return r.execute(ctx, blk.Include.Resolved)
	case javayamsql.BlockTransactionSetups:
		// Registration is a parse-time act; javayamsql already resolved every
		// setupReference against it. Nothing executes here, exactly as
		// TransactionSetupsBlock.parse returns no block.
		return nil
	case javayamsql.BlockCopy:
		r.skip(SkipCopyBlock, resource, "copy_block moves data between two clusters")
		return &skipFileError{SkipCopyBlock, "copy_block"}
	}
	return fmt.Errorf("unhandled block kind %q", blk.Kind)
}

func (r *runner) executeOptions(blk *javayamsql.Block) error {
	o := blk.Options
	if !javayamsql.SupportedAtCurrentVersion(o.SupportedVersion) {
		return &skipFileError{SkipVersionGate, "supported_version " + o.SupportedVersion.String()}
	}
	if o.RequiredClusters != nil && *o.RequiredClusters > 1 {
		return &skipFileError{SkipMultiCluster, fmt.Sprintf("required_clusters=%d, one available", *o.RequiredClusters)}
	}
	// PreambleBlock.java:75-80 sets them on the execution context.
	r.fileConnOptions = o.ConnectionOptions
	return nil
}

// generatedID is the stem of the names the runner generates for a
// schema_template block: its database, template and schema. Upper case, as
// the unquoted DDL that creates them stores them, so a connection to them
// names what is stored; the corpus's own connect URIs are taken verbatim, as
// Java's driver takes them. The private fixture derives its target from the
// same stem.
func generatedID(idPrefix string, counter int) string {
	return strings.ToUpper(fmt.Sprintf("YAML_%s_%d", idPrefix, counter))
}

// executeSchemaTemplate runs Java's five-statement lifecycle against the
// catalog connection and registers the resulting database as a connect target.
func (r *runner) executeSchemaTemplate(ctx context.Context, resource string, blk *javayamsql.Block) error {
	variant, ok := selectVariant(blk.SchemaTemplate)
	if !ok {
		return fmt.Errorf("no schema_template variant matches the version under test")
	}

	r.counter++
	id := generatedID(r.cfg.IDPrefix, r.counter)
	tmpl := id + "_TEMPLATE"
	// SetupBlock's databasePath: DOMAIN ("/FRL") + "/" + identifier + "_DB".
	dbPath := "/FRL/" + id + "_DB"
	schema := id + "_SCHEMA"

	cat, err := r.open(catalogTarget)
	if err != nil {
		return err
	}
	fileOpts, err := r.connectionOptions()
	if err != nil {
		return err
	}
	// The lifecycle runs on a catalog connection from the options-carrying
	// factory, as Java's SchemaTemplateBlock connects; the cleanups below run
	// after this connection is released, each on a connection pinned to the
	// same options.
	catConn, err := pin(ctx, cat, fileOpts)
	if err != nil {
		return fmt.Errorf("catalog conn: %w", err)
	}
	defer catConn.Close()

	if r.fixture == nil {
		for _, stmt := range []string{
			fmt.Sprintf("DROP SCHEMA TEMPLATE IF EXISTS %s", tmpl),
			fmt.Sprintf("DROP DATABASE IF EXISTS %s", dbPath),
		} {
			if _, err := catConn.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("%s: %w", stmt, err)
			}
		}
	}

	create := fmt.Sprintf("CREATE SCHEMA TEMPLATE %s %s", tmpl, variant.Definition)
	if _, err := catConn.ExecContext(ctx, create); err != nil {
		return &ddlError{stmt: "CREATE SCHEMA TEMPLATE", err: err}
	}
	r.cleanups = append(r.cleanups, func() {
		execPinned(cat, fileOpts, fmt.Sprintf("DROP SCHEMA TEMPLATE IF EXISTS %s", tmpl))
	})

	if _, err := catConn.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath)); err != nil {
		return &ddlError{stmt: "CREATE DATABASE", err: err}
	}
	r.cleanups = append(r.cleanups, func() {
		execPinned(cat, fileOpts, fmt.Sprintf("DROP DATABASE IF EXISTS %s", dbPath))
	})

	if _, err := catConn.ExecContext(ctx,
		fmt.Sprintf("CREATE SCHEMA %s/%s WITH TEMPLATE %s", dbPath, schema, tmpl)); err != nil {
		return &ddlError{stmt: "CREATE SCHEMA", err: err}
	}

	r.connsByResource[resource] = append(r.connsByResource[resource], connTarget{Path: dbPath, Schema: schema})
	return nil
}

// setupError marks a failure in a `setup:` block's steps — data loading, not
// assertion.
//
// It is a distinct type because the polarity accounting needs to tell "the file
// failed" from "the file failed for the reason upstream is testing". Every
// assertion in the corpus lives in a test block; a setup step that dies takes
// the file down before any of them run.
type setupError struct {
	line  int
	query string
	err   error
}

func (e *setupError) Error() string {
	return fmt.Sprintf("setup step at line %d %q: %v", e.line, e.query, e.err)
}
func (e *setupError) Unwrap() error { return e.err }

// ddlError marks a schema_template failure so the ledger can classify the gap
// rather than reporting it as an undifferentiated red file.
type ddlError struct {
	stmt string
	err  error
}

func (e *ddlError) Error() string { return e.stmt + ": " + e.err.Error() }
func (e *ddlError) Unwrap() error { return e.err }

// classifyDDLGap buckets a rejected schema_template by which declaration it
// carries.
//
// This scans the DDL text, which the SQL surface elsewhere forbids — but the
// prohibition is on deciding BEHAVIOUR by string matching, and nothing is
// decided here: the file is skipped either way and the engine's own rejection
// message is what caused it. The scan only chooses which ledger bucket the
// already-taken skip is counted in, so that "struct types" and "SQL functions"
// show up as the named, separately-sized gaps RFC-201 phases 3 and 4 close
// rather than as one anonymous pile.
func classifyDDLGap(f *javayamsql.File, ddl *ddlError) SkipClass {
	// The value-index-as-select class (CQ-71, once the corpus's largest at 42
	// files) is DELETED: RFC-202 routed both AS-SELECT arms through the
	// MaterializedViewIndexGenerator port, its three classifier messages no
	// longer exist in the engine, and every former carrier lands in pass or a
	// narrower named class.
	return classifyDDLByDeclaration(f, ddl)
}

func classifyDDLByDeclaration(f *javayamsql.File, ddl *ddlError) SkipClass {
	var body strings.Builder
	var walk func(*javayamsql.File)
	walk = func(file *javayamsql.File) {
		for _, b := range file.Blocks {
			switch b.Kind {
			case javayamsql.BlockSchemaTemplate:
				for _, v := range b.SchemaTemplate.Variants {
					body.WriteString(strings.ToLower(v.Definition))
					body.WriteByte('\n')
				}
			case javayamsql.BlockInclude:
				if b.Include.Resolved != nil {
					walk(b.Include.Resolved)
				}
			}
		}
	}
	walk(f)
	text := body.String()
	// Struct DDL itself builds (RFC-204 Phase 1), so a struct-declaring
	// template that still fails does so on a DIFFERENT declaration — and the
	// engine's own rejection message names which one. Bucketing by that
	// message (function / view / nested index) keeps "struct types don't
	// declare" (closed) from wearing the label of the gap that actually
	// blocks the file; the declaration scan remains the fallback for
	// rejections the message rule doesn't name.
	if ddl != nil {
		msg := ddl.Error()
		switch {
		case strings.Contains(msg, "SQL functions (CREATE FUNCTION)"), strings.Contains(msg, "only query bodies are supported"):
			return SkipDDLFunction
		case strings.Contains(msg, "views (CREATE VIEW)"):
			return SkipDDLOther
		case strings.Contains(text, "create type as struct") && strings.Contains(msg, "index"):
			return SkipDDLStructIndex
		}
	}
	switch {
	case strings.Contains(text, "create type as struct"):
		return SkipDDLStruct
	case strings.Contains(text, "create function"):
		return SkipDDLFunction
	default:
		return SkipDDLOther
	}
}

// selectVariant mirrors the execution-time `findFirst` over variants whose
// version range contains the connection's initial version.
func selectVariant(b *javayamsql.SchemaTemplateBlock) (javayamsql.SchemaTemplateVariant, bool) {
	for _, v := range b.Variants {
		if javayamsql.SelectedAtCurrentVersion(v.AtLeast, v.LessThan) {
			return v, true
		}
	}
	return javayamsql.SchemaTemplateVariant{}, false
}

// resolveConnect mirrors YamlExecutionContext.resolveConnectionURI.
func (r *runner) resolveConnect(resource string, v *javayamsql.Value) (connTarget, error) {
	local := r.connsByResource[resource]

	if v == nil || v.IsNull() {
		if len(local) == 1 {
			return local[0], nil
		}
		if len(local) > 1 {
			return connTarget{}, fmt.Errorf("requested a default connection URI, but multiple available to choose from in local")
		}
		global := r.globalConns()
		if len(global) != 1 {
			return connTarget{}, fmt.Errorf("requested a default connection URI, but %d available globally", len(global))
		}
		return global[0], nil
	}

	if n, ok := v.AsInt(); ok {
		return pick(local, n)
	}

	s, ok := v.AsString()
	if !ok {
		return connTarget{}, fmt.Errorf("connect must be an integer or a string")
	}
	if strings.HasPrefix(s, "(global)") {
		n, err := parseIndex(strings.TrimSpace(s[len("(global)"):]))
		if err != nil {
			return connTarget{}, err
		}
		return pick(r.globalConns(), n)
	}
	// Java takes any other string as a verbatim JDBC URI.
	return parseJDBCURI(s)
}

// parseJDBCURI maps a verbatim `jdbc:embed:<path>[?schema=<name>]` target onto
// the Go DSN's (path, schema) pair.
//
// The corpus writes these to reach the system catalog explicitly — a setup
// block that issues its own CREATE DATABASE, say — which is the same
// connection `connect: 0` resolves to, so it normalises onto that rather than
// carrying a second spelling of one destination.
func parseJDBCURI(s string) (connTarget, error) {
	const prefix = "jdbc:embed:"
	if !strings.HasPrefix(s, prefix) {
		return connTarget{}, fmt.Errorf("connection URI %q is not a jdbc:embed: target", s)
	}
	rest := strings.TrimPrefix(s, prefix)
	path, query, _ := strings.Cut(rest, "?")
	if path == "" {
		return connTarget{}, fmt.Errorf("connection URI %q has no database path", s)
	}
	var schema string
	for _, kv := range strings.Split(query, "&") {
		if k, v, ok := strings.Cut(kv, "="); ok && k == "schema" {
			schema = v
		}
	}
	if path == catalogPath {
		// Every catalog connection is Java's connection 0, schema CATALOG.
		return catalogTarget, nil
	}
	return connTarget{Path: path, Schema: schema}, nil
}

func parseIndex(s string) (int64, error) {
	var n int64
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return 0, fmt.Errorf("connect index %q is not an integer", s)
	}
	return n, nil
}

func pick(list []connTarget, idx int64) (connTarget, error) {
	if idx == 0 {
		return catalogTarget, nil
	}
	if idx < 1 || idx > int64(len(list)) {
		return connTarget{}, fmt.Errorf("requested connection URI at index %d, but only have %d available connection URIs", idx, len(list))
	}
	return list[idx-1], nil
}

// globalConns is the ancestor-first concatenation Java builds from the include
// call stack, outermost resource first and the current one last.
func (r *runner) globalConns() []connTarget {
	var out []connTarget
	for _, res := range r.resourceOrder {
		out = append(out, r.connsByResource[res]...)
	}
	return out
}

func (r *runner) open(t connTarget) (*sql.DB, error) {
	if db, ok := r.dbs[t]; ok {
		return db, nil
	}
	if r.engine == nil {
		// The plan cache Java's yaml tests run against (EmbeddedConfig.beforeAll):
		// one-hour TTLs, so a slow block's check_cache pass still finds the plans
		// its tests wrote, and ten templates.
		planCacheOpts := api.NewOptionsBuilder().
			Set(api.OptPlanCachePrimaryTimeToLiveMillis, int64(3600000)).
			Set(api.OptPlanCacheSecondaryTimeToLiveMillis, int64(3600000)).
			Set(api.OptPlanCacheTertiaryTimeToLiveMillis, int64(3600000)).
			Set(api.OptPlanCachePrimaryMaxEntries, int64(10)).
			Build()
		// Java's yaml runner registers FRL before it connects (Command.java).
		cfg := sqldriver.EngineConfig{
			Domains:          []string{"FRL"},
			Limits:           sqldriver.UnboundedJavaCompatLimits(),
			PlanCacheOptions: planCacheOpts,
		}
		var engOpts []sqldriver.EngineOption
		if r.cfg.Database != nil {
			engOpts = append(engOpts, sqldriver.WithDatabase(r.cfg.Database))
		} else {
			cfg.ClusterFile = r.cfg.ClusterFile
		}
		eng, err := sqldriver.NewEngine(cfg, engOpts...)
		if err != nil {
			return nil, fmt.Errorf("open engine %s: %w", t.Path, err)
		}
		r.engine = eng
	}
	var connOpts []sqldriver.ConnOption
	if t.Schema != "" {
		connOpts = append(connOpts, sqldriver.WithSchema(t.Schema))
	}
	db, err := r.engine.OpenDB(t.Path, connOpts...)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", t.Path, err)
	}
	r.dbs[t] = db
	return db, nil
}

func (r *runner) executeSetup(ctx context.Context, resource string, blk *javayamsql.Block) error {
	if !javayamsql.SupportedAtCurrentVersion(blk.Setup.SupportedVersion) {
		r.skip(SkipVersionGate, resource+" setup", "block supported_version")
		return nil
	}
	target, err := r.resolveConnect(resource, blk.Setup.Connect)
	if err != nil {
		return fmt.Errorf("setup connect: %w", err)
	}
	db, err := r.open(target)
	if err != nil {
		return err
	}
	opts, err := r.connectionOptions(blk.Setup.ConnectionOptions)
	if err != nil {
		return fmt.Errorf("setup: %w", err)
	}
	conn, err := pin(ctx, db, opts)
	if err != nil {
		return fmt.Errorf("setup conn: %w", err)
	}
	defer conn.Close()
	if r.fixture != nil {
		if target != r.fixture.target {
			return fmt.Errorf("factory fixture setup resolved outside its private schema")
		}
		// On a pinned connection like every other block: a fixture file has
		// no connection_options (validatePrivateFixture), so this is the
		// empty set, installed rather than whatever the pool last held.
		return r.fixture.load(ctx, conn, blk.Setup.Steps, r.result)
	}
	for _, step := range blk.Setup.Steps {
		if step.Kind != javayamsql.CommandQuery {
			if err := r.runSchemaCommand(ctx, conn, step); err != nil {
				return &setupError{line: step.Line, query: string(step.Kind) + ": " + truncate(step.Payload), err: err}
			}
			continue
		}
		if len(step.Segments) > 0 {
			// A setup step is parsed with a nil Random in Java, which asserts
			// that no injections are present. Reaching here means the corpus
			// changed shape.
			return fmt.Errorf("setup step at line %d carries a parameter injection", step.Line)
		}
		if _, err := execAny(ctx, conn, step.Query); err != nil {
			return &setupError{line: step.Line, query: truncate(step.Query), err: err}
		}
	}
	return nil
}

func truncate(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}

// execer is the shared surface of *sql.DB and *sql.Conn, so a block that holds
// one connection for its whole run and one that takes a fresh connection per
// test go down the same code path.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// isRowReturning reports whether a statement must go down database/sql's Query
// path rather than its Exec path.
//
// Java calls Statement.execute and reads back whichever of the two results
// materialised; database/sql forces the choice up front, and the Go driver
// rejects a DML on Query and a row-returning statement on Exec, so the routing
// has to happen here. It is the same leading-keyword rule the Go-format runner
// already applies, reused rather than re-derived.
func isRowReturning(query string) bool { return yamsql.IsQuery(query) }

// execAny runs a statement through whichever path its shape requires and
// returns the update count for the non-row-returning path.
func execAny(ctx context.Context, db execer, query string) (int64, error) {
	if isRowReturning(query) {
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			return 0, err
		}
		defer rows.Close()
		for rows.Next() { //nolint:revive // draining the result set is the point
		}
		return 0, rows.Err()
	}
	res, err := db.ExecContext(ctx, query)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return n, nil
}

// queryRows materialises a row-returning statement into a resultSet.
func queryRows(ctx context.Context, db execer, query string) (*resultSet, error) {
	var meta api.ResultSetMetaData
	ctx = api.WithResultSetMetaDataObserver(ctx, func(md api.ResultSetMetaData) { meta = md })
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	rs := &resultSet{Cols: cols, Types: make([]string, len(cols)), Meta: meta}
	if ct, err := rows.ColumnTypes(); err == nil {
		for i, c := range ct {
			if i < len(rs.Types) {
				rs.Types[i] = c.DatabaseTypeName()
			}
		}
	}
	for rows.Next() {
		dest := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range dest {
			ptrs[i] = &dest[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		rs.Rows = append(rs.Rows, dest)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return rs, nil
}

// errorCodeOf extracts the SQLSTATE Java's `error:` config compares against.
func errorCodeOf(err error) (string, bool) {
	var apiErr *api.Error
	if errors.As(err, &apiErr) {
		return strings.TrimSpace(string(apiErr.Code)), true
	}
	return "", false
}
