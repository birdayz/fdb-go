package plugin

import (
	"errors"
	"testing"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/data/sqlutil"
)

// 2024-01-02T03:04:05.006007008Z .. +1h30m, sub-second ends to exercise rounding.
var (
	macroFrom = time.Date(2024, 1, 2, 3, 4, 5, 6_007_008, time.UTC)
	macroTo   = macroFrom.Add(90*time.Minute + 500*time.Millisecond)
)

func interpolate(t *testing.T, sql string) (string, error) {
	t.Helper()
	q := &sqlutil.Query{RawSQL: sql, TimeRange: backend.TimeRange{From: macroFrom, To: macroTo}, Interval: 30 * time.Second}
	return sqlutil.Interpolate(q, macros)
}

func TestMacrosExpand(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"$__timeFilter(start_ns)", "start_ns >= 1704164645006007008 AND start_ns < 1704170045506007008"},
		{"$__timeFilter(start_ns, ns)", "start_ns >= 1704164645006007008 AND start_ns < 1704170045506007008"},
		{"$__timeFilter(t.start_us, us)", "t.start_us >= 1704164645006007 AND t.start_us < 1704170045506008"},
		{"$__timeFilter(ms_col, ms)", "ms_col >= 1704164645006 AND ms_col < 1704170045507"},
		{"$__timeFilter(\"Sec\", s)", "\"Sec\" >= 1704164645 AND \"Sec\" < 1704170046"},
		{"$__timeFrom()", "1704164645006007008"},
		{"$__timeFrom", "1704164645006007008"},
		{"$__timeFrom(ms)", "1704164645006"},
		{"$__timeTo(s)", "1704170046"},
		{"$__timeTo(us)", "1704170045506008"},
		{"$__timeGroup(start_ns, 1m)", "(start_ns / 60000000000) * 60000000000"},
		{"$__timeGroup(start_ns, '5m')", "(start_ns / 300000000000) * 300000000000"},
		{"$__timeGroup(start_ns, $__interval)", "(start_ns / 30000000000) * 30000000000"},
		{"$__timeGroup(c, 1h, s)", "(c / 3600) * 3600"},
		{"$__timeGroup(c, 1d, ms)", "(c / 86400000) * 86400000"},
		{"$__timeGroupAlias(start_ns, 1m)", "(start_ns / 60000000000) * 60000000000 AS \"time\""},
		{"$__timeGroupAlias(c, 10s, ms)", "((c / 10000) * 10000) * 1000000 AS \"time\""},
		{"$__time(start_ns)", "start_ns AS \"time\""},
		{"$__time(c, us)", "(c) * 1000 AS \"time\""},
		{"$__unixEpochNanoFilter(start_ns)", "start_ns >= 1704164645006007008 AND start_ns < 1704170045506007008"},
		{"$__unixEpochNanoFrom()", "1704164645006007008"},
		{"$__unixEpochNanoTo()", "1704170045506007008"},
		{"$__hex(0A1b)", "X'0a1b'"},
		{
			"SELECT $__time(start_ns), count(*) FROM spans WHERE $__timeFilter(start_ns) AND trace_id = $__hex(00ff) GROUP BY $__timeGroup(start_ns, 1m)",
			"SELECT start_ns AS \"time\", count(*) FROM spans WHERE start_ns >= 1704164645006007008 AND start_ns < 1704170045506007008 AND trace_id = X'00ff' GROUP BY (start_ns / 60000000000) * 60000000000",
		},
	}
	for _, c := range cases {
		got, err := interpolate(t, c.in)
		if err != nil {
			t.Errorf("%s: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s\n got %s\nwant %s", c.in, got, c.want)
		}
	}
}

func TestMacrosRefuseMalformedAndInjection(t *testing.T) {
	t.Parallel()
	cases := []string{
		"$__timeFilter",
		"$__timeFilter()",
		"$__timeFilter(a, ns, x)",
		"$__timeFilter(a, minutes)",
		"$__timeFilter(1=1 OR start_ns)",
		"$__timeFilter(a; DROP TABLE spans)",
		"$__timeFilter(\"a\"\" OR 1=1\")",
		"$__timeFilter(a.)",
		"$__timeFilter(.a)",
		"$__timeFilter('a')",
		"$__timeFilter($tenant)",
		"$__timeFrom(ns, ms)",
		"$__timeTo(hours)",
		"$__timeGroup(c)",
		"$__timeGroup(c, 1m; DELETE FROM spans)",
		"$__timeGroup(c, 0s)",
		"$__timeGroup(c, -1m)",
		"$__timeGroup(c, 500ms, s)",
		"$__timeGroup(c, 1500ms, s)",
		"$__timeGroup(c(1), 1m)",
		"$__timeGroupAlias(c, 1m, ms, x)",
		"$__time()",
		"$__time(c, ps)",
		"$__unixEpochNanoFilter(c, ms)",
		"$__unixEpochNanoFrom(ms)",
		"$__unixEpochNanoTo(x)",
		"$__hex()",
		"$__hex(abc)",
		"$__hex(zz)",
		"$__hex(ab' OR '1'='1)",
		"$__hex(ab, cd)",
		"$__table",
		"$__column",
		"$__timeFilter(a",
	}
	for _, in := range cases {
		got, err := interpolate(t, in)
		if err == nil {
			t.Errorf("%s accepted as %s", in, got)
			continue
		}
		var me *MacroError
		if !errors.As(err, &me) && err.Error() != "failed to parse macro arguments (missing close bracket?)" {
			t.Errorf("%s: unexpected error type %T %v", in, err, err)
		}
	}
}

func TestMacroIntervalNeedsQueryInterval(t *testing.T) {
	t.Parallel()
	q := &sqlutil.Query{RawSQL: "$__timeGroup(c, $__interval)", TimeRange: backend.TimeRange{From: macroFrom, To: macroTo}}
	if _, err := sqlutil.Interpolate(q, macros); err == nil {
		t.Fatal("a zero query interval produced a bucket")
	}
}
