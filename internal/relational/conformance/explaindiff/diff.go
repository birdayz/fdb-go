package explaindiff

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Baseline is a parsed baseline file: its header plus its entries. The header
// is not decoration — the format version decides whether the two files are
// even comparable, and the stats line is the only way to tell a baseline that
// legitimately holds N entries from one whose dump was killed after N.
type Baseline struct {
	// Version is the format tag from the first header line.
	Version string
	// Stats is the reconciliation header the dump wrote.
	Stats Stats
	// Entries are the per-query records, in file order.
	Entries []Entry
	// Path is where the baseline was read from, when it came from disk.
	// ValidateDiffInputs uses it to reject a file diffed against itself.
	Path string
}

// header line prefixes. Kept as constants because Render writes them and
// Parse enforces them; a drift between the two is exactly the bug that let
// the version tag go unchecked.
const (
	statsPrefix   = "# files="
	statsTemplate = "# files=%d queries=%d dml=%d non_query=%d plan_errors=%d unexpected_errors=%d"
)

// Parse reads back a rendered baseline. Round-tripping through Parse is what
// lets Diff report per-QUERY verdicts instead of per-line text hunks.
//
// The header is PARSED, not skipped:
//
//   - a formatVersion other than the current one is refused outright, because
//     a rendering change makes every entry differ for reasons that have
//     nothing to do with the planner;
//   - the stats line must be present and well-formed;
//   - the stats line's `queries=` + `dml=` must equal the number of entries
//     that follow (both classes produce one entry each). A dump interrupted
//     mid-write keeps its header count and loses the tail, which is precisely
//     the truncation this check catches.
func Parse(text string) (*Baseline, error) {
	var (
		b   Baseline
		cur *Entry
	)
	flush := func() {
		if cur != nil {
			b.Entries = append(b.Entries, *cur)
			cur = nil
		}
	}
	sawVersion, sawStats := false, false
	for n, line := range strings.Split(text, "\n") {
		lineNo := n + 1
		switch {
		case !sawVersion && strings.HasPrefix(line, "# ") && !strings.HasPrefix(line, statsPrefix):
			sawVersion = true
			b.Version = strings.TrimSpace(strings.TrimPrefix(line, "# "))
			if b.Version != formatVersion {
				return nil, fmt.Errorf(
					"line %d: baseline format is %q but this build writes %q — regenerate both baselines with `go run ./cmd/explain-differ dump`",
					lineNo, b.Version, formatVersion)
			}
		case !sawStats && strings.HasPrefix(line, statsPrefix):
			if !sawVersion {
				return nil, fmt.Errorf("line %d: stats header before the format-version header", lineNo)
			}
			sawStats = true
			var st Stats
			if _, err := fmt.Sscanf(line, statsTemplate,
				&st.Files, &st.Queries, &st.DML, &st.NonQuery, &st.PlanErrors, &st.UnexpectedErrors); err != nil {
				return nil, fmt.Errorf("line %d: malformed stats header %q: %w", lineNo, line, err)
			}
			b.Stats = st
		case line == "" || strings.HasPrefix(line, "#"):
			continue
		case strings.HasPrefix(line, "=== "):
			flush()
			key := strings.TrimPrefix(line, "=== ")
			pin := ""
			if k, tail, ok := strings.Cut(key, " expect-error="); ok {
				key, pin = k, tail
			}
			file, idx, err := splitKey(key)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNo, err)
			}
			cur = &Entry{File: file, Index: idx, ErrorPin: pin}
		case cur == nil:
			return nil, fmt.Errorf("line %d: payload before any === entry header", lineNo)
		case strings.HasPrefix(line, "sql:   "):
			cur.SQL = strings.TrimPrefix(line, "sql:   ")
		case strings.HasPrefix(line, "plan:  "):
			cur.Plan = strings.TrimPrefix(line, "plan:  ")
		case strings.HasPrefix(line, "shape: "):
			cur.Shape = append(cur.Shape, strings.TrimPrefix(line, "shape: "))
		default:
			return nil, fmt.Errorf("line %d: unrecognized line %q", lineNo, line)
		}
	}
	flush()

	switch {
	case !sawVersion:
		return nil, fmt.Errorf("baseline has no `# %s` format-version header — not an explain baseline (regenerate with `go run ./cmd/explain-differ dump`)", formatVersion)
	case !sawStats:
		return nil, errors.New("baseline has no `# files=… queries=…` stats header — the dump did not finish writing it")
	case b.Stats.Queries+b.Stats.DML != len(b.Entries):
		return nil, fmt.Errorf(
			"baseline is TRUNCATED: header declares queries=%d + dml=%d = %d but the file holds %d entries — the dump was interrupted; regenerate it",
			b.Stats.Queries, b.Stats.DML, b.Stats.Queries+b.Stats.DML, len(b.Entries))
	}
	return &b, nil
}

