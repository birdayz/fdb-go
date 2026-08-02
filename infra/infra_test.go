package infra

import (
	"os"
	"os/exec"
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
