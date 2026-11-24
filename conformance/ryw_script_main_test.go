//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gofdb "fdb.dev/pkg/fdbgo/fdb"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
	"github.com/testcontainers/testcontainers-go"
)

var (
	sharedContainer *foundationdbtc.Container
	sharedDB        gofdb.Database
)

const (
	rywTotalScenarios = 31
	rywTotalTestFuncs = 15
)

type rywPopulation struct {
	mu                 sync.Mutex
	Scenarios          int
	Lines              int
	Pinned             int
	Conflicts          int
	ConflictingReports int
	Reads              int
}

var rywPop rywPopulation

func (p *rywPopulation) record(javaOut []string, pinned int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Scenarios++
	p.Lines += len(javaOut)
	p.Pinned += pinned
	for _, l := range javaOut {
		f := strings.Fields(l)
		switch {
		case f[2] == "commit" && f[3] == "err=1020":
			p.Conflicts++
		case f[2] == "conflicting" && f[3] != "[]":
			p.ConflictingReports++
		case f[2] == "get" || f[2] == "getKey" || f[2] == "getRange" || f[2] == "await":
			p.Reads++
		}
	}
}

func (p *rywPopulation) snapshot() (scenarios, lines, pinned, conflicts, conflictingReports, reads int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.Scenarios, p.Lines, p.Pinned, p.Conflicts, p.ConflictingReports, p.Reads
}

// require verifies the scenario and population counts for a single TestRYWScript_*
// function. Because each test asserts its own slice of the 31 scenarios, 13
// conflicts/conflicting reports (>= 10), and 311 reads (>= 150), population
// non-vacuity holds regardless of Bazel shard_count and surfaces directly in
// JUnit test.xml.
func (p *rywPopulation) require(t *testing.T, wantScenarios, minConflicts, minConflictingReports, minReads int) {
	t.Helper()
	if t.Failed() {
		return
	}
	scenarios, _, _, conflicts, conflictingReports, reads := p.snapshot()
	if scenarios != wantScenarios {
		t.Errorf("population: ran %d scenarios, want %d", scenarios, wantScenarios)
	}
	if conflicts < minConflicts {
		t.Errorf("population: Conflicts = %d, want >= %d", conflicts, minConflicts)
	}
	if conflictingReports < minConflictingReports {
		t.Errorf("population: ConflictingReports = %d, want >= %d", conflictingReports, minConflictingReports)
	}
	if reads < minReads {
		t.Errorf("population: Reads = %d, want >= %d", reads, minReads)
	}
}

func TestMain(m *testing.M) {
	os.Exit(runRYWScriptMain(m))
}

