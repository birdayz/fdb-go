package factory

import (
	"context"
	"database/sql"
	"fmt"
)

// Sweep runs whole seeds: it materializes each seed's case once and evaluates
// every candidate derived from it against that one schema.
//
// One schema per SEED, not per candidate. A seed's candidates share a table,
// a row set and an index set — they differ only in the query — so a schema per
// candidate would pay the CREATE SCHEMA TEMPLATE / CREATE SCHEMA / INSERT cost
// several times over for identical state, and that cost dominates a factory
// run (the queries themselves are milliseconds against a warm container).
type Sweep struct {
	// SetupDB executes the schema DDL; it must already point at an existing
	// database path.
	SetupDB *sql.DB
	DBPath  string
	// ClusterFile is the path the per-schema query connections open.
	ClusterFile string
	// Java, when non-nil, adds the cross-engine oracle.
	Java CrossEngine
	// Date is stamped into every header.
	Date string
	// Filter, when non-nil, restricts the sweep to the candidates it accepts.
	// It exists for re-emission (the format migration regenerates exactly the
	// committed (seed, query, projection) triples): re-running a seed evaluates
	// every candidate it derives, and the ones the original batches
	// dedup-rejected must not be offered now, or a re-emission would grow the
	// corpus as a side effect of rewriting it.
	Filter func(Candidate) bool
	// Nested sweeps the NESTED candidate family instead of the flat one: same
	// seeds, a different derivation, a disjoint file namespace. It is a mode
	// rather than an additional pass because a seed materializes ONE schema and
	// the two families need different ones.
	Nested bool
}

// seedCandidates derives one seed's candidates under EXACTLY ONE family, and
// names the schema prefix that family writes under.
//
// The choice is expressed as early RETURNS, so the exclusivity is structural
// rather than a property of statement order. The form this replaces was
//
//	cands, prefix := Candidates(seed), "fc"
//	if s.Nested {
//		cands, prefix = NestedCandidates(seed), "fcn"
//	}
//
// which READS as an either/or and is not one: a short variable declaration
// evaluates its right-hand side before the following `if` runs, so the FLAT
// derivation executed on every nested seed and was immediately overwritten —
// a whole seed's generation and feature-vector computation done for nothing.
// A comment above it asserted the opposite, and the wasted work was the lesser
// problem: source that claims a behaviour it does not have is the same defect
// class this corpus's census exists to keep out.
//
// The two derivations therefore arrive as PARAMETERS. That is the only shape in
// which the one-family claim is a property a test can COUNT rather than read,
// and reading is precisely what produced the wrong verdict on the previous
// attempt at this fix.
func seedCandidates(nested bool, flat, nest func(uint64) []Candidate, seed uint64) ([]Candidate, string) {
	if nested {
		return nest(seed), "fcn"
	}
	return flat(seed), "fc"
}

// RunSeed evaluates every candidate of one seed and offers each outcome to the
// batch. A seed with no TLP-eligible candidate is not an error — the generator
// emits aggregates, unions and LIMIT queries the partition oracle declines by
// construction — so it returns cleanly with nothing offered.
func (s Sweep) RunSeed(ctx context.Context, seed uint64, batch *Batch) ([]Outcome, error) {
	cands, schemaPrefix := seedCandidates(s.Nested, Candidates, NestedCandidates, seed)
	if s.Filter != nil {
		kept := cands[:0]
		for _, c := range cands {
			if s.Filter(c) {
				kept = append(kept, c)
			}
		}
		cands = kept
	}
	if len(cands) == 0 {
		return nil, nil
	}
	for range cands {
		batch.CountCandidate()
	}

	schema := fmt.Sprintf("%s%d", schemaPrefix, seed)
	tmpl := schema + "t"
	ddl := cands[0].Case.DDL()
	for _, stmt := range []string{
		fmt.Sprintf("CREATE SCHEMA TEMPLATE %s %s", tmpl, ddl),
		fmt.Sprintf("CREATE SCHEMA %s/%s WITH TEMPLATE %s", s.DBPath, schema, tmpl),
	} {
		if _, err := s.SetupDB.ExecContext(ctx, stmt); err != nil {
			return nil, fmt.Errorf("setup %q: %w", stmt, err)
		}
	}
	defer func() {
		_, _ = s.SetupDB.ExecContext(ctx, fmt.Sprintf("DROP SCHEMA %s/%s", s.DBPath, schema))
		_, _ = s.SetupDB.ExecContext(ctx, fmt.Sprintf("DROP SCHEMA TEMPLATE %s", tmpl))
	}()

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=%s", s.DBPath, s.ClusterFile, schema))
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	defer db.Close()

	defaultConn, err := PinConn(ctx, db, nil)
	if err != nil {
		return nil, fmt.Errorf("pin default conn: %w", err)
	}
	defer defaultConn.Close() //nolint:errcheck
	secondConn, err := PinConn(ctx, db, SecondPlanRules())
	if err != nil {
		return nil, fmt.Errorf("pin second-plan conn: %w", err)
	}
	defer secondConn.Close() //nolint:errcheck

	if _, err := defaultConn.ExecContext(ctx, cands[0].Case.InsertSQL()); err != nil {
		return nil, fmt.Errorf("insert: %w", err)
	}

	runner := &Runner{
		DefaultConn:    defaultConn,
		SecondPlanConn: secondConn,
		Java:           s.Java,
		Date:           s.Date,
	}
	outcomes := make([]Outcome, 0, len(cands))
	for _, cand := range cands {
		o := runner.Run(ctx, cand)
		if _, err := batch.Offer(o); err != nil {
			return outcomes, err
		}
		outcomes = append(outcomes, o)
	}
	return outcomes, nil
}