// splitKey parses a "file.yaml#12" diff key.
func splitKey(key string) (string, int, error) {
	i := strings.LastIndex(key, "#")
	if i < 0 {
		return "", 0, fmt.Errorf("malformed entry key %q: want <file>#<index>", key)
	}
	idx, err := strconv.Atoi(key[i+1:])
	if err != nil {
		return "", 0, fmt.Errorf("malformed entry key %q: %w", key, err)
	}
	return key[:i], idx, nil
}

// Change classifies one key's verdict between two baselines.
type Change int

const (
	// Changed means the key exists on both sides with a different plan or shape.
	Changed Change = iota
	// Added means the key exists only in the new baseline.
	Added
	// Removed means the key exists only in the old baseline.
	Removed
)

// String renders a Change as its report tag.
func (c Change) String() string {
	switch c {
	case Added:
		return "ADDED"
	case Removed:
		return "REMOVED"
	}
	return "CHANGED"
}

// Delta is one differing key.
type Delta struct {
	// Key is the file#index the delta belongs to.
	Key string
	// Kind classifies the delta.
	Kind Change
	// SQL is the query text (new side's, falling back to old).
	SQL string
	// OldPlan / NewPlan are the two renderings; the missing side of an
	// ADDED / REMOVED delta is empty.
	OldPlan, NewPlan string
	// OldShape / NewShape are the two structural skeletons.
	OldShape, NewShape []string
	// ShapeChanged distinguishes a STRUCTURAL flip (different operators or
	// tree) from a rendering-only delta (same skeleton, different labels).
	// RFC-183's risk is the former; separating them keeps a formatting
	// churn from drowning a real flip. Only set when BOTH sides planned —
	// a planned↔error transition is reported by RegressedToError /
	// RecoveredFromError, and double-tagging it as a shape flip would
	// inflate the count the exit gate reads.
	ShapeChanged bool
	// RegressedToError marks old-planned → new-fails. This is the single
	// most important signal in the report: a query that stopped planning.
	RegressedToError bool
	// RecoveredFromError marks old-failed → new-plans.
	RecoveredFromError bool
}

// DiffReport is the full verdict between two baselines.
type DiffReport struct {
	// Deltas are the differing keys, sorted by (file, index).
	Deltas []Delta
	// Same is the count of byte-identical entries.
	Same int
	// TotalOld / TotalNew are the two baselines' entry counts.
	TotalOld, TotalNew int
	// ShapeFlips counts deltas whose structural skeleton moved.
	ShapeFlips int
	// Regressions counts planned → error transitions.
	Regressions int
	// Recoveries counts error → planned transitions.
	Recoveries int
	// Mispaired counts file#index positions held by a DIFFERENT query on
	// each side — the fingerprint of a corpus insertion, removal, or
	// reorder. Non-zero means the ADDED/REMOVED noise below is corpus
	// churn, not a planner change.
	Mispaired int
}

// Clean reports whether the two baselines are identical.
func (r DiffReport) Clean() bool { return len(r.Deltas) == 0 }

// pairKey is the identity Diff pairs entries on: the corpus position AND the
// query text.
//
// file#index alone is NOT an identity. Inserting a stanza in the middle of a
// corpus file renumbers every following stanza, so position N in the old
// baseline and position N in the new one are different QUERIES — pairing them
// prints one query's SQL beside another query's plans and reports the
// mismatch as a shape flip. Including the SQL makes a shifted entry fall out
// as REMOVED + ADDED, which is what actually happened.
type pairKey struct {
	key string // file#index
	sql string
}

