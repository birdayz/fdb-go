// Command vulnscan runs govulncheck as a CI security gate that cannot be
// silenced by an upstream outage and cannot silently pass because of one.
//
// It exists because the obvious invocation —
//
//	go run golang.org/x/vuln/cmd/govulncheck@latest ./pkg/...
//
// has three independent defects, and the fix for each is why a wrapper is
// warranted at all:
//
//  1. It fetches the vulnerability database over the network on EVERY job,
//     one request per advisory. A fleet of self-hosted runners sharing a small
//     block of addresses gets rate-limited into `403 Forbidden`, and the job
//     reports that as a FAILED SECURITY GATE. Fixed by mirroring the database
//     locally (mirror.go): one request per refresh window, zero in steady
//     state.
//
//  2. `@latest` re-resolves the tool on every run, so the gate's verdict can
//     change with no diff in this repository — and the resolution is itself
//     another network dependency that can fail. Fixed by pinning
//     govulncheckVersion below.
//
//  3. Its exit code alone cannot distinguish "found nothing" from "could not
//     look". Pointed at an empty directory, govulncheck exits 0 and emits a
//     well-formed, zero-finding report. Fixed by classifying on the scanner's
//     own machine-readable output (classify.go), which is why this runs with
//     `-format json` rather than parsing prose.
//
//  4. Its coverage of the STANDARD LIBRARY is silently conditional on the
//     toolchain's version string, and a run whose stdlib half never happened is
//     byte-for-byte indistinguishable from a clean one. Measured: under a
//     locally rebuilt `go1.26.5-X:nodwarf5` toolchain the scan reported no
//     called vulnerabilities against the same database on which stock go1.26.5
//     reports four. Fixed by making a clean verdict require a released
//     toolchain (classify.go), so the gate is only quiet when it actually
//     looked everywhere it claims to.
//
// The two halves are held apart deliberately. A fetch failure is retried and,
// with a mirror inside the staleness bound, tolerated — that is defect 1. A
// scan that ran and found a called vulnerability fails the build, and a scan
// that could not consult a database fails the build with a DIFFERENT message —
// neither is ever reported as clean. Retries apply only to the fetch, never to
// a verdict, so no amount of retrying can turn a real finding into a pass.
//
// Fleet-wide serialisation via a workflow `concurrency:` group was considered
// and rejected: it would queue five runners behind one scan, adding PR latency
// proportional to fleet size, and it only narrows the window — a single
// serialised job still issues hundreds of per-advisory requests and can still
// be refused. The mirror removes the requests instead of spacing them out.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// govulncheckVersion pins the scanner.
//
// A moving tool version inside a gate is its own defect: it can flip the build
// from green to red with no change in this repository, which makes the failure
// unattributable and trains everyone to rerun it. Bumping this is a reviewable
// diff, which is the point.
const govulncheckVersion = "v1.6.0"

// scanPatterns is the shipped surface. pkg/testcontainers is excluded because
// it wraps the Docker SDK — TEST infrastructure, not shipped to users — which
// carries open upstream advisories with no fixed release (GO-2026-4887,
// GO-2026-4883). See SECURITY.md. Everything else must be vulnerability-free.
var scanPatterns = []string{"./pkg/..."}

const excludeSubstring = "/testcontainers"

func main() {
	var (
		cacheDir = flag.String("cache", defaultCacheDir(), "directory holding the vulnerability database mirror and pinned scanner")
		attempts = flag.Int("attempts", 4, "fetch attempts for the vulnerability database")
		// Overridable so the gate itself can be aimed at a module with a known
		// vulnerable dependency and observed to fail. A gate never exercised
		// against a real finding is only known to be quiet.
		patterns = flag.String("patterns", strings.Join(scanPatterns, ","), "comma-separated package patterns to scan")
	)
	flag.Parse()

	if err := run(*cacheDir, *attempts, strings.Split(*patterns, ",")); err != nil {
		fmt.Fprintf(os.Stderr, "::error::%v\n", err)
		os.Exit(1)
	}
}

// defaultCacheDir prefers the persistent CI volume so the mirror survives job
// boundaries, and falls back to a user cache so the tool is runnable on a
// developer machine that has no such volume.
func defaultCacheDir() string {
	if v := os.Getenv("VULNSCAN_CACHE"); v != "" {
		return v
	}
	if fi, err := os.Stat("/mnt/ci-data"); err == nil && fi.IsDir() {
		return "/mnt/ci-data/vulndb-mirror"
	}
	if home, err := os.UserCacheDir(); err == nil {
		return filepath.Join(home, "fdb-vulnscan")
	}
	return filepath.Join(os.TempDir(), "fdb-vulnscan")
}

