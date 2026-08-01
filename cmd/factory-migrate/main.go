// factory-migrate re-emits the committed RFC-201 factory corpus from its
// seeds into the grouped `.yamsql` family-file format (RFC-201 §5.7).
//
// Migration is BY RE-EMISSION, not by transliteration: every committed
// scenario is regenerated from its recorded (generator, seed, query,
// projection, date) recipe, executed against a real FDB through the same
// oracle pipeline that blessed it, and written through the new family writer.
// The old files' frozen rows are then compared VALUE-BY-VALUE against the
// re-emitted ones; any mismatch is a bug to root-cause, never to accept —
// same seed plus same oracle output must mean the same rows.
//
//	bazelisk run //cmd/factory-migrate -- \
//	  -old pkg/relational/conformance/factorycorpus/testdata \
//	  -out pkg/relational/conformance/factorycorpus/testdata.new \
//	  -census pkg/relational/conformance/factorycorpus/census_baseline.json
//
// Exit codes: 0 = every scenario re-emitted and verified; 1 = a verification
// mismatch (details on stderr); 2 = infra failure.
package main

import (
	"bufio"
	"context"
	"database/sql"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"fdb.dev/pkg/relational/conformance/factory"
	"fdb.dev/pkg/relational/conformance/factorycorpus"
	"fdb.dev/pkg/relational/conformance/yamsql"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"

	_ "fdb.dev/pkg/relational/sqldriver"
)

const (
	exitOK       = 0
	exitMismatch = 1
	exitInfra    = 2
)

func main() {
	var oldDir, outDir, censusPath string
	flag.StringVar(&oldDir, "old", "pkg/relational/conformance/factorycorpus/testdata", "directory holding the v1 fc_*.yaml corpus")
	flag.StringVar(&outDir, "out", "", "directory the .yamsql family files are written into (must not equal -old)")
	flag.StringVar(&censusPath, "census", "", "path the regenerated census baseline is written to")
	flag.Parse()
	if outDir == "" || censusPath == "" || outDir == oldDir {
		fmt.Fprintln(os.Stderr, "INFRA: -out and -census are required, and -out must differ from -old")
		os.Exit(exitInfra)
	}
	os.Exit(run(oldDir, outDir, censusPath))
}

// oldScenario is one committed v1 file: its provenance and its frozen content.
type oldScenario struct {
	path     string
	name     string
	seed     uint64
	qi, pj   int
	date     string
	blessing string
	oracles  string
	fv       string
	shape    string
	dedupKey string
	doc      *yamsql.Scenario
}

