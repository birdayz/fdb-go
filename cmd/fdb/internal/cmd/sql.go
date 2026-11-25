// Package cmd (sql.go) — interactive SQL REPL for the fdbsql driver.
//
// Design notes:
//
//   - Line editing via chzyer/readline (history + arrow keys + ^R search);
//     bubbletea would be overkill for a line-at-a-time prompt and breaks
//     stdout piping.
//   - Styling via charm.land/lipgloss/v2 (already a transitive dep of
//     fang); used for the prompt + result-set table rendering.
//   - Multi-line input terminates on `;` or on a single-line backslash
//     meta-command; mirrors psql's input rules.
//   - Non-interactive execution via `-c <sql>` (single statement),
//     `-f <path>` (file), or redirected stdin, for CI/scripts.
package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"fdb.dev/cmd/fdb/internal/config"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/core/functions"

	"fdb.dev/pkg/relational/sqldriver"
)

// openSQLRunner opens the fdbsql connection pool for one `fdb sql`
// invocation and the runner over it, from the flags as the user spelled them.
//
// The database path and the schema are SQL identifiers, read as DDL reads
// them: unquoted folds to upper case, a path whole (CREATE DATABASE /FRL/myapp
// creates /FRL/MYAPP, CREATE SCHEMA /FRL/myapp/main creates MAIN). The driver takes
// the DSN's names as given, and so do the catalog lookups behind the
// meta-commands (`\d`), so both are fed the one folded pair; deriving them
// separately is how `\d` came to look up a path the connection never used.
func openSQLRunner(
	ctx context.Context,
	out, errOut io.Writer,
	clusterFile, databaseURI, schema, outputFmt string,
) (*sqlRunner, error) {
	databaseURI = functions.NormalizeIdentifier(databaseURI)
	schema = functions.NormalizeIdentifier(schema)
	// Pick the style profile per-writer: real terminal → colors +
	// box-drawing; pipe/redirect/test buffer → plain ASCII. Scripts
	// consuming `fdb sql -c … | jq` must never see ANSI escapes.
	st := plainSQLStyles()
	if isTerminalWriter(out) {
		st = ttySQLStyles()
	}
	return &sqlRunner{
		out:         out,
		errOut:      errOut,
		ctx:         ctx,
		clusterFile: clusterFile,
		database:    databaseURI,
		schema:      schema,
		st:          st,
		format:      outputFmt,
		timing:      true,
	}, nil
}

// newSQLCmd is the top-level `sql` noun — opens an interactive REPL
// against a relational cluster. Non-interactive modes (-c, -f, stdin)
// share script-friendly output.
func newSQLCmd() *cobra.Command {
	var (
		contextName string
		databaseURI string
		initSchema  string
		cmdline     string
		filePath    string
		clusterFile string
		outputFmt   string
	)
	c := &cobra.Command{
		Use:   "sql",
		Short: "Interactive SQL REPL against the relational layer (psql-style)",
		Example: `  fdb sql --database /FRL/myapp
  fdb sql --database /FRL/myapp --schema main
  fdb sql --database /FRL/myapp -c "SELECT count(*) FROM orders"
  fdb sql --database /FRL/myapp -f migrations/001.sql`,
		Long: "Opens a psql-style REPL via the fdbsql driver " +
			"(`database/sql`). Multi-line statements end at `;`; single-" +
			"line `\\`-prefixed meta-commands run immediately.\n\n" +
			"Meta-commands (subset of psql):\n" +
			"  \\?          — show this help\n" +
			"  \\q          — quit (same as Ctrl-D)\n" +
			"  \\d [table]  — list tables / describe one (columns, types, PK)\n" +
			"  \\dt         — list schema templates\n" +
			"  \\c <name>   — switch to schema <name>\n" +
			"  \\i <file>   — execute statements from file\n" +
			"  \\e          — open $EDITOR, run the buffer on exit\n" +
			"  \\x          — toggle expanded (record-per-block) display\n" +
			"  \\timing     — toggle the row-count/duration footer\n" +
			"  \\explain    — re-run the previous query wrapped in EXPLAIN\n\n" +
			"Non-interactive alternatives:\n" +
			"  --command / -c  run one statement, print result, exit\n" +
			"  --file / -f     run a .sql file, stopping on the first error\n" +
			"  redirected stdin runs a SQL script without prompts, stopping on error\n\n" +
			"SQL schemas and metadata live in the relational catalog. " +
			"Create them with SQL DDL before querying tables. --database is " +
			"the database URI (e.g. /FRL/myapp); --schema sets the default " +
			"search scope. Both default to the selected relational context; " +
			"an explicit --database does not inherit the context's schema.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateOutputFormat(outputFmt,
				sqlFormatTable, sqlFormatCSV, sqlFormatJSON, sqlFormatNDJSON); err != nil {
				return err
			}
			target, err := resolveSQLAddress(cmd, contextName, clusterFile, databaseURI, initSchema)
			if err != nil {
				return err
			}
			runner, err := openSQLRunner(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(),
				target.clusterFile(), target.database, target.schema, outputFmt)
			if err != nil {
				return err
			}
			defer runner.close()
			switch {
			case cmd.Flags().Changed("command"):
				return runner.runOnce(cmdline)
			case cmd.Flags().Changed("file"):
				return runner.runFile(filePath)
			case !isTerminalReader(cmd.InOrStdin()):
				return runner.runScript(cmd.InOrStdin(), "stdin")
			default:
				return runner.repl()
			}
		},
	}
	c.Flags().StringVar(&contextName, "context", "", "context name to use")
	c.Flags().StringVar(&databaseURI, "database", "", "database URI (defaults to the relational context, e.g. /FRL/myapp)")
	c.Flags().StringVar(&initSchema, "schema", "", "default schema for un-qualified references (defaults to the relational context)")
	c.Flags().StringVarP(&cmdline, "command", "c", "", "run a single SQL statement non-interactively and exit")
	c.Flags().StringVarP(&filePath, "file", "f", "", "execute SQL statements from a file and exit")
	c.Flags().StringVar(&clusterFile, "cluster-file", "", "FDB cluster file; overrides the context's cluster_file — chains with `fdb dev up`")
	c.Flags().StringVarP(&outputFmt, "output", "o", sqlFormatTable,
		"result format: table, csv, json, or ndjson (machine formats send the row-count footer to stderr)")
	_ = c.RegisterFlagCompletionFunc("output",
		func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
			return []string{sqlFormatTable, sqlFormatCSV, sqlFormatJSON, sqlFormatNDJSON}, cobra.ShellCompDirectiveNoFileComp
		})
	return c
}

