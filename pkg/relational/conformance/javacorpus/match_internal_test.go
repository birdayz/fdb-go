package javacorpus

import (
	"strings"
	"testing"

	"fdb.dev/pkg/relational/conformance/javayamsql"
)

// The corpus run exercises the matcher's ACCEPT path 518 times and its REJECT
// path almost never — nearly every file whose rows would disagree is blocked
// earlier by a DDL gap. That asymmetry was measured: making matchField return
// success unconditionally moved exactly ONE file. A matcher pinned only by the
// cases it accepts is not pinned at all, so the rejections are asserted here
// directly, against the semantics Java's Matchers defines.

// configFrom builds one query config by parsing a real `.yamsql` document, so
// these tests run the same parse→match path the corpus run does rather than a
// hand-built AST that could drift from it.
func configFrom(t *testing.T, configLine string) *javayamsql.Config {
	t.Helper()
	src := "---\n" +
		"schema_template: create table t(a bigint, primary key(a))\n" +
		"---\n" +
		"test_block:\n" +
		"  tests:\n" +
		"    -\n" +
		"      - query: select a from t\n" +
		"      - " + configLine + "\n"
	f, err := javayamsql.Parse("match_test.yamsql", []byte(src))
	if err != nil {
		t.Fatalf("parse %q: %v", configLine, err)
	}
	for _, b := range f.Blocks {
		if b.Kind == javayamsql.BlockTest {
			return b.Test.Tests[0].Command.Configs[0]
		}
	}
	t.Fatalf("no test block parsed from %q", configLine)
	return nil
}

func rs(cols []string, types []string, rows ...[]any) *resultSet {
	return &resultSet{Cols: cols, Types: types, Rows: rows}
}

func TestMatchResultSetOrdering(t *testing.T) {
	t.Parallel()

	two := rs([]string{"A"}, []string{"BIGINT"}, []any{int64(1)}, []any{int64(2)})

	ordered := configFrom(t, `result: [{A: !l 2}, {A: !l 1}]`)
	if err := matchResultSet(ordered, two, true); err == nil {
		t.Error("an ordered expectation must reject rows delivered in the other order")
	}
	// The SAME expectation as a multiset must accept: that is the whole
	// difference between the two directives, and a matcher that ignored order
	// in both would pass every ordered test it should fail.
	if err := matchResultSet(configFrom(t, `unorderedResult: [{A: !l 2}, {A: !l 1}]`), two, false); err != nil {
		t.Errorf("an unordered expectation must accept any permutation: %v", err)
	}
}

// TestMatchUnorderedIsAMultisetNotASet pins the case a set-based
// implementation gets wrong while looking correct on every distinct-row test:
// an expectation listed twice must be satisfied twice.
func TestMatchUnorderedIsAMultisetNotASet(t *testing.T) {
	t.Parallel()

	cfg := configFrom(t, `unorderedResult: [{A: !l 1}, {A: !l 1}]`)

	twoOnes := rs([]string{"A"}, []string{"BIGINT"}, []any{int64(1)}, []any{int64(1)})
	if err := matchResultSet(cfg, twoOnes, false); err != nil {
		t.Errorf("two expected 1s against two actual 1s must match: %v", err)
	}
	oneOne := rs([]string{"A"}, []string{"BIGINT"}, []any{int64(1)})
	if err := matchResultSet(cfg, oneOne, false); err == nil {
		t.Error("two expected 1s against ONE actual 1 must NOT match — a set would accept this")
	}
	threeOnes := rs([]string{"A"}, []string{"BIGINT"}, []any{int64(1)}, []any{int64(1)}, []any{int64(1)})
	if err := matchResultSet(cfg, threeOnes, false); err == nil {
		t.Error("two expected 1s against THREE actual 1s must NOT match")
	}
}

func TestMatchRowCardinality(t *testing.T) {
	t.Parallel()
	cfg := configFrom(t, `result: [{A: !l 1}]`)
	wide := rs([]string{"A", "B"}, []string{"BIGINT", "BIGINT"}, []any{int64(1), int64(2)})
	if err := matchResultSet(cfg, wide, true); err == nil {
		t.Error("a one-column expectation against a two-column row must be a cardinality mismatch")
	}
	extra := rs([]string{"A"}, []string{"BIGINT"}, []any{int64(1)}, []any{int64(9)})
	if err := matchResultSet(cfg, extra, true); err == nil {
		t.Error("an unconsumed actual row must be reported, not ignored")
	}
}