func run(oldDir, outDir, censusPath string) int {
	old, err := loadOldCorpus(oldDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "INFRA: %v\n", err)
		return exitInfra
	}
	fmt.Printf("v1 corpus: %d scenarios across %d seeds\n", len(old), countSeeds(old))

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "INFRA: %v\n", err)
		return exitInfra
	}

	startCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	container, err := foundationdbtc.Run(startCtx, "")
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "INFRA: FDB container: %v\n", err)
		return exitInfra
	}
	defer container.Terminate(context.Background()) //nolint:errcheck

	ctx := context.Background()
	clusterContent, err := container.ClusterFile(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "INFRA: cluster file: %v\n", err)
		return exitInfra
	}
	tmp, err := os.CreateTemp("", "factory-migrate-*.cluster")
	if err != nil {
		fmt.Fprintf(os.Stderr, "INFRA: %v\n", err)
		return exitInfra
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(clusterContent); err != nil {
		fmt.Fprintf(os.Stderr, "INFRA: %v\n", err)
		return exitInfra
	}
	tmp.Close()

	const dbPath = "/factorymigrate"
	setupDB, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s", dbPath, tmp.Name()))
	if err != nil {
		fmt.Fprintf(os.Stderr, "INFRA: open: %v\n", err)
		return exitInfra
	}
	defer setupDB.Close()
	if _, err := setupDB.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		fmt.Fprintf(os.Stderr, "INFRA: create database: %v\n", err)
		return exitInfra
	}

	batch, err := factory.NewBatch(outDir, len(old)+1)
	if err != nil {
		fmt.Fprintf(os.Stderr, "INFRA: %v\n", err)
		return exitInfra
	}

	// Index the old corpus by (seed, qi, pj) and enumerate seeds in ascending
	// order — the order the original batch swept them in, which is what makes
	// the dedup walk reproduce the original outcome exactly.
	byTriple := map[string]*oldScenario{}
	seedDates := map[uint64]string{}
	var seeds []uint64
	for _, o := range old {
		byTriple[tripleKey(o.seed, o.qi, o.pj)] = o
		if prev, ok := seedDates[o.seed]; ok && prev != o.date {
			fmt.Fprintf(os.Stderr, "INFRA: seed %d carries two dates (%s, %s); re-emission needs one date per seed\n", o.seed, prev, o.date)
			return exitInfra
		}
		if _, ok := seedDates[o.seed]; !ok {
			seeds = append(seeds, o.seed)
		}
		seedDates[o.seed] = o.date
	}
	sort.Slice(seeds, func(i, j int) bool { return seeds[i] < seeds[j] })

	matchedScenarios, matchedRows, matchedCells := 0, 0, 0
	for n, seed := range seeds {
		sweep := factory.Sweep{
			SetupDB:     setupDB,
			DBPath:      dbPath,
			ClusterFile: tmp.Name(),
			Date:        seedDates[seed],
			Filter: func(c factory.Candidate) bool {
				_, ok := byTriple[tripleKey(c.Seed, c.QueryIndex, c.ProjIndex)]
				return ok
			},
		}
		outcomes, err := sweep.RunSeed(ctx, seed, batch)
		if err != nil {
			fmt.Fprintf(os.Stderr, "INFRA: seed %d: %v\n", seed, err)
			return exitInfra
		}
		for _, o := range outcomes {
			ref := byTriple[tripleKey(o.Candidate.Seed, o.Candidate.QueryIndex, o.Candidate.ProjIndex)]
			rows, cells, err := verify(ref, o)
			if err != nil {
				fmt.Fprintf(os.Stderr, "MISMATCH: %s: %v\n", ref.name, err)
				return exitMismatch
			}
			matchedScenarios++
			matchedRows += rows
			matchedCells += cells
		}
		if (n+1)%50 == 0 {
			fmt.Printf("  %d/%d seeds re-emitted (%d scenarios verified)\n", n+1, len(seeds), matchedScenarios)
		}
	}

	if matchedScenarios != len(old) {
		fmt.Fprintf(os.Stderr, "MISMATCH: %d of %d committed scenarios were re-emitted; the remainder no longer regenerate\n",
			matchedScenarios, len(old))
		return exitMismatch
	}

	manifest, err := batch.Finish(seeds[0], seeds[len(seeds)-1]-seeds[0]+1, seedDates[seeds[0]], "metamorphic")
	if err != nil {
		fmt.Fprintf(os.Stderr, "INFRA: finish: %v\n", err)
		return exitInfra
	}
	data, err := factorycorpus.RenderCensus(manifest.Census)
	if err != nil {
		fmt.Fprintf(os.Stderr, "INFRA: census: %v\n", err)
		return exitInfra
	}
	if err := os.WriteFile(censusPath, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "INFRA: census: %v\n", err)
		return exitInfra
	}

	fmt.Printf("re-emitted %d scenarios (%d rows, %d cells) into %d family files; census: %d scenarios, %d tests\n",
		matchedScenarios, matchedRows, matchedCells, countFamilies(outDir), manifest.Census.Scenarios, manifest.Census.Tests)
	return exitOK
}

func tripleKey(seed uint64, qi, pj int) string { return fmt.Sprintf("%d/%d/%d", seed, qi, pj) }

func countSeeds(old []*oldScenario) int {
	seen := map[uint64]bool{}
	for _, o := range old {
		seen[o.seed] = true
	}
	return len(seen)
}

func countFamilies(dir string) int {
	m, _ := filepath.Glob(filepath.Join(dir, "*.yamsql"))
	return len(m)
}

