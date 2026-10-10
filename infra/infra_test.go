package infra

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// hetznerUserDataLimit is Hetzner's hard cap on hcloud_server.user_data. Exceeding it is
// rejected by the API at apply time, before the box exists.
const hetznerUserDataLimit = 32768

// templateVars mirrors what infra/main.tf passes to templatefile(), with the
// length-variable inputs set to their worst realistic case rather than their committed
// defaults: a registration token is empty in the checked-in variable but ~40 chars on a
// real apply, and pool names grow with the index. A guard that only measured the
// defaults would pass while the payload an actual apply builds does not fit.
//
// bazelscaleset_app_private_key is "" because the fleet runs runner_mode=classic and
// main.tf's pool passes "" explicitly. It is NOT free headroom: the PEM is interpolated
// unconditionally (the mode switch is a shell if, not a template directive), so supplying
// a real ~1.7 KB key base64s to ~2.3 KB of payload and overflows this limit on its own.
// If the scale set is ever revived, that is the first thing to fix.
var templateVars = map[string]string{
	"fdb_version":         "7.3.77",
	"github_repo":         "birdayz/fdb-go",
	"github_runner_token": strings.Repeat("A", 40),
	"runner_name":         "gh-runner-drain-99",
	"runner_labels":       "hetzner-fdb-vm",
	"runner_ephemeral":    "false",
	"runner_version":      "2.337.0",
	"runner_sha256":       strings.Repeat("0", 64),
	"go_version":          "1.26.5",
	"go_sha256":           strings.Repeat("0", 64),
	"bazelisk_version":    "1.28.1",
	"bazelisk_sha256":     strings.Repeat("0", 64),
	"just_version":        "1.48.1",
	"just_sha256":         strings.Repeat("0", 64),
	"mc_release":          "mc.RELEASE.2025-05-21T01-59-54Z",
	"mc_sha256":           strings.Repeat("0", 64),
	"fdb_clients_sha256":  strings.Repeat("0", 64),
	"runner_mode":         "classic",

	"bazelscaleset_app_client_id":       "Iv23litTscHcbrnmhtKa",
	"bazelscaleset_app_installation_id": "143183909",
	"bazelscaleset_app_private_key":     "",
}

// TestUserDataFitsHetznerLimit is the guard that was missing when a provisioning attempt
// died on the 32 KiB cap. cloud-init.yaml is ~95% comments and every one of them is
// payload, so a few paragraphs of perfectly good WHY can silently make the fleet
// unprovisionable. Fail here, in `just test`, not at `tofu apply`.
func TestUserDataFitsHetznerLimit(t *testing.T) {
	t.Parallel()

	tmpl, err := os.ReadFile("cloud-init.yaml")
	if err != nil {
		t.Fatalf("read cloud-init.yaml: %v", err)
	}
	rendered, err := renderTemplate(string(tmpl), templateVars)
	if err != nil {
		t.Fatalf("render cloud-init.yaml: %v", err)
	}

	// A leftover-marker scan would be theatre: renderTemplate consumes every ${ and %{ in
	// one pass, so any marker in the output is a deliberate $${ escape reaching the box's
	// shell. The real guarantee is that renderTemplate REFUSES anything it does not model
	// (unknown variable, unsupported directive), which is asserted by its own tests.
	// The payload is what cloud-init parses on the box, and a YAML error there is
	// discovered as "the box never came up" with the reason buried in its console log.
	// Parse it here, where the failure has a line number.
	if !strings.HasPrefix(rendered, "#cloud-config\n") {
		t.Error("payload does not start with the #cloud-config header; cloud-init will not treat it as cloud-config")
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(rendered), &doc); err != nil {
		t.Fatalf("rendered user_data is not valid YAML: %v", err)
	}
	for _, key := range []string{"bootcmd", "packages", "write_files", "runcmd"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("rendered cloud-config has no %q section", key)
		}
	}

	n := len(rendered)
	t.Logf("rendered user_data: %d bytes, %d of %d headroom", n, hetznerUserDataLimit-n, hetznerUserDataLimit)
	if n > hetznerUserDataLimit {
		t.Fatalf("rendered user_data is %d bytes, over Hetzner's %d-byte user_data limit by %d. "+
			"`tofu apply` will be REJECTED by the API. Shorten cloud-init.yaml (prose that is not "+
			"about the line it sits on belongs in infra/README.md) before merging.",
			n, hetznerUserDataLimit, n-hetznerUserDataLimit)
	}
}

