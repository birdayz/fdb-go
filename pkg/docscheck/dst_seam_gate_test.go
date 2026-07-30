package docscheck

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The DST seam (RFC-199 Tier 0) claims every persisted-byte nondeterminism site in the record
// and relational layers draws through *dst.Env — the clock, the randomness, the fault chooser —
// so a seeded run replays byte-for-byte. That claim was defended by a hand-written list of four
// sites in dst_nonce_seam_test.go. A hand-written list cannot defend it: it says nothing about
// the site someone adds tomorrow, and the one site that had silently regressed
// (spfresh_build.go's builder token) was not on it.
//
// This gate inverts the burden. Every raw `time.Now()` / `crypto/rand` / `math/rand` call in a
// non-test file of the record or relational layer must appear on seamAllowlist with a written
// reason, or the build fails. Adding a new one forces exactly one question to be answered out
// loud: do these bytes get PERSISTED? If yes, route it through the env. If no — a latency timer,
// an in-memory cache stamp, a rate limiter — say so on the list.
//
// It is a WHITELIST, not a heuristic: the gate cannot tell a persisted timestamp from a latency
// probe, and should not try. A human decides once, in writing, and the decision survives.

// seamSite is one raw nondeterminism call site, keyed by file and enclosing function — stable
// under edits that move lines around, unlike a line number.
type seamSite struct {
	file string // repo-relative
	fn   string // enclosing function or method, "init" for package-level initialisers
	call string // the call as written, e.g. "time.Now"
}

func (s seamSite) String() string { return fmt.Sprintf("%s:%s: %s", s.file, s.fn, s.call) }

// seamAllowlist enumerates the raw nondeterminism sites that are DELIBERATELY not seamed, each
// with the reason it does not affect persisted bytes. Adding an entry is a decision; removing
// one is a cleanup. An entry that no longer matches a real site fails the gate too, so the list
// cannot rot into fiction.
var seamAllowlist = map[string]string{
	// ---- (A) Latency and duration measurement. These values feed a StoreTimer or a log line
	// and are never written to FDB. Seaming them would make a simulation report zero elapsed
	// time for every operation, which is worse than useless for the metrics.
	"pkg/recordlayer/database.go:CommitWithVersionstamp: time.Now":        "commit-latency metric",
	"pkg/recordlayer/database.go:GetReadVersion: time.Now":                "GRV-latency metric",
	"pkg/recordlayer/index_scan.go:ScanIndex: time.Now":                   "scan-latency metric",
	"pkg/recordlayer/online_indexer.go:BuildIndex: time.Now":              "build-duration metric",
	"pkg/recordlayer/online_indexer.go:shouldLogBuildProgress: time.Now":  "progress-log throttle",
	"pkg/recordlayer/spfresh_write.go:spfreshInsert: time.Now":            "insert-latency metric",
	"pkg/recordlayer/store.go:saveRecordInternal: time.Now":               "save-latency metric",
	"pkg/recordlayer/store_builder.go:Create: time.Now":                   "store-create-latency metric",
	"pkg/recordlayer/store_builder.go:Open: time.Now":                     "store-open-latency metric",
	"pkg/recordlayer/store_builder.go:RebuildIndex: time.Now":             "rebuild-latency metric",
	"pkg/relational/core/embedded/plan_logging.go:beginPlanLog: time.Now": "plan-log timestamp; log output, not persisted rows",
	"pkg/recordlayer/spfresh_query.go:search: time.Now":                   "search-latency metric",
	"pkg/recordlayer/store.go:DeleteRecord: time.Now":                     "delete-latency metric",
	"pkg/recordlayer/store.go:LoadRecord: time.Now":                       "load-latency metric",
	"pkg/recordlayer/store.go:ScanRecords: time.Now":                      "scan-latency metric",
	"pkg/recordlayer/store_builder.go:CreateOrOpen: time.Now":             "store-open-latency metric",

	// ---- (B) Scheduling and in-memory bookkeeping. A simulated clock here would freeze LRU
	// eviction or a rate limiter without changing any byte on disk, so the wall clock is the
	// correct source.
	"pkg/recordlayer/indexing_throttle.go:waitForRateLimit: time.Now":     "inter-transaction rate limiting, wall-clock by definition",
	"pkg/recordlayer/store_state_cache.go:addToCache: time.Now":           "in-memory LRU access stamp",
	"pkg/recordlayer/store_state_cache.go:getIfPresent: time.Now":         "in-memory LRU access stamp",
	"pkg/recordlayer/runner.go:calculateDelay: rand.Float64":              "retry-backoff jitter; affects WHEN a retry happens, never what it writes",
	"pkg/recordlayer/indexing_mutual.go:computeFragments: rand.Int63":     "seeds the fragment-visit ORDER for a mutual index build; the built index is identical whatever order the fragments are visited in",
	"pkg/recordlayer/scan_limiter_state.go:NewScanLimiterState: time.Now": "scan time budget, wall-clock by definition",

	// ---- (C) The production arm of a seamed site. The seam is present at each of these; the
	// raw call is the branch taken when no simulation env is installed, and it is what makes
	// production byte-identical to the pre-seam code. Removing the raw call here does not
	// improve determinism — it changes production.
	"pkg/recordlayer/indexing_heartbeat.go:newIndexerID: uuid.New":                 "nil-env arm; the sim arm draws the UUID from env.Read",
	"pkg/recordlayer/spfresh_build.go:newSPFreshBuilder: rand.Uint64":              "nil-env arm; this site used math/rand pre-seam, so the crypto-backed nil default would change production bytes",
	"pkg/recordlayer/spfresh_rebalancer.go:newSPFreshProcessNonce: time.Now":       "error-path fallback only; the normal path is env.Read",
	"pkg/recordlayer/query/plan/cascades/values/values.go:statementTime: time.Now": "nil-clock arm; a StatementClock supplies the value when one is installed",
	"pkg/relational/core/session/session.go:now: time.Now":                         "nil-clock arm; s.Clock supplies the value when one is installed",
	"pkg/relational/core/embedded/scalar_functions.go:statementNow: time.Now":      "nil-session arm; Session.StatementNow supplies the value when a session is in flight",

	// ---- (D) Test and development harnesses. Not on any persisted-byte path.
	"pkg/recordlayer/chaos/concurrent.go:RunConcurrent: time.Now":                   "chaos driver's own scheduling",
	"pkg/relational/conformance/plandiff/go_runner.go:runEphemeral: uuid.NewString": "ephemeral database name for a cross-engine plan-diff run",

	// ---- (E) KNOWN-UNSEAMED PERSISTED SITE. This one does reach persisted rows and is the
	// standing exception to the bit-exact-replay claim; it is named as such in the RFC-199
	// coverage ledger. Seaming it is real work, not a formality: 23 call sites.
	"pkg/recordlayer/spfresh_util.go:spfreshNowMs: time.Now": "SPFresh task/lease timestamps DO reach persisted rows; unseamed, and named as an exception in the RFC-199 coverage ledger",
}

