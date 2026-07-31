package factory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"fdb.dev/pkg/relational/conformance/factorycorpus"
)

// Manifest is a factory run's whole output ledger — the numbers RFC-201 §5.1
// asks every stage to report and §5.5's PR description carries.
//
// It is a ledger, not a summary: a run that commits nothing must still say
// what it saw and why nothing survived, because "the batch was empty" and "the
// generator produced nothing" and "everything deduplicated" are three
// different states and only the first is healthy.
type Manifest struct {
	Date          string `json:"date"`
	Generator     string `json:"generator"`
	SeedStart     uint64 `json:"seed_start"`
	Seeds         uint64 `json:"seeds"`
	Quota         int    `json:"quota"`
	BlessingMode  string `json:"blessing_mode"`
	CandidatesGen int    `json:"candidates_generated"`
	CandidatesRun int    `json:"candidates_executed"`
	Blessed       int    `json:"blessed"`
	Committed     int    `json:"committed"`
	// DedupRejected counts blessed candidates dropped because their (feature
	// vector, plan shape) point was already covered. A high number is HEALTHY
	// — it is the frontier flattening — and a zero is the number to be
	// suspicious of, because it means the key is discriminating on something
	// that varies per candidate.
	DedupRejected int `json:"dedup_rejected"`
	// DedupPoints is the number of distinct points the committed corpus covers
	// after this batch.
	DedupPoints int `json:"dedup_points"`
	// NameCollisions counts blessed candidates whose file name was already
	// taken on disk. It is called out as its own field rather than left in the
	// skip ledger because it is the one skip class that means a committed
	// scenario was nearly destroyed: the file name and the dedup key are
	// independent, so a planner change moves the key (dedup misses) without
	// moving the name (the write would clobber). A non-zero value is a
	// statement about the ENGINE, not about the batch.
	NameCollisions int `json:"name_collisions"`
	// SkipsByReason is the counted-skip ledger. Every candidate that did not
	// become a test appears here under a reason class; a run whose skips do
	// not account for its candidates is a run with a silent drop in it.
	SkipsByReason map[string]int `json:"skips_by_reason"`
	// SkipSamples holds a few verbatim messages per reason class. A class with
	// a count and no example is a class nobody can triage: "second-plan infra
	// 28" is indistinguishable between a transport hiccup and the disabled-rule
	// option being accepted and ignored, and those are a shrug and an emergency.
	SkipSamples map[string][]string `json:"skip_samples"`
	Oracles     OracleStats         `json:"oracles"`
	Blessings   map[string]int      `json:"blessings"`
	// Exemptions names the structural-inapplicability families available to
	// this run, each with the pin establishing it and the oracle that would
	// retire it. A batch that blessed anything on fewer oracles than the rest
	// says so here, in the same document as the counts — an exemption a
	// reader has to go find in the source is one they will not.
	Exemptions []string `json:"exemptions"`
	// BlessedByFamily counts TLP-only blessings per exemption family, so the
	// SIZE of each weakening is visible next to its justification.
	BlessedByFamily map[string]int       `json:"blessed_by_family"`
	Findings        int                  `json:"findings"`
	Census          factorycorpus.Census `json:"census_after"`
}

// Batch accumulates a run's committed output and enforces the dedup rule.
type Batch struct {
	// Dir is the corpus testdata directory files are written into.
	Dir string
	// Quota caps committed scenarios per run. §5.2: per-run quotas keep PRs
	// reviewable; the steady state is months of batches, not one dump.
	Quota int

	seen     map[string]string // dedup key -> the path that claimed it
	manifest Manifest
}

// NewBatch prepares a batch against the corpus already committed in dir.
//
// Seeding the dedup set from the EXISTING corpus is what makes the factory
// cumulative rather than repetitive: without it every run would re-commit the
// shapes the last run already covered, and a year of nightlies would produce
// one night's coverage a hundred times over.
func NewBatch(dir string, quota int) (*Batch, error) {
	b := &Batch{Dir: dir, Quota: quota, seen: map[string]string{}}
	b.manifest.SkipsByReason = map[string]int{}
	b.manifest.SkipSamples = map[string][]string{}
	b.manifest.Blessings = map[string]int{}
	b.manifest.BlessedByFamily = map[string]int{}
	b.manifest.Exemptions = InapplicabilityLedger()
	matches, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	for _, m := range matches {
		f, err := factorycorpus.Load(m)
		if err != nil {
			return nil, fmt.Errorf("existing corpus file %s: %w", m, err)
		}
		if prev, dup := b.seen[f.Header.DedupKey]; dup {
			return nil, fmt.Errorf("existing corpus already holds two files at dedup key %s (%s, %s)", f.Header.DedupKey, prev, m)
		}
		b.seen[f.Header.DedupKey] = m
	}
	return b, nil
}

// Full reports whether the quota is reached.
func (b *Batch) Full() bool { return b.manifest.Committed >= b.Quota }

// Committed is the number of scenarios written so far.
func (b *Batch) Committed() int { return b.manifest.Committed }

// CountCandidate records a generated candidate.
func (b *Batch) CountCandidate() { b.manifest.CandidatesGen++ }