// TestMatchNumericPromotion pins Java's asymmetry, which is the single easiest
// thing to get wrong here: Matchers.matchIntField promotes an Integer
// expectation up to a Long actual, but a Long expectation goes through
// Objects.equals and does NOT demote. Same shape for Float vs Double.
func TestMatchNumericPromotion(t *testing.T) {
	t.Parallel()

	intCol := rs([]string{"A"}, []string{"INTEGER"}, []any{int64(1)})
	longCol := rs([]string{"A"}, []string{"BIGINT"}, []any{int64(1)})

	// A plain YAML integer inside int32 range is a Java Integer: it matches
	// both widths.
	plain := configFrom(t, `result: [{A: 1}]`)
	if err := matchResultSet(plain, intCol, true); err != nil {
		t.Errorf("Integer expectation vs INTEGER column: %v", err)
	}
	if err := matchResultSet(plain, longCol, true); err != nil {
		t.Errorf("Integer expectation must promote to a Long actual: %v", err)
	}

	// `!l` is a Java Long: it matches BIGINT and must NOT demote to INTEGER.
	long := configFrom(t, `result: [{A: !l 1}]`)
	if err := matchResultSet(long, longCol, true); err != nil {
		t.Errorf("Long expectation vs BIGINT column: %v", err)
	}
	if err := matchResultSet(long, intCol, true); err == nil {
		t.Error("a !l (Long) expectation must NOT match an INTEGER column — Objects.equals(1L, 1) is false")
	}

	floatCol := rs([]string{"A"}, []string{"FLOAT"}, []any{float64(1.5)})
	doubleCol := rs([]string{"A"}, []string{"DOUBLE"}, []any{float64(1.5)})

	f := configFrom(t, `result: [{A: !f 1.5}]`)
	if err := matchResultSet(f, floatCol, true); err != nil {
		t.Errorf("Float expectation vs FLOAT column: %v", err)
	}
	if err := matchResultSet(f, doubleCol, true); err != nil {
		t.Errorf("Float expectation must promote to a Double actual: %v", err)
	}
	d := configFrom(t, `result: [{A: 1.5}]`)
	if err := matchResultSet(d, doubleCol, true); err != nil {
		t.Errorf("Double expectation vs DOUBLE column: %v", err)
	}
	if err := matchResultSet(d, floatCol, true); err == nil {
		t.Error("a plain YAML float (Double) expectation must NOT match a FLOAT column")
	}
	if err := matchResultSet(configFrom(t, `result: [{A: 2.5}]`), doubleCol, true); err == nil {
		t.Error("a wrong Double value must be rejected")
	}
}

// TestMatchPositionalAndNamedCells pins Matchers.matchMap's hinge: whether a
// cell is matched by NAME or by POSITION is decided purely by whether the YAML
// mapping entry has a value.
func TestMatchPositionalAndNamedCells(t *testing.T) {
	t.Parallel()

	two := rs([]string{"A", "B"}, []string{"BIGINT", "BIGINT"}, []any{int64(10), int64(20)})

	// `{10, 20}` — both entries are value-less, so both are positional.
	if err := matchResultSet(configFrom(t, `result: [{!l 10, !l 20}]`), two, true); err != nil {
		t.Errorf("positional cells: %v", err)
	}
	// Named cells resolve by column identity, so declaration order need not
	// follow the result's column order.
	if err := matchResultSet(configFrom(t, `result: [{B: !l 20, A: !l 10}]`), two, true); err != nil {
		t.Errorf("named cells must resolve by name, not by ordinal: %v", err)
	}
	// …which means swapping the VALUES must still fail.
	if err := matchResultSet(configFrom(t, `result: [{B: !l 10, A: !l 20}]`), two, true); err == nil {
		t.Error("named cells with swapped values must be rejected")
	}
	// `!pos n` overrides the ordinal.
	if err := matchResultSet(configFrom(t, `result: [{!pos 2: !l 20, !pos 1: !l 10}]`), two, true); err != nil {
		t.Errorf("!pos must override the ordinal: %v", err)
	}
	// Column names match case-insensitively (getOneBasedPosition's
	// equalsIgnoreCase walk); the corpus writes both cases.
	lower := rs([]string{"a", "b"}, []string{"BIGINT", "BIGINT"}, []any{int64(10), int64(20)})
	if err := matchResultSet(configFrom(t, `result: [{A: !l 10, B: !l 20}]`), lower, true); err != nil {
		t.Errorf("column names must match case-insensitively: %v", err)
	}
	if err := matchResultSet(configFrom(t, `result: [{NOPE: !l 10, B: !l 20}]`), two, true); err == nil {
		t.Error("a name that is not a column must be rejected, not silently positional")
	}
}

