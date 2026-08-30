package factory_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/relational/conformance/rowdiff"
)

// equivProgressEvery is how often the equivalence sweep reports, in SEEDS.
const equivProgressEvery = 100

// The predicate-equivalence oracle: rewrite a WHERE into a form SQL says is
// identical, and require identical rows.
//
// WHY THIS EXISTS — the blind spot the other hunts share. Oracle M evaluates
// leaves through the ENGINE's own predicates.NewLiteralComparison(...).Eval
// (RFC-182 §3 makes the planner the component under test), so a defect in
// scalar comparison is invisible to it: engine and oracle are wrong together
// and agree. Plan diversity compares two plans, and two plans built from the
// same wrong comparison also agree. The committed corpus does catch this class,
// because its rows are FROZEN rather than re-derived — but only for shapes
// already committed, and only until someone re-blesses.
//
// The rewrites below are chosen because each crosses a DIFFERENT planner path
// while denoting the same predicate, so agreement is a real claim even when
// both sides share leaf evaluation:
//
//   - BETWEEN vs `>= AND <=`. BETWEEN can compile to one sargable index range;
//     the conjunction generally does not. Same rows required.
//   - IN vs an OR-chain of equalities. IN goes through
//     InComparisonToExplodeRule — an explode plus a join, or a collapse to
//     equality — while the OR-chain does not go near it. This is the rule that
//     carried the one real engine defect found this session, so the equivalence
//     is aimed squarely at it.
//   - Double negation. `NOT (NOT p)` is p under Kleene logic (NOT UNKNOWN is
//     UNKNOWN), and it exercises the predicate normaliser without changing the
//     answer.
//
// NULL SAFETY is why the set is this small. `NOT (x IN (a,b))` is NOT
// equivalent to `x <> a AND x <> b` once a NULL is in play, and `x = y` is not
// the negation of `x <> y` when either side is NULL. Those rewrites are
// deliberately absent: an oracle that is wrong about three-valued logic reports
// the engine as broken and is worse than no oracle at all.
type rewrite struct {
	name string
	// apply returns the rewritten tree and whether it applied at all.
	apply func(*rowdiff.BoolNode) (*rowdiff.BoolNode, bool)
	// mustRender are substrings the RENDERED predicate must contain for the
	// rewrite to be trusted. Empty means no check.
	//
	// This exists because building the tree correctly is NOT the same as
	// emitting the intended SQL, and the gap is silent. eq-to-degenerate-range
	// sets IsBetween on the leaf AND leaves Op as >=, expecting the renderer to
	// honour IsBetween. It does — for a PLAIN leaf. For a leaf carrying a CAST
	// the renderer takes a different path and emits the Op instead, so
	//
	//	CAST(b AS STRING) = '0'
	//
	// became
	//
	//	CAST(b AS STRING) >= '0'
	//
	// which is strictly WEAKER, matched more rows, and was reported as an
	// engine cardinality bug. The tree was right; the SQL was not.
	//
	// Verifying the RENDERING rather than denylisting leaf kinds is deliberate:
	// a denylist of "cast, function, arithmetic…" goes stale the moment the
	// generator grows a new leaf kind, and it fails OPEN when it does. Asking
	// the emitted string whether it is the shape intended cannot go stale.
	mustRender []string
}

// renderedAsIntended reports whether the rewrite's emitted SQL carries the
// shape it declared. A rewrite that builds the right tree and renders another
// form is not the predicate under test.
func renderedAsIntended(rw rewrite, rendered string) bool {
	for _, want := range rw.mustRender {
		if !strings.Contains(rendered, want) {
			return false
		}
	}
	return true
}

func cloneNode(n *rowdiff.BoolNode) *rowdiff.BoolNode {
	if n == nil {
		return nil
	}
	out := &rowdiff.BoolNode{And: n.And, Not: n.Not}
	if n.Leaf != nil {
		leaf := *n.Leaf
		out.Leaf = &leaf
	}
	for _, k := range n.Kids {
		out.Kids = append(out.Kids, cloneNode(k))
	}
	return out
}

