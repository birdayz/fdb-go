package javacorpus

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/conformance/javayamsql"
	"fdb.dev/pkg/relational/conformance/yamsql"
)

// Config parameterises one corpus run.
type Config struct {
	// ClusterFile is the FDB cluster file every connection is opened against.
	ClusterFile string
	// IDPrefix disambiguates the generated database/template/schema names so
	// concurrent runs of the same file cannot collide. It must be a legal SQL
	// identifier fragment.
	IDPrefix string
}

// catalogPath is the system database every DDL statement is issued against.
// Java hard-codes connection index 0 to `jdbc:embed:/__SYS?schema=CATALOG` and
// runs the whole schema_template lifecycle there.
const catalogPath = "/__SYS"

// connTarget is a resolved `connect:` destination.
type connTarget struct {
	// Path is the database path, e.g. "/YAML_X_DB" or catalogPath.
	Path string
	// Schema is the default schema, empty for the catalog connection.
	Schema string
}

// runner holds the state Java keeps on its YamlExecutionContext for one file.
type runner struct {
	cfg    Config
	result *FileResult

	// connsByResource is Java's per-resource connectionURIs list: a
	// schema_template block registers its database here, and `connect: n`
	// indexes into it 1-based. It is per-resource because an included file's
	// registrations are scoped to that file unless reached via `(global)`.
	connsByResource map[string][]connTarget
	// resourceOrder is the include call order, oldest ancestor first. The
	// global list is the concatenation of these resources' local lists.
	resourceOrder []string

	dbs      map[connTarget]*sql.DB
	cleanups []func()
	// counter makes the generated identifiers unique within one file.
	counter int
}

// Run executes one corpus file and returns its ledger entry.
//
// It never returns an error for a corpus-level outcome — a failing file is a
// StatusFail result, a non-executable one is a StatusSkip with a class. Only a
// caller mistake (no cluster file) is reported out of band.
func Run(ctx context.Context, corpus *javayamsql.Corpus, path string, cfg Config) FileResult {
	res := FileResult{Path: path}

	switch javayamsql.PolarityOf(path) {
	case javayamsql.NegativeParse:
		// The parse gate already pins these; counting them here closes the
		// file total without re-asserting someone else's check.
		res.Status = StatusSkip
		res.SkipClass = SkipPolarityNegativeParse
		return res
	case javayamsql.Fragment:
		res.Status = StatusSkip
		res.SkipClass = SkipFragment
		return res
	}

	file, err := corpus.ParseFile(path)
	if err != nil {
		res.Status = StatusFail
		res.Err = fmt.Errorf("parse: %w", err)
		return res
	}

	r := &runner{
		cfg:             cfg,
		result:          &res,
		connsByResource: map[string][]connTarget{},
		dbs:             map[connTarget]*sql.DB{},
	}
	defer r.teardown()

	runErr := r.execute(ctx, file)

	// A block that cannot run at all takes the file with it, as a counted
	// skip. Java does the same through Assumptions.assumeTrue, which aborts
	// rather than fails.
	var skipErr *skipFileError
	if errors.As(runErr, &skipErr) {
		res.Status = StatusSkip
		res.SkipClass = skipErr.class
		res.Skips = append(res.Skips, Skip{Class: skipErr.class, Where: path, Detail: skipErr.detail})
		return res
	}
	// A schema_template the engine will not create is an engine gap with a
	// name, not a corpus regression — that is what the DDL skip classes are.
	var ddl *ddlError
	if errors.As(runErr, &ddl) {
		class := classifyDDLGap(file, ddl)
		res.Status = StatusSkip
		res.SkipClass = class
		res.Skips = append(res.Skips, Skip{Class: class, Where: path, Detail: ddl.Error()})
		return res
	}
	// A failure this run has already measured, booked, and pinned to its exact
	// rejection. Any OTHER failure in the same file stays a hard failure.
	if gap, ok := gapFor(path, runErr); ok {
		res.Status = StatusSkip
		res.SkipClass = gap.Class
		res.Skips = append(res.Skips, Skip{Class: gap.Class, Where: path, Detail: gap.Booking + ": " + runErr.Error()})
		return res
	}

	// A fixed-version meta-test has no Java stream on the current version, so
	// upstream states no expectation for it. That is a reason to MEASURE it,
	// not to wave it through: Go can run what Java declines to, and the one
	// outcome that would be a real finding is a file asserting something and
	// getting it right — that would mean the gate under test resolves the same
	// way on current as on the pinned version, and the file was never
	// version-specific at all. Anything else (it fails, or it reaches no
	// assertion) is the expected "cannot be evaluated here".
	if javayamsql.PolarityOf(path) == javayamsql.FixedVersionMeta {
		res.Status = StatusSkip
		res.SkipClass = SkipFixedVersionMeta
		if runErr == nil && res.QueriesRun > 0 {
			res.Status = StatusFail
			res.Err = fmt.Errorf("file is classified fixed-version-meta (%s) — Java runs it only against a pinned "+
				"version — but on the current version it asserted %d queries and passed. It is not version-specific; "+
				"reclassify it as Positive",
				javayamsql.Manifest[path].Reason, res.QueriesRun)
		}
		return res
	}

	// Execution-level negatives are the corpus's own meta-tests: upstream
	// asserts they FAIL. Their pin is therefore inverted — a clean run is the
	// bug, not the success.
	if javayamsql.PolarityOf(path) == javayamsql.NegativeExecution {
		if runErr != nil {
			res.Status = StatusSkip
			res.SkipClass = SkipPolarityNegativeExecution
			return res
		}
		// A negative that ran clean is only a finding if the assertion
		// upstream expects to trip was actually reached. When the directive
		// carrying it was skipped for a capability reason, the file was never
		// evaluated at all, and calling that a passed negative would credit a
		// gap as a result.
		if class, ok := suppressedBy(res.Skips); ok {
			res.Status = StatusSkip
			res.SkipClass = class
			return res
		}
		res.Status = StatusFail
		res.Err = fmt.Errorf("file is classified negative(execution) — upstream asserts it fails (%s) — but it ran clean",
			javayamsql.Manifest[path].Reason)
		return res
	}

	if runErr != nil {
		res.Status = StatusFail
		res.Err = runErr
		return res
	}
	// A positive file that asserted nothing is not a pass. Reporting it as one
	// is how a skipped dimension reads as covered.
	if res.QueriesRun == 0 {
		res.Status = StatusSkip
		if class, ok := suppressedBy(res.Skips); ok {
			res.SkipClass = class
		} else {
			res.SkipClass = SkipVacuous
		}
		return res
	}
	res.Status = StatusPass
	return res
}

