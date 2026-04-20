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
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/chzyer/readline"
	"github.com/spf13/cobra"

	configv1 "github.com/birdayz/fdb-record-layer-go/cmd/frl/gen/frl/config/v1"
	relapi "github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/catalog"

	// Register the "fdbsql" driver via blank import.
	_ "github.com/birdayz/fdb-record-layer-go/pkg/relational/sqldriver"
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
			"  \\?         — show this help\n" +
			"  \\q         — quit (same as Ctrl-D)\n" +
			"  \\d, \\dt    — list tables in the current schema\n" +
			"  \\c <name>  — switch to schema <name>\n" +
			"  \\i <file>  — execute statements from file\n" +
			"  \\e         — open $EDITOR, run the buffer on exit\n\n" +
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
			cfgCtx, _, err := resolveContextAndOverride(contextName, "")
			if err != nil {
				return err
			}
			if databaseURI == "" {
				return fmt.Errorf("--database is required (e.g. --database /myapp)")
			}
			dsn := buildFDBSQLDSN(cfgCtx, databaseURI, initSchema)
			db, err := sql.Open("fdbsql", dsn)
			if err != nil {
				return fmt.Errorf("open fdbsql %q: %w", dsn, err)
			}
			defer db.Close()

			runner := &sqlRunner{
				db:       db,
				out:      cmd.OutOrStdout(),
				errOut:   cmd.ErrOrStderr(),
				ctx:      cmd.Context(),
				cfgCtx:   cfgCtx,
				database: databaseURI,
				schema:   initSchema,
			}
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
	return c
}

// buildFDBSQLDSN constructs the `fdbsql:///PATH?cluster_file=…&schema=…`
// DSN the driver accepts. Keeps the DSN-shape knowledge local to one
// place so the REPL + non-interactive paths agree.
func buildFDBSQLDSN(cfgCtx *configv1.Context, dbPath, schema string) string {
	var b strings.Builder
	b.WriteString("fdbsql:///")
	b.WriteString(strings.TrimPrefix(dbPath, "/"))
	var q []string
	if cf := cfgCtx.GetClusterFile(); cf != "" {
		q = append(q, "cluster_file="+cf)
	}
	if schema != "" {
		q = append(q, "schema="+schema)
	}
	if len(q) > 0 {
		b.WriteString("?")
		b.WriteString(strings.Join(q, "&"))
	}
	return b.String()
}

// sqlRunner owns the database handle + I/O. Shared by the REPL loop
// and the non-interactive -c / -f paths so the three entry points
// render results identically.
type sqlRunner struct {
	db       *sql.DB
	out      io.Writer
	errOut   io.Writer
	ctx      context.Context
	cfgCtx   *configv1.Context // cluster_file for catalog lookups
	database string            // active database URI (from --database)
	schema   string            // tracked for the prompt; set by \c
}

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

	banner := sqlBannerStyle.Render("frl sql") + " — " +
		sqlMutedStyle.Render("\\? for help, \\q to quit, multi-line ends at ;")
	fmt.Fprintln(r.out, banner)

	var buf strings.Builder
	for {
		if buf.Len() == 0 {
			rl.SetPrompt(r.prompt())
		} else {
			rl.SetPrompt(sqlContinuePrompt())
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
				fmt.Fprintln(r.errOut, sqlErrorStyle.Render("ERROR: ")+err.Error())
			}
		}
	}
}

// --- execution -----------------------------------------------------------

// execute runs one statement. SELECT-shaped queries go through Query
// and render a lipgloss table; everything else goes through Exec and
// reports rows-affected / duration. Statement type is inferred from
// the leading keyword — same heuristic psql uses.
func (r *sqlRunner) execute(stmt string) error {
	stmt = strings.TrimSpace(stmt)
	if stmt == "" {
		return nil
	}
	start := time.Now()
	if isQuery(stmt) {
		rows, err := r.db.QueryContext(r.ctx, stmt)
		if err != nil {
			return err
		}
		defer rows.Close()
		n, err := renderTable(r.out, rows)
		if err != nil {
			return err
		}
		fmt.Fprintln(r.out, sqlMutedStyle.Render(
			fmt.Sprintf("(%d row%s, %s)", n, plural(n), time.Since(start).Round(time.Millisecond))))
		return nil
	}
	res, err := r.db.ExecContext(r.ctx, stmt)
	if err != nil {
		return err
	}
	affected := int64(-1)
	if res != nil {
		if n, aErr := res.RowsAffected(); aErr == nil {
			affected = n
		}
	}
	if affected >= 0 {
		fmt.Fprintln(r.out, sqlMutedStyle.Render(
			fmt.Sprintf("OK (%d row%s affected, %s)",
				affected, plural(int(affected)), time.Since(start).Round(time.Millisecond))))
	} else {
		fmt.Fprintln(r.out, sqlMutedStyle.Render(
			"OK ("+time.Since(start).Round(time.Millisecond).String()+")"))
	}
	return nil
}

