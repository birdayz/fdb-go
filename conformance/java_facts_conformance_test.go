//go:build bazelrunfiles

package conformance_test

// ANSI Java?-fact verification (RFC-165 follow-up). The roster's `Java?` column
// in ansi_roster.go is HAND-AUTHORED (a fact about the frozen fdb-relational
// 4.12.11.0 reference). This lane proves it isn't a stale assertion by running
// each ANSI-tagged query against the LIVE Java conformance server and checking
// Java's real supported/rejected behaviour against the roster fact.
//
// The NULLIF episode is why this exists: the `*_java.yaml` testdata files falsely
// implied Java supports NULLIF, while the real server rejects it. A hand-authored
// `Java?` fact that contradicts the real server is the bug this catches.
//
// ASSERTION (sound, no false positives): a feature the roster marks `Java=None`
// MUST be REJECTED by Java. If Java ACCEPTS the tagged query, the roster is wrong
// — the feature is Java-supported, so it's a Java-yes row mis-recorded as a shared
// gap / Go-only extension (a HIDDEN Java-vs-Go divergence, RFC-164 territory),
// never a shared gap. This direction can't false-positive: if Java accepts a
// query, Java supports it, full stop.
//
// The other direction (roster `Java=Full/Partial` but Java rejects) is REPORTED,
// not asserted — the raw corpus queries can trip Java-side query quirks (uppercased
// identifiers, multi-column ORDER BY, ORDER BY on an expression) unrelated to the
// tagged feature, so a Java rejection there needs a human to tell a quirk from a
// genuine roster overstatement. DML / non-query cases are skipped (RunWithSetup
// runs one row-returning query).

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/bazelbuild/rules_go/go/runfiles"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"fdb.dev/pkg/relational/conformance/plandiff"
	"fdb.dev/pkg/relational/conformance/yamsql"
)

func ansiCaseKey(c yamsql.AnsiTaggedCase) string {
	return fmt.Sprintf("%s|%s|%v", c.Scenario, c.FeatureID, c.Gap)
}

// loadAnsiCasesForJavaCheck reads the ANSI-tagged corpus from the yamsql testdata
// staged into runfiles. Called at TREE-CONSTRUCTION time so the It tree is built
// from the real case list. Returns nil if runfiles/testdata can't be resolved —
// the Describe then registers a single failing spec (the empty guard below) rather
// than silently contributing zero specs.
func loadAnsiCasesForJavaCheck() []yamsql.AnsiTaggedCase {
	r, err := runfiles.New()
	if err != nil {
		return nil
	}
	sentinel, err := r.Rlocation("_main/pkg/relational/conformance/yamsql/testdata/avg.yaml")
	if err != nil {
		return nil
	}
	cases, err := yamsql.AnsiTaggedCases(filepath.Dir(sentinel))
	if err != nil {
		return nil
	}
	return cases
}

var _ = Describe("ANSI Java?-fact verification", Ordered, ContinueOnFailure, func() {
	// Cases are loaded at TREE-CONSTRUCTION time (here, in the Describe body) — not
	// in BeforeAll. Ginkgo builds the whole It tree before any BeforeAll runs, so a
	// `for range` over BeforeAll-populated data would register ZERO specs. The Java
	// RESULTS are still precomputed in BeforeAll; only the case list (the It
	// generators) must exist at construction.
	cases := loadAnsiCasesForJavaCheck()

	var (
		ctx     context.Context
		pool    *JavaServerPool
		results map[string]plandiff.RunResult // key → Java result for each QUERY case
	)

	BeforeAll(func() {
		ctx = context.Background()
		clusterFile, err := sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())

		// Only row-returning queries run against Java (DML/non-query skipped).
		var work []yamsql.AnsiTaggedCase
		for _, c := range cases {
			if c.IsQuery {
				work = append(work, c)
			}
		}

		pool = NewJavaServerPool(a3PoolSize(), a3MaxInvocations())
		out := make([]plandiff.RunResult, len(work))
		if n := a3PoolSize(); len(work) > 0 {
			if n > len(work) {
				n = len(work)
			}
			runners := make([]plandiff.SetupRunner, n)
			borrowed := make([]*JavaInvoker, n)
			for i := range runners {
				srv, berr := pool.Borrow()
				Expect(berr).NotTo(HaveOccurred(), "borrow Java server %d/%d", i+1, n)
				borrowed[i] = srv
				runners[i] = plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), clusterFile).(plandiff.SetupRunner)
			}
			idxCh := make(chan int)
			var wg sync.WaitGroup
			for w := 0; w < n; w++ {
				wg.Add(1)
				go func(rn plandiff.SetupRunner, wid int) {
					defer wg.Done()
					for idx := range idxCh {
						it := work[idx]
						jr := rn.RunWithSetup(ctx, it.SchemaTemplate, it.Setup, it.Query)
						for attempt := 1; attempt <= maxConflictRetries && isTxConflict(jr.Err); attempt++ {
							time.Sleep(time.Duration(attempt)*40*time.Millisecond + time.Duration(wid)*11*time.Millisecond)
							jr = rn.RunWithSetup(ctx, it.SchemaTemplate, it.Setup, it.Query)
						}
						out[idx] = jr
					}
				}(runners[w], w)
			}
			for i := range work {
				idxCh <- i
			}
			close(idxCh)
			wg.Wait()
			for _, srv := range borrowed {
				pool.Return(srv)
			}
		}
		results = make(map[string]plandiff.RunResult, len(work))
		for i, it := range work {
			results[ansiCaseKey(it)] = out[i]
		}
	})

	AfterAll(func() {
		if pool != nil {
			pool.Shutdown()
		}
	})

	if len(cases) == 0 {
		It("loads ANSI-tagged cases from runfiles", func() {
			Fail("no ANSI-tagged cases loaded — yamsql testdata not staged into conformance_test runfiles (check the data dep)")
		})
		return
	}

	for _, c := range cases {
		c := c
		polarity := "pos"
		if c.Gap {
			polarity = "gap"
		}
		It(fmt.Sprintf("%s · %s (%s, roster Java=%s)", c.Scenario, c.FeatureID, polarity, c.Java), func() {
			if !c.IsQuery {
				Skip("non-query (DML) case — RunWithSetup runs one row-returning query")
			}
			res, ok := results[ansiCaseKey(c)]
			Expect(ok).To(BeTrue(), "missing precomputed Java result (harness bug)")
			javaAccepted := res.Err == nil

			if c.Java == yamsql.SupportNone {
				// SOUND hard assertion: Java must reject what the roster says it
				// doesn't support. Java accepting it ⇒ the roster fact is wrong.
				Expect(javaAccepted).To(BeFalse(),
					"roster says Java=None for %s but Java ACCEPTED %q — the roster is wrong: "+
						"Java supports this feature, so it is a Java-yes row (a hidden Java-vs-Go "+
						"divergence or an understated fact), NOT a shared gap. Fix ansi_roster.go.",
					c.FeatureID, c.Query)
				return
			}
			// Roster says Java supports it (Full/Partial). Report a Java rejection
			// for human review (likely a Java query quirk, possibly an overstatement)
			// — do not fail, since the noisy direction trips Java-side quirks.
			if !javaAccepted {
				By(fmt.Sprintf("NOTE roster Java=%s for %s but Java rejected %q (err=%v) — "+
					"verify this is a Java query quirk, not a roster overstatement",
					c.Java, c.FeatureID, c.Query, res.Err))
			}
		})
	}
})
