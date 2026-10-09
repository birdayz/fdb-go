package infra

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
			cmd.Env = append(os.Environ(),
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