// SuppressesAssertion reports whether a skip class removed a result assertion
// rather than merely declining an extra dimension of the same one.
//
// The distinction decides whether a file that ran clean actually proved
// anything: skipping the prepared arm still leaves the simple arm asserting the
// rows, whereas skipping the only `result:` leaves nothing behind it. It is
// exported so the negative-execution accounting can be COUNTED against it
// rather than restated in prose.
func (c SkipClass) SuppressesAssertion() bool {
	switch c {
	// SkipResultMetadataNested belongs here even though the row assertion
	// survives it: the `check-result-metadata/shouldFail/wrong-nested-*` files
	// are negatives whose ONLY failing assertion IS the metadata one, so
	// declining it would let them run clean and be reported as negatives that
	// wrongly passed — crediting a driver gap as a finding.
	case SkipPlanAssertion, SkipResultMetadataNested, SkipContinuation,
		SkipTemporaryFunction, SkipRandomInjection, SkipVersionGate,
		SkipSchemaCommand, SkipNoChecks:
		return true
	default:
		return false
	}
}

// suppressedBy returns the first skip that removed an assertion.
func suppressedBy(skips []Skip) (SkipClass, bool) {
	for _, s := range skips {
		if s.Class.SuppressesAssertion() {
			return s.Class, true
		}
	}
	return "", false
}

func (r *runner) skip(class SkipClass, where, detail string) {
	r.result.Skips = append(r.result.Skips, Skip{Class: class, Where: where, Detail: detail})
}

func (r *runner) teardown() {
	for i := len(r.cleanups) - 1; i >= 0; i-- {
		r.cleanups[i]()
	}
	for _, db := range r.dbs {
		_ = db.Close()
	}
}

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
	return nil
}

