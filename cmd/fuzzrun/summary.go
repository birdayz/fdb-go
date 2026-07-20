package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// systematicRaceNumerator/Denominator define "systematic": more than half the
// targets in one run hitting the race. Incidentally the race is ~1% per run, so a
// majority is not bad luck — it means the Go SDK's fuzz coordinator changed and
// this tool's assumptions need re-checking before the gate can be trusted again.
const (
	systematicRaceNumerator   = 1
	systematicRaceDenominator = 2
)

// summarizeRaces reports how many targets hit the budget-expiry race and returns the
// process exit code.
//
// Individual raced runs pass — the classifier established each consumed its full
// budget and found nothing. But "most targets raced tonight" is real information
// about the toolchain that would otherwise exist only as per-target warnings inside
// a green job, which is to say nowhere. This turns that into one loud signal.
func summarizeRaces(racelogPath string, total int, w io.Writer) int {
	labels := readRaceLog(racelogPath)
	if len(labels) == 0 {
		return 0
	}

	sort.Strings(labels)
	fmt.Fprintf(w, "::warning::%d of %d fuzz targets hit the golang/go#72104 budget-expiry "+
		"race tonight: %s\n", len(labels), total, strings.Join(labels, ", "))

	// minSystematicSample keeps a tiny run from reading as systematic: 1 of 1 is a
	// majority arithmetically but says nothing at a ~1% per-run rate. Today's
	// rotations are 6 / ~9 / ~33 targets so this is unreachable, but the summary
	// must not become a false red the first time someone runs a one-target job.
	const minSystematicSample = 3
	if total >= minSystematicSample &&
		len(labels)*systematicRaceDenominator > total*systematicRaceNumerator {
		fmt.Fprintf(w, "::error::the budget-expiry race hit %d of %d targets — that is "+
			"systematic, not incidental (it is normally ~1%% per run). The Go toolchain's "+
			"fuzz coordinator has most likely changed; re-verify cmd/fuzzrun's assumptions "+
			"against $GOROOT/src/internal/fuzz/fuzz.go before trusting this gate again.\n",
			len(labels), total)
		return 1
	}
	return 0
}

// readRaceLog returns the labels recorded by -racelog. A missing file means no races,
// which is the common case and not an error.
func readRaceLog(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var labels []string
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			labels = append(labels, line)
		}
	}
	return labels
}