func TestMatchNullTags(t *testing.T) {
	t.Parallel()

	nullRow := rs([]string{"A"}, []string{"BIGINT"}, []any{nil})
	valueRow := rs([]string{"A"}, []string{"BIGINT"}, []any{int64(1)})

	if err := matchResultSet(configFrom(t, `result: [{A: !null _}]`), nullRow, true); err != nil {
		t.Errorf("!null vs NULL: %v", err)
	}
	if err := matchResultSet(configFrom(t, `result: [{A: !null _}]`), valueRow, true); err == nil {
		t.Error("!null must reject a non-NULL cell")
	}
	if err := matchResultSet(configFrom(t, `result: [{A: !not_null _}]`), valueRow, true); err != nil {
		t.Errorf("!not_null vs a value: %v", err)
	}
	if err := matchResultSet(configFrom(t, `result: [{A: !not_null _}]`), nullRow, true); err == nil {
		t.Error("!not_null must reject a NULL cell")
	}
	// A concrete expectation against a NULL actual is rejected for every
	// expectation except !null — this is the branch that made
	// cast-documentation-queries a measured gap.
	if err := matchResultSet(configFrom(t, `result: [{A: !l 1}]`), nullRow, true); err == nil {
		t.Error("a concrete expectation must reject a NULL actual")
	}
	// !ignore accepts anything, including NULL.
	if err := matchResultSet(configFrom(t, `result: [{A: !ignore _}]`), nullRow, true); err != nil {
		t.Errorf("!ignore must accept NULL: %v", err)
	}
	// An !ignore as the WHOLE result value short-circuits the row walk.
	if err := matchResultSet(configFrom(t, `result: !ignore _`), valueRow, true); err != nil {
		t.Errorf("!ignore as the whole result must accept: %v", err)
	}
}

func TestMatchStringAndBytes(t *testing.T) {
	t.Parallel()

	strRow := rs([]string{"A"}, []string{"STRING"}, []any{"hello world"})
	if err := matchResultSet(configFrom(t, `result: [{A: !sc "lo wo"}]`), strRow, true); err != nil {
		t.Errorf("!sc substring: %v", err)
	}
	if err := matchResultSet(configFrom(t, `result: [{A: !sc "nope"}]`), strRow, true); err == nil {
		t.Error("!sc must reject a string that does not contain the fragment")
	}

	bytesRow := rs([]string{"A"}, []string{"BYTES"}, []any{[]byte{0xde, 0xad, 0xbe, 0xef}})
	// The tagged form.
	if err := matchResultSet(configFrom(t, `result: [{A: !b x'deadbeef'}]`), bytesRow, true); err != nil {
		t.Errorf("!b bytes: %v", err)
	}
	if err := matchResultSet(configFrom(t, `result: [{A: !b x'cafe'}]`), bytesRow, true); err == nil {
		t.Error("!b must reject different bytes")
	}
	// The UNTAGGED form the corpus actually writes: a plain YAML string that
	// Java reaches through the String-vs-byte[] special case.
	if err := matchResultSet(configFrom(t, `result: [{A: "x'deadbeef'"}]`), bytesRow, true); err != nil {
		t.Errorf("bare x'…' string vs a byte[] actual: %v", err)
	}
	// xstartswith_<len>'<hex>' asserts a prefix plus an exact total length.
	long := rs([]string{"A"}, []string{"BYTES"}, []any{[]byte{0xde, 0xad, 0x00, 0x00}})
	if err := matchResultSet(configFrom(t, `result: [{A: "xstartswith_4'dead'"}]`), long, true); err != nil {
		t.Errorf("xstartswith_ prefix+length: %v", err)
	}
	if err := matchResultSet(configFrom(t, `result: [{A: "xstartswith_9'dead'"}]`), long, true); err == nil {
		t.Error("xstartswith_ must enforce the declared total length")
	}
}

