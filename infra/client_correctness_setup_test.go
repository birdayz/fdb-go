package infra

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestClientCorrectnessWorkflowSetupPolicy(t *testing.T) {
	t.Parallel()
	wf, _ := readClientCorrectnessWorkflow(t)
	data, err := os.ReadFile("../.github/workflows/client-correctness.yml")
	if err != nil {
		t.Fatal(err)
	}
	var policy struct {
		Defaults struct {
			Run struct{ Shell string }
		}
	}
	if err := yaml.Unmarshal(data, &policy); err != nil {
		t.Fatal(err)
	}
	if policy.Defaults.Run.Shell != "bash" {
		t.Fatalf("default shell=%q, want bash", policy.Defaults.Run.Shell)
	}
	job := wf.Jobs["bindingtester"]
	if job.Env["FDB_VERSION"] != "7.3.77" || job.Env["FDB_REQUIRE_DOCKER"] != "1" {
		t.Fatalf("version/Docker environment drift: %v", job.Env)
	}
	checkout, setup, scheduled := 0, 0, 0
	for _, step := range job.Steps {
		if strings.HasPrefix(step.Uses, "actions/checkout@") {
			checkout++
			if step.With["persist-credentials"] != "false" {
				t.Fatal("checkout must explicitly disable persisted credentials")
			}
		}
		if step.Name == "Install pinned tools and C client" {
			setup++
			if step.Timeout != 7 {
				t.Fatalf("setup timeout=%d, want 7", step.Timeout)
			}
		}
		if step.Name == "Run scheduled BindingTester on frozen inputs" {
			scheduled++
			if step.Timeout != 30 {
				t.Fatalf("scheduled-step timeout=%d, want 30", step.Timeout)
			}
		}
	}
	if checkout != 1 || setup != 1 || scheduled != 1 {
		t.Fatalf("step population checkout/setup/scheduled=%d/%d/%d, want 1/1/1", checkout, setup, scheduled)
	}
}

type clientSetupEvent struct {
	key  string
	argv []string
}

func clientSetupExpectedEvents() []clientSetupEvent {
	// This is an independent literal oracle, not parsed from the shipped script.
	curl := func(url, file string) []string {
		return []string{"curl", "--fail", "--location", "--retry", "8", "--retry-max-time", "180", "--connect-timeout", "15", "--speed-limit", "1024", "--speed-time", "60", url, "-o", file}
	}
	return []clientSetupEvent{
		{"curl-bazelisk", curl("https://github.com/bazelbuild/bazelisk/releases/download/v1.30.0/bazelisk-linux-amd64", "bazelisk.bin")},
		{"sha-bazelisk", []string{"sha256sum", "--check", "--strict"}},
		{"sudo-bazelisk", []string{"sudo", "install", "-m", "0755", "bazelisk.bin", "/usr/local/bin/bazelisk"}},
		{"install-bazelisk", []string{"install", "-m", "0755", "bazelisk.bin", "/usr/local/bin/bazelisk"}},
		{"curl-just", curl("https://github.com/casey/just/releases/download/1.48.1/just-1.48.1-x86_64-unknown-linux-musl.tar.gz", "just.tar.gz")},
		{"sha-just", []string{"sha256sum", "--check", "--strict"}},
		{"tar-just", []string{"tar", "-xzf", "just.tar.gz", "just"}},
		{"sudo-just", []string{"sudo", "install", "-m", "0755", "just", "/usr/local/bin/just"}},
		{"install-just", []string{"install", "-m", "0755", "just", "/usr/local/bin/just"}},
		{"curl-fdb", curl("https://github.com/apple/foundationdb/releases/download/7.3.77/foundationdb-clients_7.3.77-1_amd64.deb", "fdb-clients.deb")},
		{"sha-fdb", []string{"sha256sum", "--check", "--strict"}},
		{"sudo-dpkg", []string{"sudo", "dpkg", "-i", "fdb-clients.deb"}},
		{"dpkg", []string{"dpkg", "-i", "fdb-clients.deb"}},
		{"sudo-ldrefresh", []string{"sudo", "ldconfig"}},
		{"ldrefresh", []string{"ldconfig"}},
		{"ldlookup", []string{"ldconfig", "-p"}},
		{"just-version", []string{"just", "--version"}},
		{"python-version", []string{"python3", "--version"}},
		{"docker-info", []string{"docker", "info"}},
		{"docker-pull", []string{"docker", "pull", "foundationdb/foundationdb:7.3.77"}},
	}
}