func run(cacheDir string, attempts int, patterns []string) error {
	now := time.Now()
	mirror := Mirror{Root: cacheDir}

	present, fetchedAt := mirror.State()
	decision := DecideMirror(present, fetchedAt, now)
	logf("vulnerability database mirror: present=%t decision=%s", present, decision)

	if decision != MirrorUseAsIs {
		client := &http.Client{Timeout: 5 * time.Minute}
		// Overridable so the refresh-failure path can be exercised, and so
		// the fleet can be repointed at an internal mirror without a code
		// change. It is not a way to weaken the gate: whatever it serves must
		// still pass ValidateDB, and the scanner's own db_last_modified must
		// still survive the staleness bound.
		url := vulnDBZipURL
		if v := os.Getenv("VULNSCAN_DB_URL"); v != "" {
			url = v
		}
		fetch := FetchVulnDBZip(client, url, attempts, time.Sleep)
		if err := mirror.Refresh(now, fetch); err != nil {
			proceed, reason := MayProceedAfterFailedRefresh(present, fetchedAt, now)
			if !proceed {
				// The gate refuses to render a verdict rather than rendering a
				// meaningless one. This is deliberately an error and not a
				// pass: "could not consult the database" must never be
				// reportable as "no vulnerabilities found".
				return fmt.Errorf("%s: %w", reason, err)
			}
			warnf("%s (fetch error: %v)", reason, err)
		} else {
			_, fetchedAt = mirror.State()
			logf("refreshed the vulnerability database mirror at %s", fetchedAt.Format(time.RFC3339))
		}
	}

	if err := ValidateDB(mirror.DBDir()); err != nil {
		return fmt.Errorf("no usable vulnerability database to scan against: %w", err)
	}

	bin, err := ensureGovulncheck(cacheDir)
	if err != nil {
		return fmt.Errorf("preparing the pinned scanner: %w", err)
	}

	pkgs, err := listPackages(patterns)
	if err != nil {
		return fmt.Errorf("enumerating packages to scan: %w", err)
	}
	if len(pkgs) == 0 {
		// An empty package list would scan nothing and report nothing, which
		// is the same silent pass in a different costume.
		return fmt.Errorf("the package list came back empty, so the scan would have covered nothing")
	}
	logf("scanning %d packages against %s", len(pkgs), mirror.DBDir())

	args := append([]string{"-db", "file://" + mirror.DBDir(), "-format", "json"}, pkgs...)
	cmd := exec.Command(bin, args...)
	cmd.Stderr = os.Stderr
	out, runErr := cmd.Output()
	exitCode := 0
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			return fmt.Errorf("running the scanner: %w", runErr)
		}
	}

	res := Classify(exitCode, strings.NewReader(string(out)), now)
	return report(res)
}

func report(res Result) error {
	if len(res.ImportedOSVs) > 0 {
		logf("present in the dependency graph but not called: %s", strings.Join(res.ImportedOSVs, ", "))
	}
	switch res.Verdict {
	case VerdictClean:
		logf("no called vulnerabilities; database published %s", res.DBLastModified.Format(time.RFC3339))
		return nil
	case VerdictVulnerable:
		return fmt.Errorf("this code calls %d known vulnerability/ies: %s (database published %s)",
			len(res.CalledOSVs), strings.Join(res.CalledOSVs, ", "), res.DBLastModified.Format(time.RFC3339))
	case VerdictNotConsulted:
		return fmt.Errorf("the vulnerability database was not usably consulted, so this scan is not evidence of anything: %s", res.Detail)
	default:
		return fmt.Errorf("the vulnerability scan did not complete: %s", res.Detail)
	}
}

// ensureGovulncheck installs the pinned scanner into a version-keyed directory
// and reuses it thereafter, so the steady-state job touches neither the module
// proxy nor the vulnerability database.
func ensureGovulncheck(cacheDir string) (string, error) {
	binDir := filepath.Join(cacheDir, "bin", govulncheckVersion)
	bin := filepath.Join(binDir, "govulncheck")
	if _, err := os.Stat(bin); err == nil {
		return bin, nil
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", err
	}
	cmd := exec.Command("go", "install", "golang.org/x/vuln/cmd/govulncheck@"+govulncheckVersion)
	cmd.Env = append(os.Environ(), "GOBIN="+binDir)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("go install govulncheck@%s: %w", govulncheckVersion, err)
	}
	return bin, nil
}

func listPackages(patterns []string) ([]string, error) {
	cmd := exec.Command("go", append([]string{"list"}, patterns...)...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var pkgs []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, excludeSubstring) {
			continue
		}
		pkgs = append(pkgs, line)
	}
	return pkgs, nil
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "vulnscan: "+format+"\n", args...)
}

func warnf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "::warning::vulnscan: "+format+"\n", args...)
}
