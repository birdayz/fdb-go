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
//   - Non-interactive execution via `-c <sql>` (single statement) or
//     `-f <path>` (file), for CI/scripts.
package cmd

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/term"
	"github.com/chzyer/readline"
	"github.com/spf13/cobra"

	"fdb.dev/pkg/recordlayer"
	relapi "fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/catalog"

	// Register the "fdbsql" driver via blank import.
	_ "fdb.dev/pkg/relational/sqldriver"
)

// newSQLCmd is the top-level `sql` noun — opens an interactive REPL
// against a relational cluster. Non-interactive modes (-c, -f) are
// useful for shell scripts and CI smoke-tests.
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
		Example: `  frl sql --database /myapp
  frl sql --database /myapp --schema main
  frl sql --database /myapp -c "SELECT count(*) FROM orders"
  frl sql --database /myapp -f migrations/001.sql`,
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
			"  --file / -f     run every statement in a .sql file, exit\n\n" +
			"Requires the cluster to have a relational catalog " +
			"(`__SYS/CATALOG` populated) — plain-core clusters return " +
			"a clear error on the first query. --database is the " +
			"database URI (e.g. /myapp); --schema sets the default " +
			"search scope so queries don't need schema-qualified names.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateOutputFormat(outputFmt,
				sqlFormatTable, sqlFormatCSV, sqlFormatJSON, sqlFormatNDJSON); err != nil {
				return err
			}
			// --cluster-file makes the invocation self-contained (chains
			// with `frl fdb up`); otherwise the context supplies it.
			f := storeAddressFlags{contextName: contextName, clusterFile: clusterFile}
			target, err := f.resolve()
			if err != nil {
				return err
			}
			cf := target.clusterFile()
			if databaseURI == "" {
				// Starts with a sentence word: fang capitalizes the first
				// rune of error banners, which would garble a leading flag
				// name into "--Database".
				return fmt.Errorf("missing required flag --database (e.g. --database /myapp)")
			}
			dsn := buildFDBSQLDSN(cf, databaseURI, initSchema)
			db, err := sql.Open("fdbsql", dsn)
			if err != nil {
				return fmt.Errorf("open fdbsql %q: %w", dsn, err)
			}
			defer db.Close()

			// Pick the style profile per-writer: real terminal → colors +
			// box-drawing; pipe/redirect/test buffer → plain ASCII. Scripts
			// consuming `frl sql -c … | jq` must never see ANSI escapes.
			st := plainSQLStyles()
			if isTerminalWriter(cmd.OutOrStdout()) {
				st = ttySQLStyles()
			}
			runner := &sqlRunner{
				db:          db,
				out:         cmd.OutOrStdout(),
				errOut:      cmd.ErrOrStderr(),
				ctx:         cmd.Context(),
				clusterFile: cf,
				database:    databaseURI,
				schema:      initSchema,
				st:          st,
				format:      outputFmt,
				timing:      true,
			}
			defer runner.close()
			switch {
			case cmdline != "":
				return runner.runOnce(cmdline)
			case filePath != "":
				return runner.runFile(filePath)
			default:
				return runner.repl()
			}
		},
	}
	c.Flags().StringVar(&contextName, "context", "", "context name to use")
	c.Flags().StringVar(&databaseURI, "database", "", "database URI (required, e.g. /myapp)")
	c.Flags().StringVar(&initSchema, "schema", "", "default schema for un-qualified references")
	c.Flags().StringVarP(&cmdline, "command", "c", "", "run a single SQL statement non-interactively and exit")
	c.Flags().StringVarP(&filePath, "file", "f", "", "execute SQL statements from a file and exit")
	c.Flags().StringVar(&clusterFile, "cluster-file", "", "FDB cluster file; overrides the context's cluster_file — chains with `frl fdb up`")
	c.Flags().StringVarP(&outputFmt, "output", "o", sqlFormatTable,
		"result format: table, csv, json, or ndjson (machine formats send the row-count footer to stderr)")
	return c
}

// buildFDBSQLDSN constructs the `fdbsql:///PATH?cluster_file=…&schema=…`
// DSN the driver accepts. Keeps the DSN-shape knowledge local to one
// place so the REPL + non-interactive paths agree.
//
// Query params go through url.Values so paths with `&`, `=`, `?`, or
// spaces (e.g. "/home/user/my project/fdb.cluster") round-trip through
// the driver's URL parser without corrupting the DSN.
func buildFDBSQLDSN(clusterFile, dbPath, schema string) string {
	var b strings.Builder
	b.WriteString("fdbsql:///")
	b.WriteString(strings.TrimPrefix(dbPath, "/"))
	params := url.Values{}
	if clusterFile != "" {
		params.Set("cluster_file", clusterFile)
	}
	if schema != "" {
		params.Set("schema", schema)
	}
	if len(params) > 0 {
		b.WriteString("?")
		b.WriteString(params.Encode())
	}
	return b.String()
}