// walk applies f to every node, reporting whether any application changed
// something.
func walk(n *rowdiff.BoolNode, f func(*rowdiff.BoolNode) (*rowdiff.BoolNode, bool)) (*rowdiff.BoolNode, bool) {
	if n == nil {
		return nil, false
	}
	if out, ok := f(n); ok {
		return out, true
	}
	changed := false
	for i, k := range n.Kids {
		if nk, ok := walk(k, f); ok {
			n.Kids[i] = nk
			changed = true
		}
	}
	return n, changed
}

var rewrites = []rewrite{
	{
		name:       "between-to-conjunction",
		mustRender: []string{">="},
		apply: func(root *rowdiff.BoolNode) (*rowdiff.BoolNode, bool) {
			return walk(cloneNode(root), func(n *rowdiff.BoolNode) (*rowdiff.BoolNode, bool) {
				if n.Leaf == nil || !n.Leaf.IsBetween || n.Not {
					return nil, false
				}
				// NEGATION LIVES ON THE LEAF, not only on the BoolNode.
				// rowdiff.Pred carries its own Negated flag (renderBool wraps
				// such a leaf in `NOT (…)`), so `lo := *n.Leaf` copies that flag
				// into BOTH halves. The first version of this rewrite did
				// exactly that and turned
				//
				//	NOT (b BETWEEN 7 AND 10)     ==  b < 7 OR b > 10
				//
				// into
				//
				//	NOT (b >= 7) AND NOT (b <= 10)  ==  b < 7 AND b > 10
				//
				// which is a CONTRADICTION. It reported 4 findings against a
				// correct engine before this comment existed — the oracle
				// accusing the engine of its own De Morgan error.
				//
				// Handled rather than skipped, because a negated BETWEEN is
				// worth covering: De Morgan gives an OR of the two negated
				// halves, and the negation moves onto each half exactly once.
				neg := n.Leaf.Negated
				lo := *n.Leaf
				lo.IsBetween = false
				lo.Op = predicates.ComparisonGreaterThanEq
				lo.BetweenHi = nil
				lo.Negated = neg
				hi := *n.Leaf
				hi.IsBetween = false
				hi.Op = predicates.ComparisonLessThanOrEq
				hi.Lit = n.Leaf.BetweenHi
				hi.BetweenHi = nil
				hi.Negated = neg
				return &rowdiff.BoolNode{
					// NOT(a AND b) is NOT(a) OR NOT(b): negating the leaves
					// requires flipping the connective too.
					And:  !neg,
					Kids: []*rowdiff.BoolNode{{Leaf: &lo}, {Leaf: &hi}},
				}, true
			})
		},
	},
	{
		name: "in-to-or-chain",
		apply: func(root *rowdiff.BoolNode) (*rowdiff.BoolNode, bool) {
			return walk(cloneNode(root), func(n *rowdiff.BoolNode) (*rowdiff.BoolNode, bool) {
				if n.Leaf == nil || n.Not || n.Leaf.Op != predicates.ComparisonIn || len(n.Leaf.InList) == 0 {
					return nil, false
				}
				// A NULL in the list makes `x IN (…)` UNKNOWN-on-no-match while
				// the OR-chain is UNKNOWN-on-that-arm — the same answer, but the
				// reasoning is subtle enough that the case is skipped rather
				// than relied on.
				for _, v := range n.Leaf.InList {
					if v == nil {
						return nil, false
					}
				}
				// The same leaf-Negated trap as the BETWEEN rewrite above, and
				// it was latent here too: copying the leaf carries Negated onto
				// every equality, so `x NOT IN (a,b)` would become
				// `NOT(x=a) OR NOT(x=b)` — true for any x that differs from
				// EITHER value, instead of `NOT(x=a) AND NOT(x=b)`.
				//
				// De Morgan again: negating the arms flips the connective. The
				// NULL-free guard above is what makes `x NOT IN (…)` equal to
				// the conjunction of inequalities at all.
				neg := n.Leaf.Negated
				var kids []*rowdiff.BoolNode
				for _, v := range n.Leaf.InList {
					eq := *n.Leaf
					eq.Op = predicates.ComparisonEquals
					eq.Lit = v
					eq.InList = nil
					eq.Negated = neg
					kids = append(kids, &rowdiff.BoolNode{Leaf: &eq})
				}
				return &rowdiff.BoolNode{And: neg, Kids: kids}, true
			})
		},
	},
	{
		// Commutativity. AND and OR commute in Kleene three-valued logic
		// exactly as they do in two-valued logic — UNKNOWN is absorbed
		// symmetrically — so this is sound with no NULL guard, which is why it
		// is worth having: it is the one rewrite whose soundness needs no
		// argument at all.
		//
		// What it exercises is conjunct ORDER, which the planner is free to use
		// when choosing which predicate becomes a sargable index range and
		// which stays a residual filter. Two orderings that pick different
		// access paths must still return the same rows.
		name: "commute-connectives",
		apply: func(root *rowdiff.BoolNode) (*rowdiff.BoolNode, bool) {
			return walk(cloneNode(root), func(n *rowdiff.BoolNode) (*rowdiff.BoolNode, bool) {
				if len(n.Kids) < 2 {
					return nil, false
				}
				out := &rowdiff.BoolNode{And: n.And, Not: n.Not}
				for i := len(n.Kids) - 1; i >= 0; i-- {
					out.Kids = append(out.Kids, n.Kids[i])
				}
				return out, true
			})
		},
	},
	{
		// `x = c` and `x BETWEEN c AND c` denote the same set, including for a
		// NULL x (both UNKNOWN). They reach it differently: equality is a point
		// probe, BETWEEN builds a degenerate range whose endpoints coincide, so
		// this exercises range construction at its boundary — the case where lo
		// and hi are equal and both inclusive.
		//
		// Restricted to equality on a LITERAL. A column-vs-column equality
		// (RhsCol) has no literal endpoint to duplicate, and IS NULL is not an
		// ordering comparison at all.
		//
		// BOOLEANS ARE EXCLUDED, and the engine taught me that rather than the
		// other way round. `f = TRUE` is legal; `f BETWEEN TRUE AND TRUE` is
		// not, because BETWEEN is an ORDERING comparison and this engine does
		// not order booleans — it answers `42804: The operands of a comparison
		// operator are not compatible`, which is correct. The first version of
		// this rewrite produced 72 findings in 40 seeds, every one of them the
		// oracle emitting SQL the engine was right to reject.
		//
		// The general shape of the mistake: `=` is defined on every comparable
		// type, BETWEEN only on ordered ones, so "equality and a degenerate
		// range denote the same set" holds for the VALUES and not for the
		// TYPES. An equivalence has to be sound over the type system too.
		name:       "eq-to-degenerate-range",
		mustRender: []string{"BETWEEN"},
		apply: func(root *rowdiff.BoolNode) (*rowdiff.BoolNode, bool) {
			return walk(cloneNode(root), func(n *rowdiff.BoolNode) (*rowdiff.BoolNode, bool) {
				if n.Leaf == nil || n.Not || n.Leaf.IsBetween ||
					n.Leaf.Op != predicates.ComparisonEquals ||
					n.Leaf.RhsCol != "" || n.Leaf.Lit == nil || n.Leaf.Negated {
					return nil, false
				}
				if _, isBool := n.Leaf.Lit.(bool); isBool {
					return nil, false
				}
				b := *n.Leaf
				b.IsBetween = true
				b.BetweenHi = n.Leaf.Lit
				b.Op = predicates.ComparisonGreaterThanEq
				return &rowdiff.BoolNode{Leaf: &b}, true
			})
		},
	},
	{
		name: "double-negation",
		apply: func(root *rowdiff.BoolNode) (*rowdiff.BoolNode, bool) {
			c := cloneNode(root)
			// NOT(NOT(p)) is p under Kleene logic, including for UNKNOWN.
			return &rowdiff.BoolNode{
				Not: true, And: true,
				Kids: []*rowdiff.BoolNode{{Not: true, And: true, Kids: []*rowdiff.BoolNode{c}}},
			}, true
		},
	},
}

