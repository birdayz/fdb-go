package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// This file is the I/O shell: it fetches state from GitHub and prints. Every
// decision lives in classify.go, which takes explicit state and is driven
// arm-by-arm from tests. The split is deliberate — a gate whose logic can only
// be exercised by talking to GitHub is a gate whose rare arms are never
// exercised at all.

func main() {
	var (
		repo         = flag.String("repo", envOr("GITHUB_REPOSITORY", ""), "owner/name")
		workflowDir  = flag.String("workflows", ".github/workflows", "directory holding workflow definitions")
		graceMinutes = flag.Int("grace-minutes", 30, "an OPEN pull request younger than this may legitimately have checks that have not registered yet")
		mergedDays   = flag.Int("merged-days", 14, "also audit pull requests merged within this many days (0 disables)")
	)
	flag.Parse()

	if *repo == "" {
		fatal("no repository: pass -repo owner/name or set GITHUB_REPOSITORY")
	}

	expected, err := expectedFromDir(*workflowDir)
	if err != nil {
		fatal("deriving the expected check set: %v", err)
	}
	fmt.Printf("expected checks on every pull request (%d, derived from %s):\n", len(expected), *workflowDir)
	for _, e := range expected {
		fmt.Printf("  - %s\n", e)
	}
	fmt.Println()

	prs, err := listPRs(*repo, *mergedDays)
	if err != nil {
		fatal("listing pull requests: %v", err)
	}
	// NO-OP GUARD, the sibling of the vacuity guard in ExpectedChecks: a sweep
	// that iterates zero pull requests passes green having audited nothing. An
	// empty list is plausible (a quiet repository), so this warns rather than
	// fails — but it must never be silent, because "no PRs" and "the query
	// broke" look identical in the exit code.
	if len(prs) == 0 {
		fmt.Println("::warning::no open or recently-merged pull requests matched — this sweep audited nothing")
		return
	}

	violations := 0
	for _, pr := range prs {
		observed, err := observedChecks(*repo, pr.HeadSHA)
		if err != nil {
			fatal("fetching checks for #%d (%s): %v", pr.Number, pr.HeadSHA, err)
		}
		verdicts := Classify(expected, observed)
		bad := Unsatisfied(verdicts)

		// An open pull request inside the grace window is allowed to be
		// incomplete; a MERGED one never is, whatever its age.
		young := !pr.Merged && time.Since(pr.UpdatedAt) < time.Duration(*graceMinutes)*time.Minute
		if len(bad) == 0 {
			fmt.Printf("ok   #%-5d %-42s all %d expected checks PASSED\n", pr.Number, pr.HeadRef, len(expected))
			continue
		}
		if young && !anyState(bad, StateFailed) {
			fmt.Printf("wait #%-5d %-42s within the %dm grace window\n", pr.Number, pr.HeadRef, *graceMinutes)
			continue
		}
		// A merged pull request is reported separately because it is a
		// different fact: not "this must not merge" but "this ALREADY merged
		// without the evidence", which is what happened to #637.
		kind := "OPEN"
		if pr.Merged {
			kind = "MERGED"
		}
		fmt.Printf("::error::%s pull request #%d (%s) does not carry its required checks\n", kind, pr.Number, pr.HeadRef)
		for _, v := range bad {
			fmt.Printf("    %-8s %-46s %s\n", v.State, v.Name, v.Why)
		}
		if pr.Merged {
			fmt.Printf("    #%d merged at %s with the above unmet. Merging on \"no checks reported\" reads\n"+
				"    \"nothing failed\" out of \"nothing ran\" — the two are not the same state.\n",
				pr.Number, pr.MergedAt.Format(time.RFC3339))
		}
		violations++
	}

	if violations > 0 {
		fmt.Printf("\n%d pull request(s) lack required checks.\n", violations)
		os.Exit(1)
	}
	fmt.Printf("\nall %d audited pull request(s) carry their required checks.\n", len(prs))
}