// TestLinkCIVolumeScript and TestMigrateCIVolumeScript exist so `just test` reaches the
// two shell suites. They were green by hand only: nothing in any workflow, the justfile or
// a BUILD file ran them, so nothing kept them green — and both had silently stopped
// testing what their headers claimed.
func TestLinkCIVolumeScript(t *testing.T) {
	t.Parallel()
	runShellSuite(t, "link_ci_volume_test.sh")
}

func TestMigrateCIVolumeScript(t *testing.T) {
	t.Parallel()
	runShellSuite(t, "migrate_ci_volume_test.sh")
}

// TestOrphanFDBSweepScript drives the sweep that removed the LIVE container of
// every long-running test on this fleet for five weeks. Its five cases are the
// ones the incident and its two failed repairs produced, and the suite stubs
// docker, ps and pgrep so it needs no daemon and no runner.
func TestOrphanFDBSweepScript(t *testing.T) {
	t.Parallel()
	runShellSuite(t, "orphan_fdb_sweep_test.sh")
}

// TestReapLeakedContainersScript drives the job-start reaper: an earlier job's
// orphan goes; bazel-remote and the job's own containers stay.
func TestReapLeakedContainersScript(t *testing.T) {
	t.Parallel()
	runShellSuite(t, "reap_leaked_containers_test.sh")
}

func runShellSuite(t *testing.T, script string) {
	t.Helper()
	// The suites resolve the repo from their own path (dirname $0/..), which holds both in
	// a checkout and in the bazel runfiles tree, where data deps keep the same layout.
	cmd := exec.Command("bash", script)
	out, err := cmd.CombinedOutput()
	t.Logf("%s output:\n%s", script, out)
	if err != nil {
		t.Fatalf("%s failed: %v", script, err)
	}
	if !strings.Contains(string(out), "ALL OK") {
		t.Fatalf("%s did not report ALL OK", script)
	}
}

// TestEveryRunnerTokenReachesTheDocumentedVariable pins that a fresh `tofu apply` done the
// way infra/README.md documents it — one `-var github_runner_token=<TOKEN>` — actually
// registers every box.
//
// It did not. The pool's templatefile() passed var.runner_registration_token, a SECOND
// token variable that also defaults to "", so the four pool boxes rendered
// `config.sh --token ` and failed registration inside cloud-init, on a box that had
// already provisioned everything else. Nothing caught it and nothing could have: the live
// pool is pinned by ignore_changes[user_data], so a routine apply never re-renders that
// argument, and the only path that does is the one nobody runs until the fleet is being
// rebuilt — which is exactly when it is needed.
//
// The check is deliberately on the ARGUMENT, not on the local: whatever expression a
// server resource passes as its registration token must be able to resolve to the token
// the docs ask for, or that box cannot be provisioned from the documented inputs.
func TestEveryRunnerTokenReachesTheDocumentedVariable(t *testing.T) {
	t.Parallel()

	tf, err := os.ReadFile("main.tf")
	if err != nil {
		t.Fatalf("read main.tf: %v", err)
	}
	args := regexp.MustCompile(`(?m)^\s+github_runner_token\s*=\s*(.+)$`).FindAllStringSubmatch(string(tf), -1)
	if len(args) < 2 {
		t.Fatalf("main.tf passes github_runner_token to %d templatefile() calls, want at least 2 "+
			"(the grandfathered box and the pool). The fleet's shape has changed and this guard "+
			"is now checking less than it claims.", len(args))
	}

	// Resolve one level of local.* indirection: a fallback expression is the fix, and it
	// naturally lives in a local rather than inline in both resources.
	locals := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^\s+(\w+)\s*=\s*(var\..+)$`).FindAllStringSubmatch(string(tf), -1) {
		locals[m[1]] = m[2]
	}
	for _, a := range args {
		expr := strings.TrimSpace(a[1])
		if l := strings.TrimPrefix(expr, "local."); l != expr {
			if v, ok := locals[l]; ok {
				expr = v
			}
		}
		if !strings.Contains(expr, "var.github_runner_token") {
			t.Errorf("a server passes github_runner_token = %s, which cannot resolve to "+
				"var.github_runner_token — the ONE token infra/README.md tells an operator to "+
				"supply. That box renders `config.sh --token ` on a fresh apply and fails "+
				"registration during cloud-init. Give the argument a fallback to "+
				"var.github_runner_token, or document the extra variable as required in the "+
				"README's Prerequisites.", a[1])
		}
	}
}

// TestReadmeTokenCommandNamesTheRealRepo pins the README's registration-token command to
// the repo the runners actually register against.
//
// The README asked for a token from birdayz/fdb-record-layer-go — the LOCAL checkout
// directory, not a repo that exists. main.tf's github_repo description already records
// that exact mistake costing a fleet its registration (the runners POST to
// https://github.com/<github_repo>), and the README quietly still had it: an operator
// following the documented steps gets a 404 from `gh api` before they ever reach `tofu`.
func TestReadmeTokenCommandNamesTheRealRepo(t *testing.T) {
	t.Parallel()

	tf, err := os.ReadFile("main.tf")
	if err != nil {
		t.Fatalf("read main.tf: %v", err)
	}
	// The default of var github_repo, i.e. what an apply with no overrides registers against.
	m := regexp.MustCompile(`(?s)variable "github_repo".*?default\s*=\s*"([^"]+)"`).FindSubmatch(tf)
	if m == nil {
		t.Fatal("main.tf declares no default for var github_repo; this guard is checking nothing")
	}
	repo := string(m[1])

	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	for _, line := range strings.Split(string(readme), "\n") {
		if !strings.Contains(line, "registration-token") {
			continue
		}
		if !strings.Contains(line, repo) {
			t.Errorf("README's registration-token command does not name %s, the repo the runners "+
				"register against (var github_repo's default):\n  %s\nAn operator following it "+
				"asks GitHub for a token on the wrong repo and gets a 404.", repo, strings.TrimSpace(line))
		}
	}
}

