package docscheck

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// website/static/install.sh is the install path the front page advertises
// (`curl -fsSL https://fdb.dev/install.sh | sh`), and until these tests it had
// no coverage of any kind: it is not compiled, not linted, and the only lane
// that ever ran it end to end is the release workflow, which is tag-triggered
// and had never fired. A shell script nobody executes in CI is a 280-line
// untested binary shipped to every visitor.
//
// The harness below runs the REAL script against a local stand-in for GitHub,
// so what is pinned is the script's behaviour, not a transcription of it.

const installerScript = "website/static/install.sh"

// fakeGitHub serves the two endpoints install.sh talks to: the release-list API
// and the release-asset download host. Both are redirected at it via the
// script's own FRL_API_URL / FRL_BASE_URL knobs.
type fakeGitHub struct {
	// releasesJSON is the body served for the release-list query.
	releasesJSON string
	// tarball is the release archive; nil means "no asset is served".
	tarball []byte
	// corruptChecksum publishes a checksum that does not match tarball.
	corruptChecksum bool

	// assetName records what the script actually asked for, so checksums.txt can
	// name the same file. The asset name embeds the version, OS and arch the
	// script derived from uname, which is how this stays host-agnostic instead
	// of hardcoding linux/amd64.
	assetName string
}

func (f *fakeGitHub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".tar.gz"):
			if f.tarball == nil {
				http.NotFound(w, r)
				return
			}
			f.assetName = filepath.Base(r.URL.Path)
			_, _ = w.Write(f.tarball)

		case strings.HasSuffix(r.URL.Path, "checksums.txt"):
			sum := sha256.Sum256(f.tarball)
			hexSum := hex.EncodeToString(sum[:])
			if f.corruptChecksum {
				hexSum = strings.Repeat("0", len(hexSum))
			}
			fmt.Fprintf(w, "%s  %s\n", hexSum, f.assetName)

		default: // the release-list API
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(f.releasesJSON))
		}
	})
}

// frlTarball builds a release archive holding an executable `frl` that answers
// `version --short`. install.sh runs the installed binary before declaring
// success, so the payload has to actually execute.
func frlTarball(t *testing.T, version string) []byte {
	t.Helper()
	body := "#!/bin/sh\nif [ \"$1\" = version ]; then echo " + version + "; fi\n"

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{
		Name: "frl",
		Mode: 0o755,
		Size: int64(len(body)),
	}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return gz.Bytes()
}

// runInstaller executes install.sh against apiURL/baseURL and returns its
// combined output plus the directory it was told to install into.
func runInstaller(t *testing.T, apiURL, baseURL string, args ...string) (out string, exitOK bool, installDir string) {
	t.Helper()
	root := sourceTreeRoot(t)
	script := filepath.Join(root, installerScript)
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("the advertised installer is missing from the tree: %v", err)
	}

	home := t.TempDir()
	installDir = filepath.Join(home, "bin")

	argv := append([]string{script, "--dir", installDir}, args...)
	cmd := exec.Command("sh", argv...)
	cmd.Env = append(os.Environ(),
		"FRL_API_URL="+apiURL,
		"FRL_BASE_URL="+baseURL,
		"HOME="+home,
		"TMPDIR="+t.TempDir(),
		"NO_COLOR=1",
	)
	b, err := cmd.CombinedOutput()
	return string(b), err == nil, installDir
}

