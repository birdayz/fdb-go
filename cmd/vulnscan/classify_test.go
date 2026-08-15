package main

import (
	"strings"
	"testing"
	"time"
)

// now is a fixed clock so staleness assertions do not drift with wall time.
var now = time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)

func fresh() string { return "2026-07-27T20:14:16Z" }

// configMsg is the message every successful govulncheck run emits, reproduced
// from a real v1.6.0 `-format json` stream.
func configMsg(dbLastModified string) string {
	return configMsgGo(dbLastModified, "go1.26.6")
}

// configMsgGo varies the toolchain the scanner reported running under, which
// decides whether stdlib advisories were considered at all.
func configMsgGo(dbLastModified, goVersion string) string {
	return `{"config":{"protocol_version":"v1.0.0","scanner_name":"govulncheck",` +
		`"scanner_version":"v1.6.0","db":"file:///mnt/ci-data/vulndb-mirror/db",` +
		`"db_last_modified":"` + dbLastModified + `","go_version":"` + goVersion + `",` +
		`"scan_level":"symbol","scan_mode":"source"}}`
}

// symbolFinding is a CALLED vulnerability: trace[0] names a function. This is
// the shape govulncheck's own text mode exits 3 for.
func symbolFinding(osv string) string {
	return `{"finding":{"osv":"` + osv + `","trace":[{"module":"golang.org/x/text",` +
		`"package":"golang.org/x/text/language","function":"Parse"}]}}`
}

// importedFinding is present in the graph but not called: no function in the
// trace. govulncheck exits 0 for these, and so must this gate.
func importedFinding(osv string) string {
	return `{"finding":{"osv":"` + osv + `","trace":[{"module":"golang.org/x/text"}]}}`
}

// TestClassifyDistinguishesTheFourOutcomes is the core of the gate. Each case
// names the real-world event it stands for, because the entire value of this
// function is that these four do not collapse into two.
func TestClassifyDistinguishesTheFourOutcomes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		exitCode int
		stream   string
		want     Verdict
	}{
		{
			name:     "clean scan against a real database",
			exitCode: 0,
			stream:   configMsg(fresh()),
			want:     VerdictClean,
		},
		{
			name:     "called vulnerability fails the build",
			exitCode: 0,
			stream:   configMsg(fresh()) + symbolFinding("GO-2021-0113"),
			want:     VerdictVulnerable,
		},
		{
			name:     "uncalled vulnerability does not fail the build",
			exitCode: 0,
			stream:   configMsg(fresh()) + importedFinding("GO-2022-1059"),
			want:     VerdictClean,
		},
		{
			// The measured hazard: govulncheck aimed at an empty directory
			// exits 0 and emits this exact stream. Nothing but the zero time
			// distinguishes it from a clean scan.
			name:     "empty database reports the zero time and must not read as clean",
			exitCode: 0,
			stream:   configMsg("0001-01-01T00:00:00Z"),
			want:     VerdictNotConsulted,
		},
		{
			name:     "database published beyond the bound is not evidence",
			exitCode: 0,
			stream:   configMsg("2026-01-01T00:00:00Z"),
			want:     VerdictNotConsulted,
		},
		{
			name:     "no config message means no database was reported",
			exitCode: 0,
			stream:   `{"progress":{"message":"Fetching vulnerabilities from the database..."}}`,
			want:     VerdictNotConsulted,
		},
		{
			name:     "empty output is not a clean scan",
			exitCode: 0,
			stream:   "",
			want:     VerdictNotConsulted,
		},
		{
			// A 403 from vuln.go.dev lands here: govulncheck exits non-zero
			// having produced no verdict.
			name:     "non-zero exit is a failed scan, not a pass",
			exitCode: 1,
			stream:   "",
			want:     VerdictScanFailed,
		},
		{
			name:     "unparseable output is a failed scan, not a pass",
			exitCode: 0,
			stream:   configMsg(fresh()) + `{"finding":{"osv":`,
			want:     VerdictScanFailed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Classify(tc.exitCode, strings.NewReader(tc.stream), now)
			if got.Verdict != tc.want {
				t.Fatalf("Classify() = %s, want %s (detail: %s)", got.Verdict, tc.want, got.Detail)
			}
		})
	}
}

