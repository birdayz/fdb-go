package rowdiff

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/core/embedded"
)

// OutcomeKind is the RFC-182 §4 typed classification. A seed resolves to
// exactly one kind; INFRA never masquerades as a soundness finding.
type OutcomeKind int

const (
	OutcomeOK OutcomeKind = iota
	OutcomeMismatch
	OutcomeInfra
)

// Mismatch is one confirmed row divergence, carrying everything needed to
// reproduce and pin it: the failure output IS the ready-to-paste scenario.
type Mismatch struct {
	Seed       uint64
	DDL        string
	InsertSQL  string
	SQL        string
	EngineRows []Row
	OracleRows []Row
	Detail     string
}

func (m *Mismatch) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "seed %d MISMATCH: %s\n", m.Seed, m.Detail)
	fmt.Fprintf(&b, "  query:  %s\n", m.SQL)
	fmt.Fprintf(&b, "  schema: %s\n", m.DDL)
	fmt.Fprintf(&b, "  setup:  %s\n", m.InsertSQL)
	fmt.Fprintf(&b, "  engine: %v\n", m.EngineRows)
	fmt.Fprintf(&b, "  oracle: %v\n", m.OracleRows)
	return b.String()
}

// SeedResult is one seed's outcome plus its plan-family telemetry.
type SeedResult struct {
	Seed       uint64
	Kind       OutcomeKind
	Mismatches []*Mismatch
	InfraErr   error
	Histogram  map[string]int // plan family → query count
	PlanErrors []string       // first few embedded-planner failures (diagnostics for the plan-error bucket)
	Declines   []string       // documented known-gap declines (RFC-182 §4), never silent
	Executed   int            // query×projection executions compared
}

// maxPlanErrorSamples caps PlanErrors so a systematically-broken planner
// doesn't balloon results while still leaving a diagnosable trail.
const maxPlanErrorSamples = 3

// RunSeed generates the seed's case, materializes it through the given
// sqldriver DB (which must already point at a database; RunSeed creates a
// seed-unique schema inside it), executes every query under every
// projection variant, and diffs each result against Oracle M.
//
// setupDB executes DDL (CREATE SCHEMA TEMPLATE / CREATE SCHEMA); dbPath is
// the database path the caller created (e.g. "/testdb_rowdiff");
// clusterFile connects the per-schema query DB.
func RunSeed(ctx context.Context, setupDB *sql.DB, dbPath, clusterFile string, seed uint64) *SeedResult {
	return RunCase(ctx, setupDB, dbPath, clusterFile, Generate(seed), fmt.Sprintf("rd%d", seed))
}

// RunCase is RunSeed for a pre-built case (template seeds use this). id
// must be unique per case within the database (it names the schema).
func RunCase(ctx context.Context, setupDB *sql.DB, dbPath, clusterFile string, c *Case, id string) *SeedResult {
	res := &SeedResult{Seed: c.Seed, Histogram: map[string]int{}}

	schemaName := id
	tmplName := id + "t"
	ddl := c.DDL()

	for _, stmt := range []string{
		fmt.Sprintf("CREATE SCHEMA TEMPLATE %s %s", tmplName, ddl),
		fmt.Sprintf("CREATE SCHEMA %s/%s WITH TEMPLATE %s", dbPath, schemaName, tmplName),
	} {
		if _, err := setupDB.ExecContext(ctx, stmt); err != nil {
			res.Kind = OutcomeInfra
			res.InfraErr = fmt.Errorf("setup %q: %w", stmt, err)
			return res
		}
	}

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=%s", dbPath, clusterFile, schemaName))
	if err != nil {
		res.Kind = OutcomeInfra
		res.InfraErr = fmt.Errorf("open: %w", err)
		return res
	}
	defer db.Close()

	insertSQL := c.InsertSQL()
	if _, err := db.ExecContext(ctx, insertSQL); err != nil {
		res.Kind = OutcomeInfra
		res.InfraErr = fmt.Errorf("insert: %w", err)
		return res
	}

	for _, q := range c.Queries {
		for _, projection := range c.ProjectionsFor(q) {
			sqlText := c.SQL(q, projection)

			// Plan-family telemetry from the TYPED plan tree (never EXPLAIN
			// text), via the embedded planner over the same DDL. Classified
			// per projection VARIANT: projection changes the winner even for
			// an identical WHERE (a star variant can fetch where the narrow
			// variant is covering), so star-only classification under-reports
			// executed families.
			if plan, planErr := embedded.PlanPhysicalForTest(sqlText, ddl, nil); planErr == nil {
				for _, fam := range classifyPlan(plan) {
					res.Histogram[fam]++
				}
			} else {
				res.Histogram["plan-error"]++
				if len(res.PlanErrors) < maxPlanErrorSamples {
					res.PlanErrors = append(res.PlanErrors, fmt.Sprintf("%s: %v", sqlText, planErr))
				}
			}

			engineRows, cols, err := queryRows(ctx, db, sqlText)
			if err != nil {
				// RFC-182 §4: INFRA must never masquerade as a soundness
				// finding. A context/transport failure aborts the seed as
				// INFRA; every OTHER engine error stays a finding — P1
				// generates only supported shapes (§3), and widening the
				// INFRA class would open a suppression hole.
				if isInfraError(err) {
					res.InfraErr = fmt.Errorf("query %q: %w", sqlText, err)
					return finalizeResult(res)
				}
				if gap := matchKnownGap(sqlText, err); gap != nil {
					// DECLINE, not a finding: a documented limitation with a
					// live pin. Recorded (never silent) so the rate stays
					// visible and a growing bucket is noticed.
					res.Declines = append(res.Declines,
						fmt.Sprintf("%s [pin: %s]: %s", gap.name, gap.pin, sqlText))
					continue
				}
				res.Mismatches = append(res.Mismatches, &Mismatch{
					Seed: c.Seed, DDL: ddl, InsertSQL: insertSQL, SQL: sqlText,
					Detail: fmt.Sprintf("engine error: %v", err),
				})
				continue
			}
			oracle, err := OracleRows(c, q, projection)
			if err != nil {
				res.InfraErr = fmt.Errorf("oracle: %w", err)
				return finalizeResult(res)
			}
			res.Executed++
			if detail := diffRows(engineRows, cols, oracle, q); detail != "" {
				res.Mismatches = append(res.Mismatches, &Mismatch{
					Seed: c.Seed, DDL: ddl, InsertSQL: insertSQL, SQL: sqlText,
					EngineRows: engineRows, OracleRows: oracle, Detail: detail,
				})
			}
		}
	}

	return finalizeResult(res)
}