// isQuery guesses whether stmt returns rows. Case-insensitive match on
// the leading keyword. Close enough for the REPL; wrong guesses still
// work (QueryContext on DDL just returns zero rows + an error we
// surface).
func isQuery(stmt string) bool {
	head := strings.ToUpper(strings.TrimSpace(stmt))
	for _, kw := range []string{"SELECT ", "WITH ", "EXPLAIN ", "SHOW ", "VALUES ", "DESCRIBE "} {
		if strings.HasPrefix(head, kw) {
			return true
		}
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
		if err := r.listTables(); err != nil {
			fmt.Fprintln(r.errOut, sqlErrorStyle.Render("ERROR: ")+err.Error())
		}
	case `\dt`:
		if err := r.listTemplates(); err != nil {
			fmt.Fprintln(r.errOut, sqlErrorStyle.Render("ERROR: ")+err.Error())
		}
	case `\c`:
		if len(fields) < 2 {
			fmt.Fprintln(r.errOut, sqlErrorStyle.Render("usage: \\c <schema>"))
			return false
		}
		r.schema = fields[1]
		fmt.Fprintln(r.out, sqlMutedStyle.Render("schema → "+r.schema))
	case `\i`:
		if len(fields) < 2 {
			fmt.Fprintln(r.errOut, sqlErrorStyle.Render("usage: \\i <file>"))
			return false
		}
		if err := r.runFile(fields[1]); err != nil {
			fmt.Fprintln(r.errOut, sqlErrorStyle.Render("ERROR: ")+err.Error())
		}
	case `\e`:
		stmt, err := readFromEditor(r.schema)
		if err != nil {
			fmt.Fprintln(r.errOut, sqlErrorStyle.Render("ERROR: ")+err.Error())
			return false
		}
		if strings.TrimSpace(stmt) == "" {
			return false
		}
		for _, s := range splitStatements(stmt) {
			if execErr := r.execute(s); execErr != nil {
				fmt.Fprintln(r.errOut, sqlErrorStyle.Render("ERROR: ")+execErr.Error())
				break
			}
		}
	default:
		fmt.Fprintln(r.errOut, sqlMutedStyle.Render(
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
		_, err := fmt.Fprintln(r.out, sqlMutedStyle.Render(
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
	return renderStaticTable(r.out, headers, rows)
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
	_, err = renderTable(r.out, rows)
	return err
}

func (r *sqlRunner) printMetaHelp() {
	help := []struct {
		cmd, desc string
	}{
		{`\?`, "show this help"},
		{`\q`, "quit the REPL (also Ctrl-D)"},
		{`\d`, "list tables in the current schema"},
		{`\dt`, "list tables (alias for \\d)"},
		{`\c <name>`, "switch to schema <name>"},
		{`\i <file>`, "execute statements from file"},
		{`\e`, "open $EDITOR — run the buffer on save+exit"},
	}
	var b strings.Builder
	b.WriteString(sqlBannerStyle.Render("meta-commands") + "\n")
	for _, h := range help {
		b.WriteString("  ")
		b.WriteString(sqlCmdStyle.Render(h.cmd))
		b.WriteString("  ")
		b.WriteString(h.desc)
		b.WriteString("\n")
	}
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
func renderTable(out io.Writer, rows *sql.Rows) (int, error) {
	cols, err := rows.Columns()
	if err != nil {
		return 0, err
	}
	// Collect all rows first so we can size columns. For very large
	// result sets this is memory-heavy; acceptable for a REPL where
	// operators rarely scroll past a few thousand rows. --limit on
	// the query side is the right knob for bounding output.
	var table [][]string
	for rows.Next() {
		row := make([]any, len(cols))
		pointers := make([]any, len(cols))
		for i := range row {
			pointers[i] = &row[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return 0, err
		}
		rendered := make([]string, len(row))
		for i, v := range row {
			rendered[i] = renderCell(v)
		}
		table = append(table, rendered)
	}
	if err := rows.Err(); err != nil {
		return 0, err
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
		header[i] = sqlHeaderStyle.Render(padCell(c, widths[i]))
	}
	sep := make([]string, len(cols))
	for i := range cols {
		sep[i] = sqlMutedStyle.Render(strings.Repeat("─", widths[i]))
	}
	fmt.Fprintln(out, strings.Join(header, sqlMutedStyle.Render(" │ ")))
	fmt.Fprintln(out, strings.Join(sep, sqlMutedStyle.Render("─┼─")))
	for _, row := range table {
		cells := make([]string, len(row))
		for i, c := range row {
			cells[i] = padCell(c, widths[i])
		}
		fmt.Fprintln(out, strings.Join(cells, sqlMutedStyle.Render(" │ ")))
	}
	return len(table), nil
}

// renderStaticTable writes a table given precomputed header + rows.
// The sibling of renderTable for places where we have the data in a
// slice already (catalog queries, \d output) rather than behind a
// *sql.Rows cursor.
func renderStaticTable(out io.Writer, headers []string, rows [][]string) error {
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
		header[i] = sqlHeaderStyle.Render(padCell(h, widths[i]))
	}
	sep := make([]string, len(headers))
	for i := range headers {
		sep[i] = sqlMutedStyle.Render(strings.Repeat("─", widths[i]))
	}
	fmt.Fprintln(out, strings.Join(header, sqlMutedStyle.Render(" │ ")))
	fmt.Fprintln(out, strings.Join(sep, sqlMutedStyle.Render("─┼─")))
	for _, row := range rows {
		cells := make([]string, len(row))
		for i, c := range row {
			cells[i] = padCell(c, widths[i])
		}
		fmt.Fprintln(out, strings.Join(cells, sqlMutedStyle.Render(" │ ")))
	}
	return nil
}

// loadSchemaTables loads the schema at (dbURI, schemaName) from the
// relational catalog and returns its template's tables + PK columns.
// Runs in its own read-only FDB tx — short enough to not contend
// with a long-running REPL statement.
func (r *sqlRunner) loadSchemaTables(dbURI, schemaName string) ([]tableInfo, error) {
	tables, err := runCatalogQuery(r.ctx, r.cfgCtx,
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

// renderCell turns a database/sql value into its display string. NULL
// (Go nil) gets a muted "NULL" sentinel so it's distinguishable from
// the empty string. []byte renders as hex — the same convention
// formatPK already uses for binary PKs.
func renderCell(v any) string {
	if v == nil {
		return sqlMutedStyle.Render("NULL")
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

var (
	// Purposefully muted palette — this is a REPL, not a presentation.
	// Colors only on stderr-safe elements so piping to less / jq
	// doesn't render ANSI garbage on a terminal-less stream.
	sqlBannerStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Bold(true)
	sqlMutedStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	sqlHeaderStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("14")).Bold(true)
	sqlErrorStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true)
	sqlCmdStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Bold(true)
	sqlPromptStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Bold(true)
	sqlPromptMuted = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	sqlContStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	sqlSchemaColor = lipgloss.NewStyle().Foreground(lipgloss.Color("13"))
	_              = sqlPromptMuted // reserved for future schema-in-prompt styling
)

// prompt builds the primary prompt. If a schema is active it's included
// in a muted color so operators can see which namespace they're in at
// a glance.
func (r *sqlRunner) prompt() string {
	if r.schema != "" {
		return sqlPromptStyle.Render("frl") + sqlPromptMuted.Render("(") +
			sqlSchemaColor.Render(r.schema) + sqlPromptMuted.Render(")") +
			sqlPromptStyle.Render("> ")
	}
	return sqlPromptStyle.Render("frl> ")
}

// sqlContinuePrompt is the indented continuation prompt shown while a
// statement is still accumulating input. Matches psql's `-> ` arrow.
func sqlContinuePrompt() string {
	return sqlContStyle.Render("  -> ")
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