func anyState(vs []Verdict, s CheckState) bool {
	for _, v := range vs {
		if v.State == s {
			return true
		}
	}
	return false
}

// expectedFromDir reads every workflow file and derives the required set.
func expectedFromDir(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	workflows := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || (!strings.HasSuffix(e.Name(), ".yml") && !strings.HasSuffix(e.Name(), ".yaml")) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		workflows[e.Name()] = string(raw)
	}
	return ExpectedChecks(workflows)
}

type pullRequest struct {
	Number    int
	HeadRef   string
	HeadSHA   string
	Merged    bool
	UpdatedAt time.Time
	MergedAt  time.Time
}

func listPRs(repo string, mergedDays int) ([]pullRequest, error) {
	states := []string{"open"}
	if mergedDays > 0 {
		states = append(states, "merged")
	}
	var out []pullRequest
	for _, state := range states {
		raw, err := gh("pr", "list", "--repo", repo, "--state", state, "--limit", "100",
			"--json", "number,headRefName,headRefOid,mergedAt,updatedAt,state")
		if err != nil {
			return nil, err
		}
		var items []struct {
			Number     int        `json:"number"`
			HeadRef    string     `json:"headRefName"`
			HeadRefOid string     `json:"headRefOid"`
			MergedAt   *time.Time `json:"mergedAt"`
			UpdatedAt  time.Time  `json:"updatedAt"`
		}
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, fmt.Errorf("decode %s pull requests: %w", state, err)
		}
		for _, it := range items {
			pr := pullRequest{
				Number: it.Number, HeadRef: it.HeadRef, HeadSHA: it.HeadRefOid,
				UpdatedAt: it.UpdatedAt,
			}
			if it.MergedAt != nil {
				if time.Since(*it.MergedAt) > time.Duration(mergedDays)*24*time.Hour {
					continue
				}
				pr.Merged, pr.MergedAt = true, *it.MergedAt
			}
			out = append(out, pr)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

// observedChecks merges the two INDEPENDENT surfaces GitHub reports results on.
// Check runs come from Actions; commit statuses come from the older API and
// from anything posting via `gh api .../statuses/`. A gate that read only one
// would call a perfectly green pull request absent.
func observedChecks(repo, sha string) ([]ObservedCheck, error) {
	var observed []ObservedCheck

	raw, err := gh("api", "--paginate", fmt.Sprintf("repos/%s/commits/%s/check-runs", repo, sha),
		"--jq", ".check_runs[] | {id, name, status, conclusion}")
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var cr struct {
			ID                       int64 `json:"id"`
			Name, Status, Conclusion string
		}
		if err := json.Unmarshal([]byte(line), &cr); err != nil {
			return nil, fmt.Errorf("decode check run %q: %w", line, err)
		}
		observed = append(observed, ObservedCheck{ID: cr.ID, Name: cr.Name, Status: cr.Status, Conclusion: cr.Conclusion})
	}

	// The COMBINED status endpoint already reports one entry per context —
	// the latest — so these need no id-ordering of their own.
	raw, err = gh("api", fmt.Sprintf("repos/%s/commits/%s/status", repo, sha),
		"--jq", ".statuses[] | {context, state}")
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var st struct{ Context, State string }
		if err := json.Unmarshal([]byte(line), &st); err != nil {
			return nil, fmt.Errorf("decode commit status %q: %w", line, err)
		}
		// Commit statuses have no status/conclusion split; map onto it so both
		// surfaces reach the same classifier.
		oc := ObservedCheck{Name: st.Context, Status: "completed"}
		switch st.State {
		case "success":
			oc.Conclusion = "success"
		case "pending":
			oc.Status, oc.Conclusion = "in_progress", ""
		default:
			oc.Conclusion = st.State
		}
		observed = append(observed, oc)
	}
	return observed, nil
}

func gh(args ...string) ([]byte, error) {
	cmd := exec.Command("gh", args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "::error::"+format+"\n", args...)
	os.Exit(1)
}