// finalizeResult resolves the seed's single Kind with MISMATCH precedence:
// a confirmed soundness finding must never be masked by a later infra
// failure on the same seed (both reporters print InfraErr independently of
// Kind, so the infra evidence still surfaces).
func finalizeResult(res *SeedResult) *SeedResult {
	switch {
	case len(res.Mismatches) > 0:
		res.Kind = OutcomeMismatch
	case res.InfraErr != nil:
		res.Kind = OutcomeInfra
	default:
		res.Kind = OutcomeOK
	}
	return res
}

// queryRows executes and decodes to []Row keyed by UPPER-CASE column name.
func queryRows(ctx context.Context, db *sql.DB, sqlText string) ([]Row, []string, error) {
	rows, err := db.QueryContext(ctx, sqlText)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, nil, err
	}
	upper := make([]string, len(cols))
	for i, c := range cols {
		upper[i] = strings.ToUpper(c)
	}
	var out []Row
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, nil, err
		}
		r := make(Row, len(cols))
		for i, name := range upper {
			r[name] = normalizeDriverValue(vals[i])
		}
		out = append(out, r)
	}
	return out, upper, rows.Err()
}

// knownGap is one documented engine limitation the harness is allowed to
// classify as DECLINE instead of MISMATCH (RFC-182 §4). Membership is
// deliberately expensive to obtain: every entry needs a LIVE PIN that fails
// the day the limitation lifts, and a TODO.md entry — otherwise this list
// becomes the suppression hole §4 warns about.
type knownGap struct {
	name    string
	pin     string // the live test that goes red when the gap closes
	matches func(sqlText string, err error) bool
}

// knownGaps is the whole ledger. Keep it SHORT and each entry NARROW: an
// over-broad matcher silently converts real findings into declines.
var knownGaps = []knownGap{
	{
		name: "nested-IN over an intersection extracts InJoin(<nil>) (RFC-167 shell gap)",
		pin:  "TestNestedIn_OverIntersection_GatePin",
		matches: func(sqlText string, err error) bool {
			// Narrow on BOTH axes: the exact plan-invariant signature for the
			// IN-join node, AND the specific query shape (two or more IN
			// predicates). A plan-invariant failure on any other shape — or a
			// different error on this shape — stays a finding.
			return strings.Contains(err.Error(), "plan-invariant") &&
				strings.Contains(err.Error(), "RecordQueryInJoinPlan") &&
				strings.Count(strings.ToUpper(sqlText), " IN (") >= 2
		},
	},
}

// matchKnownGap returns the ledger entry covering this failure, or nil.
func matchKnownGap(sqlText string, err error) *knownGap {
	for i := range knownGaps {
		if knownGaps[i].matches(sqlText, err) {
			return &knownGaps[i]
		}
	}
	return nil
}

// isInfraError classifies the KNOWN transport/context failure signatures as
// infrastructure. The set is deliberately narrow: an unrecognized error is a
// finding, so a genuine engine defect can never hide behind the INFRA class.
func isInfraError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if errors.Is(err, driver.ErrBadConn) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