// seamScannedTrees are the packages whose non-test sources produce persisted bytes.
var seamScannedTrees = []string{"pkg/recordlayer/", "pkg/relational/"}

// seamBannedCalls are the raw nondeterminism sources. Selector-expression spellings only: a
// bare `Now()` or `Read()` on some other receiver is not one of these.
var seamBannedCalls = map[string]bool{
	"time.Now":        true,
	"rand.Read":       true,
	"rand.Int":        true,
	"rand.Intn":       true,
	"rand.Int63":      true,
	"rand.Uint64":     true,
	"rand.Uint32":     true,
	"rand.Float64":    true,
	"cryptorand.Read": true,
	"crand.Read":      true,
	"uuid.New":        true,
	"uuid.NewRandom":  true,
	"uuid.NewString":  true,
}

// TestDSTSeamGate fails on any raw nondeterminism call in the scanned trees that is not on
// seamAllowlist, and on any allowlist entry that no longer matches a real site.
func TestDSTSeamGate(t *testing.T) {
	t.Parallel()
	root := sourceTreeRoot(t)
	files := trackedGoFiles(t, root)

	found := map[string]seamSite{}
	fset := token.NewFileSet()
	for _, rel := range files {
		if !inSeamTrees(rel) || strings.HasSuffix(rel, "_test.go") {
			continue
		}
		abs := filepath.Join(root, rel)
		src, readErr := os.ReadFile(abs)
		if readErr != nil {
			continue
		}
		f, err := parser.ParseFile(fset, abs, src, parser.ParseComments)
		if err != nil {
			continue // an unparseable file is the comment gate's problem, not this one
		}
		if isGeneratedFile(src, f) {
			continue
		}
		for _, site := range seamSitesIn(rel, f) {
			found[site.String()] = site
		}
	}

	var unlisted []string
	for key := range found {
		if _, ok := seamAllowlist[key]; !ok {
			unlisted = append(unlisted, key)
		}
	}
	sort.Strings(unlisted)
	for _, u := range unlisted {
		t.Errorf("unseamed nondeterminism at %s.\n"+
			"    If these bytes are PERSISTED, draw them through the DST env seam "+
			"(rtx.Env().Now() / env.Read) so a seeded run replays.\n"+
			"    If they are not (a latency metric, an in-memory cache stamp, a rate limiter), "+
			"add the site to seamAllowlist in this file with that reason.", u)
	}

	var stale []string
	for key := range seamAllowlist {
		if _, ok := found[key]; !ok {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)
	for _, s := range stale {
		t.Errorf("seamAllowlist entry %q no longer matches any site: delete it. An allowlist "+
			"that keeps entries for code that is gone stops describing the code.", s)
	}
}

func inSeamTrees(rel string) bool {
	for _, tree := range seamScannedTrees {
		if strings.HasPrefix(rel, tree) {
			return true
		}
	}
	return false
}

// seamSitesIn walks f and reports every banned call, attributed to its enclosing function.
func seamSitesIn(rel string, f *ast.File) []seamSite {
	var out []seamSite
	var fnStack []string
	enclosing := func() string {
		if len(fnStack) == 0 {
			return "init"
		}
		return fnStack[len(fnStack)-1]
	}
	var visit func(ast.Node) bool
	visit = func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.FuncDecl:
			fnStack = append(fnStack, node.Name.Name)
			ast.Inspect(node.Body, visit)
			fnStack = fnStack[:len(fnStack)-1]
			return false
		case *ast.CallExpr:
			if sel, ok := node.Fun.(*ast.SelectorExpr); ok {
				if pkg, ok := sel.X.(*ast.Ident); ok {
					name := pkg.Name + "." + sel.Sel.Name
					if seamBannedCalls[name] {
						out = append(out, seamSite{file: rel, fn: enclosing(), call: name})
					}
				}
			}
		}
		return true
	}
	ast.Inspect(f, visit)
	return out
}
