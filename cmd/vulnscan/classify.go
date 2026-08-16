package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"
)

// Verdict is the outcome of one govulncheck invocation.
//
// The whole point of this type is that "the database could not be consulted"
// is a DIFFERENT value from "the database was consulted and found nothing".
// A security gate that collapses those two is not a gate: it reports success
// whenever the thing it depends on is broken, which is precisely when you most
// need it to speak up.
type Verdict int

const (
	// VerdictClean means the scanner ran against a database it vouched for and
	// found no vulnerability that this code actually calls.
	VerdictClean Verdict = iota

	// VerdictVulnerable means the scanner found at least one SYMBOL-level
	// finding: a vulnerable function reachable from this module's code.
	VerdictVulnerable

	// VerdictNotConsulted means the scanner exited successfully but its own
	// output shows it did not have a usable database. This is the failure mode
	// that motivates the type: pointing govulncheck at an empty directory exits
	// 0 and emits a well-formed, zero-finding JSON stream. Only the config
	// message betrays it, by reporting the zero time as db_last_modified.
	VerdictNotConsulted

	// VerdictScanFailed means the scanner did not complete: non-zero exit,
	// unparseable output, or no config message at all. Network and rate-limit
	// failures land here.
	VerdictScanFailed
)

func (v Verdict) String() string {
	switch v {
	case VerdictClean:
		return "clean"
	case VerdictVulnerable:
		return "vulnerable"
	case VerdictNotConsulted:
		return "database-not-consulted"
	case VerdictScanFailed:
		return "scan-failed"
	}
	return fmt.Sprintf("Verdict(%d)", int(v))
}

// Result is a classified scan, including the detail needed to explain it.
type Result struct {
	Verdict Verdict

	// DBLastModified is the publication timestamp the SCANNER reported for the
	// database it actually read. It is deliberately taken from govulncheck's
	// output rather than stat'ed off the mirror directory: those two can
	// disagree (wrong -db flag, half-written mirror, a stale copy shadowing a
	// fresh one), and the only one that describes the data behind the verdict
	// is the scanner's own attestation.
	DBLastModified time.Time

	// CalledOSVs are the symbol-level findings — the ones that fail the build.
	CalledOSVs []string

	// ImportedOSVs are findings that exist in the dependency graph but which
	// this code does not call. Reported for context; they do not fail the
	// build, matching govulncheck's own text-mode exit code.
	ImportedOSVs []string

	// Detail explains a ScanFailed or NotConsulted verdict.
	Detail string
}

// maxDBPublicationAge bounds how old the DATABASE ITSELF may claim to be.
//
// This is not the freshness knob — mirror fetch age is (see mirror.go). Upstream
// publication cadence is not under our control and measurably lags: the live
// database has been observed reporting a db_last_modified six days behind
// wall-clock while being entirely current. A tight bound here would therefore
// redden the build for something no change in this repo can fix. Its real job
// is catching a database that is degenerate or abandoned — the zero time an
// empty directory produces, or a mirror frozen so far in the past that its
// verdict is meaningless.
const maxDBPublicationAge = 30 * 24 * time.Hour

// releaseToolchain matches the version strings govulncheck can resolve stdlib
// advisories against: a released Go toolchain, optionally a release candidate
// or beta. Anything else — `devel`, or the `-X:` suffix a locally rebuilt
// toolchain carries — is what the guard below exists to reject.
var releaseToolchain = regexp.MustCompile(`^go1\.\d+(\.\d+)?((rc|beta)\d+)?$`)

// govulnMessage is one object in govulncheck's `-format json` stream. The
// stream is a concatenation of top-level objects, not an array, so it is read
// with a streaming decoder.
type govulnMessage struct {
	Config *struct {
		DB             string `json:"db"`
		DBLastModified string `json:"db_last_modified"`
		ScannerVersion string `json:"scanner_version"`
		GoVersion      string `json:"go_version"`
	} `json:"config"`
	Finding *struct {
		OSV   string `json:"osv"`
		Trace []struct {
			Module   string `json:"module"`
			Package  string `json:"package"`
			Function string `json:"function"`
		} `json:"trace"`
	} `json:"finding"`
}