// TestNoInputCanMakeAFailureLookClean is the direction that matters most: it is
// not enough that the good cases classify correctly. NOTHING that is not a
// verified scan against a real database may produce VerdictClean, because
// VerdictClean is the only value that lets the build go green.
func TestNoInputCanMakeAFailureLookClean(t *testing.T) {
	t.Parallel()

	// Every one of these is a way the scan can fail to establish anything.
	// A crafted advisory description containing the word "403", or an upstream
	// error rendered into the stream, must not change the outcome — which is
	// why classification never reads prose.
	notEvidence := []struct {
		name     string
		exitCode int
		stream   string
	}{
		{"rate limited", 1, ""},
		{"rate limit text on stdout", 1, `govulncheck: fetching vulnerabilities: HTTP GET https://vuln.go.dev/ID/GO-2026-4970.json.gz returned unexpected status: 403 Forbidden`},
		{"empty db", 0, configMsg("0001-01-01T00:00:00Z")},
		{"missing config", 0, `{"progress":{"message":"Checking the code..."}}`},
		{"blank stdout", 0, ""},
		{"truncated stream", 0, configMsg(fresh()) + `{"finding":`},
		{"ancient db", 0, configMsg("2025-01-01T00:00:00Z")},
		{"unparseable db stamp", 0, configMsg("not-a-timestamp")},
		{"finding text imitating a fetch error", 0, configMsg("0001-01-01T00:00:00Z") + importedFinding("403 Forbidden")},
		{"locally rebuilt toolchain skips stdlib", 0, configMsgGo(fresh(), "go1.26.5-X:nodwarf5")},
		{"devel toolchain skips stdlib", 0, configMsgGo(fresh(), "devel go1.27-abc123")},
		{"scanner reported no toolchain at all", 0, configMsgGo(fresh(), "")},
	}

	for _, tc := range notEvidence {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Classify(tc.exitCode, strings.NewReader(tc.stream), now)
			if got.Verdict == VerdictClean {
				t.Fatalf("Classify(%q) returned VerdictClean; a scan that established nothing must never "+
					"report the same value as a scan that consulted a real database and found nothing. "+
					"This is the failure mode that makes a security gate worthless.", tc.name)
			}
		})
	}
}

// TestUnconsultedDatabaseIsDiagnosedNotJustRejected keeps the two ways a
// database can be useless distinguishable.
//
// Both an EMPTY database and an ANCIENT one are VerdictNotConsulted, so a test
// asserting only the verdict passes with either guard deleted — the staleness
// bound alone catches the zero time, since year 1 is trivially outside any
// bound. That makes the verdict-only assertion blind to the loss of a guard,
// which is the thing worth knowing here.
//
// They are worth telling apart because they send triage to opposite places: an
// empty database means the mirror or the -db path is broken on this runner, an
// old one means refreshes have been failing fleet-wide. Reporting the first as
// "published 2562047h ago" — the saturated duration the zero time actually
// produces — points at neither.
func TestUnconsultedDatabaseIsDiagnosedNotJustRejected(t *testing.T) {
	t.Parallel()

	empty := Classify(0, strings.NewReader(configMsg("0001-01-01T00:00:00Z")), now)
	if empty.Verdict != VerdictNotConsulted {
		t.Fatalf("empty database = %s, want %s", empty.Verdict, VerdictNotConsulted)
	}
	if !strings.Contains(empty.Detail, "empty or malformed") {
		t.Errorf("an empty database was diagnosed as %q.\n"+
			"It must be reported as empty/malformed rather than merely old: the zero time is what a "+
			"broken mirror path yields, and describing it as a publication age both misdirects triage "+
			"and prints a saturated duration.", empty.Detail)
	}

	old := Classify(0, strings.NewReader(configMsg("2026-01-01T00:00:00Z")), now)
	if old.Verdict != VerdictNotConsulted {
		t.Fatalf("ancient database = %s, want %s", old.Verdict, VerdictNotConsulted)
	}
	if strings.Contains(old.Detail, "empty or malformed") {
		t.Errorf("a stale-but-real database was diagnosed as empty/malformed: %q", old.Detail)
	}
	if !strings.Contains(old.Detail, "beyond the") {
		t.Errorf("a stale database was not diagnosed as being beyond the staleness bound: %q", old.Detail)
	}
}

// TestCalledVulnerabilityFailsRegardlessOfSurroundingNoise pins the other
// direction: the gate must still redden on a real finding. A fix that only
// stops false failures, without preserving true ones, has replaced a flaky gate
// with a quiet one.
func TestCalledVulnerabilityFailsRegardlessOfSurroundingNoise(t *testing.T) {
	t.Parallel()

	streams := map[string]string{
		"finding alone":            configMsg(fresh()) + symbolFinding("GO-2021-0113"),
		"finding after imported":   configMsg(fresh()) + importedFinding("GO-2020-0015") + symbolFinding("GO-2021-0113"),
		"finding before imported":  configMsg(fresh()) + symbolFinding("GO-2021-0113") + importedFinding("GO-2020-0015"),
		"finding among progress":   configMsg(fresh()) + `{"progress":{"message":"Checking..."}}` + symbolFinding("GO-2021-0113"),
		"multiple called findings": configMsg(fresh()) + symbolFinding("GO-2021-0113") + symbolFinding("GO-2022-1059"),
	}

	for name, stream := range streams {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := Classify(0, strings.NewReader(stream), now)
			if got.Verdict != VerdictVulnerable {
				t.Fatalf("Classify(%q) = %s, want VerdictVulnerable. A real, reachable vulnerability "+
					"must fail the build; the tolerance this gate has for fetch failures must never "+
					"extend to findings.", name, got.Verdict)
			}
			if err := report(got); err == nil {
				t.Fatalf("report() returned nil for a called vulnerability, so the process would exit 0")
			}
		})
	}
}

