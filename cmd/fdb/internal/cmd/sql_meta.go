// Package cmd (sql_meta.go) — interactive REPL loop, statement splitting, and
// psql-style backslash meta-commands for `fdb sql`.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/chzyer/readline"

	"fdb.dev/internal/relational/catalog"
	relapi "fdb.dev/internal/relational/relapi"
	"fdb.dev/pkg/recordlayer"
)

// tableInfo is the minimal shape rendered by \d — table name + column
// count. Sourced from the current schema's template via the relational
// catalog. PK columns aren't on api.Table; `meta catalog get <tpl>`
// is the right place for the full proto breakdown.
type tableInfo struct {
	name    string
	columns int
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

	banner := r.st.banner.Render("fdb sql") + " — " +
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

// historyPath returns the file where readline persists command history.
// Stored at ~/.fdb/sql_history; best-effort — failure
// falls through to an in-memory history (readline handles the empty
// path gracefully).
func historyPath() string {
	if home, err := os.UserHomeDir(); err == nil {
		dir := filepath.Join(home, ".fdb")
		_ = os.MkdirAll(dir, 0o700)
		return filepath.Join(dir, "sql_history")
	}
	return ""
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
		// never interpretation (RFC-174: plan-shape analysis is a
		// query-engine feature, not a CLI one).
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
				"cannot switch schema inside a transaction — COMMIT or ROLLBACK first",
			))
			return false
		}
		if err := r.switchSchema(fields[1]); err != nil {
			fmt.Fprintln(r.errOut, r.st.errS.Render("ERROR: ")+err.Error())
			return false
		}
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
			fmt.Sprintf("unknown meta-command %q — try \\?", fields[0]),
		))
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
			fmt.Sprintf("(no tables in %s/%s)", r.database, r.schema),
		))
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
	if err := r.ensureDB(); err != nil {
		return err
	}
	rows, err := r.db.QueryContext(r.ctx, "SHOW SCHEMA TEMPLATES")
	if err != nil {
		return fmt.Errorf("%w (also available: `fdb rl meta catalog templates`)", err)
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
			"      writes (FDB Read-Your-Writes). ROLLBACK discards them correctly.\n",
	))
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
	f, err := os.CreateTemp("", "fdb-sql-*.sql")
	if err != nil {
		return "", err
	}
	name := f.Name()
	defer os.Remove(name)
	hint := fmt.Sprintf("-- fdb sql — edit and save; exit the editor to run.\n-- schema: %s\n\n", schema)
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
			md := metaFromSchema(sch)
			out := make([]tableInfo, 0, len(tbls))
			for _, tbl := range tbls {
				out = append(out, tableInfo{
					// SQL identifier. MetadataName is the record type's stored name,
					// which is escaped; the sort below then orders by what is printed.
					name:    userNameFor(md, tbl.MetadataName()),
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
			md := metaFromSchema(sch)
			var names []string
			for _, tbl := range tbls {
				names = append(names, userNameFor(md, tbl.MetadataName()))
				// Accept EITHER spelling. The list above offers SQL identifiers, so a
				// name copied out of it must resolve -- matching only the stored form
				// would reject the very name this command just printed.
				if !strings.EqualFold(tbl.MetadataName(), name) &&
					!strings.EqualFold(userNameFor(md, tbl.MetadataName()), name) {
					continue
				}
				for _, col := range tbl.Columns() {
					out.cols = append(out.cols, columnInfo{
						// SQL identifier: a column name is a proto FIELD name and is
						// escaped the same way a table name is.
						name:     userFieldName(col.MetadataName()),
						dataType: col.DataType().Code().String(),
						nullable: col.DataType().IsNullable(),
					})
				}
				// The PK lives on the record-layer type behind the table.
				if up, ok := sch.SchemaTemplate().(interface {
					Underlying() *recordlayer.RecordMetaData
				}); ok {
					if rt := up.Underlying().GetRecordType(tbl.MetadataName()); rt != nil && rt.PrimaryKey != nil {
						// SQL identifiers, like the column rows above -- otherwise one
						// description prints two spellings of the same column.
						out.pk = userFieldNames(rt.PrimaryKey.FieldNames())
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

// metaFromSchema digs the record-layer metadata out of a relational schema, or
// nil when the schema is not backed by one.
//
// It exists so \d can apply the same ambiguity gate the rest of the CLI does:
// under a colliding pair the decoded spelling of one type is the STORED key of
// another, and \d's either-spelling accept then describes whichever the catalog
// yields first. Read-only, so less dangerous than the completer that feeds
// `record delete` — the same defect all the same.
func metaFromSchema(sch relapi.Schema) *recordlayer.RecordMetaData {
	if sch == nil {
		return nil
	}
	up, ok := sch.SchemaTemplate().(interface {
		Underlying() *recordlayer.RecordMetaData
	})
	if !ok {
		return nil
	}
	return up.Underlying()
}
