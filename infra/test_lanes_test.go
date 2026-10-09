package infra

import (
	"os"
	"os/exec"
	"path/filepath"
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
		{name: "fast", recipe: "test", want: "test //... --build_tests_only --test_tag_filters=-test-full,-conformance_java,-stress --local_test_jobs=0 --local_resources=memory=HOST_RAM*0.8"},
		{name: "full_includes_explicit_manual", recipe: "test-full", labels: "//pkg:unit_test\n//pkg:manual_test", want: "test //pkg:unit_test //pkg:manual_test --local_test_jobs=0 --local_resources=memory=HOST_RAM*0.8"},
		{name: "fast_args", recipe: "test", args: "--test_output=errors", want: "test //... --build_tests_only --test_tag_filters=-test-full,-conformance_java,-stress --local_test_jobs=0 --local_resources=memory=HOST_RAM*0.8 --test_output=errors"},
		{name: "full_args", recipe: "test-full", labels: "//pkg:unit_test", args: "--test_output=errors", want: "test //pkg:unit_test --local_test_jobs=0 --local_resources=memory=HOST_RAM*0.8 --test_output=errors"},
		{name: "empty_is_not_green", recipe: "test-full", fail: true},
		{name: "query_failure", recipe: "test-full", labels: "//pkg:partial_test", queryFail: true, fail: true},
		{name: "full_test_failure", recipe: "test-full", labels: "//pkg:unit_test", want: "test //pkg:unit_test --local_test_jobs=0 --local_resources=memory=HOST_RAM*0.8", testFail: true, fail: true},
		{name: "fast_test_failure", recipe: "test", want: "test //... --build_tests_only --test_tag_filters=-test-full,-conformance_java,-stress --local_test_jobs=0 --local_resources=memory=HOST_RAM*0.8", testFail: true, fail: true},
		// test_full_first is stubbed as "//pkg:slow_test //pkg:gone_test": a
		// listed pole moves to the front, a stale entry is dropped, and every
		// queried label is still passed exactly once.
		{name: "full_poles_first", recipe: "test-full", labels: "//pkg:a_test\n//pkg:slow_test\n//pkg:z_test", want: "test //pkg:slow_test //pkg:a_test //pkg:z_test --local_test_jobs=0 --local_resources=memory=HOST_RAM*0.8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, rest, ok := strings.Cut(string(data), "\n"+tc.recipe+" *args:\n")
			if !ok {
				t.Fatalf("missing recipe %s", tc.recipe)
			}
			body, _, _ := strings.Cut(rest, "\n\n")
			body = strings.ReplaceAll(body, "{{args}}", tc.args)
			body = strings.ReplaceAll(body, "{{test_sched}}", justVar(t, string(data), "test_sched"))
			body = strings.ReplaceAll(body, "{{test_full_first}}", "//pkg:slow_test //pkg:gone_test")
			dir := t.TempDir()
			stub := `#!/bin/bash
if [ "$1" = query ]; then
    [ "$2" = 'kind(".*_test", //...)' ] || exit 91
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

// justVar returns the value of a `name := "value"` justfile variable.
func justVar(t *testing.T, justfile, name string) string {
	t.Helper()
	_, rest, ok := strings.Cut(justfile, "\n"+name+" := \"")
	if !ok {
		t.Fatalf("missing justfile variable %s", name)
	}
	v, _, _ := strings.Cut(rest, "\"\n")
	return v
}