// executeSchemaTemplate runs Java's five-statement lifecycle against the
// catalog connection and registers the resulting database as a connect target.
func (r *runner) executeSchemaTemplate(ctx context.Context, resource string, blk *javayamsql.Block) error {
	variant, ok := selectVariant(blk.SchemaTemplate)
	if !ok {
		return fmt.Errorf("no schema_template variant matches the version under test")
	}

	r.counter++
	id := fmt.Sprintf("YAML_%s_%d", r.cfg.IDPrefix, r.counter)
	tmpl := id + "_TEMPLATE"
	// Java's DOMAIN is "/FRL", giving a two-segment database path. The Go SQL
	// layer's DDL parser accepts only a single-segment path, so the domain is
	// folded into the identifier. Nothing in the corpus observes the path.
	dbPath := "/" + id + "_DB"
	schema := id + "_SCHEMA"

	cat, err := r.open(connTarget{Path: catalogPath})
	if err != nil {
		return err
	}

	for _, stmt := range []string{
		fmt.Sprintf("DROP SCHEMA TEMPLATE IF EXISTS %s", tmpl),
		fmt.Sprintf("DROP DATABASE IF EXISTS %s", dbPath),
	} {
		if _, err := cat.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("%s: %w", stmt, err)
		}
	}

	create := fmt.Sprintf("CREATE SCHEMA TEMPLATE %s %s", tmpl, variant.Definition)
	if _, err := cat.ExecContext(ctx, create); err != nil {
		return &ddlError{stmt: "CREATE SCHEMA TEMPLATE", err: err}
	}
	r.cleanups = append(r.cleanups, func() {
		_, _ = cat.Exec(fmt.Sprintf("DROP SCHEMA TEMPLATE IF EXISTS %s", tmpl))
	})

	if _, err := cat.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath)); err != nil {
		return &ddlError{stmt: "CREATE DATABASE", err: err}
	}
	r.cleanups = append(r.cleanups, func() {
		_, _ = cat.Exec(fmt.Sprintf("DROP DATABASE IF EXISTS %s", dbPath))
	})

	if _, err := cat.ExecContext(ctx,
		fmt.Sprintf("CREATE SCHEMA %s/%s WITH TEMPLATE %s", dbPath, schema, tmpl)); err != nil {
		return &ddlError{stmt: "CREATE SCHEMA", err: err}
	}

	r.connsByResource[resource] = append(r.connsByResource[resource], connTarget{Path: dbPath, Schema: schema})
	return nil
}

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
	// The engine's own rejection is the better signal where it is specific:
	// routing every `AS SELECT` index to the aggregate builder is one named
	// gap (CQ-71), and it must not disappear into the residual bucket.
	if msg := ddl.Error(); strings.Contains(msg, "select element must contain an aggregate function") ||
		strings.Contains(msg, "exactly one aggregate expression required in SELECT") ||
		strings.Contains(msg, "select element must be an expression") {
		return SkipDDLValueIndexAsSelect
	}
	return classifyDDLByDeclaration(f)
}

func classifyDDLByDeclaration(f *javayamsql.File) SkipClass {
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
		// The catalog connection carries no schema in the Go DSN: its schema
		// is implied, exactly as `connect: 0` builds it.
		return connTarget{Path: catalogPath}, nil
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
		return connTarget{Path: catalogPath}, nil
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
	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s", t.Path, r.cfg.ClusterFile)
	if t.Schema != "" {
		dsn += "&schema=" + t.Schema
	}
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", t.Path, err)
	}
	r.dbs[t] = db
	return db, nil
}

func (r *runner) executeSetup(ctx context.Context, resource string, blk *javayamsql.Block) error {
	target, err := r.resolveConnect(resource, blk.Setup.Connect)
	if err != nil {
		return fmt.Errorf("setup connect: %w", err)
	}
	db, err := r.open(target)
	if err != nil {
		return err
	}
	for _, step := range blk.Setup.Steps {
		if step.Kind != javayamsql.CommandQuery {
			r.skip(SkipSchemaCommand, resource, string(step.Kind))
			return &skipFileError{SkipSchemaCommand, string(step.Kind)}
		}
		if len(step.Segments) > 0 {
			// A setup step is parsed with a nil Random in Java, which asserts
			// that no injections are present. Reaching here means the corpus
			// changed shape.
			return fmt.Errorf("setup step at line %d carries a parameter injection", step.Line)
		}
		if _, err := execAny(ctx, db, step.Query); err != nil {
			return fmt.Errorf("setup step at line %d %q: %w", step.Line, truncate(step.Query), err)
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
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	rs := &resultSet{Cols: cols, Types: make([]string, len(cols))}
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