// TestFDB_PredicateEquivalenceHunt runs each query twice — once as generated,
// once with its WHERE rewritten into an equivalent form — and requires the same
// rows.
func TestFDB_PredicateEquivalenceHunt(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	start := uint64(envInt("EQUIV_SEED_START", 1))
	count := uint64(envInt("EQUIV_SEEDS", 25))
	workers := envInt("EQUIV_WORKERS", 4)

	began := time.Now()
	deadline := began.Add(huntBudget())

	var (
		mu         sync.Mutex
		findings   []string
		compared   int
		applied    = map[string]int{}
		skipped    int
		walked     int
		done       int
		unrendered int
	)

	seeds := make(chan uint64)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			// DRAIN on every exit path -- see the same guard in
			// excluded_shape_hunt_fdb_test.go. A worker returning early on a
			// setup failure stops reading `seeds`; if all of them do, the
			// producer blocks until the Go test timeout kills the run.
			defer func() {
				for range seeds {
				}
				wg.Done()
			}()
			ctx := context.Background()
			setupDB, err := sql.Open("fdbsql", "fdbsql:///__SYS?cluster_file="+clusterFilePath+"&schema=CATALOG")
			if err != nil {
				t.Errorf("worker %d: open sys: %v", w, err)
				return
			}
			defer setupDB.Close()
			dbPath := fmt.Sprintf("/EQUIV_%d", w)
			if _, err := setupDB.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
				t.Errorf("worker %d: create database: %v", w, err)
				return
			}
			defer setupDB.ExecContext(ctx, "DROP DATABASE "+dbPath) //nolint:errcheck

			for seed := range seeds {
				r := equivSeed(ctx, t, setupDB, dbPath, w, seed)
				mu.Lock()
				findings = append(findings, r.findings...)
				compared += r.compared
				skipped += r.skipped
				unrendered += r.unrendered
				for k, v := range r.applied {
					applied[k] += v
				}
				// Periodic progress, for the third harness that needed it.
				// The budget already guarantees a clean end with a full
				// summary, so this is not load-bearing the way the budget is —
				// but without it a long run is unwatchable, and "unwatchable"
				// is how the first two sweeps of this session burned three
				// hours before anyone could tell they were behind.
				done++
				if done%equivProgressEvery == 0 {
					el := time.Since(began)
					t.Logf("EQUIV progress seeds=%d compared=%d findings=%d elapsed=%s rate=%.2f seeds/s\n",
						done, compared, len(findings), el.Round(time.Second), float64(done)/el.Seconds())
				}
				mu.Unlock()
			}
		}(w)
	}
	for s := start; s < start+count; s++ {
		if time.Now().After(deadline) {
			t.Logf("EQUIV BUDGET EXHAUSTED: walked %d of %d seeds. NORMAL end.\n", walked, count)
			break
		}
		seeds <- s
		walked++
	}
	close(seeds)
	wg.Wait()

	t.Logf("EQUIV seeds=%d..%d walked=%d compared=%d skipped=%d unrendered=%d findings=%d elapsed=%s\n",
		start, start+count-1, walked, compared, skipped, unrendered, len(findings),
		time.Since(began).Round(time.Second))
	var keys []string
	for k := range applied {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("EQUIV   rewrite %-24s applied %d\n", k, applied[k])
	}

	// Every rewrite must have APPLIED at least once, or its silence says
	// nothing. A rewrite that never fires is indistinguishable in the summary
	// from one that fired and always agreed.
	if compared == 0 {
		t.Fatal("EQUIV VACUOUS: no query pair was compared")
	}
	for _, r := range rewrites {
		if applied[r.name] == 0 {
			t.Errorf("EQUIV VACUOUS: rewrite %q never applied — its agreement is vacuous", r.name)
		}
	}
	for _, f := range findings {
		t.Log("EQUIV FINDING " + f)
	}
	if len(findings) > 0 {
		t.Fatalf("EQUIV: %d predicate-equivalence findings", len(findings))
	}
}

