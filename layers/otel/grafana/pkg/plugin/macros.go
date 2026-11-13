package plugin

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend/gtime"
	"github.com/grafana/grafana-plugin-sdk-go/data/sqlutil"
)

// MacroError is a macro call with malformed or unsafe arguments.
type MacroError struct {
	Macro  string
	Reason string
}

func (e *MacroError) Error() string { return fmt.Sprintf("macro $__%s: %s", e.Macro, e.Reason) }

func macroErrorf(macro, format string, args ...any) error {
	return &MacroError{Macro: macro, Reason: fmt.Sprintf(format, args...)}
}

// macros are the SQL macros (RFC §6). They work in integer time units
// (OTEL_V1 stores ns) and splice only validated identifiers and integers.
// table and column override sqlutil's defaults, which splice free text.
var macros = sqlutil.Macros{
	"timeFilter":          macroTimeFilter,
	"timeFrom":            macroTimeBound(true),
	"timeTo":              macroTimeBound(false),
	"timeGroup":           macroTimeGroup(false),
	"timeGroupAlias":      macroTimeGroup(true),
	"time":                macroTime,
	"unixEpochNanoFilter": macroNanoFilter,
	"unixEpochNanoFrom":   macroNanoBound(true),
	"unixEpochNanoTo":     macroNanoBound(false),
	"hex":                 macroHex,
	"table":               macroRefused("table"),
	"column":              macroRefused("column"),
}

// Time units: nanoseconds per unit.
var unitNanos = map[string]int64{"ns": 1, "us": 1_000, "ms": 1_000_000, "s": 1_000_000_000}

// identifierPart is an unquoted identifier or a double-quoted one without
// quotes or control characters inside.
var identifierPart = regexp.MustCompile(`^(?:[A-Za-z_][A-Za-z0-9_]*|"[^"\x00-\x1f]+")$`)

// checkIdentifier admits a (dotted) column reference and nothing else.
func checkIdentifier(macro, s string) error {
	if s == "" {
		return macroErrorf(macro, "missing column")
	}
	for _, part := range splitDotted(s) {
		if !identifierPart.MatchString(part) {
			return macroErrorf(macro, "%q is not a column name", s)
		}
	}
	return nil
}

// splitDotted splits on dots outside double quotes.
func splitDotted(s string) []string {
	var parts []string
	inQuote := false
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			inQuote = !inQuote
		case '.':
			if !inQuote {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, s[start:])
}

func parseUnit(macro, s string) (int64, error) {
	if s == "" {
		return 1, nil
	}
	n, ok := unitNanos[s]
	if !ok {
		return 0, macroErrorf(macro, "unit %q is not ns, us, ms or s", s)
	}
	return n, nil
}

// argsOf normalizes sqlutil's argument list: no parentheses and "()" both
// mean no arguments.
func argsOf(args []string) []string {
	if len(args) == 1 && args[0] == "" {
		return nil
	}
	return args
}

// floorDiv and ceilDiv convert a ns instant to a unit, rounding the range
// outward so a unit that overlaps the range is included.
func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && a < 0 {
		q--
	}
	return q
}

func ceilDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && a > 0 {
		q++
	}
	return q
}

func rangeIn(q *sqlutil.Query, unit int64) (from, to int64) {
	return floorDiv(q.TimeRange.From.UnixNano(), unit), ceilDiv(q.TimeRange.To.UnixNano(), unit)
}

// $__timeFilter(col[, unit]) → col >= from AND col < to
func macroTimeFilter(q *sqlutil.Query, args []string) (string, error) {
	args = argsOf(args)
	if len(args) < 1 || len(args) > 2 {
		return "", macroErrorf("timeFilter", "expected (column[, unit]), got %d arguments", len(args))
	}
	if err := checkIdentifier("timeFilter", args[0]); err != nil {
		return "", err
	}
	unit, err := parseUnit("timeFilter", argAt(args, 1))
	if err != nil {
		return "", err
	}
	from, to := rangeIn(q, unit)
	return fmt.Sprintf("%s >= %d AND %s < %d", args[0], from, args[0], to), nil
}

// $__timeFrom([unit]) and $__timeTo([unit]) → integer literal
func macroTimeBound(from bool) sqlutil.MacroFunc {
	name := "timeTo"
	if from {
		name = "timeFrom"
	}
	return func(q *sqlutil.Query, args []string) (string, error) {
		args = argsOf(args)
		if len(args) > 1 {
			return "", macroErrorf(name, "expected ([unit]), got %d arguments", len(args))
		}
		unit, err := parseUnit(name, argAt(args, 0))
		if err != nil {
			return "", err
		}
		f, t := rangeIn(q, unit)
		if from {
			return strconv.FormatInt(f, 10), nil
		}
		return strconv.FormatInt(t, 10), nil
	}
}