// sqlRunner owns the database handle + I/O. Shared by the REPL loop
// and the non-interactive -c / -f paths so the three entry points
// render results identically.
//
// `conn` pins the session to one *sql.Conn: transaction state
// (BEGIN/COMMIT/ROLLBACK sent as raw SQL) is per-connection in
// fdb-relational, so every statement the REPL runs — including DDL,
// queries, and tx commands — must flow through the same handle.
// `db.Conn(ctx)` is grabbed lazily on first execute().
type sqlRunner struct {
	db          *sql.DB
	conn        *sql.Conn
	out         io.Writer
	errOut      io.Writer
	ctx         context.Context
	clusterFile string    // effective cluster file (flag > context) for DSN + catalog lookups
	database    string    // active database URI (from --database)
	schema      string    // tracked for the prompt; set by \c
	inTx        bool      // reflects the driver's tx state for prompt styling
	st          sqlStyles // TTY (color+box-drawing) or plain (ASCII) profile
	format      string    // sqlFormat* — -o flag
	expanded    bool      // \x — record-per-block table rendering
	timing      bool      // \timing — row-count/duration footer (default on)
	lastStmt    string    // last executed query, for \explain
}

// Output formats for `frl sql -o`.
const (
	sqlFormatTable  = "table"
	sqlFormatCSV    = "csv"
	sqlFormatJSON   = "json"
	sqlFormatNDJSON = "ndjson"
)

// tableInfo is the minimal shape rendered by \d — table name + column
// count. Sourced from the current schema's template via the relational
// catalog. PK columns aren't on api.Table; `meta catalog get <tpl>`
// is the right place for the full proto breakdown.
type tableInfo struct {
	name    string
	columns int
}

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

// runFile reads every statement in a .sql file, splitting on top-level
// semicolons, and runs each via execute(). A failing statement aborts
// the batch — matches psql's default `-f` behaviour.
func (r *sqlRunner) runFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	for i, stmt := range splitStatements(string(data)) {
		if err := r.execute(stmt); err != nil {
			return fmt.Errorf("statement %d in %s: %w", i+1, path, err)
		}
	}
	return nil
}