type equivResult struct {
	findings          []string
	compared, skipped int
	unrendered        int
	applied           map[string]int
}

func equivSeed(ctx context.Context, t *testing.T, setupDB *sql.DB, dbPath string, w int, seed uint64) equivResult {
	res := equivResult{applied: map[string]int{}}
	c := rowdiff.Generate(seed)
	if len(c.Rows) > 24 {
		c.Rows = c.Rows[:24]
	}
	schema := fmt.Sprintf("eq_%d_%d", w, seed)
	tmpl := schema + "t"
	ddl := c.DDL()
	for _, stmt := range []string{
		fmt.Sprintf("CREATE SCHEMA TEMPLATE %s %s", tmpl, ddl),
		fmt.Sprintf("CREATE SCHEMA %s/%s WITH TEMPLATE %s", dbPath, schema, tmpl),
	} {
		if _, err := setupDB.ExecContext(ctx, stmt); err != nil {
			// Setup failures were SILENT here: equivSeed took a *testing.T
			// and never used it, so a seed whose schema never got created
			// returned an empty result and vanished into the totals. A hunt
			// where every seed failed setup would have reported
			// `compared=0 findings=0` and passed its own vacuity floor only
			// by luck. huntSeed in the plan-diversity harness reports these;
			// this one did not.
			t.Errorf("seed %d: setup %q: %v", seed, stmt, err)
			return res
		}
	}
	defer setupDB.ExecContext(ctx, fmt.Sprintf("DROP SCHEMA %s/%s", dbPath, schema)) //nolint:errcheck
	defer setupDB.ExecContext(ctx, fmt.Sprintf("DROP SCHEMA TEMPLATE %s", tmpl))     //nolint:errcheck

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=%s", dbPath, clusterFilePath, schema))
	if err != nil {
		t.Errorf("seed %d: open: %v", seed, err)
		return res
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Errorf("seed %d: conn: %v", seed, err)
		return res
	}
	defer conn.Close() //nolint:errcheck
	if _, err := conn.ExecContext(ctx, c.InsertSQL()); err != nil {
		t.Errorf("seed %d: insert: %v", seed, err)
		return res
	}

	for _, q := range c.Queries {
		if q.Where == nil || q.Union != nil || q.Derived != nil {
			// The WHERE override does not reach a union's or derived table's
			// own predicate, so a rewrite there would compare a query against
			// itself and agree trivially.
			continue
		}
		for _, proj := range c.ProjectionsFor(q) {
			baseSQL := c.SQL(q, proj)
			baseRows, err := rowsOf(ctx, conn, baseSQL)
			if err != nil || len(baseRows) == 0 {
				res.skipped++
				continue
			}
			for _, rw := range rewrites {
				nn, ok := rw.apply(q.Where)
				if !ok {
					continue
				}
				sqlText := rowdiff.PredicateSQL(nn)
				// The rewrite must have EMITTED the shape it claims, not merely
				// built a tree for it. A rewrite whose rendering silently took
				// another path is not the predicate under test, and comparing
				// it accuses the engine of the oracle's own error.
				if !renderedAsIntended(rw, sqlText) {
					res.unrendered++
					continue
				}
				alt := q
				override := "(" + sqlText + ")"
				alt.WhereOverride = &override
				altSQL := c.SQL(alt, proj)
				if altSQL == baseSQL {
					continue
				}
				altRows, err := rowsOf(ctx, conn, altSQL)
				if err != nil {
					res.findings = append(res.findings, fmt.Sprintf(
						"seed=%d rewrite=%s: the REWRITTEN form errored on a query the original answered\n"+
							"    err: %v\n    original:  %s\n    rewritten: %s\n    DDL: %s",
						seed, rw.name, err, baseSQL, altSQL, ddl))
					continue
				}
				res.compared++
				res.applied[rw.name]++
				if d := compareResults(q, baseRows, altRows); d != "" {
					res.findings = append(res.findings, fmt.Sprintf(
						"seed=%d rewrite=%s: %s\n    original:  %s\n    rewritten: %s\n    DDL: %s",
						seed, rw.name, d, baseSQL, altSQL, ddl))
				}
			}
		}
	}
	return res
}