func clientSetupEncodeEvents(events []clientSetupEvent) string {
	var b strings.Builder
	for _, event := range events {
		for _, arg := range event.argv {
			b.WriteString(arg)
			b.WriteByte(0)
		}
		b.WriteByte(0)
	}
	return b.String()
}

// No command in this double can invoke sudo, install host files, dpkg, Docker or
// the network. The production shell and its control flow are not rewritten.
// sha256sum independently checks each real remote pin, then delegates to the real
// checksum executable with the independently computed fixture-payload digest.
const clientSetupToolDouble = `#!/bin/bash
set -euo pipefail
tool=${0##*/}
{ printf '%s\0' "$tool" "$@"; printf '\0'; } >> "$TRACE"
[[ "$FDB_VERSION" = 7.3.77 && "$FDB_REQUIRE_DOCKER" = 1 ]] || exit 91
expect() {
 local want=$1; shift
 [[ "$*" = "$want" ]] || { printf 'unexpected %s argv: %q\n' "$tool" "$*" >&2; exit 92; }
}
fail_at() {
 if [[ "$FAULT" = "$1" ]]; then printf 'injected:%s\n' "$FAULT" >&2; exit 41; fi
}
verified() { [[ -f "$HARNESS_EVIDENCE/verified-$1" ]] || exit 93; }
case "$tool" in
 curl)
  [[ $# = 15 ]] || exit 94
  expect '--fail --location --retry 8 --retry-max-time 180 --connect-timeout 15 --speed-limit 1024 --speed-time 60' "${@:1:12}"
  case "${13}" in
   https://github.com/bazelbuild/bazelisk/releases/download/v1.30.0/bazelisk-linux-amd64) file=bazelisk.bin; id=bazelisk ;;
   https://github.com/casey/just/releases/download/1.48.1/just-1.48.1-x86_64-unknown-linux-musl.tar.gz) file=just.tar.gz; id=just ;;
   https://github.com/apple/foundationdb/releases/download/7.3.77/foundationdb-clients_7.3.77-1_amd64.deb) file=fdb-clients.deb; id=fdb ;;
   *) exit 95 ;;
  esac
  [[ "${14}" = -o && "${15}" = "$file" ]] || exit 96
  case "$FAULT" in
   http-$id) printf 'injected:%s HTTP 503\n' "$FAULT" >&2; exit 22 ;;
   partial-$id) printf 'partial\n' > "$file"; printf 'injected:%s partial body\n' "$FAULT" >&2; exit 18 ;;
   connect-$id|stall-$id) printf 'injected:%s deadline exhausted\n' "$FAULT" >&2; exit 28 ;;
   retry-$id) printf 'injected:%s retry budget exhausted\n' "$FAULT" >&2; exit 7 ;;
  esac
  if [[ "$FAULT" = corrupt-$id ]]; then printf 'corrupt\n' > "$file"; else printf 'fixture:%s\n' "$file" > "$file"; fi
  ;;
 sha256sum)
  expect '--check --strict' "$@"
  IFS=' ' read -r pin file
  case "$file" in
   bazelisk.bin) want=a248ae2b2b62b5baebb748f6fba7203bff1a27e5d01b55399a8406744cb539e8; fixture=$FIXTURE_BAZELISK ;;
   just.tar.gz) want=9293e553ce401d1b524bf4e104918f72f268e3f9c6827e0055fe98d84a1b2522; fixture=$FIXTURE_JUST ;;
   fdb-clients.deb) want=b6c263723009bddbc30b6ae8cf9854340bd35cef14152abb0da8fb5af2b91254; fixture=$FIXTURE_FDB ;;
   *) exit 97 ;;
  esac
  [[ "$pin" = "$want" ]] || { echo 'wrong independently pinned digest' >&2; exit 98; }
  printf '%s  %s\n' "$fixture" "$file" | "$REAL_SHA256SUM" --check --strict
  : > "$HARNESS_EVIDENCE/verified-$file"
  ;;
 sudo)
  case "$1" in install|dpkg|ldconfig) exec "$HARNESS_BIN/$1" "${@:2}" ;; *) exit 99 ;; esac
  ;;
 install)
  if [[ "$*" = '-m 0755 bazelisk.bin /usr/local/bin/bazelisk' ]]; then
   verified bazelisk.bin; fail_at install-bazelisk
   cp bazelisk.bin "$HARNESS_EVIDENCE/installed-bazelisk"
  elif [[ "$*" = '-m 0755 just /usr/local/bin/just' ]]; then
   verified just.tar.gz; [[ -f "$HARNESS_EVIDENCE/extracted-just" ]] || exit 100; fail_at install-just
   cp just "$HARNESS_EVIDENCE/installed-just"
  else exit 101; fi
  ;;
 tar)
  expect '-xzf just.tar.gz just' "$@"; verified just.tar.gz; fail_at tar-just
  printf 'fixture:just\n' > just
  : > "$HARNESS_EVIDENCE/extracted-just"
  ;;
 dpkg)
  expect '-i fdb-clients.deb' "$@"; verified fdb-clients.deb; fail_at dpkg
  : > "$HARNESS_EVIDENCE/installed-fdb"
  ;;
 ldconfig)
  [[ -f "$HARNESS_EVIDENCE/installed-fdb" ]] || exit 102
  if [[ $# = 0 ]]; then
   fail_at ldrefresh; : > "$HARNESS_EVIDENCE/refreshed-loader"
  else
   expect '-p' "$@"; [[ -f "$HARNESS_EVIDENCE/refreshed-loader" ]] || exit 103
   if [[ "$FAULT" = loader-missing ]]; then
    printf 'injected:loader-missing\n' >&2
    printf 'libother.so (libc6,x86-64) => /fixture/libother.so\n'
   else
    printf 'libfdb_c.so (libc6,x86-64) => /fixture/libfdb_c.so\n'
    fail_at ldlookup
   fi
  fi
  ;;
 just)
  expect '--version' "$@"; [[ -f "$HARNESS_EVIDENCE/installed-just" ]] || exit 104
  fail_at just-version; printf 'just 1.48.1\n'
  ;;
 python3)
  expect '--version' "$@"; fail_at python-version; printf 'Python 3.11.18\n'
  ;;
 docker)
  if [[ "$*" = info ]]; then fail_at docker-info; printf 'fixture Docker available\n'
  elif [[ "$*" = 'pull foundationdb/foundationdb:7.3.77' ]]; then fail_at docker-pull; printf 'fixture image pulled\n'
  else exit 105; fi
  ;;
 *) exit 106 ;;
esac
`