// TestReadmeDistinguishesLegacySweepFromCorrectedGuard pins the incident evidence's
// scope. One journal proves the obsolete age-only timer deleted two RowDiff containers;
// the later deployment journal proves the worker-aware guard kept the live replacement
// after it crossed the same age threshold. Collapsing those observations made the README
// contradict itself.
func TestReadmeDistinguishesLegacySweepFromCorrectedGuard(t *testing.T) {
	t.Parallel()

	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	body := strings.Join(strings.Fields(string(readme)), " ")
	if strings.Contains(body, "neither has the timer been caught firing") {
		t.Error("README still denies observing any timer firing even though the recorded journal proves the obsolete age-only sweep fired")
	}
	for _, want := range []string{
		"The obsolete age-only timer was observed firing",
		"The corrected worker-aware guard was then exercised during RowDiff run 34673258982",
		"executed 12,396 of 15,000 seeds within the normal 3h30 budget",
		"container stayed live until normal teardown",
		"including 35 starts after the container crossed the 1800-second threshold",
		"later paging sweep used a second container and executed 932 of 5,000 seeds within its normal 1h10 budget",
		"with zero kill decisions",
		"Future over-age keeps emit `keeping live FDB container`",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("README does not distinguish the observed legacy removal from the corrected guard's observed keep decision; missing %q", want)
		}
	}
}

