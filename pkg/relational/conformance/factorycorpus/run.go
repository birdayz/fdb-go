package factorycorpus

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"fdb.dev/pkg/relational/conformance/yamsql"
)

// RunFile executes one committed scenario against db and returns the yamsql
// runner's result.
//
// Execution is NOT re-implemented: it delegates to yamsql.Run, so a committed
// factory scenario is executed by exactly the machinery that executes a
// hand-authored one — same schema-template lifecycle, same row diff, same
// column and error-code assertions. That is what makes the output format worth
// having. Only the LOADING is this package's own, because the corpus must not
// land in the ledgers the hand-authored corpus generates.
//
// Names are derived from the scenario, and the scenario names are unique by
// construction (seed, query index, projection), so parallel execution cannot
// collide on a database path or a template name.
func RunFile(ctx context.Context, db *sql.DB, f *File) (*yamsql.Result, error) {
	name := sanitize(f.Header.Name)
	return yamsql.Run(ctx, f.Scenario, yamsql.RunConfig{
		DB:           db,
		DBPath:       DBPathFor(f),
		SchemaName:   SchemaName,
		TemplateName: "FC_TMPL_" + strings.ToUpper(name),
	})
}

// SchemaName is the schema every committed scenario is instantiated under.
const SchemaName = "fc"

// DBPathFor is the database path a scenario runs in.
//
// It exists so the CALLER's DSN and the RUNNER's CREATE DATABASE cannot drift:
// the caller has to open a connection pointed at a database the runner has not
// created yet, so the two derive the same path from the same file, and a
// change to the naming moves both at once. Deriving it twice is how a rename
// turns into a scenario that creates one database and queries another.
func DBPathFor(f *File) string { return "/_fc_" + sanitize(f.Header.Name) }

// ScenarioTimeout bounds one scenario. A committed scenario runs four queries
// over at most a couple of dozen rows; anything approaching this bound is a
// hang, not slow work, and failing at the bound keeps a hung scenario from
// consuming the whole target's budget and reporting as a timeout with no name
// attached.
const ScenarioTimeout = 2 * time.Minute

// Describe renders a failed result for a test log: which stanza failed, its
// query, and the diff.
func Describe(f *File, r *yamsql.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (seed %d, query %d, projection %d, blessing %s, oracles %s)\n",
		f.Path, f.Header.Seed, f.Header.QueryIndex, f.Header.Projection,
		f.Header.Blessing, strings.Join(f.Header.Oracles, "+"))
	if r.SetupError != nil {
		fmt.Fprintf(&b, "  setup: %v\n", r.SetupError)
	}
	for _, fail := range r.Failures {
		fmt.Fprintf(&b, "  tests[%d] %s\n    %s\n", fail.Index, fail.Query, fail.Message)
	}
	fmt.Fprintf(&b, "  regenerate: go run ./cmd/factory-run -seed-start %d -seeds 1 -date %s\n",
		f.Header.Seed, f.Header.Date)
	return b.String()
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
