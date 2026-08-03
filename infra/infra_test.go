package infra

import (
	"os"
	"os/exec"
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
	"runner_version":      "2.335.1",
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