// verify compares a freshly re-emitted outcome against the committed v1
// scenario it regenerates, and returns the number of rows and cells compared.
func verify(ref *oldScenario, o factory.Outcome) (int, int, error) {
	if !o.Blessed {
		return 0, 0, fmt.Errorf("was blessed %s in the committed corpus, re-emission did not bless it (skip: %s, findings: %d)",
			ref.blessing, o.Skip, len(o.Findings))
	}
	h := o.Header
	if h.Name != ref.name {
		return 0, 0, fmt.Errorf("name %s != %s", h.Name, ref.name)
	}
	if string(h.Blessing) != ref.blessing {
		return 0, 0, fmt.Errorf("blessing %s != committed %s", h.Blessing, ref.blessing)
	}
	if got := strings.Join(h.Oracles, ","); got != ref.oracles {
		return 0, 0, fmt.Errorf("oracles %s != committed %s", got, ref.oracles)
	}
	if h.FeatureVector != ref.fv {
		return 0, 0, fmt.Errorf("feature vector drifted\n  committed: %s\n  re-emitted: %s", ref.fv, h.FeatureVector)
	}
	if h.PlanShape != ref.shape || h.DedupKey != ref.dedupKey {
		return 0, 0, fmt.Errorf("plan-shape/dedup-key drifted (%s/%s != committed %s/%s)", h.PlanShape, h.DedupKey, ref.shape, ref.dedupKey)
	}
	if o.Scenario.SchemaTemplate != ref.doc.SchemaTemplate {
		return 0, 0, fmt.Errorf("schema_template drifted")
	}
	if len(o.Scenario.Setup) != len(ref.doc.Setup) || o.Scenario.Setup[0] != ref.doc.Setup[0] {
		return 0, 0, fmt.Errorf("setup drifted")
	}
	if len(o.Scenario.Tests) != len(ref.doc.Tests) {
		return 0, 0, fmt.Errorf("%d tests != committed %d", len(o.Scenario.Tests), len(ref.doc.Tests))
	}
	rows, cells := 0, 0
	for i := range ref.doc.Tests {
		want, got := ref.doc.Tests[i], o.Scenario.Tests[i]
		if want.Query != got.Query {
			return 0, 0, fmt.Errorf("tests[%d] query drifted\n  committed: %s\n  re-emitted: %s", i, want.Query, got.Query)
		}
		if want.Unordered != got.Unordered {
			return 0, 0, fmt.Errorf("tests[%d] unordered %v != committed %v", i, got.Unordered, want.Unordered)
		}
		if len(want.Rows) != len(got.Rows) {
			return 0, 0, fmt.Errorf("tests[%d] %d rows != committed %d", i, len(got.Rows), len(want.Rows))
		}
		for r := range want.Rows {
			if len(want.Rows[r]) != len(got.Rows[r]) {
				return 0, 0, fmt.Errorf("tests[%d] row %d width %d != committed %d", i, r, len(got.Rows[r]), len(want.Rows[r]))
			}
			for c := range want.Rows[r] {
				w, g := cellKey(want.Rows[r][c]), cellKey(got.Rows[r][c])
				if w != g {
					return 0, 0, fmt.Errorf("tests[%d] row %d col %d: committed %s, re-emitted %s", i, r, c, w, g)
				}
				cells++
			}
			rows++
		}
	}
	return rows, cells, nil
}

// cellKey normalizes a cell to the identity the v1 format froze: the v1 writer
// widened every float32 to float64 text, so a re-emitted float32 cell and a
// committed float64 cell are the same value exactly when their float64 images
// share a bit pattern. Integer widths collapse to int64 the same way the v1
// loader read them back.
func cellKey(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return fmt.Sprintf("bool:%t", x)
	case string:
		return "str:" + x
	case int:
		return fmt.Sprintf("int:%d", int64(x))
	case int32:
		return fmt.Sprintf("int:%d", int64(x))
	case int64:
		return fmt.Sprintf("int:%d", x)
	case float32:
		return fmt.Sprintf("float:%016x", math.Float64bits(float64(x)))
	case float64:
		return fmt.Sprintf("float:%016x", math.Float64bits(x))
	default:
		return fmt.Sprintf("%T:%v", v, v)
	}
}

// loadOldCorpus reads the v1 corpus: `# key: value` provenance comments over a
// single-document yamsql-lite scenario the go-format loader still reads.
func loadOldCorpus(dir string) ([]*oldScenario, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "fc_*.yaml"))
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("no fc_*.yaml under %s", dir)
	}
	sort.Strings(matches)
	keyLine := regexp.MustCompile(`^#\s*([a-z][a-z0-9-]*):\s*(.*)$`)
	var out []*oldScenario
	for _, m := range matches {
		f, err := os.Open(m)
		if err != nil {
			return nil, err
		}
		kv := map[string]string{}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "#") {
				break
			}
			if g := keyLine.FindStringSubmatch(line); g != nil {
				kv[g[1]] = strings.TrimSpace(g[2])
			}
		}
		f.Close()
		if kv["format-version"] != "1" {
			return nil, fmt.Errorf("%s: format-version %q, the migrator reads v1", m, kv["format-version"])
		}
		doc, err := yamsql.Load(m)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", m, err)
		}
		o := &oldScenario{
			path: m, name: doc.Name, date: kv["date"], blessing: kv["blessing"],
			oracles: kv["oracles"], fv: kv["feature-vector"], shape: kv["plan-shape"],
			dedupKey: kv["dedup-key"], doc: doc,
		}
		if o.seed, err = strconv.ParseUint(kv["seed"], 10, 64); err != nil {
			return nil, fmt.Errorf("%s: seed: %w", m, err)
		}
		if o.qi, err = strconv.Atoi(kv["query-index"]); err != nil {
			return nil, fmt.Errorf("%s: query-index: %w", m, err)
		}
		if o.pj, err = strconv.Atoi(kv["projection"]); err != nil {
			return nil, fmt.Errorf("%s: projection: %w", m, err)
		}
		out = append(out, o)
	}
	return out, nil
}
