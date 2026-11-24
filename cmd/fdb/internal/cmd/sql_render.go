// Package cmd (sql_render.go) — result-set table/CSV/JSON rendering and
// terminal styling profiles for `fdb sql`.
package cmd

import (
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
)

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
// the rest of fdb's NDJSON envelopes).
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
// `fdb sql -c … | jq` received raw ESC sequences and `─┼─` box-drawing —
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
	return isTerminalReader(w)
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
		return r.st.prompt.Render("fdb") + r.st.promptMuted.Render("(") +
			r.st.schema.Render(r.schema) + r.st.promptMuted.Render(")") +
			marker
	}
	return r.st.prompt.Render("fdb") + marker
}

// sqlContinuePrompt is the indented continuation prompt shown while a
// statement is still accumulating input. Matches psql's `-> ` arrow.
func (r *sqlRunner) continuePrompt() string {
	return r.st.cont.Render("  -> ")
}

// plural appends "s" when n != 1 — saves an if at every callsite.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
