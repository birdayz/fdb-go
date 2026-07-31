package javacorpus

import (
	"strings"
	"testing"

	"fdb.dev/pkg/relational/conformance/javayamsql"
)

// classifyConfigs decides POSITIONAL questions — which consumer a sticky
// `resultMetadata:` attaches to, what an initialVersion marker removes, which
// directive claims the query — and every one of them is invisible to a test
// that can only observe rows. These assert them directly.

// commandFrom parses a query with an arbitrary config block and returns the
// parsed command, so these run the same parse path the corpus does.
func commandFrom(t *testing.T, configLines ...string) *javayamsql.Command {
	t.Helper()
	var b strings.Builder
	b.WriteString("---\nschema_template: create table t(a bigint, primary key(a))\n---\n" +
		"test_block:\n  tests:\n    -\n      - query: select a from t\n")
	for _, line := range configLines {
		b.WriteString("      - " + line + "\n")
	}
	f, err := javayamsql.Parse("configplan_test.yamsql", []byte(b.String()))
	if err != nil {
		t.Fatalf("parse %v: %v", configLines, err)
	}
	for _, blk := range f.Blocks {
		if blk.Kind == javayamsql.BlockTest {
			return blk.Test.Tests[0].Command
		}
	}
	t.Fatalf("no test block parsed from %v", configLines)
	return nil
}

func TestClassifyConfigsMetadataAttachment(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		configs []string
		// wantMeta is the 1-based position in configs of the resultMetadata the
		// first consumer should pick up, or 0 for none. Identity, not line
		// number: two directives on one query differ only by which object the
		// consumer got.
		wantMeta      int
		wantConsuming int
		wantSkipQuery SkipClass
	}{
		{
			name:          "metadata before the consumer attaches",
			configs:       []string{"resultMetadata: [{A: BIGINT}]", "result: []"},
			wantMeta:      1,
			wantConsuming: 1,
		},
		{
			// Java sets stickyMetadata, the loop ends, and it is never read.
			// Nothing is asserted about it and nothing is skipped: the
			// directive is inert by the corpus's own semantics.
			name:          "metadata after the only consumer attaches to nothing",
			configs:       []string{"result: []", "resultMetadata: [{A: BIGINT}]"},
			wantMeta:      0,
			wantConsuming: 1,
		},
		{
			// The continuation-page shape: the directive lands mid-walk and
			// Java re-checks it on every later page. More than one consumer
			// hands the whole query to the continuation surface.
			name: "metadata mid-walk with several consumers",
			configs: []string{
				"result: []", "resultMetadata: [{A: BIGINT}]", "result: []",
			},
			wantMeta:      0,
			wantConsuming: 2,
		},
		{
			// The last one before the first consumer wins — Java overwrites the
			// sticky field on each directive it passes.
			name: "the nearest preceding metadata wins",
			configs: []string{
				"resultMetadata: [{A: BIGINT}]", "resultMetadata: [{A: STRING}]", "result: []",
			},
			wantMeta:      2,
			wantConsuming: 1,
		},
		{
			// A Go run is a current-version run, so the less-than branch is
			// never selected: both the directive and the consumer inside it are
			// dropped. Nothing is skipped for the directive because Java drops
			// it identically — `shouldExecute` gates it out before the sticky
			// field is ever set.
			//
			// The branches must be exhaustive or the parser refuses the file,
			// which is why each case carries both markers.
			name: "an initialVersion gate drops the directive with its branch",
			configs: []string{
				"initialVersionLessThan: 1.0.0.0", "resultMetadata: [{A: BIGINT}]", "result: []",
				"initialVersionAtLeast: 1.0.0.0", "count: 0",
			},
			wantMeta:      0,
			wantConsuming: 1,
		},
		{
			name: "the selected branch keeps the attachment",
			configs: []string{
				"initialVersionAtLeast: 1.0.0.0", "resultMetadata: [{A: BIGINT}]", "result: []",
				"initialVersionLessThan: 1.0.0.0", "count: 0",
			},
			wantMeta:      2,
			wantConsuming: 1,
		},
		{
			name:          "maxRows claims the whole query",
			configs:       []string{"maxRows: 1", "resultMetadata: [{A: BIGINT}]", "result: []"},
			wantConsuming: 1,
			wantMeta:      2,
			wantSkipQuery: SkipContinuation,
		},
		{
			name:          "a setup config claims the whole query",
			configs:       []string{"setup: CREATE TEMPORARY FUNCTION f() AS SELECT 1", "result: []"},
			wantConsuming: 1,
			wantSkipQuery: SkipTemporaryFunction,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := commandFrom(t, tc.configs...)
			plan := classifyConfigs(cmd)

			if len(plan.Consuming) != tc.wantConsuming {
				t.Fatalf("consuming = %d, want %d", len(plan.Consuming), tc.wantConsuming)
			}
			if plan.SkipQuery != tc.wantSkipQuery {
				t.Errorf("skipQuery = %q, want %q", plan.SkipQuery, tc.wantSkipQuery)
			}
			switch {
			case tc.wantMeta == 0 && plan.Metadata != nil:
				t.Fatalf("metadata attached (line %d) but nothing should pick it up", plan.Metadata.Line)
			case tc.wantMeta != 0 && plan.Metadata == nil:
				t.Fatalf("no metadata attached, want the one at config %d", tc.wantMeta)
			case tc.wantMeta != 0:
				want := cmd.Configs[tc.wantMeta-1]
				if plan.Metadata != want {
					t.Fatalf("metadata attached from the config at line %d, want the one at line %d",
						plan.Metadata.Line, want.Line)
				}
			}
		})
	}
}