// TestInstallerTellsAnEmptyReleaseListFromAnUnreachableAPI is the regression for
// the failure a user actually hit: `curl -fsSL https://fdb.dev/install.sh | sh`
// died with a message blaming rate-limiting and offering `FRL_VERSION=v0.1.0`,
// when the real and only cause was that the repository had published no
// releases at all and v0.1.0 did not exist. Following the advice produced a 404,
// i.e. a second wrong diagnosis stacked on the first.
//
// The cause was structural, not a bad string. The script piped the fetch into
// `grep | sed | head`, and a pipeline's exit status is its LAST command's — so
// curl's failure was discarded and BOTH outcomes arrived as an empty version
// variable. One message then had to cover two conditions with different fixes,
// and it was written for the one that was not happening.
//
// This is the empty-set false positive from CLAUDE.md wearing a new face: an
// absent answer and an answer of "nothing" are different facts, and any layer
// that renders them identically will describe one of them wrongly. So the
// assertion here is not "the message says X" but that the two conditions are
// DISTINGUISHABLE, and that neither is described in the other's terms.
func TestInstallerTellsAnEmptyReleaseListFromAnUnreachableAPI(t *testing.T) {
	t.Parallel()

	// Condition 1: the API answers, with an empty release list.
	empty := &fakeGitHub{releasesJSON: "[]"}
	emptySrv := httptest.NewServer(empty.handler())
	defer emptySrv.Close()
	emptyOut, emptyOK, _ := runInstaller(t, emptySrv.URL, emptySrv.URL)

	// Condition 2: nothing answers. Bind then close, so the port is dead for
	// certain rather than merely unlikely to be in use.
	deadSrv := httptest.NewServer(http.NotFoundHandler())
	deadURL := deadSrv.URL
	deadSrv.Close()
	deadOut, deadOK, _ := runInstaller(t, deadURL, deadURL)

	if emptyOK || deadOK {
		t.Fatalf("installer reported success with no release to install\nempty-list run: %s\nunreachable run: %s", emptyOut, deadOut)
	}

	if emptyOut == deadOut {
		t.Fatalf("an empty release list and an unreachable API produce byte-identical output, so the\n"+
			"message cannot be describing both correctly — this is the exact collapse that told a user\n"+
			"to work around rate-limiting when the repository simply had no releases:\n%s", emptyOut)
	}

	// The empty-list message must not send the reader chasing the network, and
	// must not prescribe a pin: no FRL_VERSION value can conjure a release.
	for _, forbidden := range []string{"rate-limited", "Offline", "could not reach"} {
		if strings.Contains(emptyOut, forbidden) {
			t.Errorf("the empty-release-list message contains %q, which describes the OTHER condition.\n"+
				"The release list was fetched successfully; blaming the network sends the reader at a\n"+
				"problem that is not happening. Output:\n%s", forbidden, emptyOut)
		}
	}
	if !strings.Contains(emptyOut, "no releases") {
		t.Errorf("the empty-release-list message never says the repository has no releases, which is the\n"+
			"one fact that explains the failure and points at the fix. Output:\n%s", emptyOut)
	}
	if !strings.Contains(emptyOut, "go install fdb.dev/cmd/frl@latest") {
		t.Errorf("the empty-release-list message offers no way forward; building from source is the only\n"+
			"one that exists when nothing is published. Output:\n%s", emptyOut)
	}

	// The unreachable message must not assert a fact it cannot know: with no
	// answer, the release list's contents are unobserved.
	if strings.Contains(deadOut, "no releases") {
		t.Errorf("the unreachable-API message claims the repository has published no releases — it never\n"+
			"got an answer, so that is an assertion about data it did not see. Output:\n%s", deadOut)
	}
	if !strings.Contains(deadOut, "could not reach") {
		t.Errorf("the unreachable-API message does not say the API could not be reached. Output:\n%s", deadOut)
	}
}

// TestInstallerVerifiesAndInstalls drives the whole advertised path — resolve
// the newest release, download, verify sha256, extract, install, run — against
// a stand-in GitHub. Without it, "the installer works" rests on the release
// workflow, which is tag-triggered and so ran zero times before v0.1.0.
func TestInstallerVerifiesAndInstalls(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{
		// Newest-first, with a prerelease and a foreign tag ahead of the real
		// one: the script must skip both. A bare `v9.9.9` belongs to the library,
		// not the CLI, and installing it would fetch an asset that is not there.
		releasesJSON: `[
			{"tag_name": "v9.9.9"},
			{"tag_name": "cmd/frl/v0.2.0-rc1"},
			{"tag_name": "cmd/frl/v0.1.0"},
			{"tag_name": "cmd/frl/v0.0.9"}
		]`,
		tarball: frlTarball(t, "v0.1.0"),
	}
	srv := httptest.NewServer(gh.handler())
	defer srv.Close()

	out, ok, installDir := runInstaller(t, srv.URL, srv.URL)
	if !ok {
		t.Fatalf("installer failed on the happy path:\n%s", out)
	}
	if !strings.Contains(out, "v0.1.0") {
		t.Errorf("installer never reported the version it resolved; a prerelease or the library's own\n"+
			"v9.9.9 tag may have been selected. Output:\n%s", out)
	}
	if strings.Contains(out, "rc1") {
		t.Errorf("installer selected a prerelease. Output:\n%s", out)
	}

	installed := filepath.Join(installDir, "frl")
	info, err := os.Stat(installed)
	if err != nil {
		t.Fatalf("installer reported success but installed nothing at %s: %v\noutput:\n%s", installed, err, out)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("installed frl is not executable (mode %v)", info.Mode().Perm())
	}
	got, err := exec.Command(installed, "version", "--short").Output()
	if err != nil {
		t.Fatalf("installed frl does not run: %v", err)
	}
	if strings.TrimSpace(string(got)) != "v0.1.0" {
		t.Errorf("installed frl reports %q, want v0.1.0", strings.TrimSpace(string(got)))
	}
}

// TestInstallerRefusesACorruptedDownload pins the security property the script
// exists to provide. It is the reason install.sh is preferable to piping a raw
// binary: a checksum that does not match must stop the install, not warn about
// it. A gate that reports a mismatch and installs anyway is worse than no gate,
// because the output still says "verified".
func TestInstallerRefusesACorruptedDownload(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{
		releasesJSON:    `[{"tag_name": "cmd/frl/v0.1.0"}]`,
		tarball:         frlTarball(t, "v0.1.0"),
		corruptChecksum: true,
	}
	srv := httptest.NewServer(gh.handler())
	defer srv.Close()

	out, ok, installDir := runInstaller(t, srv.URL, srv.URL)
	if ok {
		t.Fatalf("installer accepted a binary whose sha256 did not match checksums.txt:\n%s", out)
	}
	if !strings.Contains(out, "sha256 mismatch") {
		t.Errorf("installer failed, but not with a checksum complaint — the reader cannot tell a tampered\n"+
			"download from a network hiccup. Output:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(installDir, "frl")); err == nil {
		t.Errorf("installer left a binary behind after refusing it; the refusal must install nothing")
	}
}