// Diff compares two baselines. Entries pair only when both their corpus
// position AND their SQL match; a corpus insertion, removal, or reorder
// therefore surfaces as ADDED/REMOVED (and is counted in Mispaired) instead
// of being misattributed as a plan change on an unrelated query.
func Diff(oldEntries, newEntries []Entry) DiffReport {
	oldByKey := make(map[pairKey]Entry, len(oldEntries))
	oldSQL := make(map[string]string, len(oldEntries))
	for _, e := range oldEntries {
		oldByKey[pairKey{e.Key(), e.SQL}] = e
		oldSQL[e.Key()] = e.SQL
	}
	newByKey := make(map[pairKey]Entry, len(newEntries))
	newSQL := make(map[string]string, len(newEntries))
	for _, e := range newEntries {
		newByKey[pairKey{e.Key(), e.SQL}] = e
		newSQL[e.Key()] = e.SQL
	}

	rep := DiffReport{TotalOld: len(oldEntries), TotalNew: len(newEntries)}
	// A position occupied on both sides by DIFFERENT queries is the
	// mispairing signal: the corpus moved under the baseline.
	for k, os := range oldSQL {
		if ns, ok := newSQL[k]; ok && ns != os {
			rep.Mispaired++
		}
	}

	// Walk the union in sorted order — never map order — so the report is
	// byte-stable and itself diffable.
	keys := make([]pairKey, 0, len(oldByKey)+len(newByKey))
	for k := range oldByKey {
		keys = append(keys, k)
	}
	for k := range newByKey {
		if _, dup := oldByKey[k]; !dup {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].key != keys[j].key {
			return lessKey(keys[i].key, keys[j].key)
		}
		return keys[i].sql < keys[j].sql
	})

	for _, pk := range keys {
		k := pk.key
		o, inOld := oldByKey[pk]
		n, inNew := newByKey[pk]
		switch {
		case inOld && !inNew:
			rep.Deltas = append(rep.Deltas, Delta{
				Key: k, Kind: Removed, SQL: o.SQL, OldPlan: o.Plan, OldShape: o.Shape,
			})
		case !inOld && inNew:
			rep.Deltas = append(rep.Deltas, Delta{
				Key: k, Kind: Added, SQL: n.SQL, NewPlan: n.Plan, NewShape: n.Shape,
			})
		default:
			bothPlanned := !o.Failed() && !n.Failed()
			shapeMoved := bothPlanned && !equalLines(o.Shape, n.Shape)
			if o.Plan == n.Plan && equalLines(o.Shape, n.Shape) {
				rep.Same++
				continue
			}
			d := Delta{
				Key: k, Kind: Changed, SQL: n.SQL,
				OldPlan: o.Plan, NewPlan: n.Plan,
				OldShape: o.Shape, NewShape: n.Shape,
				ShapeChanged:       shapeMoved,
				RegressedToError:   !o.Failed() && n.Failed(),
				RecoveredFromError: o.Failed() && !n.Failed(),
			}
			if d.ShapeChanged {
				rep.ShapeFlips++
			}
			if d.RegressedToError {
				rep.Regressions++
			}
			if d.RecoveredFromError {
				rep.Recoveries++
			}
			rep.Deltas = append(rep.Deltas, d)
		}
	}
	return rep
}

// lessKey orders keys by file name then numeric index, so file#2 sorts
// before file#10 (a lexical sort would not).
func lessKey(a, b string) bool {
	af, ai, aerr := splitKey(a)
	bf, bi, berr := splitKey(b)
	if aerr != nil || berr != nil {
		return a < b
	}
	if af != bf {
		return af < bf
	}
	return ai < bi
}

func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// RenderDiff formats a DiffReport for a human. The summary leads with the
// counts that decide RFC-183's P0 exit criteria: structural flips and
// plan-time regressions.
func RenderDiff(r DiffReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s diff\n", formatVersion)
	fmt.Fprintf(&b, "# entries: old=%d new=%d identical=%d differing=%d\n",
		r.TotalOld, r.TotalNew, r.Same, len(r.Deltas))
	fmt.Fprintf(&b, "# shape_flips=%d plan_regressions=%d plan_recoveries=%d mispaired=%d\n",
		r.ShapeFlips, r.Regressions, r.Recoveries, r.Mispaired)
	if r.Clean() {
		b.WriteString("#\n# CLEAN — no plan-shape change across the corpus.\n")
		return b.String()
	}
	if r.Mispaired > 0 {
		fmt.Fprintf(&b, "#\n# WARNING: %d corpus positions hold a different query on each side.\n"+
			"# The corpus itself changed (stanza inserted/removed/reordered), so the\n"+
			"# ADDED/REMOVED deltas below are position shifts, not planner changes.\n"+
			"# Re-run the comparison against baselines from the SAME corpus revision.\n", r.Mispaired)
	}
	b.WriteString("#\n")
	for _, d := range r.Deltas {
		tags := d.Kind.String()
		if d.ShapeChanged {
			tags += " SHAPE"
		}
		if d.RegressedToError {
			tags += " STOPPED-PLANNING"
		}
		if d.RecoveredFromError {
			tags += " NOW-PLANS"
		}
		fmt.Fprintf(&b, "=== %s [%s]\n", d.Key, tags)
		fmt.Fprintf(&b, "sql:   %s\n", d.SQL)
		if d.Kind != Added {
			fmt.Fprintf(&b, "-plan: %s\n", d.OldPlan)
		}
		if d.Kind != Removed {
			fmt.Fprintf(&b, "+plan: %s\n", d.NewPlan)
		}
		if d.ShapeChanged {
			for _, l := range d.OldShape {
				fmt.Fprintf(&b, "-shape: %s\n", l)
			}
			for _, l := range d.NewShape {
				fmt.Fprintf(&b, "+shape: %s\n", l)
			}
		}
	}
	return b.String()
}