// Preserve quoted names until SQL normalization; an explicit database must not
// inherit a schema from a different context address.
func resolveSQLAddress(cmd *cobra.Command, contextName, clusterFile, database, schema string) (*storeTarget, error) {
	target, err := (&storeAddressFlags{contextName: contextName, clusterFile: clusterFile}).resolve()
	if err != nil {
		var missingContext *config.ContextNotFoundError
		if !errors.Is(err, config.ErrNoContext) || errors.As(err, &missingContext) || contextName != "" {
			return nil, err
		}
		// An explicit database can use default cluster-file discovery without
		// a config, just as an explicit --cluster-file can.
		target = &storeTarget{clusterFileFlag: clusterFile}
	}
	if !cmd.Flags().Changed("database") && database == "" {
		database = target.cfgCtx.GetDatabase()
		if !cmd.Flags().Changed("schema") && schema == "" {
			schema = target.cfgCtx.GetSchema()
		}
	}
	if database == "" {
		// A sentence word keeps fang from capitalizing the flag spelling.
		const missing = "missing required flag --database (e.g. --database /FRL/myapp)"
		if err != nil {
			return nil, fmt.Errorf("%s: %w", missing, err)
		}
		return nil, errors.New(missing)
	}
	target.database, target.schema = database, schema
	return target, nil
}

// sqlRunner owns the database handle + I/O. Shared by the REPL loop
// and the non-interactive -c / -f / stdin paths so all entry points
// render results identically.
//
// `conn` pins the session to one *sql.Conn: transaction state
// (BEGIN/COMMIT/ROLLBACK sent as raw SQL) is per-connection in
// fdb-relational, so every statement the REPL runs — including DDL,
// queries, and tx commands — must flow through the same handle.
// `db.Conn(ctx)` is grabbed lazily on first execute().
type sqlRunner struct {
	engine      *sqldriver.Engine
	db          *sql.DB
	conn        *sql.Conn
	out         io.Writer
	errOut      io.Writer
	ctx         context.Context
	clusterFile string    // effective cluster file (flag > context) for DSN + catalog lookups
	database    string    // active database URI (flag or relational context)
	schema      string    // tracked for the prompt; set by \c
	inTx        bool      // reflects the driver's tx state for prompt styling
	st          sqlStyles // TTY (color+box-drawing) or plain (ASCII) profile
	format      string    // sqlFormat* — -o flag
	expanded    bool      // \x — record-per-block table rendering
	timing      bool      // \timing — row-count/duration footer (default on)
	lastStmt    string    // last executed query, for \explain
}

