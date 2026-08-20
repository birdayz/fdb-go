package embedded

import (
	"errors"
	"reflect"
	"testing"

	"fdb.dev/pkg/recordlayer"
)

// decideStatistics is the whole read-side gate (RFC-236). It is exercised here
// rather than through a cluster because two of its arms need a cluster
// MISBEHAVING — a version read that fails, an entry stamped ahead of the
// cluster after a restore from backup — and an arm no test drives fires for the
// first time in front of an operator, where it reads as a finding rather than
// as an untested branch.
//
// TestDecideStatisticsCoversEveryRefusal is the vacuity guard: adding a refusal
// constant without a case here fails the build. A census whose rare arms are
// only reached by whatever the corpus happens to hit is not a census.

const testVersion int64 = 1_000_000_000_000

func statsAt(version int64, counts map[string]int64) recordlayer.StoreStatistics {
	s := recordlayer.StoreStatistics{
		PerType:            make(map[string]recordlayer.RecordTypeStatistic, len(counts)),
		CollectedAtVersion: version,
	}
	for name, c := range counts {
		s.PerType[name] = recordlayer.RecordTypeStatistic{Count: c, CollectedAtVersion: version}
	}
	return s
}

type decideCase struct {
	name string
	in   statisticsGateInput
	want StatisticsRefusal
	// check runs extra assertions on a verdict whose refusal already matched,
	// for the fields the CLI renders.
	check func(t *testing.T, got StatisticsStatus)
}

func decideStatisticsCases() []decideCase {
	return []decideCase{
		{
			name: "usable",
			in: statisticsGateInput{
				Found:          true,
				Stats:          statsAt(testVersion, map[string]int64{"A": 7, "B": 400}),
				CurrentVersion: testVersion + 1000,
				DeclaredTypes:  []string{"A", "B"},
			},
			want: StatisticsOK,
			check: func(t *testing.T, got StatisticsStatus) {
				if !got.Usable {
					t.Errorf("Usable = false for the OK arm")
				}
				want := map[string]float64{"A": 7, "B": 400}
				if !reflect.DeepEqual(got.perType, want) {
					t.Errorf("perType = %v, want %v", got.perType, want)
				}
				if got.AgeVersions != 1000 {
					t.Errorf("AgeVersions = %d, want 1000", got.AgeVersions)
				}
			},
		},
		{
			name: "read failed",
			in: statisticsGateInput{
				ReadErr:       errors.New("transaction too old"),
				DeclaredTypes: []string{"A"},
			},
			want: StatisticsReadFailed,
			check: func(t *testing.T, got StatisticsStatus) {
				// A failed read must not claim statistics exist — an operator
				// reading "found, but unusable" would go looking for a stale
				// entry that may not be there at all.
				if got.Found {
					t.Errorf("Found = true after a failed read")
				}
			},
		},
		{
			name: "never collected",
			in: statisticsGateInput{
				Found:          false,
				CurrentVersion: testVersion,
				DeclaredTypes:  []string{"A"},
			},
			want: StatisticsNotCollected,
			check: func(t *testing.T, got StatisticsStatus) {
				if got.Found {
					t.Errorf("Found = true when nothing was collected")
				}
			},
		},
		{
			name: "version read failed",
			in: statisticsGateInput{
				Found:         true,
				Stats:         statsAt(testVersion, map[string]int64{"A": 7}),
				VersionErr:    errors.New("cluster unreachable"),
				DeclaredTypes: []string{"A"},
			},
			want: StatisticsVersionUnavailable,
			check: func(t *testing.T, got StatisticsStatus) {
				// Found stays true: the entry IS there, its age is what is
				// unknown. That distinction is the whole reason Found is a
				// separate field from Usable.
				if !got.Found {
					t.Errorf("Found = false though the entry was read")
				}
			},
		},
		{
			name: "stamped ahead of the cluster",
			in: statisticsGateInput{
				Found: true,
				Stats: statsAt(testVersion+1, map[string]int64{"A": 7}),
				// A restore from backup moves the cluster's version BACKWARDS,
				// leaving an entry stamped in the abandoned future. Read as
				// "age is negative, therefore fresh", it would never expire.
				CurrentVersion: testVersion,
				DeclaredTypes:  []string{"A"},
			},
			want: StatisticsStampedInFuture,
			check: func(t *testing.T, got StatisticsStatus) {
				if got.AgeVersions >= 0 {
					t.Errorf("AgeVersions = %d, want negative", got.AgeVersions)
				}
			},
		},
		{
			name: "exactly at the age bound is still fresh",
			in: statisticsGateInput{
				Found:          true,
				Stats:          statsAt(testVersion, map[string]int64{"A": 7}),
				CurrentVersion: testVersion + statisticsMaxAgeVersions,
				DeclaredTypes:  []string{"A"},
			},
			// The bound is inclusive. Pinned because an off-by-one here is
			// invisible in production: it only ever costs a plan quality, never
			// a wrong row, so nothing would ever report it.
			want: StatisticsOK,
		},
		{
			name: "one version past the bound is expired",
			in: statisticsGateInput{
				Found:          true,
				Stats:          statsAt(testVersion, map[string]int64{"A": 7}),
				CurrentVersion: testVersion + statisticsMaxAgeVersions + 1,
				DeclaredTypes:  []string{"A"},
			},
			want: StatisticsExpired,
		},
		{
			name: "incomplete — a declared type has no entry",
			in: statisticsGateInput{
				Found:          true,
				Stats:          statsAt(testVersion, map[string]int64{"A": 7}),
				CurrentVersion: testVersion + 1,
				DeclaredTypes:  []string{"A", "B", "C"},
			},
			want: StatisticsIncomplete,
			check: func(t *testing.T, got StatisticsStatus) {
				want := []string{"B", "C"}
				if !reflect.DeepEqual(got.MissingTypes, want) {
					t.Errorf("MissingTypes = %v, want %v (sorted)", got.MissingTypes, want)
				}
			},
		},
		{
			name: "empty schema",
			in: statisticsGateInput{
				Found:          true,
				Stats:          statsAt(testVersion, nil),
				CurrentVersion: testVersion + 1,
				DeclaredTypes:  nil,
			},
			want: StatisticsEmptySchema,
		},
		{
			name: "an orphan entry does not refuse",
			in: statisticsGateInput{
				Found: true,
				// DROPPED was collected before the table was dropped. The
				// planner asks by declared type name, so it never asks for it.
				Stats:          statsAt(testVersion, map[string]int64{"A": 7, "DROPPED": 99}),
				CurrentVersion: testVersion + 1,
				DeclaredTypes:  []string{"A"},
			},
			want: StatisticsOK,
			check: func(t *testing.T, got StatisticsStatus) {
				if want := []string{"DROPPED"}; !reflect.DeepEqual(got.ExtraTypes, want) {
					t.Errorf("ExtraTypes = %v, want %v", got.ExtraTypes, want)
				}
				// And the orphan must not reach the provider — its count would
				// inflate the whole-store total that an unknown-type leaf reads.
				if _, leaked := got.perType["DROPPED"]; leaked {
					t.Errorf("perType carries the dropped type: %v", got.perType)
				}
			},
		},
	}
}

