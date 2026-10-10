package infra

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRunnerJobGuard(t *testing.T) {
	t.Parallel()
	guard, err := filepath.Abs("runner-job-guard.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, repository, event, payload, want string
		allow                                  bool
	}{
		{"push", "birdayz/fdb-go", "push", `{}`, "", true},
		{"issues", "birdayz/fdb-go", "issues", `{"issue":{"body":"@claude"}}`, "", true},
		{"wrong_repository", "outsider/fdb-go", "push", `{}`, "refusing job for repository", false},
		{"missing_repository", "", "push", `{}`, "refusing job for repository", false},
		{"own_pull_request", "birdayz/fdb-go", "pull_request", `{"pull_request":{"head":{"repo":{"full_name":"birdayz/fdb-go"}}}}`, "", true},
		{"fork_pull_request", "birdayz/fdb-go", "pull_request", `{"pull_request":{"head":{"repo":{"full_name":"outsider/fdb-go"}}}}`, "refusing pull_request from fork", false},
		{"fork_pull_request_target", "birdayz/fdb-go", "pull_request_target", `{"pull_request":{"head":{"repo":{"full_name":"outsider/fdb-go"}}}}`, "refusing pull_request_target from fork", false},
		{"fork_review", "birdayz/fdb-go", "pull_request_review", `{"pull_request":{"head":{"repo":{"full_name":"outsider/fdb-go"}}}}`, "refusing pull_request_review from fork", false},
		{"fork_review_comment", "birdayz/fdb-go", "pull_request_review_comment", `{"pull_request":{"head":{"repo":{"full_name":"outsider/fdb-go"}}}}`, "refusing pull_request_review_comment from fork", false},
		{"missing_head", "birdayz/fdb-go", "pull_request", `{"pull_request":{}}`, "from fork 'unknown'", false},
		{"deleted_head_repository", "birdayz/fdb-go", "pull_request", `{"pull_request":{"head":{"repo":null}}}`, "from fork 'unknown'", false},
		{"malformed_pull_request", "birdayz/fdb-go", "pull_request", `{`, "", false},
		{"issue_comment", "birdayz/fdb-go", "issue_comment", `{"issue":{"number":1}}`, "", true},
		{"malformed_issue_comment", "birdayz/fdb-go", "issue_comment", `{`, "", false},
		{"missing_issue", "birdayz/fdb-go", "issue_comment", `{}`, "", false},
		{"null_issue", "birdayz/fdb-go", "issue_comment", `{"issue":null}`, "", false},
		{"null_pull_request", "birdayz/fdb-go", "issue_comment", `{"issue":{"pull_request":null}}`, "refusing issue_comment on a pull request", false},
		{"pull_request_comment", "birdayz/fdb-go", "issue_comment", `{"issue":{"pull_request":{"url":"https://api.github.com/repos/birdayz/fdb-go/pulls/1"}}}`, "refusing issue_comment on a pull request", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			payload := filepath.Join(t.TempDir(), "event.json")
			if err := os.WriteFile(payload, []byte(tc.payload), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", guard)
			cmd.Env = append(
				os.Environ(),
				"GITHUB_REPOSITORY="+tc.repository,
				"GITHUB_EVENT_NAME="+tc.event,
				"GITHUB_EVENT_PATH="+payload,
			)
			out, err := cmd.CombinedOutput()
			if (err == nil) != tc.allow {
				t.Fatalf("guard exit=%v, want allow=%v: %s", err, tc.allow, out)
			}
			if !strings.Contains(string(out), tc.want) {
				t.Fatalf("guard output=%q, want %q", out, tc.want)
			}
		})
	}
}

func TestCloudInitInstallsRunnerJobGuard(t *testing.T) {
	t.Parallel()
	tmpl, err := os.ReadFile("cloud-init.yaml")
	if err != nil {
		t.Fatal(err)
	}
	guard, err := os.ReadFile("runner-job-guard.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"classic", "scaleset"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			vars := make(map[string]string, len(templateVars))
			for key, value := range templateVars {
				vars[key] = value
			}
			vars["runner_mode"] = mode
			rendered, err := renderTemplate(string(tmpl), vars)
			if err != nil {
				t.Fatal(err)
			}
			var config struct {
				Files []struct {
					Path, Owner, Permissions, Content string
				} `yaml:"write_files"`
				Commands []string `yaml:"runcmd"`
			}
			if err := yaml.Unmarshal([]byte(rendered), &config); err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			found := false
			for _, file := range config.Files {
				if file.Path == "/usr/local/bin/runner-job-guard.sh" {
					found = true
					if file.Owner != "root:root" || file.Permissions != "0755" || file.Content != string(guard) {
						t.Fatal("cloud-init must bake the exact reviewed guard as root:root 0755, not fetch it from a checkout")
					}
				}
				path := filepath.Join(root, file.Path)
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(file.Content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if !found {
				t.Fatal("cloud-init does not install the runner job guard")
			}
			var provision string
			for _, command := range config.Commands {
				if strings.Contains(command, "RUNNER_MODE=") {
					if provision != "" {
						t.Fatal("multiple runner provisioning blocks")
					}
					provision = command
				}
			}
			if provision == "" {
				t.Fatal("no runner provisioning block")
			}
			// Run the shipped mode branch in a fixture filesystem; no downloads,
			// registration, systemd services or account changes reach the host.
			provision = strings.NewReplacer(
				"/home/runner", root+"/home/runner",
				"/mnt/ci-data", root+"/mnt/ci-data",
				"/etc/", root+"/etc/",
				"/usr/local/bin/fetch-verified.sh", "infraTestFetch",
				"./svc.sh", "infraTestService",
				"-o runner -g runner ", "",
			).Replace(provision)
			harness := `
infraTestFetch() { : > "$3"; }
tar() { :; }
chown() { :; }
su() { :; }
rsync() { :; }
infraTestService() {
  if [ "$1" = start ]; then
    cat "$FIXTURE/etc/systemd/system/actions.runner.fixture.service.d/"*.conf > "$FIXTURE/started-unit"
  fi
}
systemctl() {
  case "$*" in
    'list-unit-files --no-legend actions.runner.*.service') echo actions.runner.fixture.service ;;
    'enable --now bazelscaleset.service') cat "$FIXTURE/etc/systemd/system/bazelscaleset.service" > "$FIXTURE/started-unit" ;;
  esac
}
`
			if err := os.WriteFile(filepath.Join(root, "etc", "environment"), []byte("PATH=\"/usr/bin:/bin\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", "-euo", "pipefail", "-c", harness+provision)
			cmd.Env = append(os.Environ(), "FIXTURE="+root)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("provision %s: %v\n%s", mode, err, out)
			}
			unit, err := os.ReadFile(filepath.Join(root, "started-unit"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(unit), "\nEnvironment=ACTIONS_RUNNER_HOOK_JOB_STARTED=/usr/local/bin/runner-job-guard.sh\n") {
				t.Fatalf("%s runner started without its job hook environment:\n%s", mode, unit)
			}
		})
	}
}