// Output formats for `fdb sql -o`.
const (
	sqlFormatTable  = "table"
	sqlFormatCSV    = "csv"
	sqlFormatJSON   = "json"
	sqlFormatNDJSON = "ndjson"
)

// --- entry points ---------------------------------------------------------

// runOnce runs a single SQL statement non-interactively — the -c path.
// Same rendering as the REPL, minus prompts.
func (r *sqlRunner) runOnce(stmt string) error {
	stmt = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(stmt), ";"))
	if stmt == "" {
		return nil
	}
	return r.execute(stmt)
}

// runFile and redirected stdin share one fail-fast script path.
func (r *sqlRunner) runFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	defer f.Close()
	return r.runScript(f, path)
}

func (r *sqlRunner) runScript(in io.Reader, label string) error {
	ctx := r.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("read %s: %w", label, err)
	}
	data, err := readSQLScript(ctx, in)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("read %s: %w", label, ctxErr)
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", label, err)
	}
	for i, stmt := range splitStatements(string(data)) {
		if err := r.execute(stmt); err != nil {
			return fmt.Errorf("statement %d in %s: %w", i+1, label, err)
		}
	}
	return nil
}

func readSQLScript(ctx context.Context, in io.Reader) ([]byte, error) {
	if f, ok := in.(*os.File); ok && f == os.Stdin {
		// Inherited stdin may be a blocking NewFile descriptor. Poll it without
		// changing OFD flags shared with the parent, and leave borrowed stdin open.
		raw, err := f.SyscallConn()
		if err != nil {
			return nil, err
		}
		return io.ReadAll(&sqlStdinReader{ctx: ctx, raw: raw})
	}
	if closer, ok := in.(io.ReadCloser); ok {
		closed := make(chan struct{})
		stop := context.AfterFunc(ctx, func() {
			defer close(closed)
			_ = closer.Close()
		})
		defer func() {
			if !stop() {
				<-closed
			}
		}()
	}
	return io.ReadAll(in)
}

// sqlStdinReader polls idle inherited pipes without a goroutine. It assumes
// exclusive stdin ownership and cannot interrupt a blocking filesystem Read.
type sqlStdinReader struct {
	ctx context.Context
	raw syscall.RawConn
}

func (r *sqlStdinReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		var n int
		var readErr error
		var ready bool
		err := r.raw.Control(func(fd uintptr) {
			fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
			_, readErr = unix.Poll(fds, 50)
			if readErr != nil || fds[0].Revents == 0 {
				return
			}
			if readErr = r.ctx.Err(); readErr != nil {
				return
			}
			if fds[0].Revents&unix.POLLNVAL != 0 {
				readErr = unix.EBADF
				return
			}
			if fds[0].Revents&(unix.POLLIN|unix.POLLHUP|unix.POLLERR) != 0 {
				ready = true
				n, readErr = unix.Read(int(fd), p)
			}
		})
		if ctxErr := r.ctx.Err(); ctxErr != nil {
			return 0, ctxErr
		}
		if err != nil {
			return 0, err
		}
		if errors.Is(readErr, unix.EINTR) || errors.Is(readErr, unix.EAGAIN) {
			continue
		}
		if readErr != nil {
			return 0, readErr
		}
		if ready {
			if n == 0 {
				return 0, io.EOF
			}
			return n, nil
		}
	}
}

// --- execution -----------------------------------------------------------