func TestClientCorrectnessSetupShell(t *testing.T) {
	t.Parallel()
	wf, _ := readClientCorrectnessWorkflow(t)
	job := wf.Jobs["bindingtester"]
	script, count := "", 0
	for _, step := range job.Steps {
		if step.Name == "Install pinned tools and C client" {
			script, count = step.Run, count+1
		}
	}
	if count != 1 || script == "" {
		t.Fatalf("setup shell population=%d, want one nonempty script", count)
	}
	type setupCase struct {
		name, fault, stop, diagnostic string
		exit                          int
		unsetTemp                     bool
	}
	cases := []setupCase{{name: "success"}, {name: "unset_required_runner_temp", diagnostic: "RUNNER_TEMP: unbound variable", exit: 1, unsetTemp: true}}
	for _, id := range []string{"bazelisk", "just", "fdb"} {
		for _, failure := range []struct {
			kind string
			exit int
		}{{"http", 22}, {"partial", 18}, {"connect", 28}, {"stall", 28}, {"retry", 7}} {
			fault := failure.kind + "-" + id
			cases = append(cases, setupCase{name: fault, fault: fault, stop: "curl-" + id, diagnostic: "injected:" + fault, exit: failure.exit})
		}
		cases = append(cases, setupCase{name: "checksum_mismatch_" + id, fault: "corrupt-" + id, stop: "sha-" + id, diagnostic: "FAILED", exit: 1})
	}
	for _, stage := range []string{"install-bazelisk", "tar-just", "install-just", "dpkg", "ldrefresh", "ldlookup", "just-version", "python-version", "docker-info", "docker-pull"} {
		cases = append(cases, setupCase{name: stage + "_failure", fault: stage, stop: stage, diagnostic: "injected:" + stage, exit: 41})
	}
	cases = append(cases, setupCase{name: "loader_library_missing", fault: "loader-missing", stop: "ldlookup", diagnostic: "injected:loader-missing", exit: 1})
	if len(cases) != 31 {
		t.Fatalf("setup case population=%d, want 31", len(cases))
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Sequential subtests bound subprocess population on the shared machine.
			root := t.TempDir()
			bin, evidence, temp := filepath.Join(root, "bin"), filepath.Join(root, "evidence"), filepath.Join(root, "runner-temp")
			for _, dir := range []string{bin, evidence, temp} {
				if err := os.Mkdir(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			trace := filepath.Join(evidence, "commands")
			if err := os.WriteFile(trace, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			for _, tool := range []string{"curl", "sha256sum", "sudo", "install", "tar", "dpkg", "ldconfig", "just", "python3", "docker"} {
				if err := os.WriteFile(filepath.Join(bin, tool), []byte(clientSetupToolDouble), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			realSHA, err := exec.LookPath("sha256sum")
			if err != nil {
				t.Fatal(err)
			}
			// GitHub's configured bash supplies -e/-o pipefail already. Do not
			// accidentally make deleting the shipped `set` look more dangerous.
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", "--noprofile", "--norc", "-eo", "pipefail", "-c", script)
			evidenceDraftOwnProcessGroup(cmd)
			cmd.Dir = root
			// Remove inherited values first: an unset-env row must actually be unset.
			for _, entry := range os.Environ() {
				key, _, _ := strings.Cut(entry, "=")
				switch key {
				case "RUNNER_TEMP", "EVIDENCE", "FDB_VERSION", "FDB_REQUIRE_DOCKER", "BASH_ENV", "ENV":
					continue
				}
				if strings.HasPrefix(key, "BASH_FUNC_") {
					continue
				}
				cmd.Env = append(cmd.Env, entry)
			}
			cmd.Env = append(cmd.Env, "PATH="+bin+":"+os.Getenv("PATH"), "EVIDENCE="+evidence, "HARNESS_EVIDENCE="+evidence, "HARNESS_BIN="+bin, "TRACE="+trace, "FAULT="+tc.fault, "REAL_SHA256SUM="+realSHA, "FDB_VERSION="+job.Env["FDB_VERSION"], "FDB_REQUIRE_DOCKER="+job.Env["FDB_REQUIRE_DOCKER"], "LC_ALL=C")
			if !tc.unsetTemp {
				cmd.Env = append(cmd.Env, "RUNNER_TEMP="+temp)
			}
			for _, item := range []struct{ env, file string }{{"FIXTURE_BAZELISK", "bazelisk.bin"}, {"FIXTURE_JUST", "just.tar.gz"}, {"FIXTURE_FDB", "fdb-clients.deb"}} {
				cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%x", item.env, sha256.Sum256([]byte("fixture:"+item.file+"\n"))))
			}
			out, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("setup watchdog expired: %v\n%s", ctx.Err(), out)
			}
			if got := clientCorrectnessExit(err); got != tc.exit {
				t.Fatalf("setup exit=%d want %d: %v\n%s", got, tc.exit, err, out)
			}
			log, err := os.ReadFile(filepath.Join(evidence, "setup.log"))
			if err != nil {
				t.Fatalf("setup failure/success did not retain log: %v\n%s", err, out)
			}
			if tc.diagnostic != "" && (!strings.Contains(string(out), tc.diagnostic) || !strings.Contains(string(log), tc.diagnostic)) {
				t.Fatalf("missing diagnostic %q in output/log: output=%q log=%q", tc.diagnostic, out, log)
			}
			wantEvents := clientSetupExpectedEvents()
			if tc.unsetTemp {
				wantEvents = nil
			} else if tc.stop != "" {
				found := false
				for i, event := range wantEvents {
					if event.key == tc.stop {
						wantEvents, found = wantEvents[:i+1], true
						break
					}
				}
				if !found {
					t.Fatalf("unknown expected stop stage %q", tc.stop)
				}
			}
			gotTrace, err := os.ReadFile(trace)
			if err != nil {
				t.Fatal(err)
			}
			if want := clientSetupEncodeEvents(wantEvents); string(gotTrace) != want {
				t.Fatalf("setup command sequence=%q want %q\n%s", gotTrace, want, out)
			}
			if tc.name == "success" {
				for _, file := range []string{"verified-bazelisk.bin", "installed-bazelisk", "verified-just.tar.gz", "extracted-just", "installed-just", "verified-fdb-clients.deb", "installed-fdb", "refreshed-loader"} {
					if _, err := os.Stat(filepath.Join(evidence, file)); err != nil {
						t.Fatalf("successful setup missing side effect %s: %v", file, err)
					}
				}
			}
		})
	}
}