// LoadBaseline reads and parses a baseline file from disk.
func LoadBaseline(path string) (*Baseline, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	b, err := Parse(string(data))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	b.Path = path
	return b, nil
}

// MinBaselineEntries is the floor below which a baseline is treated as
// broken rather than small. The yamsql corpus plans ~2400 queries; a dump
// holding a double-digit count means the glob, the loader, or the dump run
// itself failed, and letting that compare CLEAN is the single worst failure
// mode this harness has — it reports "no plan changed" because it looked at
// nothing.
const MinBaselineEntries = 100

// MaxEntryCountDrift is the largest relative gap tolerated between the two
// baselines' entry counts, as a fraction of the larger side. A real corpus
// edit moves a handful of stanzas out of thousands; a dump that was killed
// or aborted loses a whole tail. 10% sits far above the former and far below
// the latter.
const MaxEntryCountDrift = 0.10

// ValidateDiffInputs rejects baseline pairs that cannot produce meaningful
// evidence, BEFORE Diff gets a chance to report CLEAN on them. Every check
// here exists because passing it silently is worse than failing loudly: this
// harness's whole job is to be the thing that says "nothing moved", so it
// must refuse to say that when it did not actually look.
func ValidateDiffInputs(oldB, newB *Baseline) error {
	if oldB == nil || newB == nil {
		return errors.New("both baselines must be loaded")
	}
	if err := checkDistinctSources(oldB.Path, newB.Path); err != nil {
		return err
	}
	for _, b := range []*Baseline{oldB, newB} {
		switch {
		case len(b.Entries) == 0:
			return fmt.Errorf("baseline %s holds ZERO entries — it proves nothing; regenerate it with `go run ./cmd/explain-differ dump`", describe(b))
		case len(b.Entries) < MinBaselineEntries:
			return fmt.Errorf("baseline %s holds only %d entries (floor is %d) — the dump did not cover the corpus; regenerate it",
				describe(b), len(b.Entries), MinBaselineEntries)
		}
	}
	lo, hi := len(oldB.Entries), len(newB.Entries)
	if lo > hi {
		lo, hi = hi, lo
	}
	if drift := float64(hi-lo) / float64(hi); drift > MaxEntryCountDrift {
		return fmt.Errorf(
			"baselines differ by %.1f%% in entry count (old=%d new=%d, tolerance %.0f%%) — one dump is truncated or ran against a different corpus; regenerate both",
			drift*100, len(oldB.Entries), len(newB.Entries), MaxEntryCountDrift*100)
	}
	return nil
}

// checkDistinctSources rejects diffing a file against itself. It is always
// operator error (a repeated shell variable, a copy-pasted path) and it
// always reports CLEAN, which reads as "the change moved no plan" — the
// exact conclusion the harness exists to justify.
//
// The check is on the SOURCE, not the content: two dumps of the same tree
// are byte-identical by design, and that comparison is the legitimate
// smoke test that the harness still works.
func checkDistinctSources(oldPath, newPath string) error {
	if oldPath == "" || newPath == "" {
		return nil // in-memory baselines; there is no file to confuse.
	}
	same := oldPath == newPath
	if !same {
		if a, err := filepath.Abs(oldPath); err == nil {
			if bp, err := filepath.Abs(newPath); err == nil {
				same = a == bp
			}
		}
	}
	if !same {
		// Catches the symlink / hard-link spellings of the same file that a
		// string comparison misses.
		if ai, err := os.Stat(oldPath); err == nil {
			if bi, err := os.Stat(newPath); err == nil {
				same = os.SameFile(ai, bi)
			}
		}
	}
	if same {
		return fmt.Errorf("refusing to diff %q against itself: a file always matches itself, so the CLEAN verdict would be meaningless — pass the BEFORE and AFTER baselines", oldPath)
	}
	return nil
}

// describe names a baseline for an error message: its path when it has one.
func describe(b *Baseline) string {
	if b.Path != "" {
		return b.Path
	}
	return "<in-memory>"
}