// repl is the interactive loop. Reads lines until a statement
// terminator (`;`) or a single-line meta-command, then dispatches.
func (r *sqlRunner) repl() error {
	rl, err := readline.NewEx(&readline.Config{
		Prompt:            r.prompt(),
		HistoryFile:       historyPath(),
		HistoryLimit:      1000,
		InterruptPrompt:   "^C",
		EOFPrompt:         "exit",
		HistorySearchFold: true,
	})
	if err != nil {
		return fmt.Errorf("start readline: %w", err)
	}
	defer rl.Close()

	banner := r.st.banner.Render("frl sql") + " — " +
		r.st.muted.Render("\\? for help, \\q to quit, multi-line ends at ; — BEGIN/COMMIT/ROLLBACK supported")
	fmt.Fprintln(r.out, banner)

	var buf strings.Builder
	for {
		if buf.Len() == 0 {
			rl.SetPrompt(r.prompt())
		} else {
			rl.SetPrompt(r.continuePrompt())
		}
		line, readErr := rl.Readline()
		if readErr != nil {
			// Ctrl-C mid-statement clears the buffer; Ctrl-C on empty
			// line and Ctrl-D exit.
			if errors.Is(readErr, readline.ErrInterrupt) {
				if buf.Len() > 0 {
					buf.Reset()
					continue
				}
				return nil
			}
			if errors.Is(readErr, io.EOF) {
				fmt.Fprintln(r.out)
				return nil
			}
			return readErr
		}

		// Meta-command shortcut — only when the buffer is empty.
		trimmed := strings.TrimSpace(line)
		if buf.Len() == 0 && strings.HasPrefix(trimmed, `\`) {
			if stop := r.runMeta(trimmed); stop {
				return nil
			}
			continue
		}

		// Accumulate, looking for the terminating `;`.
		buf.WriteString(line)
		buf.WriteString("\n")
		if endsStatement(line) {
			stmt := strings.TrimSpace(buf.String())
			stmt = strings.TrimSuffix(strings.TrimSpace(stmt), ";")
			buf.Reset()
			if stmt == "" {
				continue
			}
			if err := r.execute(stmt); err != nil {
				fmt.Fprintln(r.errOut, r.st.errS.Render("ERROR: ")+err.Error())
			}
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
		// re-run, no plan interpretation (RFC-174 Graefe G3). EXPLAINs
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

// footer emits the row-count/duration line. Suppressed by `\timing off`;
// in machine formats (csv/json/ndjson) it goes to stderr so stdout stays
// parseable.
func (r *sqlRunner) footer(msg string) {
	if !r.timing {
		return
	}
	w := r.out
	if r.format != sqlFormatTable {
		w = r.errOut
	}
	fmt.Fprintln(w, r.st.muted.Render(msg))
}

// ensureConn lazily pins the runner to a single *sql.Conn so raw-SQL
// transaction commands (`BEGIN` / `COMMIT` / `ROLLBACK`) see a
// consistent session — they're per-connection in fdb-relational,
// and *sql.DB doesn't guarantee sticky connections by default.
func (r *sqlRunner) ensureConn() error {
	if r.conn != nil {
		return nil
	}
	c, err := r.db.Conn(r.ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	r.conn = c
	return nil
}

// close releases the pinned connection. If the REPL is mid-tx, issue
// a best-effort ROLLBACK first — psql does the same on EOF, and
// leaking an open tx to a pool would pin the connection forever.
func (r *sqlRunner) close() error {
	if r.conn == nil {
		return nil
	}
	if r.inTx {
		// Best-effort — the whole point is we're closing anyway.
		_, _ = r.conn.ExecContext(r.ctx, "ROLLBACK")
		r.inTx = false
	}
	err := r.conn.Close()
	r.conn = nil
	return err
}

// txCommandKind identifies the transaction-control statements the REPL
// recognises. Detecting them lets us flip prompt styling and surface a
// "mid-tx" warning at EOF without waiting on the driver to tell us.
type txCommandKind int

const (
	txNone txCommandKind = iota
	txBegin
	txCommit
	txRollback
)

// txCommand maps a raw statement to its tx kind. Case-insensitive;
// matches `BEGIN`, `START TRANSACTION`, `COMMIT`, and `ROLLBACK`
// optionally followed by whitespace. Anything else is txNone.
func txCommand(stmt string) txCommandKind {
	head := strings.ToUpper(strings.TrimSpace(stmt))
	switch {
	case head == "BEGIN" || strings.HasPrefix(head, "BEGIN "),
		head == "START TRANSACTION" || strings.HasPrefix(head, "START TRANSACTION "):
		return txBegin
	case head == "COMMIT" || strings.HasPrefix(head, "COMMIT "):
		return txCommit
	case head == "ROLLBACK" || strings.HasPrefix(head, "ROLLBACK "):
		return txRollback
	}
	return txNone
}

// isQuery guesses whether stmt returns rows. Case-insensitive match on
// the leading keyword. Uses strings.Fields so newlines after the
// keyword still match — earlier HasPrefix("SELECT ") version missed
// multi-line statements like `SELECT\n  foo\nFROM bar;` and routed
// them to ExecContext.
func isQuery(stmt string) bool {
	fields := strings.Fields(strings.ToUpper(strings.TrimSpace(stmt)))
	if len(fields) == 0 {
		return false
	}
	switch fields[0] {
	case "SELECT", "WITH", "EXPLAIN", "SHOW", "VALUES", "DESCRIBE":
		return true
	}
	return false
}

// endsStatement reports whether a line terminates the current
// statement — either it ends with `;` (possibly followed by whitespace
// or a trailing comment) or the whole line is a meta-command.
func endsStatement(line string) bool {
	// Strip trailing comments (simple -- only; block comments would need
	// a real parser and psql punts on those too for terminator detection).
	l := line
	if i := strings.Index(l, "--"); i >= 0 {
		l = l[:i]
	}
	return strings.HasSuffix(strings.TrimSpace(l), ";")
}

// splitStatements splits a .sql file on top-level semicolons. Handles
// `--` comments; does NOT handle quoted semicolons or block comments —
// that's a full parser's job and not worth it for -f scripts. Good
// enough for migration/ seed files which keep statements simple.
func splitStatements(s string) []string {
	var out []string
	var cur strings.Builder
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") {
			continue
		}
		cur.WriteString(line)
		cur.WriteString("\n")
		if endsStatement(line) {
			stmt := strings.TrimSpace(cur.String())
			stmt = strings.TrimSuffix(strings.TrimSpace(stmt), ";")
			cur.Reset()
			if stmt != "" {
				out = append(out, stmt)
			}
		}
	}
	// Trailing statement without terminator — accept rather than drop.
	if tail := strings.TrimSpace(cur.String()); tail != "" {
		out = append(out, tail)
	}
	return out
}

// --- meta-commands --------------------------------------------------------

// runMeta handles a `\`-prefixed input line. Returns true when the
// REPL should exit (only `\q` does that today). Unknown commands print
// a hint but don't exit — matches psql's tolerance.
func (r *sqlRunner) runMeta(line string) (stop bool) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return false
	}
	switch fields[0] {
	case `\q`, `\quit`, `\exit`:
		return true
	case `\?`, `\help`:
		r.printMetaHelp()
	case `\d`:
		// Bare `\d` lists tables; `\d <table>` describes one (columns,
		// types, nullability, PK).
		if len(fields) >= 2 {
			if err := r.describeTable(fields[1]); err != nil {
				fmt.Fprintln(r.errOut, r.st.errS.Render("ERROR: ")+err.Error())
			}
			return false
		}
		if err := r.listTables(); err != nil {
			fmt.Fprintln(r.errOut, r.st.errS.Render("ERROR: ")+err.Error())
		}
	case `\x`:
		r.expanded = !r.expanded
		state := "off"
		if r.expanded {
			state = "on"
		}
		fmt.Fprintln(r.out, r.st.muted.Render("expanded display is "+state))
	case `\timing`:
		r.timing = !r.timing
		state := "off"
		if r.timing {
			state = "on"
		}
		fmt.Fprintln(r.out, r.st.muted.Render("timing is "+state))
	case `\explain`:
		// Verbatim EXPLAIN re-run of the previous query — rendering only,
		// never interpretation (RFC-174 Graefe G3: plan-shape analysis is
		// a query-engine feature, not a CLI one).
		if r.lastStmt == "" {
			fmt.Fprintln(r.errOut, r.st.muted.Render("no previous query to explain — run one first"))
			return false
		}
		if err := r.execute("EXPLAIN " + r.lastStmt); err != nil {
			fmt.Fprintln(r.errOut, r.st.errS.Render("ERROR: ")+err.Error())
		}
	case `\dt`:
		if err := r.listTemplates(); err != nil {
			fmt.Fprintln(r.errOut, r.st.errS.Render("ERROR: ")+err.Error())
		}
	case `\c`:
		if len(fields) < 2 {
			fmt.Fprintln(r.errOut, r.st.errS.Render("usage: \\c <schema>"))
			return false
		}
		if r.inTx {
			// Schema changes and open transactions don't mix cleanly —
			// fdb-relational would either leak the tx or refuse the
			// schema swap. Tell the operator to pick one.
			fmt.Fprintln(r.errOut, r.st.errS.Render(
				"cannot switch schema inside a transaction — COMMIT or ROLLBACK first"))
			return false
		}
		r.schema = fields[1]
		fmt.Fprintln(r.out, r.st.muted.Render("schema → "+r.schema))
	case `\i`:
		if len(fields) < 2 {
			fmt.Fprintln(r.errOut, r.st.errS.Render("usage: \\i <file>"))
			return false
		}
		if err := r.runFile(fields[1]); err != nil {
			fmt.Fprintln(r.errOut, r.st.errS.Render("ERROR: ")+err.Error())
		}
	case `\e`:
		stmt, err := readFromEditor(r.schema)
		if err != nil {
			fmt.Fprintln(r.errOut, r.st.errS.Render("ERROR: ")+err.Error())
			return false
		}
		if strings.TrimSpace(stmt) == "" {
			return false
		}
		for _, s := range splitStatements(stmt) {
			if execErr := r.execute(s); execErr != nil {
				fmt.Fprintln(r.errOut, r.st.errS.Render("ERROR: ")+execErr.Error())
				break
			}
		}
	default:
		fmt.Fprintln(r.errOut, r.st.muted.Render(
			fmt.Sprintf("unknown meta-command %q — try \\?", fields[0])))
	}
	return false
}

// listTables powers `\d` — tables in the current database+schema.
// fdb-relational doesn't expose information_schema, so we route through
// the same catalog API `meta catalog` uses: load the Schema record,
// then enumerate its template's tables.
func (r *sqlRunner) listTables() error {
	if r.database == "" || r.schema == "" {
		return fmt.Errorf("no active schema — use --database and --schema (or \\c <schema>)")
	}
	tables, err := r.loadSchemaTables(r.database, r.schema)
	if err != nil {
		return err
	}
	if len(tables) == 0 {
		_, err := fmt.Fprintln(r.out, r.st.muted.Render(
			fmt.Sprintf("(no tables in %s/%s)", r.database, r.schema)))
		return err
	}
	// Render a small two-column table. Detailed per-table shape lives in
	// `meta catalog get <template>` — that renders the full proto.
	headers := []string{"TABLE", "COLUMNS"}
	rows := make([][]string, 0, len(tables))
	for _, tbl := range tables {
		rows = append(rows, []string{tbl.name, fmt.Sprintf("%d", tbl.columns)})
	}
	return r.renderStaticTable(r.out, headers, rows)
}

// listTemplates powers `\dt` — every template visible on the cluster.
// Equivalent to `SHOW SCHEMA TEMPLATES` at SQL level; we use the
// catalog directly so the output shape matches `\d` (table render with
// headers, not raw SQL rows).
func (r *sqlRunner) listTemplates() error {
	rows, err := r.db.QueryContext(r.ctx, "SHOW SCHEMA TEMPLATES")
	if err != nil {
		return fmt.Errorf("%w (also available: `frl meta catalog templates`)", err)
	}
	defer rows.Close()
	_, err = r.renderTable(r.out, rows)
	return err
}

func (r *sqlRunner) printMetaHelp() {
	help := []struct {
		cmd, desc string
	}{
		{`\?`, "show this help"},
		{`\q`, "quit the REPL (also Ctrl-D)"},
		{`\d [table]`, "list tables / describe one (columns, types, PK)"},
		{`\dt`, "list schema templates (SHOW SCHEMA TEMPLATES)"},
		{`\c <name>`, "switch to schema <name>"},
		{`\i <file>`, "execute statements from file"},
		{`\e`, "open $EDITOR — run the buffer on save+exit"},
		{`\x`, "toggle expanded (record-per-block) display"},
		{`\timing`, "toggle the row-count/duration footer"},
		{`\explain`, "re-run the previous query wrapped in EXPLAIN"},
	}
	var b strings.Builder
	b.WriteString(r.st.banner.Render("meta-commands") + "\n")
	for _, h := range help {
		b.WriteString("  ")
		b.WriteString(r.st.cmd.Render(h.cmd))
		b.WriteString("  ")
		b.WriteString(h.desc)
		b.WriteString("\n")
	}
	// Inside BEGIN/COMMIT, SELECT sees the transaction's own
	// uncommitted writes — that's FDB's Read-Your-Writes semantics,
	// not an autocommit leak. ROLLBACK still discards the writes
	// correctly. Worth calling out because the behaviour surprises
	// operators coming from Postgres's default READ COMMITTED.
	b.WriteString("\n")
	b.WriteString(r.st.muted.Render(
		"note: within BEGIN/COMMIT, SELECT returns rows including uncommitted\n" +
			"      writes (FDB Read-Your-Writes). ROLLBACK discards them correctly.\n"))
	fmt.Fprint(r.out, b.String())
}

// readFromEditor opens $EDITOR on a temp file prefilled with a hint
// comment and returns the saved buffer. psql's `\e` equivalent.
func readFromEditor(schema string) (string, error) {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		if runtime.GOOS == "windows" {
			editor = "notepad"
		} else {
			editor = "vi"
		}
	}
	f, err := os.CreateTemp("", "frl-sql-*.sql")
	if err != nil {
		return "", err
	}
	name := f.Name()
	defer os.Remove(name)
	hint := fmt.Sprintf("-- frl sql — edit and save; exit the editor to run.\n-- schema: %s\n\n", schema)
	if _, err := f.WriteString(hint); err != nil {
		f.Close()
		return "", err
	}
	f.Close()

	ec := exec.Command(editor, name)
	ec.Stdin = os.Stdin
	ec.Stdout = os.Stdout
	ec.Stderr = os.Stderr
	if err := ec.Run(); err != nil {
		return "", fmt.Errorf("editor %q: %w", editor, err)
	}
	data, err := os.ReadFile(name)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// --- rendering -----------------------------------------------------------

// renderTable writes an ASCII-table view of rows to out, returning the
// row count. Empty result sets are rendered as just the header so the
// operator can see which columns came back. NULL values render as
// italic "NULL" so they don't blend with empty strings.
func (r *sqlRunner) renderTable(out io.Writer, rows *sql.Rows) (int, error) {
	cols, data, err := collectRows(rows)
	if err != nil {
		return 0, err
	}
	if err := r.renderCollected(out, cols, data); err != nil {
		return 0, err
	}
	return len(data), nil
}

// renderRows is renderTable bound to the runner's writer — the query
// path in execute().
func (r *sqlRunner) renderRows(rows *sql.Rows) (int, error) {
	return r.renderTable(r.out, rows)
}

// collectRows drains a *sql.Rows into memory. Collecting first is what
// lets the table renderer size columns; for very large result sets this
// is memory-heavy but acceptable for a CLI — LIMIT on the query side is
// the right knob for bounding output.
func collectRows(rows *sql.Rows) ([]string, [][]any, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, nil, err
	}
	var data [][]any
	for rows.Next() {
		row := make([]any, len(cols))
		pointers := make([]any, len(cols))
		for i := range row {
			pointers[i] = &row[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, nil, err
		}
		data = append(data, row)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return cols, data, nil
}

// renderCollected dispatches on the runner's output format (RFC-174
// §3.2 sql scriptability). table is the human default; csv/json/ndjson
// are the machine formats (their footers go to stderr, see footer()).
func (r *sqlRunner) renderCollected(out io.Writer, cols []string, data [][]any) error {
	switch r.format {
	case sqlFormatCSV:
		return r.renderCSV(out, cols, data)
	case sqlFormatJSON:
		return r.renderJSON(out, cols, data, false)
	case sqlFormatNDJSON:
		return r.renderJSON(out, cols, data, true)
	default:
		if r.expanded {
			return r.renderExpanded(out, cols, data)
		}
		return r.renderAligned(out, cols, data)
	}
}

// renderAligned is the classic aligned table (the only pre-v2 format).
func (r *sqlRunner) renderAligned(out io.Writer, cols []string, data [][]any) error {
	table := make([][]string, len(data))
	for i, row := range data {
		rendered := make([]string, len(row))
		for j, v := range row {
			rendered[j] = r.renderCell(v)
		}
		table[i] = rendered
	}

	widths := make([]int, len(cols))
	for i, c := range cols {
		widths[i] = lipgloss.Width(c)
	}
	for _, row := range table {
		for i, cell := range row {
			if w := lipgloss.Width(cell); w > widths[i] {
				widths[i] = w
			}
		}
	}

	header := make([]string, len(cols))
	for i, c := range cols {
		header[i] = r.st.header.Render(padCell(c, widths[i]))
	}
	sep := make([]string, len(cols))
	for i := range cols {
		sep[i] = r.st.muted.Render(strings.Repeat(r.st.hLine, widths[i]))
	}
	fmt.Fprintln(out, strings.Join(header, r.st.muted.Render(r.st.vSep)))
	fmt.Fprintln(out, strings.Join(sep, r.st.muted.Render(r.st.cross)))
	for _, row := range table {
		cells := make([]string, len(row))
		for i, c := range row {
			cells[i] = padCell(c, widths[i])
		}
		fmt.Fprintln(out, strings.Join(cells, r.st.muted.Render(r.st.vSep)))
	}
	return nil
}

// renderExpanded is psql's `\x` record-per-block view — one
// "-[ RECORD N ]-" header then col/value pairs, one per line. Wide rows
// stay readable without horizontal scrolling.
func (r *sqlRunner) renderExpanded(out io.Writer, cols []string, data [][]any) error {
	nameWidth := 0
	for _, c := range cols {
		if w := lipgloss.Width(c); w > nameWidth {
			nameWidth = w
		}
	}
	for i, row := range data {
		fmt.Fprintln(out, r.st.muted.Render(fmt.Sprintf("-[ RECORD %d ]-", i+1)))
		for j, c := range cols {
			fmt.Fprintf(out, "%s%s%s\n",
				r.st.header.Render(padCell(c, nameWidth)),
				r.st.muted.Render(r.st.vSep),
				r.renderCell(row[j]))
		}
	}
	return nil
}

// renderCSV emits RFC-4180 CSV with a header row. NULL renders as the
// empty string (psql's CSV convention); []byte as hex.
func (r *sqlRunner) renderCSV(out io.Writer, cols []string, data [][]any) error {
	w := csv.NewWriter(out)
	if err := w.Write(cols); err != nil {
		return err
	}
	for _, row := range data {
		rec := make([]string, len(row))
		for i, v := range row {
			if v == nil {
				rec[i] = ""
				continue
			}
			rec[i] = plainCell(v)
		}
		if err := w.Write(rec); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

// renderJSON emits rows as objects keyed by column name — one indented
// array (json) or one object per line (ndjson, composing with jq and
// the rest of frl's NDJSON envelopes).
func (r *sqlRunner) renderJSON(out io.Writer, cols []string, data [][]any, ndjson bool) error {
	objs := make([]map[string]any, len(data))
	for i, row := range data {
		obj := make(map[string]any, len(cols))
		for j, c := range cols {
			obj[c] = jsonCell(row[j])
		}
		objs[i] = obj
	}
	enc := json.NewEncoder(out)
	if ndjson {
		for _, obj := range objs {
			if err := enc.Encode(obj); err != nil {
				return err
			}
		}
		return nil
	}
	enc.SetIndent("", "  ")
	return enc.Encode(objs)
}

// plainCell renders a value without any styling — CSV cells.
func plainCell(v any) string {
	switch t := v.(type) {
	case []byte:
		return fmt.Sprintf("%x", t)
	case time.Time:
		return t.Format(time.RFC3339Nano)
	case string:
		return t
	default:
		return fmt.Sprintf("%v", t)
	}
}

// jsonCell maps a driver value to its JSON representation: nil → null,
// []byte → hex string, time → RFC3339; scalars pass through.
func jsonCell(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case []byte:
		return fmt.Sprintf("%x", t)
	case time.Time:
		return t.Format(time.RFC3339Nano)
	default:
		return v
	}
}

// renderStaticTable writes a table given precomputed header + rows.
// The sibling of renderTable for places where we have the data in a
// slice already (catalog queries, \d output) rather than behind a
// *sql.Rows cursor.
func (r *sqlRunner) renderStaticTable(out io.Writer, headers []string, rows [][]string) error {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = lipgloss.Width(h)
	}
	for _, row := range rows {
		for i, c := range row {
			if i >= len(widths) {
				continue
			}
			if w := lipgloss.Width(c); w > widths[i] {
				widths[i] = w
			}
		}
	}
	header := make([]string, len(headers))
	for i, h := range headers {
		header[i] = r.st.header.Render(padCell(h, widths[i]))
	}
	sep := make([]string, len(headers))
	for i := range headers {
		sep[i] = r.st.muted.Render(strings.Repeat(r.st.hLine, widths[i]))
	}
	fmt.Fprintln(out, strings.Join(header, r.st.muted.Render(r.st.vSep)))
	fmt.Fprintln(out, strings.Join(sep, r.st.muted.Render(r.st.cross)))
	for _, row := range rows {
		cells := make([]string, len(row))
		for i, c := range row {
			cells[i] = padCell(c, widths[i])
		}
		fmt.Fprintln(out, strings.Join(cells, r.st.muted.Render(r.st.vSep)))
	}
	return nil
}

// loadSchemaTables loads the schema at (dbURI, schemaName) from the
// relational catalog and returns its template's tables + PK columns.
// Runs in its own read-only FDB tx — short enough to not contend
// with a long-running REPL statement.
func (r *sqlRunner) loadSchemaTables(dbURI, schemaName string) ([]tableInfo, error) {
	tables, err := runCatalogQuery(r.ctx, r.clusterFile,
		func(ctx context.Context, cat *catalog.RecordLayerStoreCatalog, txn relapi.Transaction) ([]tableInfo, error) {
			sch, err := cat.LoadSchema(txn, dbURI, schemaName)
			if err != nil {
				return nil, err
			}
			tbls, err := sch.Tables()
			if err != nil {
				return nil, err
			}
			out := make([]tableInfo, 0, len(tbls))
			for _, tbl := range tbls {
				out = append(out, tableInfo{
					name:    tbl.MetadataName(),
					columns: len(tbl.Columns()),
				})
			}
			sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
			return out, nil
		})
	return tables, err
}

// columnInfo is one row of `\d <table>` — column name, declared type,
// and nullability, in declared order.
type columnInfo struct {
	name     string
	dataType string
	nullable bool
}

// describeTable powers `\d <table>`: columns (name, type, nullability)
// in declared order plus the primary-key fields from the template's
// underlying RecordMetaData (api.Table doesn't model the PK).
func (r *sqlRunner) describeTable(name string) error {
	if r.database == "" || r.schema == "" {
		return fmt.Errorf("no active schema — use --database and --schema (or \\c <schema>)")
	}
	type described struct {
		cols []columnInfo
		pk   []string
	}
	d, err := runCatalogQuery(r.ctx, r.clusterFile,
		func(ctx context.Context, cat *catalog.RecordLayerStoreCatalog, txn relapi.Transaction) (described, error) {
			var out described
			sch, err := cat.LoadSchema(txn, r.database, r.schema)
			if err != nil {
				return out, err
			}
			tbls, err := sch.Tables()
			if err != nil {
				return out, err
			}
			var names []string
			for _, tbl := range tbls {
				names = append(names, tbl.MetadataName())
				if !strings.EqualFold(tbl.MetadataName(), name) {
					continue
				}
				for _, col := range tbl.Columns() {
					out.cols = append(out.cols, columnInfo{
						name:     col.MetadataName(),
						dataType: col.DataType().Code().String(),
						nullable: col.DataType().IsNullable(),
					})
				}
				// The PK lives on the record-layer type behind the table.
				if up, ok := sch.SchemaTemplate().(interface {
					Underlying() *recordlayer.RecordMetaData
				}); ok {
					if rt := up.Underlying().GetRecordType(tbl.MetadataName()); rt != nil && rt.PrimaryKey != nil {
						out.pk = rt.PrimaryKey.FieldNames()
					}
				}
				return out, nil
			}
			sort.Strings(names)
			return out, fmt.Errorf("table %q not found in %s/%s — available: %s",
				name, r.database, r.schema, strings.Join(names, ", "))
		})
	if err != nil {
		return err
	}
	headers := []string{"COLUMN", "TYPE", "NULLABLE"}
	rows := make([][]string, 0, len(d.cols))
	for _, c := range d.cols {
		rows = append(rows, []string{c.name, c.dataType, fmt.Sprintf("%t", c.nullable)})
	}
	if err := r.renderStaticTable(r.out, headers, rows); err != nil {
		return err
	}
	if len(d.pk) > 0 {
		_, err = fmt.Fprintln(r.out, r.st.muted.Render("primary key: "+strings.Join(d.pk, ", ")))
	}
	return err
}

// renderCell turns a database/sql value into its display string. NULL
// (Go nil) gets a muted "NULL" sentinel so it's distinguishable from
// the empty string. []byte renders as hex — the same convention
// formatPK already uses for binary PKs.
func (r *sqlRunner) renderCell(v any) string {
	if v == nil {
		return r.st.muted.Render("NULL")
	}
	switch t := v.(type) {
	case []byte:
		return fmt.Sprintf("%x", t)
	case time.Time:
		return t.Format(time.RFC3339Nano)
	case string:
		return t
	default:
		return fmt.Sprintf("%v", t)
	}
}

// padCell space-pads s on the right to w display columns. Uses
// lipgloss.Width so multi-byte runes and ANSI escapes (from
// renderCell's muted NULL) count correctly.
func padCell(s string, w int) string {
	pad := w - lipgloss.Width(s)
	if pad <= 0 {
		return s
	}
	return s + strings.Repeat(" ", pad)
}

// --- styling -------------------------------------------------------------

// sqlStyles carries every visual element the sql command emits — lipgloss
// styles plus the table border glyphs. Two profiles exist: ttySQLStyles
// (colors + Unicode box-drawing) when stdout is a real terminal, and
// plainSQLStyles (no ANSI, pure ASCII) when stdout is piped/redirected.
// Regression context: styles used to be unconditional package vars, so
// `frl sql -c … | jq` received raw ESC sequences and `─┼─` box-drawing —
// unusable in scripts. Off-TTY output must contain no byte ≥ 0x80 and no
// `\x1b` (pinned by TestRenderStaticTable_PlainIsASCII).
type sqlStyles struct {
	banner      lipgloss.Style
	muted       lipgloss.Style
	header      lipgloss.Style
	errS        lipgloss.Style
	cmd         lipgloss.Style
	prompt      lipgloss.Style
	promptMuted lipgloss.Style
	cont        lipgloss.Style
	schema      lipgloss.Style
	// txMarker is the `*` that appears before `>` while a transaction is
	// open — yellow so it's noticeable without being alarming (errors
	// use red).
	txMarker lipgloss.Style

	vSep  string // column separator in table rows, e.g. " │ " / " | "
	hLine string // horizontal rule rune under the header, e.g. "─" / "-"
	cross string // separator-row junction, e.g. "─┼─" / "-+-"
}

// ttySQLStyles is the interactive profile. Purposefully muted palette —
// this is a REPL, not a presentation.
func ttySQLStyles() sqlStyles {
	return sqlStyles{
		banner:      lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Bold(true),
		muted:       lipgloss.NewStyle().Foreground(lipgloss.Color("8")),
		header:      lipgloss.NewStyle().Foreground(lipgloss.Color("14")).Bold(true),
		errS:        lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true),
		cmd:         lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Bold(true),
		prompt:      lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Bold(true),
		promptMuted: lipgloss.NewStyle().Foreground(lipgloss.Color("8")),
		cont:        lipgloss.NewStyle().Foreground(lipgloss.Color("8")),
		schema:      lipgloss.NewStyle().Foreground(lipgloss.Color("13")),
		txMarker:    lipgloss.NewStyle().Foreground(lipgloss.Color("11")).Bold(true),
		vSep:        " │ ",
		hLine:       "─",
		cross:       "─┼─",
	}
}

// plainSQLStyles is the piped/scripted profile: zero-value lipgloss
// styles render text unmodified (no ANSI), and the borders are ASCII so
// awk/cut/grep pipelines see clean 7-bit output.
func plainSQLStyles() sqlStyles {
	return sqlStyles{
		vSep:  " | ",
		hLine: "-",
		cross: "-+-",
	}
}

// isTerminalWriter reports whether w is an interactive terminal. A
// bytes.Buffer (tests), a pipe (`| jq`), or a redirect (`> f`) all
// return false and get the plain profile.
func isTerminalWriter(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(f.Fd())
}

// prompt builds the primary prompt. If a schema is active it's
// included in a muted color so operators can see which namespace they
// are in at a glance. When a transaction is open (r.inTx), a `*`
// is injected before `>` — psql uses the same signal.
func (r *sqlRunner) prompt() string {
	marker := r.st.prompt.Render("> ")
	if r.inTx {
		marker = r.st.txMarker.Render("*") + r.st.prompt.Render("> ")
	}
	if r.schema != "" {
		return r.st.prompt.Render("frl") + r.st.promptMuted.Render("(") +
			r.st.schema.Render(r.schema) + r.st.promptMuted.Render(")") +
			marker
	}
	return r.st.prompt.Render("frl") + marker
}

// sqlContinuePrompt is the indented continuation prompt shown while a
// statement is still accumulating input. Matches psql's `-> ` arrow.
func (r *sqlRunner) continuePrompt() string {
	return r.st.cont.Render("  -> ")
}

// --- helpers -------------------------------------------------------------

// historyPath returns the file where readline persists command history.
// Under $XDG_DATA_HOME or ~/.frl/sql_history; best-effort — failure
// falls through to an in-memory history (readline handles the empty
// path gracefully).
func historyPath() string {
	if home, err := os.UserHomeDir(); err == nil {
		dir := filepath.Join(home, ".frl")
		_ = os.MkdirAll(dir, 0o700)
		return filepath.Join(dir, "sql_history")
	}
	return ""
}

// plural appends "s" when n != 1 — saves an if at every callsite.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// --- sanity on startup ---------------------------------------------------

// sortedStringCopy returns a fresh sorted copy of a string slice. Used
// for deterministic ordering in error suggestion lists. (Declared here
// rather than next to other helpers so tests can assert stable output.)
func sortedStringCopy(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	sort.Strings(out)
	return out
}

// Assigning to _ so unused-linter doesn't complain about sortedStringCopy
// on builds that don't yet reference it. (Reserved for future use by
// meta-command handlers that need to list schemas.)
var _ = sortedStringCopy