// Offer folds one outcome into the batch, writing the file when the candidate
// is blessed and its point is new. It returns the path written, or "".
func (b *Batch) Offer(o Outcome) (string, error) {
	b.manifest.CandidatesRun++
	b.manifest.Oracles.Add(o.Stats)
	b.manifest.Findings += len(o.Findings)
	if len(o.Findings) > 0 {
		b.manifest.SkipsByReason["oracle-disagreement:"+o.Findings[0].Oracle]++
		return "", nil
	}
	if !o.Blessed {
		b.noteSkip(o.Skip)
		return "", nil
	}
	b.manifest.Blessed++
	if prev, dup := b.seen[o.Header.DedupKey]; dup {
		b.manifest.DedupRejected++
		_ = prev
		return "", nil
	}
	if b.Full() {
		b.manifest.SkipsByReason["quota-reached"]++
		return "", nil
	}
	data, err := factorycorpus.Marshal(o.Header, o.Scenario)
	if err != nil {
		return "", fmt.Errorf("marshal %s: %w", o.Header.Name, err)
	}
	path := filepath.Join(b.Dir, o.Header.Name+".yaml")

	// A batch must never overwrite a committed scenario, and dedup is not what
	// stops it: the two keys are INDEPENDENT. The file name comes from (seed,
	// query index, projection); the dedup key comes from (feature vector, plan
	// shape). Change the planner and a seed's plan shape moves, so the dedup key
	// moves, so the dedup lookup MISSES — while the name is byte-identical to
	// the file already on disk. The write would then silently replace a
	// committed expectation with a freshly blessed one, the census would be
	// recomputed from the overwritten directory, and the ratchet, which measures
	// that same directory, would see nothing. Every instrument would agree that
	// nothing happened.
	//
	// So the collision is refused and NAMED. There is no benign version of it to
	// carve out: a re-run against an unchanged engine re-derives the same dedup
	// key, and the dedup check above rejects it before the writer is reached.
	// Getting here at all means the key moved while the name did not, and the
	// only content that could be under this name is content this batch was
	// about to replace.
	if _, err := os.Lstat(path); err == nil {
		b.manifest.NameCollisions++
		b.manifest.SkipsByReason["name-collision"]++
		if len(b.manifest.SkipSamples["name-collision"]) < maxSkipSamples {
			b.manifest.SkipSamples["name-collision"] = append(b.manifest.SkipSamples["name-collision"],
				fmt.Sprintf("%s already exists at a DIFFERENT dedup key (%s); refusing to overwrite a committed scenario",
					path, o.Header.DedupKey))
		}
		return "", nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("stat %s: %w", path, err)
	}

	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	// Read it straight back through the loader that CI will use. A file the
	// corpus cannot load is worse than no file: the batch reports a win and
	// the next `just test` goes red on a gate that has nothing to do with the
	// engine.
	if _, err := factorycorpus.Load(path); err != nil {
		return "", fmt.Errorf("just-written file does not load: %w", err)
	}
	b.seen[o.Header.DedupKey] = path
	b.manifest.Committed++
	b.manifest.Blessings[string(o.Header.Blessing)]++
	if o.InapplicableFamily != "" {
		b.manifest.BlessedByFamily[o.InapplicableFamily]++
	}
	return path, nil
}

// maxSkipSamples caps the examples kept per reason class: enough to triage,
// few enough that a manifest stays readable when a class has thousands.
const maxSkipSamples = 3

func (b *Batch) noteSkip(msg string) {
	class := skipClass(msg)
	b.manifest.SkipsByReason[class]++
	if len(b.manifest.SkipSamples[class]) < maxSkipSamples {
		b.manifest.SkipSamples[class] = append(b.manifest.SkipSamples[class], msg)
	}
}

// skipClass folds a skip message into a reason CLASS, so the ledger stays a
// small set of named states instead of one bucket per message.
func skipClass(msg string) string {
	for _, prefix := range []string{
		"plan-error", "engine error", "expectation too large",
		"degenerate partition",
		"second-plan infra", "java leg failed", "metamorphic blessing needs",
	} {
		if len(msg) >= len(prefix) && msg[:len(prefix)] == prefix {
			return prefix
		}
	}
	if msg == "" {
		return "unclassified-empty"
	}
	return "other"
}

// Finish completes the manifest with the corpus census measured from disk.
func (b *Batch) Finish(seedStart, seeds uint64, date, blessingMode string) (Manifest, error) {
	b.manifest.Date = date
	b.manifest.Generator = GeneratorVersion
	b.manifest.SeedStart = seedStart
	b.manifest.Seeds = seeds
	b.manifest.Quota = b.Quota
	b.manifest.BlessingMode = blessingMode
	b.manifest.DedupPoints = len(b.seen)
	files, err := factorycorpus.LoadDir(b.Dir)
	if err != nil {
		return b.manifest, err
	}
	b.manifest.Census = factorycorpus.ComputeCensus(files)
	return b.manifest, nil
}

// WriteManifest persists the manifest next to the batch.
func WriteManifest(path string, m Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// WriteFindings persists oracle disagreements. Each one is a potential engine
// bug and the run's exit code reports them; auto-minimization is not yet built,
// so the persisted record carries the full reproduction — seed, DDL, setup and
// every query — rather than a shrunk one.
func WriteFindings(dir string, findings []*Finding) error {
	if len(findings) == 0 {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Seed != findings[j].Seed {
			return findings[i].Seed < findings[j].Seed
		}
		return findings[i].Candidate < findings[j].Candidate
	})
	for _, f := range findings {
		data, err := json.MarshalIndent(f, "", "  ")
		if err != nil {
			return err
		}
		name := fmt.Sprintf("%s_%s.json", f.Oracle, f.Candidate)
		if err := os.WriteFile(filepath.Join(dir, name), append(data, '\n'), 0o644); err != nil {
			return err
		}
	}
	return nil
}