func TestDecideStatistics(t *testing.T) {
	t.Parallel()
	for _, tc := range decideStatisticsCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := decideStatistics(tc.in)
			if got.Refusal != tc.want {
				t.Fatalf("Refusal = %q, want %q (Usable=%v)", got.Refusal, tc.want, got.Usable)
			}
			// Usable and Refusal are two spellings of one fact; a verdict where
			// they disagree would let a caller checking either one be right and
			// the other be wrong.
			if got.Usable != (tc.want == StatisticsOK) {
				t.Errorf("Usable = %v but Refusal = %q — the two disagree", got.Usable, got.Refusal)
			}
			// A refusal must never hand the planner a provider input.
			if !got.Usable && got.perType != nil {
				t.Errorf("refused with %q but perType = %v", got.Refusal, got.perType)
			}
			if got.MaxAgeVersions != statisticsMaxAgeVersions {
				t.Errorf("MaxAgeVersions = %d, want %d", got.MaxAgeVersions, statisticsMaxAgeVersions)
			}
			if tc.check != nil {
				tc.check(t, got)
			}
		})
	}
}

// TestDecideStatisticsCoversEveryRefusal is the vacuity guard. Every refusal
// constant must be produced by at least one case above, so a new gate cannot be
// added with no test driving it — the failure mode this whole file exists to
// prevent.
func TestDecideStatisticsCoversEveryRefusal(t *testing.T) {
	t.Parallel()
	all := []StatisticsRefusal{
		StatisticsOK,
		StatisticsNotCollected,
		StatisticsReadFailed,
		StatisticsVersionUnavailable,
		StatisticsStampedInFuture,
		StatisticsExpired,
		StatisticsIncomplete,
		StatisticsEmptySchema,
	}
	covered := map[StatisticsRefusal]int{}
	for _, tc := range decideStatisticsCases() {
		covered[decideStatistics(tc.in).Refusal]++
	}
	for _, r := range all {
		if covered[r] == 0 {
			t.Errorf("no case in decideStatisticsCases produces refusal %q — add one", r)
		}
	}
	// And the reverse: a verdict the list does not name means a constant was
	// added without being registered here, which would silently shrink the
	// guard above to whatever it still happens to cover.
	named := map[StatisticsRefusal]bool{}
	for _, r := range all {
		named[r] = true
	}
	for r := range covered {
		if !named[r] {
			t.Errorf("cases produce refusal %q, which this guard's list does not name", r)
		}
	}
	if len(covered) != len(all) {
		t.Errorf("covered %d distinct refusals, want %d", len(covered), len(all))
	}
}
