// sql-diff-stress is the RFC-182 generative row-soundness differential
// stress binary: seeded random (schema, data, query) cases executed through
// the full production SQL path against a real FDB testcontainer, rows
// diffed against the in-memory brute-force oracle (Oracle M).
//
// Reproduce a failing seed:
//
//	bazelisk run //cmd/sql-diff-stress -- -seeds 1 -seed-start NNN
//
// Exit codes (RFC-182 §4/§6 typed outcomes): 0 = all seeds OK,
// 1 = at least one MISMATCH (soundness finding), 2 = INFRA failure.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"time"

	"fdb.dev/pkg/relational/conformance/rowdiff"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"

	_ "fdb.dev/pkg/relational/sqldriver"
)

func main() {
	seeds := flag.Uint64("seeds", 100, "number of seeds to run")
	seedStart := flag.Uint64("seed-start", 1, "first seed number")
	templates := flag.Bool("templates", true, "also run the directed template seeds (per-family hard gate)")
	flag.Parse()

	os.Exit(run(*seeds, *seedStart, *templates))
}

func run(seeds, seedStart uint64, templates bool) int {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	container, err := foundationdbtc.Run(ctx, "")
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "INFRA: FDB container: %v\n", err)
		return 2
	}
	defer container.Terminate(context.Background()) //nolint:errcheck

	ctx = context.Background()
	clusterContent, err := container.ClusterFile(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "INFRA: cluster file: %v\n", err)
		return 2
	}
	tmp, err := os.CreateTemp("", "sql-diff-stress-*.cluster")
	if err != nil {
		fmt.Fprintf(os.Stderr, "INFRA: temp file: %v\n", err)
		return 2
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(clusterContent); err != nil {
		fmt.Fprintf(os.Stderr, "INFRA: write cluster file: %v\n", err)
		return 2
	}
	tmp.Close()
	clusterFile := tmp.Name()

	const dbPath = "/sqldiffstress"
	setup, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s", dbPath, clusterFile))
	if err != nil {
		fmt.Fprintf(os.Stderr, "INFRA: open: %v\n", err)
		return 2
	}
	defer setup.Close()
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		fmt.Fprintf(os.Stderr, "INFRA: create database: %v\n", err)
		return 2
	}

	start := time.Now()
	histogram := map[string]int{}
	var executed, mismatches, infra, declines int

	report := func(label string, res *rowdiff.SeedResult) {
		executed += res.Executed
		for fam, n := range res.Histogram {
			histogram[fam] += n
		}
		for _, pe := range res.PlanErrors {
			fmt.Fprintf(os.Stderr, "%s plan-error: %s\n", label, pe)
		}
		declines += len(res.Declines)
		for _, d := range res.Declines {
			fmt.Fprintf(os.Stderr, "%s DECLINE: %s\n", label, d)
		}
		// Report each dimension independently of Kind: a seed can hold
		// confirmed mismatches AND a later infra failure (mismatch takes
		// Kind precedence; the infra evidence must still surface and count).
		if res.InfraErr != nil {
			infra++
			fmt.Fprintf(os.Stderr, "%s INFRA: %v\n", label, res.InfraErr)
		}
		mismatches += len(res.Mismatches)
		for _, m := range res.Mismatches {
			fmt.Fprintf(os.Stderr, "%s", m.String())
		}
	}

	if templates {
		for _, tpl := range rowdiff.Templates() {
			if err := rowdiff.CheckTemplateFamily(tpl); err != nil {
				mismatches++
				fmt.Fprintf(os.Stderr, "template family gate: %v\n", err)
				continue
			}
			report("template "+tpl.Name,
				rowdiff.RunCase(ctx, setup, dbPath, clusterFile, tpl.Case, "tpl"+tpl.Name, 0))
		}
	}

	for seed := seedStart; seed < seedStart+seeds; seed++ {
		report(fmt.Sprintf("seed %d", seed),
			rowdiff.RunSeed(ctx, setup, dbPath, clusterFile, seed))
		if (seed-seedStart+1)%100 == 0 {
			fmt.Printf("... %d/%d seeds, %d comparisons, %d mismatches\n",
				seed-seedStart+1, seeds, executed, mismatches)
		}
	}

	elapsed := time.Since(start)
	fmt.Printf("sql-diff-stress: %d seeds (start %d), %d comparisons in %s (%.1f seeds/s)\n",
		seeds, seedStart, executed, elapsed.Round(time.Millisecond),
		float64(seeds)/elapsed.Seconds())
	fmt.Printf("plan-family histogram: %v\n", histogram)
	fmt.Printf("outcomes: %d mismatches, %d infra, %d known-gap declines\n", mismatches, infra, declines)

	switch {
	case mismatches > 0:
		return 1
	case infra > 0:
		return 2
	}
	return 0
}
