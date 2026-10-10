package infra

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
)

func TestFDBCppBuildResourceContract(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../cmd/fdb-schema-extract/fdb_cpp_build.bzl")
	if err != nil {
		t.Fatal(err)
	}
	rule := string(data)
	for _, want := range []string{
		`return {"cpu": _COMPACT_CPUS, "memory": _COMPACT_MEMORY_MB}`,
		`return {"cpu": _LARGE_CPUS, "memory": _LARGE_MEMORY_MB}`,
		`profile = ctx.attr._resources[FDBCppResourcesInfo].value`,
		`resources = _compact_resources`,
		`resources = _large_resources`,
		`resource_set = resources,`,
		`"FDB_BUILD_CPUS": str(cpus),`,
		`"FDB_BUILD_MEMORY_MB": str(memory),`,
		`cxx_flags = "-O3 -DNDEBUG"`,
		`fail("FDB C++ resources must be compact or large, got " + value)`,
		`build_setting = config.string(flag = True)`,
		`"_resources": attr.label(default = "//cmd/fdb-schema-extract:resources")`,
		`execution_requirements = {"no-sandbox": "1", "no-remote-exec": "1"}`,
	} {
		if !strings.Contains(rule, want) {
			t.Errorf("missing scheduler/runtime resource contract: %s", want)
		}
	}
	if !regexp.MustCompile(`_BUILD_IMAGE = "foundationdb/build@sha256:[a-f0-9]{64}"`).MatchString(rule) {
		t.Fatal("Docker build image must be pinned by digest")
	}
}