func normalizeDriverValue(v any) any {
	switch t := v.(type) {
	case []byte:
		return string(t)
	default:
		return v
	}
}

// diffRows compares engine output against the oracle's FULL result multiset
// (the oracle never applies LIMIT):
//   - no LIMIT: multiset equality; with ORDER BY additionally the exact
//     sequence (generated sort keys are ID-suffixed total orders).
//   - LIMIT k (RFC-182 §3): the correct answer is a SET of valid results —
//     the engine must return exactly min(k, |oracle|) rows; with ORDER BY
//     they must equal the oracle's prefix of that length (total order makes
//     the tie-straddle rule degenerate to an exact prefix); without ORDER BY
//     any sub-multiset of the oracle of that size is valid.
//
// Returns "" when valid, else a human-readable divergence description.
func diffRows(engine []Row, cols []string, oracle []Row, q Query) string {
	keyOf := func(r Row) string {
		parts := make([]string, 0, len(cols))
		for _, c := range cols {
			parts = append(parts, fmt.Sprintf("%s=%v(%T)", c, r[c], r[c]))
		}
		return strings.Join(parts, "|")
	}

	// OFFSET is generated only alongside a total ORDER BY, so the skipped
	// prefix is well-defined: drop it from the oracle before comparing.
	if q.Offset > 0 {
		if q.Offset >= len(oracle) {
			oracle = nil
		} else {
			oracle = oracle[q.Offset:]
		}
	}

	want := len(oracle)
	if q.Limit > 0 && q.Limit < want {
		want = q.Limit
	}
	if len(engine) != want {
		return fmt.Sprintf("row count: engine %d, oracle expects %d (|M|=%d, limit=%d)",
			len(engine), want, len(oracle), q.Limit)
	}

	if len(q.OrderBy) > 0 {
		// Ordered: exact prefix of the oracle's total order.
		for i := range engine {
			if keyOf(engine[i]) != keyOf(oracle[i]) {
				return fmt.Sprintf("ordered row %d differs: engine %s, oracle %s", i, keyOf(engine[i]), keyOf(oracle[i]))
			}
		}
		return ""
	}

	e := make([]string, 0, len(engine))
	o := make([]string, 0, len(oracle))
	for i := range engine {
		e = append(e, keyOf(engine[i]))
	}
	for i := range oracle {
		o = append(o, keyOf(oracle[i]))
	}
	sort.Strings(e)
	sort.Strings(o)

	if q.Limit > 0 && len(oracle) > want {
		// Sub-multiset membership: every engine row must be consumable from
		// the oracle multiset.
		oi := 0
		for _, ek := range e {
			for oi < len(o) && o[oi] < ek {
				oi++
			}
			if oi >= len(o) || o[oi] != ek {
				return fmt.Sprintf("LIMIT sub-multiset violation: engine row %s not in oracle result", ek)
			}
			oi++
		}
		return ""
	}

	for i := range e {
		if e[i] != o[i] {
			return fmt.Sprintf("multiset differs at sorted position %d: engine %s, oracle %s", i, e[i], o[i])
		}
	}
	return ""
}

// classifyPlan buckets a typed plan tree into RFC-182 §5 plan families.
func classifyPlan(p plans.RecordQueryPlan) []string {
	fams := map[string]bool{}
	var sawIndex, sawFilter bool
	var walk func(plans.RecordQueryPlan)
	walk = func(p plans.RecordQueryPlan) {
		if p == nil {
			return
		}
		switch t := p.(type) {
		case *plans.RecordQueryScanPlan:
			fams["FullScan"] = true
		case *plans.RecordQueryIndexPlan:
			sawIndex = true
			if t.IsCovering() {
				fams["Covering"] = true
			} else {
				fams["IndexScan"] = true
			}
		case *plans.RecordQueryIntersectionPlan:
			fams["Intersection"] = true
		case *plans.RecordQueryMultiIntersectionOnValuesPlan:
			fams["Intersection"] = true
		case *plans.RecordQueryUnionPlan, *plans.RecordQueryUnorderedUnionPlan,
			*plans.RecordQueryMergeSortUnionPlan, *plans.RecordQueryInUnionPlan:
			fams["Union"] = true
		case *plans.RecordQueryPredicatesFilterPlan:
			sawFilter = true
		case *plans.RecordQueryInJoinPlan:
			fams["InJoin"] = true
		case *plans.RecordQueryInMemorySortPlan:
			fams["Sort"] = true
		}
		for _, child := range p.GetChildren() {
			walk(child)
		}
	}
	walk(p)
	if sawIndex && sawFilter {
		fams["IndexScan+residual"] = true
	}
	out := make([]string, 0, len(fams))
	for f := range fams {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}
