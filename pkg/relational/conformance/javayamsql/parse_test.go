package javayamsql_test

import (
	"strings"
	"testing"

	"fdb.dev/pkg/relational/conformance/javayamsql"
)

// wrap puts body inside the minimal file that will carry a single test_block.
func wrap(body string) []byte {
	return []byte("test_block:\n  tests:\n" + body)
}

func parseOne(t *testing.T, body string) *javayamsql.File {
	t.Helper()
	f, err := javayamsql.Parse("t.yamsql", wrap(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return f
}

func firstConfigRows(t *testing.T, body string) []javayamsql.Row {
	t.Helper()
	f := parseOne(t, body)
	cfgs := f.Blocks[0].Test.Tests[0].Command.Configs
	if len(cfgs) == 0 {
		t.Fatal("no configs parsed")
	}
	return cfgs[0].Rows
}

// TestRowCellPolarity pins the rule the whole result model hangs on.
//
// Java decides by-name vs positional on one question — is the mapping entry's
// VALUE a YAML null — and the third case below is the one that looks like an
// exception and is not. `!null` is a custom tag, so SnakeYAML constructs an
// IsNullMatcher object; the entry's value is therefore non-null and the cell is
// matched BY NAME, asserting that the named column is SQL NULL. Reading `!null`
// as "no value here" would silently turn every such assertion into a positional
// match against a different column.
func TestRowCellPolarity(t *testing.T) {
	t.Parallel()

	rows := firstConfigRows(t, `    -
      - query: select * from t
      - result:
        - {ID: 10, NAME: 'a', MISSING: !null }
        - {!pos 7: 'x', 42}
`)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}

	type want struct {
		positional bool
		column     string
		index      int64
	}
	byName := []want{
		{false, "ID", 0},
		{false, "NAME", 0},
		{false, "MISSING", 0}, // !null is a tag, NOT a YAML null
	}
	positional := []want{
		{true, "", 7}, // !pos overrides the ordinal
		{true, "", 2}, // bare value: positional at its ordinal
	}

	check := func(cells []javayamsql.Cell, wants []want) {
		t.Helper()
		if len(cells) != len(wants) {
			t.Fatalf("got %d cells, want %d", len(cells), len(wants))
		}
		for i, w := range wants {
			c := cells[i]
			if got := c.Positional(); got != w.positional {
				t.Errorf("cell %d: Positional()=%v, want %v", i, got, w.positional)
			}
			if w.positional {
				if got := c.ColumnIndex(i + 1); got != w.index {
					t.Errorf("cell %d: ColumnIndex=%d, want %d", i, got, w.index)
				}
				continue
			}
			name, ok := c.ColumnName()
			if !ok || name != w.column {
				t.Errorf("cell %d: ColumnName()=%q,%v, want %q,true", i, name, ok, w.column)
			}
		}
	}
	check(rows[0].Cells, byName)
	check(rows[1].Cells, positional)

	// The !null cell's expectation is its VALUE, which carries the tag.
	if got := rows[0].Cells[2].Expected().TagName(); got != javayamsql.TagNull {
		t.Errorf("!null cell expectation tag = %q, want %q", got, javayamsql.TagNull)
	}
	// The bare positional cell's expectation is its KEY, since its value is absent.
	if got, _ := rows[1].Cells[1].Expected().AsInt(); got != 42 {
		t.Errorf("positional cell expectation = %v, want 42", got)
	}
}

// TestExplicitYAMLNullIsPositional pins that an explicit `null` value is the
// same thing as an elided one.
//
// SnakeYAML constructs Java null for `{A: }`, `{A: null}` and `{A: ~}` alike, so
// Matchers.valueElseKey cannot tell them apart and all three mean "positional".
// Anything that made the explicit spellings by-name would produce a lookup for a
// column named "A" where Java looks up column 1.
func TestExplicitYAMLNullIsPositional(t *testing.T) {
	t.Parallel()

	for _, spelling := range []string{"{A: }", "{A: null}", "{A: ~}", "{A}"} {
		rows := firstConfigRows(t, `    -
      - query: select * from t
      - result: [`+spelling+`]
`)
		c := rows[0].Cells[0]
		if !c.Positional() {
			t.Errorf("%s: want positional, got by-name", spelling)
		}
		if got, _ := c.Expected().AsString(); got != "A" {
			t.Errorf("%s: expectation = %q, want the key %q", spelling, got, "A")
		}
	}
}

// TestNullKeyAndNullValueRejected pins Matchers.valueElseKey's guard: with both
// halves absent there is no expectation left to state, and the author almost
// certainly wanted the !null matcher.
func TestNullKeyAndNullValueRejected(t *testing.T) {
	t.Parallel()

	_, err := javayamsql.Parse("t.yamsql", wrap(`    -
      - query: select * from t
      - result: [{null: null}]
`))
	if err == nil || !strings.Contains(err.Error(), "consider using '!null'") {
		t.Fatalf("got %v, want a rejection pointing at !null", err)
	}
}

// TestSingletonTagsAcceptDummyArguments pins that the matcher tags ignore
// whatever follows them.
//
// IgnoreTag, IsNullTag, NotNullTag and CurrentVersionTag all `return INSTANCE`
// without reading the node, and the corpus depends on it: a flow mapping needs
// a key to hang the tag on, so it writes throwaway placeholders. Rejecting the
// placeholder as "unexpected argument" refuses 50-odd corpus files that are
// entirely valid upstream.
func TestSingletonTagsAcceptDummyArguments(t *testing.T) {
	t.Parallel()

	rows := firstConfigRows(t, `    -
      - query: select * from t
      - result: [{!ignore dc, !null _, !not_null x}]
`)
	got := make([]string, 0, 3)
	for _, c := range rows[0].Cells {
		got = append(got, c.Expected().TagName())
	}
	want := []string{javayamsql.TagIgnore, javayamsql.TagNull, javayamsql.TagNotNull}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("cell %d tag = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestDebuggerValues pins DebuggerImplementation's actual constants.
//
// QueryConfig's javadoc advertises "SANE, VERBOSE, QUIET"; the enum declares
// INSANE, SANE, RECORDING and REPL. Trusting the prose rejects
// simple-query-with-different-debuggers.yamsql, which uses `insane`.
func TestDebuggerValues(t *testing.T) {
	t.Parallel()

	for _, ok := range []string{"insane", "sane", "recording", "repl"} {
		if _, err := javayamsql.Parse("t.yamsql", wrap(`    -
      - query: select * from t
      - debugger: `+ok+`
`)); err != nil {
			t.Errorf("debugger %q: %v", ok, err)
		}
	}
	for _, bad := range []string{"verbose", "quiet", "nonsense"} {
		if _, err := javayamsql.Parse("t.yamsql", wrap(`    -
      - query: select * from t
      - debugger: `+bad+`
`)); err == nil {
			t.Errorf("debugger %q: parsed clean, want rejection", bad)
		}
	}
}

// TestErrorCodeResolution pins QueryConfig.resolveErrorCode: a five-character
// value is a SQLSTATE verbatim, anything else must name an ErrorCode constant.
func TestErrorCodeResolution(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ in, want string }{
		{"42601", "42601"},
		{"SYNTAX_ERROR", "42601"},
		{"UNIQUE_CONSTRAINT_VIOLATION", "23505"},
	} {
		f, err := javayamsql.Parse("t.yamsql", wrap(`    -
      - query: select * from t
      - error: "`+tc.in+`"
`))
		if err != nil {
			t.Fatalf("error %q: %v", tc.in, err)
		}
		got := f.Blocks[0].Test.Tests[0].Command.Configs[0].ErrorCode
		if got != tc.want {
			t.Errorf("error %q resolved to %q, want %q", tc.in, got, tc.want)
		}
	}

	if _, err := javayamsql.Parse("t.yamsql", wrap(`    -
      - query: select * from t
      - error: "NO_SUCH_ERROR_CODE"
`)); err == nil {
		t.Error("unknown ErrorCode name parsed clean, want rejection")
	}
}

// TestConfigOrdering pins QueryConfig.parseConfigs' rule and its one exemption.
func TestConfigOrdering(t *testing.T) {
	t.Parallel()

	// A non-result config after a result config is refused.
	if _, err := javayamsql.Parse("t.yamsql", wrap(`    -
      - query: select * from t
      - result: []
      - explain: "X"
`)); err == nil {
		t.Error("explain after result parsed clean, want rejection")
	}

	// resultMetadata does not arm the rule, so explain may still follow it.
	if _, err := javayamsql.Parse("t.yamsql", wrap(`    -
      - query: select * from t
      - resultMetadata: [{ID: BIGINT}]
      - explain: "X"
      - result: []
`)); err != nil {
		t.Errorf("explain after resultMetadata: %v", err)
	}

	// resultMetadata still needs something that actually consumes rows.
	if _, err := javayamsql.Parse("t.yamsql", wrap(`    -
      - query: select * from t
      - resultMetadata: [{ID: BIGINT}]
`)); err == nil {
		t.Error("resultMetadata without a consumer parsed clean, want rejection")
	}
}

// TestErrorConfigNeedNotBeLast pins a NEGATIVE result, and it is load-bearing.
//
// Java does require an `error` config to come last — but it asserts that in
// QueryCommand.executeInternal, at execution. Promoting it to a parse rule
// would refuse files upstream loads happily, so the parser must NOT enforce it.
// If the assertion ever moves into QueryConfig.validateConfigs, this test goes
// red and the rule should move here with it.
func TestErrorConfigNeedNotBeLast(t *testing.T) {
	t.Parallel()

	if _, err := javayamsql.Parse("t.yamsql", wrap(`    -
      - query: select * from t
      - error: "42601"
      - count: 1
`)); err != nil {
		t.Fatalf("error followed by count is a parse error here, but Java only "+
			"rejects it at execution (QueryCommand.executeInternal): %v", err)
	}
}

// TestSupportedVersionMustBeFirstConfig pins QueryConfig.validateConfigs.
func TestSupportedVersionMustBeFirstConfig(t *testing.T) {
	t.Parallel()

	if _, err := javayamsql.Parse("t.yamsql", wrap(`    -
      - query: select * from t
      - supported_version: 4.0.559.0
      - result: []
`)); err != nil {
		t.Errorf("supported_version first: %v", err)
	}
	if _, err := javayamsql.Parse("t.yamsql", wrap(`    -
      - query: select * from t
      - maxRows: 1
      - supported_version: 4.0.559.0
`)); err == nil {
		t.Error("supported_version in second position parsed clean, want rejection")
	}
}

// TestVersionCoverage pins that initialVersion* ranges must partition every
// version, including across the !current_version sentinel, which sorts above
// every literal version.
func TestVersionCoverage(t *testing.T) {
	t.Parallel()

	if _, err := javayamsql.Parse("t.yamsql", wrap(`    -
      - query: select * from t
      - initialVersionLessThan: 3.0.19.0
      - result: []
      - initialVersionAtLeast: 3.0.19.0
      - result: []
`)); err != nil {
		t.Errorf("complementary ranges: %v", err)
	}

	if _, err := javayamsql.Parse("t.yamsql", wrap(`    -
      - query: select * from t
      - initialVersionLessThan: 3.0.19.0
      - result: []
      - initialVersionAtLeast: 3.0.20.0
      - result: []
`)); err == nil {
		t.Error("gap between ranges parsed clean, want rejection")
	}
}

// TestSegmentTagSets pins that the parameter-injection tag registry is a
// different set from the row registry, in both directions.
//
// QueryParameterYamlConstructor registers !r/!a/!in/!n plus !l/!b/!f and the
// vector and uuid tags, but none of the matcher tags. An unregistered tag inside
// a segment does not raise upstream — it falls through to a plain scalar and the
// tag is lost — so a `!! !null !!` would silently bind something unintended.
func TestSegmentTagSets(t *testing.T) {
	t.Parallel()

	if !javayamsql.IsRowTag(javayamsql.TagNull) || javayamsql.IsSegmentTag(javayamsql.TagNull) {
		t.Error("!null must be a row tag and not a segment tag")
	}
	if javayamsql.IsRowTag(javayamsql.TagRandom) || !javayamsql.IsSegmentTag(javayamsql.TagRandom) {
		t.Error("!r must be a segment tag and not a row tag")
	}
	if !javayamsql.IsRowTag(javayamsql.TagLong) || !javayamsql.IsSegmentTag(javayamsql.TagLong) {
		t.Error("!l must be in both registries")
	}

	if _, err := javayamsql.ParseSegments("select * from t where a > !! !null !!", 1); err == nil {
		t.Error("a matcher tag inside a segment parsed clean, want rejection")
	}
}

// TestParseSegments pins the shape of parameter injection.
func TestParseSegments(t *testing.T) {
	t.Parallel()

	segs, err := javayamsql.ParseSegments("select * from t where a > !! 10 !! and b in !! !r [2, 9] !!", 1)
	if err != nil {
		t.Fatalf("parse segments: %v", err)
	}
	if len(segs) != 2 {
		t.Fatalf("got %d segments, want 2", len(segs))
	}
	if segs[0].Kind != javayamsql.SegmentLiteral {
		t.Errorf("segment 0 kind = %v, want literal", segs[0].Kind)
	}
	if got, _ := segs[0].Value.AsInt(); got != 10 {
		t.Errorf("segment 0 value = %v, want 10", got)
	}
	if segs[1].Kind != javayamsql.SegmentUnbound {
		t.Errorf("segment 1 kind = %v, want unbound (a !r generator needs a Random)", segs[1].Kind)
	}
	if segs[1].Raw != "!! !r [2, 9] !!" {
		t.Errorf("segment 1 raw = %q; it must be the exact substring Java replaces", segs[1].Raw)
	}

	// An unpaired delimiter is what QueryInterpreter.getInjections asserts on.
	if _, err := javayamsql.ParseSegments("select !! 10", 1); err == nil {
		t.Error("unpaired !! parsed clean, want rejection")
	}
}

// TestRandomStrDescriptor pins that RandomStringParser's format is validated
// while the tag is constructed, so a malformed descriptor is a parse error.
func TestRandomStrDescriptor(t *testing.T) {
	t.Parallel()

	if _, err := javayamsql.Parse("t.yamsql", wrap(`    -
      - query: select * from t
      - result: [{A: !randomStr seed 1001 length 500}]
`)); err != nil {
		t.Errorf("well-formed descriptor: %v", err)
	}
	if _, err := javayamsql.Parse("t.yamsql", wrap(`    -
      - query: select * from t
      - result: [{A: !randomStr length 500}]
`)); err == nil {
		t.Error("descriptor missing a seed parsed clean, want rejection")
	}
}

// TestUnknownDirectivesRejected pins the four dispatches Java genuinely closes.
func TestUnknownDirectivesRejected(t *testing.T) {
	t.Parallel()

	cases := map[string][]byte{
		"unknown block key":         []byte("no_such_block:\n  x: 1\n"),
		"unknown command":           wrap("    -\n      - no_such_command: x\n"),
		"unknown config":            wrap("    -\n      - query: select 1\n      - no_such_config: x\n"),
		"unknown tag":               wrap("    -\n      - query: select 1\n      - result: [{A: !no_such_tag 1}]\n"),
		"noChecks is internal only": wrap("    -\n      - query: select 1\n      - noChecks: true\n"),
	}
	for name, src := range cases {
		if _, err := javayamsql.Parse("t.yamsql", src); err == nil {
			t.Errorf("%s: parsed clean, want rejection", name)
		}
	}
}

// TestImplicitNoChecks pins that a query with no configs gets Java's synthetic
// noChecks rather than being treated as malformed.
func TestImplicitNoChecks(t *testing.T) {
	t.Parallel()

	f := parseOne(t, `    -
      - query: select * from t
`)
	cmd := f.Blocks[0].Test.Tests[0].Command
	if len(cmd.Configs) != 0 || !cmd.ImplicitNoChecks() {
		t.Errorf("got %d configs / ImplicitNoChecks=%v, want 0 / true",
			len(cmd.Configs), cmd.ImplicitNoChecks())
	}
}

// TestOptionsBlockPosition pins Block.parse's assertion, whose isTopLevel half
// is the reason an included file may never carry file-wide options.
func TestOptionsBlockPosition(t *testing.T) {
	t.Parallel()

	if _, err := javayamsql.Parse("t.yamsql", []byte("options:\n  supported_version: 4.0.559.0\n")); err != nil {
		t.Errorf("options as the first document: %v", err)
	}
	if _, err := javayamsql.Parse("t.yamsql", []byte(
		"transaction_setups:\n  a: SELECT 1\n---\noptions:\n  supported_version: 4.0.559.0\n")); err == nil {
		t.Error("options as the second document parsed clean, want rejection")
	}
}