// execute runs one statement on the session's pinned *sql.Conn.
// SELECT-shaped queries go through Query + table render; everything
// else goes through Exec and reports rows-affected / duration.
// Statement type is inferred from the leading keyword — same heuristic
// psql uses. Tx commands (BEGIN / COMMIT / ROLLBACK) flip r.inTx so
// the prompt and meta-commands reflect the driver's state.
func (r *sqlRunner) execute(stmt string) error {
	stmt = strings.TrimSpace(stmt)
	if stmt == "" {
		return nil
	}
	if err := r.ensureConn(); err != nil {
		return err
	}
	start := time.Now()
	if isQuery(stmt) {
		// Remember the statement for `\explain` — verbatim EXPLAIN
		// re-run, no plan interpretation (RFC-174). EXPLAINs
		// themselves are not remembered, so repeated \explain stays
		// idempotent instead of explaining the explain.
		if fields := strings.Fields(strings.ToUpper(stmt)); len(fields) > 0 && fields[0] != "EXPLAIN" {
			r.lastStmt = stmt
		}
		rows, err := r.conn.QueryContext(r.ctx, stmt)
		if err != nil {
			return err
		}
		defer rows.Close()
		n, err := r.renderRows(rows)
		if err != nil {
			return err
		}
		r.footer(fmt.Sprintf("(%d row%s, %s)", n, plural(n), time.Since(start).Round(time.Millisecond)))
		return nil
	}
	// Pre-detect tx kind so we can (a) rewrite `BEGIN` to the spelling
	// the driver actually parses and (b) update our prompt state on
	// success. psql accepts both `BEGIN` and `START TRANSACTION`;
	// fdb-relational only parses the latter — translate transparently
	// so operators don't hit a parser error on the common spelling.
	kind := txCommand(stmt)
	sent := stmt
	if kind == txBegin {
		sent = "START TRANSACTION"
	}
	res, err := r.conn.ExecContext(r.ctx, sent)
	if err != nil {
		return err
	}
	// Update the prompt state *after* a successful tx command — the
	// driver's state is authoritative; we only mirror it for display.
	switch kind {
	case txBegin:
		r.inTx = true
	case txCommit, txRollback:
		r.inTx = false
	}
	affected := int64(-1)
	if res != nil {
		if n, aErr := res.RowsAffected(); aErr == nil {
			affected = n
		}
	}
	if affected >= 0 {
		r.footer(fmt.Sprintf("OK (%d row%s affected, %s)",
			affected, plural(int(affected)), time.Since(start).Round(time.Millisecond)))
	} else {
		r.footer("OK (" + time.Since(start).Round(time.Millisecond).String() + ")")
	}
	return nil
}

// ensureDB lazily constructs the runner's sqldriver.Engine and *sql.DB pool.
func (r *sqlRunner) ensureDB() error {
	if r.db != nil {
		return nil
	}
	engine, err := sqldriver.NewEngine(sqldriver.EngineConfig{
		ClusterFile: r.clusterFile,
		Domains:     []string{"FRL"},
		Limits:      sqldriver.UnboundedJavaCompatLimits(),
	})
	if err != nil {
		return fmt.Errorf("open sql engine: %w", err)
	}
	var connOpts []sqldriver.ConnOption
	if r.schema != "" {
		connOpts = append(connOpts, sqldriver.WithSchema(r.schema))
	}
	db, err := engine.OpenDB(r.database, connOpts...)
	if err != nil {
		_ = engine.Close()
		return fmt.Errorf("open fdbsql %q: %w", r.database, err)
	}
	r.engine = engine
	r.db = db
	return nil
}

// ensureConn lazily pins the runner to a single *sql.Conn so raw-SQL
// transaction commands (`BEGIN` / `COMMIT` / `ROLLBACK`) see a
// consistent session — they're per-connection in fdb-relational,
// and *sql.DB doesn't guarantee sticky connections by default.
func (r *sqlRunner) ensureConn() error {
	if r.conn != nil {
		return nil
	}
	if err := r.ensureDB(); err != nil {
		return err
	}
	c, err := r.db.Conn(r.ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	r.conn = c
	return nil
}

// switchSchema is `\c <name>`: the name is an SQL identifier, folded as
// --schema is, and the switch is the CONNECTION's, so the statements that
// follow run in the new schema and not only the meta-commands' lookups (which
// read r.schema). A schema the database does not hold is refused by the
// connection and leaves both on the old one.
func (r *sqlRunner) switchSchema(name string) error {
	name = functions.NormalizeIdentifier(name)
	if err := r.ensureConn(); err != nil {
		return err
	}
	if err := r.conn.Raw(func(dc any) error {
		ec, ok := dc.(*embedded.EmbeddedConnection)
		if !ok {
			return fmt.Errorf("driver connection is %T, not *embedded.EmbeddedConnection", dc)
		}
		return ec.SetSchema(name)
	}); err != nil {
		return err
	}
	r.schema = name
	return nil
}

// close releases the pinned connection. If the REPL is mid-tx, issue
// a best-effort ROLLBACK first — psql does the same on EOF, and
// leaking an open tx to a pool would pin the connection forever.
func (r *sqlRunner) close() error {
	var err error
	if r.conn != nil {
		if r.inTx {
			// Best-effort — the whole point is we're closing anyway.
			_, _ = r.conn.ExecContext(r.ctx, "ROLLBACK")
			r.inTx = false
		}
		err = r.conn.Close()
		r.conn = nil
	}
	if r.db != nil {
		_ = r.db.Close()
		r.db = nil
	}
	if r.engine != nil {
		if eErr := r.engine.Close(); err == nil {
			err = eErr
		}
		r.engine = nil
	}
	return err
}