// TestSQLTextRendersParameterSegments pins adaptToSimpleStatement's
// substitution, which is what lets a literal `!!…!!` segment run at all.
func TestSQLTextRendersParameterSegments(t *testing.T) {
	t.Parallel()

	cases := []struct{ body, want string }{
		{"10", "10"},
		{"'abc'", "'abc'"},
		{"!n _", "null"},
		{"!l 42", "42"},
		{"!b x'cafe'", "x'cafe'"},
		{"[1, 2]", "[1, 2]"},
		{"{1, 2, 3}", "(1, 2, 3)"},
		// `!in` wraps a single LIST and renders its elements parenthesised —
		// `(1, 2)`, never `([1, 2])`. The corpus writes exactly this shape
		// (`!! !in {[1]} !!`).
		{"!in {[1, 2]}", "(1, 2)"},
	}
	for _, tc := range cases {
		segs, err := javayamsql.ParseSegments("select "+"!!"+tc.body+"!!", 1)
		if err != nil {
			t.Errorf("ParseSegments(%q): %v", tc.body, err)
			continue
		}
		if len(segs) != 1 {
			t.Errorf("ParseSegments(%q): got %d segments", tc.body, len(segs))
			continue
		}
		got, err := sqlText(segs[0].Value)
		if err != nil {
			t.Errorf("sqlText(%q): %v", tc.body, err)
			continue
		}
		if got != tc.want {
			t.Errorf("sqlText(%q) = %q, want %q", tc.body, got, tc.want)
		}
	}
}

// TestDecodeBytesTagRejectsMalformed pins the shape check BytesTag performs
// while CONSTRUCTING the node — a malformed `!b` is an error before any query
// runs, not a silent mismatch at compare time.
func TestDecodeBytesTagRejectsMalformed(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"deadbeef", "x'zz'", "x'abc", "'abcd'"} {
		v := &javayamsql.Value{Kind: javayamsql.KindString, Str: bad}
		if _, err := decodeBytesTag(v); err == nil {
			t.Errorf("decodeBytesTag(%q) should be rejected", bad)
		}
	}
	got, err := decodeBytesTag(&javayamsql.Value{Kind: javayamsql.KindString, Str: "x'DEAD'"})
	if err != nil || len(got) != 2 || got[0] != 0xde || got[1] != 0xad {
		t.Errorf("decodeBytesTag(x'DEAD') = %v, %v", got, err)
	}
}

// TestShuffleIsCollectionsShuffle pins that the non-ordered modes reorder at
// all, and do so reproducibly from the seed. Java's default seed is the wall
// clock, which cannot be reproduced; a stable seed is the only rerunnable
// choice and this is what keeps it stable.
func TestShuffleIsDeterministic(t *testing.T) {
	t.Parallel()

	build := func() []executable {
		out := make([]executable, 10)
		for i := range out {
			out[i] = executable{rep: int64(i)}
		}
		return out
	}
	a, b := build(), build()
	shuffle(a, 12345)
	shuffle(b, 12345)
	for i := range a {
		if a[i].rep != b[i].rep {
			t.Fatalf("same seed must give the same order: %v vs %v", a, b)
		}
	}
	c := build()
	shuffle(c, 999)
	same := true
	for i := range a {
		if a[i].rep != c[i].rep {
			same = false
			break
		}
	}
	if same {
		t.Error("different seeds produced the same order — the seed is not reaching the shuffle")
	}
	identity := build()
	moved := false
	for i := range a {
		if a[i].rep != identity[i].rep {
			moved = true
			break
		}
	}
	if !moved {
		t.Error("shuffle left the list in its original order")
	}
}