func TestClassifyConfigsCountedSkips(t *testing.T) {
	t.Parallel()

	plan := classifyConfigs(commandFrom(t,
		"explain: SCAN(T)", "debugger: sane", "result: []"))
	if len(plan.Skips) != 2 {
		t.Fatalf("got %d counted skips, want 2 (plan-assertion + debugger): %+v", len(plan.Skips), plan.Skips)
	}
	if plan.Skips[0].Class != SkipPlanAssertion || plan.Skips[1].Class != SkipDebugger {
		t.Fatalf("skip classes = %v, want [%s %s]", plan.Skips, SkipPlanAssertion, SkipDebugger)
	}
	// Neither claims the query: both decline an extra dimension of an assertion
	// that still runs.
	if plan.SkipQuery != "" || len(plan.Consuming) != 1 {
		t.Fatalf("a per-directive skip must not claim the query; got skipQuery=%q consuming=%d",
			plan.SkipQuery, len(plan.Consuming))
	}
}

// TestMetadataIsRead pins QueryExecutor.checkInlineMetadataIfPresent's guard.
//
// All four shapes are LEGAL corpus input — QueryConfig.validateConfigs admits
// `error:` and `count:` as the required consumer — and Java no-ops on each. A
// runner that errored instead would reject files Java accepts, which is how a
// stricter-than-Java runner turns into a wrong one.
func TestMetadataIsRead(t *testing.T) {
	t.Parallel()

	cfg := func(k javayamsql.ConfigKind) *javayamsql.Config { return &javayamsql.Config{Kind: k} }

	cases := []struct {
		name         string
		target       *javayamsql.Config
		rowReturning bool
		want         bool
	}{
		{"result on a row-returning statement", cfg(javayamsql.ConfigResult), true, true},
		{"unorderedResult on a row-returning statement", cfg(javayamsql.ConfigUnorderedResult), true, true},
		{"error produces no result set", cfg(javayamsql.ConfigError), true, false},
		{"count yields an update count", cfg(javayamsql.ConfigCount), false, false},
		{"a non-row-returning statement has no result set", cfg(javayamsql.ConfigResult), false, false},
		{"no consumer at all", nil, true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := metadataIsRead(tc.target, tc.rowReturning); got != tc.want {
				t.Fatalf("metadataIsRead = %v, want %v", got, tc.want)
			}
		})
	}
}