// $__timeGroup(col, interval[, unit]) → (col / step) * step
// $__timeGroupAlias(...) → the bucket in ns AS "time"
func macroTimeGroup(alias bool) sqlutil.MacroFunc {
	name := "timeGroup"
	if alias {
		name = "timeGroupAlias"
	}
	return func(q *sqlutil.Query, args []string) (string, error) {
		args = argsOf(args)
		if len(args) < 2 || len(args) > 3 {
			return "", macroErrorf(name, "expected (column, interval[, unit]), got %d arguments", len(args))
		}
		if err := checkIdentifier(name, args[0]); err != nil {
			return "", err
		}
		unit, err := parseUnit(name, argAt(args, 2))
		if err != nil {
			return "", err
		}
		interval, err := parseInterval(name, args[1], q.Interval)
		if err != nil {
			return "", err
		}
		step := interval.Nanoseconds() / unit
		if step < 1 || interval.Nanoseconds()%unit != 0 {
			return "", macroErrorf(name, "interval %s is not a whole number of the column's unit", interval)
		}
		expr := fmt.Sprintf("(%s / %d) * %d", args[0], step, step)
		if !alias {
			return expr, nil
		}
		return inNanos(expr, unit) + ` AS "time"`, nil
	}
}

// $__time(col[, unit]) → col in ns AS "time"
func macroTime(_ *sqlutil.Query, args []string) (string, error) {
	args = argsOf(args)
	if len(args) < 1 || len(args) > 2 {
		return "", macroErrorf("time", "expected (column[, unit]), got %d arguments", len(args))
	}
	if err := checkIdentifier("time", args[0]); err != nil {
		return "", err
	}
	unit, err := parseUnit("time", argAt(args, 1))
	if err != nil {
		return "", err
	}
	return inNanos(args[0], unit) + ` AS "time"`, nil
}

func inNanos(expr string, unit int64) string {
	if unit == 1 {
		return expr
	}
	return fmt.Sprintf("(%s) * %d", expr, unit)
}

// $__unixEpochNanoFilter(col) is $__timeFilter(col).
func macroNanoFilter(q *sqlutil.Query, args []string) (string, error) {
	args = argsOf(args)
	if len(args) != 1 {
		return "", macroErrorf("unixEpochNanoFilter", "expected (column), got %d arguments", len(args))
	}
	if err := checkIdentifier("unixEpochNanoFilter", args[0]); err != nil {
		return "", err
	}
	return macroTimeFilter(q, args)
}

// $__unixEpochNanoFrom() and $__unixEpochNanoTo() are $__timeFrom() and $__timeTo().
func macroNanoBound(from bool) sqlutil.MacroFunc {
	name, inner := "unixEpochNanoTo", macroTimeBound(false)
	if from {
		name, inner = "unixEpochNanoFrom", macroTimeBound(true)
	}
	return func(q *sqlutil.Query, args []string) (string, error) {
		if len(argsOf(args)) != 0 {
			return "", macroErrorf(name, "takes no arguments")
		}
		return inner(q, nil)
	}
}

// $__hex(v) → X'v' for even-length hex v.
func macroHex(_ *sqlutil.Query, args []string) (string, error) {
	args = argsOf(args)
	if len(args) != 1 {
		return "", macroErrorf("hex", "expected (hex digits), got %d arguments", len(args))
	}
	v := strings.ToLower(args[0])
	if v == "" || len(v)%2 != 0 || !isHex(v) {
		return "", macroErrorf("hex", "%q is not an even-length hex string", args[0])
	}
	return "X'" + v + "'", nil
}

func macroRefused(name string) sqlutil.MacroFunc {
	return func(*sqlutil.Query, []string) (string, error) {
		return "", macroErrorf(name, "not supported: it would splice free text into the SQL")
	}
}

// parseInterval reads a Grafana interval ("5m", '1h', $__interval).
func parseInterval(macro, s string, queryInterval time.Duration) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		s = s[1 : len(s)-1]
	}
	switch s {
	case "$__interval", "${__interval}":
		if queryInterval <= 0 {
			return 0, macroErrorf(macro, "the query has no interval")
		}
		return queryInterval, nil
	}
	// ParseDuration, not ParseInterval: a bucket width must not depend on the clock.
	d, err := gtime.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, macroErrorf(macro, "%q is not an interval like 30s, 5m or 1h", s)
	}
	return d, nil
}

func argAt(args []string, i int) string {
	if i < len(args) {
		return args[i]
	}
	return ""
}