func runRYWScriptMain(m *testing.M) (code int) {
	suiteCtx := context.Background()
	setupCtx, setupCancel := context.WithTimeout(suiteCtx, 2*time.Minute)
	defer setupCancel()

	knobs, err := rywServerKnobs()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	fmt.Fprintf(os.Stderr, "🚀 Starting shared FDB container for test suite...\n")
	fmt.Fprintf(os.Stderr, "FDB server knobs from CONFORMANCE_FDB_KNOBS: %q\n", knobs)

	opts := []testcontainers.ContainerCustomizer{foundationdbtc.WithAPIVersion(720), foundationdbtc.WithDirectIP()}
	for _, k := range knobs {
		opts = append(opts, foundationdbtc.WithKnob(k[0], k[1]))
	}
	sharedContainer, err = foundationdbtc.Run(setupCtx, "", opts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start shared FDB container: %v\n", err)
		return 1
	}

	defer func() {
		// Shut down Java conformance servers before terminating FDB container,
		// so they can perform graceful shutdown while FDB is still available.
		CloseJavaInvoker()
		CloseAllJavaServers()

		if sharedContainer != nil {
			fmt.Fprintf(os.Stderr, "🧹 Terminating shared FDB container...\n")
			if termErr := foundationdbtc.Terminate(suiteCtx, sharedContainer); termErr != nil {
				fmt.Fprintf(os.Stderr, "terminate shared FDB container: %v\n", termErr)
				code = 1
			}
		}
	}()

	for _, k := range knobs {
		if err := waitForServerKnob(setupCtx, sharedContainer, k[0], k[1]); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return 1
		}
	}

	sharedDB, err = openGoDatabase(setupCtx, sharedContainer)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open Go database: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "✅ Shared FDB container + database ready\n")

	code = m.Run()

	scenarios, lines, pinned, conflicts, conflictingReports, reads := rywPop.snapshot()
	fmt.Fprintf(os.Stderr, "ryw script conformance: {Scenarios:%d Lines:%d Pinned:%d Conflicts:%d ConflictingReports:%d Reads:%d}\n",
		scenarios, lines, pinned, conflicts, conflictingReports, reads)

	if code == 0 && (flag.Lookup("test.run") == nil || flag.Lookup("test.run").Value.String() == "") {
		totalShards := 1
		if s := os.Getenv("TEST_TOTAL_SHARDS"); s != "" {
			if n, err := strconv.Atoi(s); err == nil && n > 1 {
				totalShards = n
			}
		}
		if totalShards == 1 {
			if scenarios != rywTotalScenarios {
				fmt.Fprintf(os.Stderr, "ryw script conformance: ran %d scenarios, want %d\n", scenarios, rywTotalScenarios)
				code = 1
			}
			if conflicts < 10 {
				fmt.Fprintf(os.Stderr, "ryw script conformance: Conflicts = %d, want >= 10\n", conflicts)
				code = 1
			}
			if conflictingReports < 10 {
				fmt.Fprintf(os.Stderr, "ryw script conformance: ConflictingReports = %d, want >= 10\n", conflictingReports)
				code = 1
			}
			if reads < 150 {
				fmt.Fprintf(os.Stderr, "ryw script conformance: Reads = %d, want >= 150\n", reads)
				code = 1
			}
		} else {
			shardIdx := 0
			if s := os.Getenv("TEST_SHARD_INDEX"); s != "" {
				if n, err := strconv.Atoi(s); err == nil {
					shardIdx = n
				}
			}
			if shardIdx < rywTotalTestFuncs && (scenarios == 0 || reads == 0) {
				fmt.Fprintf(os.Stderr, "ryw script conformance shard %d/%d ran %d scenarios and %d reads, want > 0\n",
					shardIdx, totalShards, scenarios, reads)
				code = 1
			}
		}
	}

	return code
}

// rywServerKnobs parses CONFORMANCE_FDB_KNOBS ("name=value,..."), set by a target's env.
func rywServerKnobs() ([][2]string, error) {
	var knobs [][2]string
	for _, kv := range strings.Split(os.Getenv("CONFORMANCE_FDB_KNOBS"), ",") {
		if kv == "" {
			continue
		}
		name, value, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, fmt.Errorf("CONFORMANCE_FDB_KNOBS entry %q is not name=value", kv)
		}
		knobs = append(knobs, [2]string{name, value})
	}
	return knobs, nil
}

// waitForServerKnob waits until an fdbserver runs with the knob (fdb.bash passes it as "--knob_name value");
// recovery may restart the process.
func waitForServerKnob(ctx context.Context, c *foundationdbtc.Container, name, value string) error {
	re := regexp.MustCompile(`(^|\s)--knob_` + regexp.QuoteMeta(name) + `[= ]` + regexp.QuoteMeta(value) + `(\s|$)`)
	deadline := time.Now().Add(30 * time.Second)
	var last string
	for {
		_, out, err := c.Exec(ctx, []string{
			"/bin/bash", "-c",
			`pgrep fdbserver | while read pid; do tr '\0' ' ' < /proc/$pid/cmdline; echo; done`,
		})
		if err != nil {
			last = err.Error()
		} else {
			b, _ := io.ReadAll(out)
			last = string(b)
			if re.MatchString(last) {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("fdbserver knob %s=%s not observed within 30s (last output: %q)", name, value, last)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for fdbserver knob %s=%s: %w (last output: %q)", name, value, ctx.Err(), last)
		case <-time.After(500 * time.Millisecond):
		}
	}
}