func TestFDBCppBuildFailureAndCleanup(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../cmd/fdb-schema-extract/fdb_cpp_build.bzl")
	if err != nil {
		t.Fatal(err)
	}
	profiles := map[string]map[string]string{}
	for _, profile := range []string{"compact", "large"} {
		constants := map[string]string{}
		for _, name := range []string{"CPUS", "MEMORY_MB"} {
			match := regexp.MustCompile(`_` + strings.ToUpper(profile) + `_` + name + ` = ([1-9][0-9]*)`).FindSubmatch(data)
			if match == nil {
				t.Fatalf("missing positive resource constant %s/%s", profile, name)
			}
			constants[name] = string(match[1])
		}
		profiles[profile] = constants
	}
	// The default profile must fit a cpx32 runner (main.tf) under Bazel's 0.67 of RAM,
	// and keep the -j3 width that finished a cold build inside the CI job.
	if c := profiles["compact"]; c["CPUS"] != "3" || c["MEMORY_MB"] != "5120" {
		t.Errorf("compact profile = %s CPUs / %s MB, want 3 / 5120 for the 4 vCPU / 8 GB CI runners", c["CPUS"], c["MEMORY_MB"])
	}
	commands := regexp.MustCompile(`(?s)fdb_cpp_build\(\s*name = "([^"]+)",.*?\n    cmd = """(.*?)"""`)
	count := 0
	for _, build := range []string{"../cmd/fdb-schema-extract/BUILD.bazel", "../cmd/fdb-diff-oracle/BUILD.bazel"} {
		data, err := os.ReadFile(build)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range commands.FindAllStringSubmatch(string(data), -1) {
			count++
			for profile, constants := range profiles {
				t.Run(match[1]+"/"+profile, func(t *testing.T) {
					t.Parallel()
					body := match[2]
					for _, want := range []string{
						"--cpus=$(FDB_BUILD_CPUS)",
						"--memory=$(FDB_BUILD_MEMORY_MB)m",
						"--memory-swap=$(FDB_BUILD_MEMORY_MB)m",
						"ninja -C /tmp/build -j$(FDB_BUILD_CPUS)",
						"IMAGE=\"$(FDB_BUILD_IMAGE)\"",
					} {
						if !strings.Contains(body, want) {
							t.Errorf("missing enforced build resource bound: %s", want)
						}
					}
					if match[1] == "fdb_cmake_build" {
						for _, want := range []string{
							"$(location @foundationdb//:CMakeLists.txt)",
							`-DCMAKE_CXX_FLAGS_RELEASE="$(FDB_CXX_FLAGS)"`,
							"ninja -C /tmp/build -j$(FDB_BUILD_CPUS) fdbclient ",
						} {
							if !strings.Contains(body, want) {
								t.Errorf("missing tool-only CMake build input: %s", want)
							}
						}
					}
					for _, forbidden := range []string{"nproc", "MemAvailable", "head -1"} {
						if strings.Contains(body, forbidden) {
							t.Errorf("build width escapes scheduler reservation via %s", forbidden)
						}
					}
					dir := t.TempDir()
					writeBuildStub(t, filepath.Join(dir, "docker"), `#!/bin/bash
if [ "$1" = run ]; then
    printf '%s\n' "$@" > "$CALLS"
    for arg; do
        case "$arg" in
            --cidfile=*)
                cidfile="${arg#--cidfile=}"
                printf fake-fdb-cpp > "$cidfile"
                printf '%s' "${cidfile%/*}" > "$TEMP_DIR"
                ;;
        esac
    done
    exit 23
fi
if [ "$*" = 'rm -f fake-fdb-cpp' ]; then
    touch "$CLEANED"
    exit 0
fi
exit 98
`)
					body = strings.ReplaceAll(body, "$(FDB_BUILD_CPUS)", constants["CPUS"])
					body = strings.ReplaceAll(body, "$(FDB_BUILD_MEMORY_MB)", constants["MEMORY_MB"])
					body = strings.ReplaceAll(body, "$(FDB_BUILD_IMAGE)", "fixture-image")
					body = strings.ReplaceAll(body, "$(FDB_CXX_FLAGS)", "-O3 -DNDEBUG")
					body = strings.ReplaceAll(body, "$(SRCS)", filepath.Join(dir, "CMakeLists.txt"))
					body = regexp.MustCompile(`\$\(location [^)]+\)`).ReplaceAllString(body, filepath.Join(dir, "input"))
					body = strings.ReplaceAll(body, "$$", "$")
					cmd := exec.Command("bash", "-c", body)
					cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "CALLS="+filepath.Join(dir, "calls"), "TEMP_DIR="+filepath.Join(dir, "temp-dir"), "CLEANED="+filepath.Join(dir, "cleaned"))
					output, err := cmd.CombinedOutput()
					requireBuildExit(t, err, 23, output)
					calls, err := os.ReadFile(filepath.Join(dir, "calls"))
					if err != nil {
						t.Fatal(err)
					}
					for _, limit := range []string{"--cpus=" + constants["CPUS"], "--memory=" + constants["MEMORY_MB"] + "m", "--memory-swap=" + constants["MEMORY_MB"] + "m"} {
						if !strings.Contains(string(calls), "\n"+limit+"\n") {
							t.Errorf("Docker did not receive scheduler-matched limit %s", limit)
						}
					}
					if _, err := os.Stat(filepath.Join(dir, "cleaned")); err != nil {
						t.Fatalf("failed build did not clean its container: %v", err)
					}
					tempDir, err := os.ReadFile(filepath.Join(dir, "temp-dir"))
					if err != nil {
						t.Fatal(err)
					}
					if _, err := os.Stat(string(tempDir)); !os.IsNotExist(err) {
						t.Fatalf("build temp directory remains: %s (stat: %v)", tempDir, err)
					}

					// Exercise the shipped inner shell options and compiler pipelines:
					// tail succeeding must not turn a compiler failure into a green build.
					inner := regexp.MustCompile(`(?s)bash -c '\s*(set -[^\n]+).*?\n            '`).FindStringSubmatch(body)
					if inner == nil {
						t.Fatal("missing Docker shell options")
					}
					pipelines := regexp.MustCompile(`(?s)(?:ninja|cmake) -[^\n]*(?:\\\n[^\n]*)*?\| tail -[0-9]+`).FindAllString(inner[0], -1)
					if len(pipelines) == 0 {
						t.Fatal("no compiler pipelines found")
					}
					for _, tool := range []string{"ninja", "cmake"} {
						writeBuildStub(t, filepath.Join(dir, tool), "#!/bin/bash\nexit 43\n")
					}
					for _, pipeline := range pipelines {
						pipeline = strings.ReplaceAll(pipeline, "/out/build.log", filepath.Join(dir, "build.log"))
						cmd := exec.Command("bash", "-c", inner[1]+"\n"+pipeline)
						cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
						output, err := cmd.CombinedOutput()
						requireBuildExit(t, err, 43, output)
					}
				})
			}
		}
	}
	if count != 3 {
		t.Fatalf("found %d C++ build actions, want 3", count)
	}
}

func writeBuildStub(t *testing.T, path, body string) {
	t.Helper()
	syscall.ForkLock.Lock()
	err := os.WriteFile(path, []byte(body), 0o700)
	syscall.ForkLock.Unlock()
	if err != nil {
		t.Fatal(err)
	}
}

func requireBuildExit(t *testing.T, err error, want int, output []byte) {
	t.Helper()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != want {
		t.Fatalf("exit=%v, want %d: %s", err, want, output)
	}
}
