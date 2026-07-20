package main

import (
	"os"
	"strings"
	"testing"
)

func writeRaceLog(t *testing.T, labels ...string) string {
	t.Helper()
	path := t.TempDir() + "/races"
	if len(labels) > 0 {
		if err := os.WriteFile(path, []byte(strings.Join(labels, "\n")+"\n"), 0o644); err != nil {
			t.Fatalf("writing race log: %v", err)
		}
	}
	return path
}

func TestSummarizeRaces(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		labels   []string
		total    int
		wantCode int
		wantIn   []string
	}{
		// The common case: no races at all, and nothing to say.
		"no races": {nil, 33, 0, nil},
		// Incidental — warn, but the gate still passes.
		"one of 33": {[]string{"FuzzA"}, 33, 0, []string{"::warning::", "1 of 33"}},
		"half of 6": {[]string{"FuzzA", "FuzzB", "FuzzC"}, 6, 0, []string{"::warning::"}},
		// Systematic — a majority means the toolchain changed, not bad luck.
		"majority of 6": {
			[]string{"FuzzA", "FuzzB", "FuzzC", "FuzzD"},
			6, 1,
			[]string{"::error::", "systematic"},
		},
		"all of 3": {
			[]string{"FuzzA", "FuzzB", "FuzzC"},
			3, 1,
			[]string{"::error::", "3 of 3"},
		},
	}
	for name, tc := range cases {
		path := writeRaceLog(t, tc.labels...)
		var sb strings.Builder
		if code := summarizeRaces(path, tc.total, &sb); code != tc.wantCode {
			t.Errorf("%s: exit = %d, want %d (out=%q)", name, code, tc.wantCode, sb.String())
		}
		for _, want := range tc.wantIn {
			if !strings.Contains(sb.String(), want) {
				t.Errorf("%s: output missing %q; got %q", name, want, sb.String())
			}
		}
		if len(tc.labels) == 0 && sb.String() != "" {
			t.Errorf("%s: expected silence, got %q", name, sb.String())
		}
	}
}

// A missing race log is the no-races case, not an error: the nightly only creates it
// when something races.
func TestSummarizeRaces_MissingLogIsClean(t *testing.T) {
	t.Parallel()
	var sb strings.Builder
	if code := summarizeRaces(t.TempDir()+"/never-written", 10, &sb); code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if sb.String() != "" {
		t.Errorf("expected silence, got %q", sb.String())
	}
}

// Guard against a divide-by-zero / vacuous-majority reading when the caller forgets
// -total: warn, but never fail the job on an unknowable ratio.
func TestSummarizeRaces_ZeroTotalNeverFails(t *testing.T) {
	t.Parallel()
	path := writeRaceLog(t, "FuzzA", "FuzzB")
	var sb strings.Builder
	if code := summarizeRaces(path, 0, &sb); code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if !strings.Contains(sb.String(), "::warning::") {
		t.Error("races should still be reported when -total is unknown")
	}
}