// TestStdlibCoverageIsPartOfACleanVerdict drives every arm of the toolchain
// guard, in both directions.
//
// The bug it pins was measured, not hypothesised: on a `go1.26.5-X:nodwarf5`
// toolchain the gate reported "no called vulnerabilities" against the same
// database on which stock go1.26.5 reports four called stdlib vulnerabilities.
// Nothing in the stream distinguishes the two but this field, so a scan whose
// stdlib half never ran renders exactly like a scan that ran and found nothing.
//
// Both directions are driven because a guard that only rejects is as broken as
// one that only accepts: reject every real toolchain and the gate is a
// permanent red nobody can clear, which gets it deleted.
func TestStdlibCoverageIsPartOfACleanVerdict(t *testing.T) {
	t.Parallel()

	// Released toolchains: stdlib advisories were considered, so a zero-finding
	// stream is real evidence and must be allowed to go green.
	for _, v := range []string{"go1.26.6", "go1.27", "go1.27rc1", "go1.28beta2"} {
		t.Run("released/"+v, func(t *testing.T) {
			t.Parallel()
			got := Classify(0, strings.NewReader(configMsgGo(fresh(), v)), now)
			if got.Verdict != VerdictClean {
				t.Fatalf("Classify(go_version=%q) = %s, want VerdictClean (detail: %s).\n"+
					"%q is a released toolchain, so govulncheck did consult stdlib advisories and a "+
					"zero-finding scan is genuine evidence. Rejecting it makes the gate unclearable.",
					v, got.Verdict, got.Detail, v)
			}
		})
	}

	// Toolchains govulncheck cannot resolve stdlib advisories against.
	for _, v := range []string{"go1.26.5-X:nodwarf5", "devel go1.27-abc123", "", "gccgo-15", "1.26.6"} {
		t.Run("unresolvable/"+v, func(t *testing.T) {
			t.Parallel()
			got := Classify(0, strings.NewReader(configMsgGo(fresh(), v)), now)
			if got.Verdict != VerdictNotConsulted {
				t.Fatalf("Classify(go_version=%q) = %s, want VerdictNotConsulted.\n"+
					"govulncheck skips every stdlib advisory under a toolchain it cannot parse, so this "+
					"zero-finding stream covers only the module graph. Reporting it as clean is the "+
					"silent pass this gate exists to prevent.", v, got.Verdict)
			}
			if !strings.Contains(got.Detail, "standard-library") {
				t.Errorf("Classify(go_version=%q) was diagnosed as %q; it must name the standard library "+
					"as the uncovered half, or triage looks for a broken mirror instead of a toolchain.", v, got.Detail)
			}
		})
	}

	// A called finding under a blind toolchain still reports the finding: that
	// half of the scan DID establish something, and naming the vulnerability is
	// more actionable than calling the whole run inconclusive. Both fail the
	// build, so this is about the message, not the exit code.
	blindWithFinding := Classify(0, strings.NewReader(
		configMsgGo(fresh(), "go1.26.5-X:nodwarf5")+symbolFinding("GO-2021-0113")), now)
	if blindWithFinding.Verdict != VerdictVulnerable {
		t.Fatalf("a called finding under an unresolvable toolchain = %s, want VerdictVulnerable; "+
			"the toolchain guard must not mask a vulnerability the scan actually found",
			blindWithFinding.Verdict)
	}
}

// TestReportExitsNonZeroForEverythingButClean pins the mapping from verdict to
// process exit, which is where a correct classification could still be thrown
// away.
func TestReportExitsNonZeroForEverythingButClean(t *testing.T) {
	t.Parallel()

	if err := report(Result{Verdict: VerdictClean, DBLastModified: now}); err != nil {
		t.Fatalf("report(clean) = %v, want nil", err)
	}
	for _, v := range []Verdict{VerdictVulnerable, VerdictNotConsulted, VerdictScanFailed} {
		if err := report(Result{Verdict: v, DBLastModified: now, CalledOSVs: []string{"GO-2021-0113"}}); err == nil {
			t.Fatalf("report(%s) = nil, want an error so the gate fails the build", v)
		}
	}
}

// TestNotConsultedAndCleanDoNotShareAMessage guards the human-facing half of
// the contract. Even with correct exit codes, two failures that read alike get
// triaged alike.
func TestNotConsultedAndCleanDoNotShareAMessage(t *testing.T) {
	t.Parallel()

	notConsulted := Classify(0, strings.NewReader(configMsg("0001-01-01T00:00:00Z")), now)
	err := report(notConsulted)
	if err == nil {
		t.Fatal("report(not-consulted) = nil, want an error")
	}
	if !strings.Contains(err.Error(), "not usably consulted") {
		t.Fatalf("report(not-consulted) said %q; it must say the database was not consulted, "+
			"never anything a reader could mistake for a vulnerability finding or a clean run", err)
	}
}