// TestResolveOptionsLayering pins TestBlock's defaults < preset < options-map
// precedence, including the two defaults that are easy to miss because they
// are NOT "everything absent": repetition 5 and check_cache on.
func TestResolveOptionsLayering(t *testing.T) {
	t.Parallel()

	bare := resolveOptions(&javayamsql.TestBlock{})
	if bare.Repetition != 5 || bare.Mode != "parallelized" || !bare.CheckCache ||
		bare.ConnectionLifecycle != "test" || bare.StatementType != "both" {
		t.Errorf("bare test_block defaults drifted: %+v", bare)
	}

	// single_repetition_* sets repetition 1 AND turns check_cache off.
	single := resolveOptions(&javayamsql.TestBlock{Preset: "single_repetition_ordered"})
	if single.Repetition != 1 || single.CheckCache || single.Mode != "ordered" {
		t.Errorf("single_repetition_ordered: %+v", single)
	}
	multi := resolveOptions(&javayamsql.TestBlock{Preset: "multi_repetition_randomized"})
	if multi.Repetition != 5 || !multi.CheckCache || multi.Mode != "randomized" {
		t.Errorf("multi_repetition_randomized: %+v", multi)
	}

	// The options map outranks the preset.
	three := int64(3)
	over := resolveOptions(&javayamsql.TestBlock{
		Preset:  "single_repetition_ordered",
		Options: javayamsql.TestBlockOptions{Repetition: &three, Mode: "randomized"},
	})
	if over.Repetition != 3 || over.Mode != "randomized" {
		t.Errorf("options map must outrank the preset: %+v", over)
	}
}

// TestParseJDBCURI pins the verbatim-URI form, which twelve corpus files use to
// reach the catalog explicitly and which normalises onto `connect: 0`.
func TestParseJDBCURI(t *testing.T) {
	t.Parallel()

	got, err := parseJDBCURI("jdbc:embed:/__SYS?schema=CATALOG")
	if err != nil || got.Path != catalogPath || got.Schema != "" {
		t.Errorf("catalog URI = %+v, %v", got, err)
	}
	got, err = parseJDBCURI("jdbc:embed:/FRL/X_DB?schema=S1")
	if err != nil || got.Path != "/FRL/X_DB" || got.Schema != "S1" {
		t.Errorf("database URI = %+v, %v", got, err)
	}
	if _, err := parseJDBCURI("relational://localhost:1234/db"); err == nil {
		t.Error("a non-embed scheme must be refused rather than silently mapped")
	}
}

// TestVersionGatesCollapseToCurrent pins the fact every version-dependent
// decision in the runner rests on, and names what re-arms if it changes.
func TestVersionGatesCollapseToCurrent(t *testing.T) {
	t.Parallel()

	literal := &javayamsql.Version{Major: 4, Minor: 0, Build: 559}
	current := &javayamsql.Version{Current: true}

	if !javayamsql.SelectedAtCurrentVersion(literal, nil) {
		t.Error("initialVersionAtLeast <literal> must select on current")
	}
	if javayamsql.SelectedAtCurrentVersion(nil, literal) {
		t.Error("initialVersionLessThan <literal> must NOT select on current — " +
			"if this flips, the four initial-version/wrong-*-less-than files stop being positive")
	}
	if javayamsql.SelectedAtCurrentVersion(nil, current) {
		t.Error("initialVersionLessThan !current_version must NOT select on current")
	}
	if !javayamsql.SelectedAtCurrentVersion(current, nil) {
		t.Error("initialVersionAtLeast !current_version must select on current")
	}
	if !javayamsql.SupportedAtCurrentVersion(literal) || !javayamsql.SupportedAtCurrentVersion(current) {
		t.Error("a supported_version gate must ADMIT on current — if this flips, " +
			"the nine supported-version fixed-version-meta files change meaning")
	}
}