// TestFleetGoMatchesGoMod pins the runner's system Go to the toolchain the repo
// actually builds with.
//
// main.tf hardcodes go_version; go.mod is what Bazel's go_sdk resolves from
// (MODULE.bazel: go_sdk.from_file(go_mod = "//:go.mod")). Nothing connected the
// two, so a routine `go 1.x` bump in go.mod would leave every box provisioning a
// Go the repo no longer builds with, silently and only on the fleet.
//
// It matters because the boxes DO use their system Go. The vulnerability gate
// runs govulncheck through it, and a govulncheck compiled against a different
// toolchain than the code under test is a scanner reasoning about a stdlib that
// is not the one shipping — which reports as a clean scan, the failure mode that
// is indistinguishable from working.
//
// This is the same class as the clang and gh gaps: a dependency the fleet really
// had, that nothing declared, found only when a box failed to do its job.
func TestFleetGoMatchesGoMod(t *testing.T) {
	t.Parallel()

	tf, err := os.ReadFile("main.tf")
	if err != nil {
		t.Fatalf("read main.tf: %v", err)
	}
	m := regexp.MustCompile(`(?m)^\s*go_version\s*=\s*"([^"]+)"`).FindSubmatch(tf)
	if m == nil {
		t.Fatal("main.tf declares no go_version — the fleet's Go pin has moved or been " +
			"renamed, and this guard is now checking nothing")
	}
	fleet := string(m[1])

	gomod, err := os.ReadFile("../go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	g := regexp.MustCompile(`(?m)^go\s+(\S+)`).FindSubmatch(gomod)
	if g == nil {
		t.Fatal("go.mod has no `go` directive")
	}
	repo := string(g[1])

	if fleet != repo {
		t.Errorf("the fleet provisions Go %s but the repo builds with Go %s (go.mod, which is "+
			"where MODULE.bazel's go_sdk.from_file reads its toolchain). Bump main.tf's "+
			"go_version AND go_sha256 together — a version bumped without its checksum fails "+
			"provisioning at fetch-verified.sh, which is the loud outcome; a checksum left "+
			"matching a stale version is the quiet one.", fleet, repo)
	}
}

func TestPRCIThirdPartyNoticesFreshness(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		On   map[string]yaml.Node `yaml:"on"`
		Jobs map[string]struct {
			If              string `yaml:"if"`
			ContinueOnError bool   `yaml:"continue-on-error"`
			Steps           []struct {
				Name, Uses, Run string
				If              string            `yaml:"if"`
				ContinueOnError bool              `yaml:"continue-on-error"`
				With            map[string]string `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	if _, ok := workflow.On["pull_request"]; !ok {
		t.Fatal("notice freshness must run on pull requests, not only release tags")
	}
	job, ok := workflow.Jobs["ci"]
	if !ok || job.If != "" || job.ContinueOnError {
		t.Fatal("notice freshness must be in the required CI job")
	}
	var check string
	goReady := false
	for _, step := range job.Steps {
		if strings.HasPrefix(step.Uses, "actions/setup-go@") {
			goReady = step.With["go-version-file"] == "go.mod" && step.If == "" && !step.ContinueOnError
		}
		if step.Name == "Verify third-party notices are current" {
			if !goReady || step.If != "" || step.ContinueOnError || check != "" {
				t.Fatal("notice freshness must run once, after setup-go from go.mod, without an optional condition")
			}
			check = step.Run
		}
	}
	if check == "" {
		t.Fatal("PR CI has no third-party notice freshness check")
	}
	for _, tc := range []struct {
		name           string
		fail, generate bool
	}{
		{"current", false, true},
		{"stale", true, true},
		{"staged_regeneration", true, true},
		{"missing", true, false},
		{"untracked", true, false},
		{"empty", true, false},
		{"generator_failure", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			run := func(script string) ([]byte, error) {
				cmd := exec.Command("bash", "-euo", "pipefail", "-c", script)
				cmd.Dir = root
				cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "CASE="+tc.name)
				return cmd.CombinedOutput()
			}
			setup := `
git init -q
if [ "$CASE" != missing ] && [ "$CASE" != untracked ]; then
  case "$CASE" in
    stale|staged_regeneration) printf 'stale notice\n' > THIRD_PARTY_NOTICES.txt ;;
    empty) : > THIRD_PARTY_NOTICES.txt ;;
    *) printf 'current notice\n' > THIRD_PARTY_NOTICES.txt ;;
  esac
  git add THIRD_PARTY_NOTICES.txt
fi
git -c user.name=fixture -c user.email=fixture@example.invalid -c core.hooksPath=/dev/null commit -qm fixture --allow-empty
if [ "$CASE" = untracked ] || [ "$CASE" = staged_regeneration ]; then
  printf 'current notice\n' > THIRD_PARTY_NOTICES.txt
fi
if [ "$CASE" = staged_regeneration ]; then git add THIRD_PARTY_NOTICES.txt; fi
`
			if out, err := run(setup); err != nil {
				t.Fatalf("notice fixture: %v\n%s", err, out)
			}
			// Exercise the workflow's shell, stubbing only metadata generation.
			harness := `
python3() {
  [ "$*" = scripts/update-third-party-notices.py ] || return 91
  printf 'generated\n' > generator-called
  [ "$CASE" != generator_failure ] || return 92
  printf 'current notice\n' > THIRD_PARTY_NOTICES.txt
}
`
			out, err := run(harness + check)
			if (err != nil) != tc.fail {
				t.Fatalf("notice check %s: exit=%v, want failure=%v\n%s", tc.name, err, tc.fail, out)
			}
			_, err = os.Stat(filepath.Join(root, "generator-called"))
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if (err == nil) != tc.generate {
				t.Fatalf("notice check %s: generator called=%v, want %v", tc.name, err == nil, tc.generate)
			}
		})
	}
}

func TestFRLReleaseLegalNotices(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../.github/workflows/frl-release.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	var build, smoke string
	for _, step := range workflow.Jobs["release"].Steps {
		switch step.Name {
		case "Cross-compile static binaries":
			build = step.Run
		case "Smoke test (extract, run, verify checksum path)":
			smoke = step.Run
		}
	}
	loop := regexp.MustCompile(`(?s)for target in ([^;]+); do\n(.*?)\ndone`).FindStringSubmatch(build)
	if loop == nil || smoke == "" {
		t.Fatal("release packaging loop or smoke check is missing")
	}
	targets := strings.Fields(loop[1])
	if strings.Join(targets, " ") != "linux/amd64 linux/arm64 darwin/amd64 darwin/arm64" {
		t.Fatalf("review legal-notice coverage for release targets %v", targets)
	}
	// Exercise the shipped packaging shell with fixture binaries, not a compiler.
	compile := `  go build -trimpath -ldflags='-s -w' -o "$out/frl" .`
	if strings.Count(loop[0], compile) != 1 {
		t.Fatal("expected one cross-compile command in the packaging loop")
	}
	packaging := strings.Replace(loop[0], compile, "  : # fixture binary already exists", 1)
	for _, damage := range []string{"none", "missing", "empty", "altered"} {
		t.Run(damage, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			cli := filepath.Join(root, "cmd", "frl")
			dist := filepath.Join(cli, "dist")
			notices := []string{"LICENSE", "NOTICE", "THIRD_PARTY_NOTICES.txt"}
			for _, name := range notices {
				if err := os.WriteFile(filepath.Join(root, name), []byte("fixture "+name+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			for _, target := range targets {
				dir := filepath.Join(dist, strings.ReplaceAll(target, "/", "_"))
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "frl"), []byte("#!/bin/sh\necho v0.0.0\n"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			run := func(dir, script string) ([]byte, error) {
				cmd := exec.Command("bash", "-euo", "pipefail", "-c", script)
				cmd.Dir = dir
				cmd.Env = append(os.Environ(), "VERSION=v0.0.0")
				return cmd.CombinedOutput()
			}
			if out, err := run(cli, packaging); err != nil {
				t.Fatalf("package release: %v\n%s", err, out)
			}
			for _, target := range targets {
				archive := "frl_v0.0.0_" + strings.ReplaceAll(target, "/", "_") + ".tar.gz"
				for _, name := range notices {
					out, err := run(dist, "tar -xOzf "+archive+" "+name)
					if err != nil || string(out) != "fixture "+name+"\n" {
						t.Fatalf("%s must carry exact %s: %v\n%s", archive, name, err, out)
					}
				}
			}
			if damage != "none" {
				path := filepath.Join(dist, "darwin_arm64", "THIRD_PARTY_NOTICES.txt")
				files := "frl LICENSE NOTICE THIRD_PARTY_NOTICES.txt"
				if damage == "missing" {
					files = "frl LICENSE NOTICE"
				} else {
					content := []byte{}
					if damage == "altered" {
						content = []byte("wrong notice\n")
					}
					if err := os.WriteFile(path, content, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if out, err := run(dist, "tar -C darwin_arm64 -czf frl_v0.0.0_darwin_arm64.tar.gz "+files); err != nil {
					t.Fatalf("damage fixture: %v\n%s", err, out)
				}
			}
			if out, err := run(dist, "sha256sum frl_*.tar.gz > checksums.txt"); err != nil {
				t.Fatalf("checksum fixtures: %v\n%s", err, out)
			}
			out, err := run(dist, smoke)
			if (err != nil) != (damage != "none") {
				t.Fatalf("smoke check for %s notice: %v\n%s", damage, err, out)
			}
		})
	}
}
