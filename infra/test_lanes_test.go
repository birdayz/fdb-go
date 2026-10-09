package infra

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestPrecommitFastLane(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../justfile")
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(data), "<< 'HOOK'\n")
	if !ok {
		t.Fatal("missing pre-commit hook")
	}
	hook, _, ok := strings.Cut(rest, "\n    HOOK")
	if !ok {
		t.Fatal("missing hook terminator")
	}
	for _, tc := range []struct {
		name, secretExit, dirtyExit, testExit, want string
		fail                                        bool
	}{
		{"clean", "0", "0", "0", "secret-scan\ntest\n", false},
		{"secret_failure", "1", "0", "0", "secret-scan\n", true},
		{"dirty_tree", "0", "1", "0", "secret-scan\n", true},
		{"test_failure", "0", "0", "1", "secret-scan\ntest\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			for name, body := range map[string]string{
				"just": "#!/bin/bash\nprintf '%s\\n' \"$*\" >> \"$CALLS\"\nif [ \"$1\" = secret-scan ]; then exit \"$SECRET_EXIT\"; fi\nexit \"$TEST_EXIT\"\n",
				"git":  "#!/bin/bash\nif [ \"$*\" = 'diff --quiet' ]; then exit \"$DIRTY_EXIT\"; fi\n",
			} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			calls := filepath.Join(dir, "calls")
			cmd := exec.Command("bash", "-c", hook)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "CALLS="+calls, "SECRET_EXIT="+tc.secretExit, "DIRTY_EXIT="+tc.dirtyExit, "TEST_EXIT="+tc.testExit)
			out, err := cmd.CombinedOutput()
			if (err != nil) != tc.fail {
				t.Fatalf("exit=%v, want failure=%v: %s", err, tc.fail, out)
			}
			got, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("hook commands=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestJustTestLanes(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../justfile")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, recipe, labels, args, want string
		queryFail, testFail, fail        bool
	}{
		{name: "fast", recipe: "test", want: "test //... --build_tests_only --test_tag_filters=-test-full,-conformance_java,-stress"},
		{name: "full_includes_explicit_manual", recipe: "test-full", labels: "//pkg:unit_test\n//pkg:manual_test", want: "test //pkg:unit_test //pkg:manual_test"},
		{name: "fast_args", recipe: "test", args: "--test_output=errors", want: "test //... --build_tests_only --test_tag_filters=-test-full,-conformance_java,-stress --test_output=errors"},
		{name: "full_args", recipe: "test-full", labels: "//pkg:unit_test", args: "--test_output=errors", want: "test //pkg:unit_test --test_output=errors"},
		{name: "empty_is_not_green", recipe: "test-full", fail: true},
		{name: "query_failure", recipe: "test-full", labels: "//pkg:partial_test", queryFail: true, fail: true},
		{name: "full_test_failure", recipe: "test-full", labels: "//pkg:unit_test", want: "test //pkg:unit_test", testFail: true, fail: true},
		{name: "fast_test_failure", recipe: "test", want: "test //... --build_tests_only --test_tag_filters=-test-full,-conformance_java,-stress", testFail: true, fail: true},
		// test_full_first is stubbed as "//pkg:slow_test //pkg:gone_test": a
		// listed pole moves to the front, a stale entry is dropped, and every
		// queried label is still passed exactly once.
		{name: "full_poles_first", recipe: "test-full", labels: "//pkg:a_test\n//pkg:slow_test\n//pkg:z_test", want: "test //pkg:slow_test //pkg:a_test //pkg:z_test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, rest, ok := strings.Cut(string(data), "\n"+tc.recipe+" *args:\n")
			if !ok {
				t.Fatalf("missing recipe %s", tc.recipe)
			}
			body, _, _ := strings.Cut(rest, "\n\n")
			body = strings.ReplaceAll(body, "{{args}}", tc.args)
			body = strings.ReplaceAll(body, "{{test_full_first}}", "//pkg:slow_test //pkg:gone_test")
			dir := t.TempDir()
			stub := `#!/bin/bash
if [ "$1" = query ]; then
    [ "$2" = 'kind(".*_test", //...) except attr(tags, "\bnightly\b", //...)' ] || exit 91
    printf '%s\n' "$LABELS"
    exit "$QUERY_EXIT"
fi
printf '%s\n' "$*" > "$CALLS"
exit "$TEST_EXIT"
`
			if err := os.WriteFile(filepath.Join(dir, "bazelisk"), []byte(stub), 0o700); err != nil {
				t.Fatal(err)
			}
			calls := filepath.Join(dir, "calls")
			queryExit, testExit := "0", "0"
			if tc.queryFail {
				queryExit = "1"
			}
			if tc.testFail {
				testExit = "1"
			}
			cmd := exec.Command("bash", "-c", body)
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "LABELS="+tc.labels, "CALLS="+calls, "QUERY_EXIT="+queryExit, "TEST_EXIT="+testExit)
			out, err := cmd.CombinedOutput()
			if (err != nil) != tc.fail {
				t.Fatalf("exit=%v, want failure=%v: %s", err, tc.fail, out)
			}
			got, err := os.ReadFile(calls)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if strings.TrimSpace(string(got)) != tc.want {
				t.Fatalf("Bazel invocation=%q, want %q", got, tc.want)
			}
		})
	}
}

// TestWorkflowsUseTheCIConfig requires every Bazel command a workflow runs to
// pass --config=ci, so the CI-only settings in .bazelrc (test:ci: the runner's
// memory fraction, the detailed summary, the profile) reach every lane rather
// than only the ones someone remembered.
func TestWorkflowsUseTheCIConfig(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("../.github/workflows/*.yml")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no workflow files found: the check would pass over nothing")
	}
	cmd := regexp.MustCompile(`\bbazelisk((?:\s+--\S+|\s+\\\n\s*)*)\s+(test|build|query|cquery|aquery|coverage|run|info|fetch|mod)\b(\s+--config=ci)?`)
	checked := 0
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range cmd.FindAllStringSubmatchIndex(string(data), -1) {
			lineStart := strings.LastIndex(string(data[:m[0]]), "\n") + 1
			if strings.HasPrefix(strings.TrimSpace(string(data[lineStart:m[0]])), "#") {
				continue // prose in a comment
			}
			checked++
			if m[6] < 0 {
				line := strings.Count(string(data[:m[0]]), "\n") + 1
				t.Errorf("%s:%d: %q runs without --config=ci", filepath.Base(f), line, string(data[m[0]:m[1]]))
			}
		}
	}
	if checked == 0 {
		t.Fatal("found no bazelisk commands in the workflows: the check matched nothing")
	}
	t.Logf("%d workflow Bazel commands pass --config=ci", checked)
}