// TestMatchArrayIgnoresSurplusActualElements pins a LENIENCY, which is the
// harder kind of behaviour to keep: nothing in a passing corpus notices a
// matcher that is too strict until a file that Java blesses turns red.
//
// Matchers' array loop is bounded by the EXPECTED element count and returns
// success without inspecting the rest, so [1, 2] matches an actual [1, 2, 3].
// Being stricter here would be a Go-only rejection of a Java-blessed
// expectation — latent while array columns are DDL-blocked, and a wave of
// false failures the moment Phase 3 unblocks them.
func TestMatchArrayIgnoresSurplusActualElements(t *testing.T) {
	t.Parallel()

	surplus := rs([]string{"A"}, []string{""}, []any{[]any{int64(1), int64(2), int64(3)}})
	if err := matchResultSet(configFrom(t, `result: [{A: [!l 1, !l 2]}]`), surplus, true); err != nil {
		t.Errorf("Java's array loop ignores surplus actual elements; Go must not reject them: %v", err)
	}
	// A MISSING element is still an error — the loop runs out of actual data
	// before it runs out of expectations, which Java does report.
	short := rs([]string{"A"}, []string{""}, []any{[]any{int64(1)}})
	if err := matchResultSet(configFrom(t, `result: [{A: [!l 1, !l 2]}]`), short, true); err == nil {
		t.Error("a missing array element must still be rejected")
	}
	// And a wrong element within the compared prefix is still an error.
	wrong := rs([]string{"A"}, []string{""}, []any{[]any{int64(1), int64(9), int64(3)}})
	if err := matchResultSet(configFrom(t, `result: [{A: [!l 1, !l 2]}]`), wrong, true); err == nil {
		t.Error("a wrong element inside the compared prefix must be rejected")
	}
	// The row-level counterpart is the OPPOSITE, and deliberately so: Java
	// drains the result set after the expectations run out and fails on
	// surplus ROWS. Pinning both sides together is what stops someone
	// "harmonising" them later.
	extraRow := rs([]string{"A"}, []string{"BIGINT"}, []any{int64(1)}, []any{int64(2)})
	if err := matchResultSet(configFrom(t, `result: [{A: !l 1}]`), extraRow, true); err == nil {
		t.Error("surplus ROWS must be rejected even though surplus array ELEMENTS are not")
	}
}

// TestGapSignaturesAreSpecific keeps the engine-gap table from becoming a mute
// allowlist: an entry with an empty or generic signature would absorb any
// future failure at that path.
func TestGapSignaturesAreSpecific(t *testing.T) {
	t.Parallel()
	for _, g := range EngineGaps() {
		if len(strings.TrimSpace(g.Signature)) < 8 {
			t.Errorf("gap %s has signature %q, too generic to distinguish a NEW failure", g.Path, g.Signature)
		}
		if g.Booking == "" {
			t.Errorf("gap %s has no booking — a gap nobody owns", g.Path)
		}
		if g.Class == "" {
			t.Errorf("gap %s has no skip class", g.Path)
		}
		if want, ok := statementExactGaps[g.Path]; ok && !strings.Contains(g.Signature, want) {
			t.Errorf("gap %s must stay STATEMENT-EXACT: signature %q no longer quotes %q, so any other "+
				"failure of the same class anywhere in the file would be swallowed as this one",
				g.Path, g.Signature, want)
		}
	}
	for path := range statementExactGaps {
		found := false
		for _, g := range EngineGaps() {
			if g.Path == path {
				found = true
			}
		}
		if !found {
			t.Errorf("statementExactGaps names %s, which is no longer in the gap table — delete the "+
				"requirement in the same change that deletes the entry", path)
		}
	}
}

// statementExactGaps are the gap entries whose signature must quote the EXACT
// failing statement rather than the class-level rejection text alone.
//
// Both files are big — array-join-at.yamsql is thirty PartiQL AT shapes, and
// functions.yamsql asserts 111 queries — and both were originally pinned to a
// message their file can produce from many different statements ("Cascades
// planner could not plan query"; "actual result set is NULL, expecting non-NULL
// result set"). A signature that generic converts the entry from "this measured
// divergence" into "any failure of this shape anywhere in the file", which is
// precisely the mute allowlist the gap table exists not to be: a NEW decline at
// a different query would have been counted as the known one and the run would
// have stayed green.
//
// The values are prefixes of the runner's `%q`-formatted statement text, so the
// embedded SQL quotes appear escaped.
var statementExactGaps = map[string]string{
	"array-join-at.yamsql": `"SELECT \"subquery\".\"id\" - 100 AS \"id\", \"at\", \"val\" FROM (SELECT \"id\" + 100 AS \"id\", \"arr1\" FROM T1) AS \"subquery\"`,
	"functions.yamsql":     `"update C set st = coalesce(st, null) where c1 = 4 returning \"new\".st"`,
}