// Classify turns a govulncheck run into a Verdict.
//
// The classification is driven by the exit code plus the parsed JSON stream,
// never by pattern-matching the tool's prose. Matching on text (grepping for
// "403", say) would make the gate's verdict a function of an upstream error
// string, and would be imitable by any finding whose description happened to
// contain the magic token.
//
// Two facts about govulncheck make this clean, both measured against v1.6.0:
//   - Under `-format json` the exit code reports only whether the scan RAN.
//     Findings do not change it — a scan that reports four vulnerabilities
//     still exits 0. So a non-zero exit unambiguously means "no verdict", and
//     can be retried without any risk of retrying away a real finding.
//   - Every successful run emits exactly one config message carrying
//     db_last_modified. Its absence, or the zero time, means the scan produced
//     no evidence about anything.
func Classify(exitCode int, stdout io.Reader, now time.Time) Result {
	if exitCode != 0 {
		return Result{
			Verdict: VerdictScanFailed,
			Detail:  fmt.Sprintf("govulncheck exited %d without producing a verdict", exitCode),
		}
	}

	var (
		cfgSeen   bool
		modified  time.Time
		goVersion string
		called    []string
		imported  []string
		seen      = map[string]bool{}
	)

	dec := json.NewDecoder(stdout)
	for {
		var msg govulnMessage
		if err := dec.Decode(&msg); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return Result{
				Verdict: VerdictScanFailed,
				Detail:  fmt.Sprintf("govulncheck exited 0 but its JSON output did not parse: %v", err),
			}
		}

		if c := msg.Config; c != nil {
			cfgSeen = true
			if c.DBLastModified == "" {
				return Result{
					Verdict: VerdictNotConsulted,
					Detail:  "govulncheck reported no db_last_modified, so nothing establishes which database (if any) it read",
				}
			}
			t, err := time.Parse(time.RFC3339, c.DBLastModified)
			if err != nil {
				return Result{
					Verdict: VerdictNotConsulted,
					Detail:  fmt.Sprintf("govulncheck reported an unparseable db_last_modified %q: %v", c.DBLastModified, err),
				}
			}
			modified = t
			goVersion = c.GoVersion
		}

		if f := msg.Finding; f != nil {
			// govulncheck emits one finding per reachability level, so the same
			// OSV appears as separate module-, package- and symbol-level
			// entries. Only the symbol level means "this code calls it", which
			// is exactly the set govulncheck's own text mode exits non-zero
			// for. Failing on the others would redden the build for a
			// transitive package nothing reaches.
			symbol := len(f.Trace) > 0 && f.Trace[0].Function != ""
			key := f.OSV + "|"
			if symbol {
				key += "sym"
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			if symbol {
				called = append(called, f.OSV)
			} else {
				imported = append(imported, f.OSV)
			}
		}
	}

	if !cfgSeen {
		return Result{
			Verdict: VerdictNotConsulted,
			Detail:  "govulncheck exited 0 but emitted no config message, so it never reported reading a database",
		}
	}

	// The zero time is what an empty or structurally absent database produces.
	// Treating it as "clean" is the exact silent pass this gate exists to
	// prevent, so it is checked before any finding-based verdict.
	if modified.IsZero() {
		return Result{
			Verdict:        VerdictNotConsulted,
			DBLastModified: modified,
			Detail:         "govulncheck reported the zero time as db_last_modified, which is what an empty or malformed database yields; the scan proves nothing",
		}
	}

	if age := now.Sub(modified); age > maxDBPublicationAge {
		return Result{
			Verdict:        VerdictNotConsulted,
			DBLastModified: modified,
			Detail: fmt.Sprintf("the database govulncheck read was published %s ago (%s), beyond the %s bound; a verdict from it is not evidence of anything current",
				age.Round(time.Hour), modified.Format(time.RFC3339), maxDBPublicationAge),
		}
	}

	// Deduplicate: an OSV that reached symbol level is already fatal, so do not
	// also list it as merely imported.
	calledSet := map[string]bool{}
	for _, o := range called {
		calledSet[o] = true
	}
	var onlyImported []string
	for _, o := range imported {
		if !calledSet[o] {
			onlyImported = append(onlyImported, o)
		}
	}

	if len(called) > 0 {
		return Result{
			Verdict:        VerdictVulnerable,
			DBLastModified: modified,
			CalledOSVs:     called,
			ImportedOSVs:   onlyImported,
		}
	}

	// A clean verdict also asserts that the STANDARD LIBRARY was covered, and
	// that half of the scan is silently conditional on the toolchain's version
	// string. govulncheck resolves stdlib advisories by comparing that string
	// against each advisory's fixed-version ranges; when it does not parse as a
	// released toolchain, every stdlib advisory is skipped and the run reports
	// no findings — the same well-formed, zero-finding stream a genuinely clean
	// scan produces. Measured: a `go1.26.5-X:nodwarf5` toolchain reported clean
	// against the very database on which stock go1.26.5 reports four called
	// stdlib vulnerabilities (GO-2026-6218, GO-2026-6090, GO-2026-5972,
	// GO-2026-5026).
	//
	// This sits after the vulnerable branch on purpose: a run that did surface a
	// called vulnerability already fails the build, and saying so names the
	// actual finding, which is more actionable than reporting the scan as
	// inconclusive. Only the CLEAN verdict needs the coverage it implies.
	if !releaseToolchain.MatchString(goVersion) {
		return Result{
			Verdict:        VerdictNotConsulted,
			DBLastModified: modified,
			Detail: fmt.Sprintf("the scan ran under toolchain %q, which is not a released Go version; "+
				"govulncheck skips every standard-library advisory in that case, so a zero-finding result "+
				"says nothing about stdlib. Re-run with a released toolchain (GOTOOLCHAIN=go1.x.y)", goVersion),
		}
	}

	return Result{
		Verdict:        VerdictClean,
		DBLastModified: modified,
		ImportedOSVs:   onlyImported,
	}
}
