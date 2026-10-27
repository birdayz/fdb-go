package integration

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	foundationdb "fdb.dev/pkg/testcontainers/foundationdb"
	"fdb.dev/tools/clientbench/internal/bench"
	"github.com/bazelbuild/rules_go/go/runfiles"
)

var (
	goBinary  = flag.String("go-binary", "", "Go backend runfile")
	cgoBinary = flag.String("cgo-binary", "", "CGo backend runfile")
)

func TestBothBackendsAgainstFDB(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container, err := foundationdb.Run(ctx, "", foundationdb.WithDirectIP())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stop, cancelStop := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelStop()
		if err := foundationdb.Terminate(stop, container); err != nil {
			t.Error(err)
		}
	})
	cluster, err := container.ClusterFilePath(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rf, err := runfiles.New()
	if err != nil {
		t.Fatal(err)
	}
	for _, arm := range []struct {
		name, path string
		native     bool
	}{{"go", *goBinary, false}, {"cgo", *cgoBinary, true}} {
		binary, err := rf.Rlocation(arm.path)
		if err != nil {
			t.Fatal(err)
		}
		binaryBytes, err := os.ReadFile(binary)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(binaryBytes)
		for _, name := range []string{"get100", "get1000", "get10000", "read1000", "grv", "set100", "range10", "range100", "ryw", "batchset50", "batchset50x1000", "getset100", "set100-paced"} {
			workload, paced := strings.CutSuffix(name, "-paced")
			t.Run(arm.name+"/"+name, func(t *testing.T) {
				output := filepath.Join(t.TempDir(), "result")
				args := []string{
					"--cluster-file", cluster, "--workload", workload, "--iterations", "5", "--warmup", "3", "--workers", "2",
					"--output", output, "--run-id", arm.name + "-" + name, "--block", "0", "--source-revision", "smoke-test",
					"--harness-id", "smoke-test", "--prefix", "clientbench/smoke/" + arm.name + "/" + name + "/",
				}
				if paced {
					args = append(args, "--rate", "200", "--seed", "5")
				}
				execute := func(args []string, environment ...string) ([]byte, error) {
					cmd := exec.CommandContext(ctx, binary, args...)
					cmd.Env = append(os.Environ(), "GOMAXPROCS=2", "GOGC=100", "GOMEMLIMIT=off", "GODEBUG=")
					cmd.Env = append(cmd.Env, environment...)
					return cmd.CombinedOutput()
				}
				if text, err := execute(args); err == nil || !strings.Contains(string(text), "--allow-writes is required") {
					t.Fatalf("missing write authority: err=%v output=%s", err, text)
				}
				args = append(args, "--allow-writes")
				for _, env := range []string{"GOMAXPROCS=", "FDB_NETWORK_OPTION_EXTERNAL_CLIENT_LIBRARY=unexpected"} {
					if text, err := execute(args, env); err == nil || !strings.Contains(string(text), "lifecycle accounting") {
						t.Fatalf("unsafe lifecycle environment %q accepted: err=%v output=%s", env, err, text)
					}
				}
				if text, err := execute(args); err != nil {
					t.Fatalf("benchmark failed: %v\n%s", err, text)
				}
				var summary struct {
					Success             bool                 `json:"success"`
					Completed           int                  `json:"completed"`
					Errors              int                  `json:"errors"`
					WarmupCompleted     int                  `json:"warmup_completed"`
					WarmupErrors        int                  `json:"warmup_errors"`
					PersistedValidation string               `json:"persisted_validation"`
					AfterIdentity       bench.Stats          `json:"stats_after_identity"`
					Before              bench.Stats          `json:"stats_before"`
					After               bench.Stats          `json:"stats_after"`
					Counters            bench.ClientCounters `json:"client_counters_delta"`
					Lifecycle           bench.Lifecycle      `json:"lifecycle"`
				}
				readJSON(t, filepath.Join(output, "summary.json"), &summary)
				if !summary.Success || summary.Completed != 5 || summary.WarmupCompleted != 3 || summary.Errors != 0 || summary.WarmupErrors != 0 || summary.PersistedValidation != "ok" {
					t.Fatalf("invalid completed run: %+v", summary)
				}
				if summary.After.UserUS+summary.After.SystemUS <= summary.Before.UserUS+summary.Before.SystemUS {
					t.Fatal("missing measured-phase getrusage CPU")
				}
				life := summary.Lifecycle
				identityCPU := summary.AfterIdentity.UserUS + summary.AfterIdentity.SystemUS
				beforeCPU := summary.Before.UserUS + summary.Before.SystemUS
				shutdownCPU := life.AfterShutdown.UserUS + life.AfterShutdown.SystemUS
				if identityCPU <= 0 || identityCPU >= beforeCPU || beforeCPU >= shutdownCPU || shutdownCPU-identityCPU >= shutdownCPU {
					t.Fatalf("identity hashing was not excluded before opening the backend: identity=%d before=%d shutdown=%d", identityCPU, beforeCPU, shutdownCPU)
				}
				if !life.Complete || !life.DatabaseClosed || !life.MeasurementBarrier || life.NetworkStopped != arm.native ||
					life.AfterDrain.UserUS+life.AfterDrain.SystemUS <= summary.After.UserUS+summary.After.SystemUS ||
					life.AfterShutdown.UserUS+life.AfterShutdown.SystemUS <= life.AfterDrain.UserUS+life.AfterDrain.SystemUS {
					t.Fatalf("missing complete-lifecycle CPU accounting: %+v", life)
				}
				for _, drain := range []bench.FinalizerDrain{life.MeasurementDrain, life.ShutdownDrain} {
					if !drain.Complete || drain.Rounds < 2 || drain.WallNS <= 0 ||
						drain.After.FinalizersExecuted != drain.After.FinalizersQueued ||
						drain.After.CleanupsExecuted != drain.After.CleanupsQueued {
						t.Fatalf("incomplete finalizer fixpoint: %+v", drain)
					}
				}
				observation := "observed"
				if arm.native {
					observation = "unobserved"
				}
				if summary.Counters.Observation != observation {
					t.Fatalf("counter observation = %q", summary.Counters.Observation)
				}
				var manifest struct {
					Config  bench.Config `json:"config"`
					Backend struct {
						Name           string            `json:"backend"`
						Native         bool              `json:"native_library"`
						ExecutableSHA  string            `json:"executable_sha256"`
						Libraries      map[string]string `json:"loaded_fdb_libraries_sha256"`
						EffectiveMinNS int64             `json:"effective_min_ns"`
						EffectiveMaxNS int64             `json:"effective_max_ns"`
						AdaptiveLinger int64             `json:"adaptive_linger_ns"`
					} `json:"backend"`
				}
				readJSON(t, filepath.Join(output, "manifest.json"), &manifest)
				if wantRate := map[bool]float64{true: 200}[paced]; manifest.Config.Rate != wantRate || (paced && manifest.Config.Seed != 5) {
					t.Fatalf("manifest pacing %v/%d, want rate %v", manifest.Config.Rate, manifest.Config.Seed, wantRate)
				}
				if manifest.Config.Workload != workload || manifest.Config.Workers != 2 || manifest.Config.Profile != "none" || manifest.Backend.Name != arm.name || manifest.Backend.Native != arm.native || (len(manifest.Backend.Libraries) > 0) != arm.native || manifest.Backend.ExecutableSHA != hex.EncodeToString(digest[:]) {
					t.Fatalf("invalid backend identity: %+v", manifest)
				}
				checkSamples(t, filepath.Join(output, "samples.csv"), "measure", 5, paced)
				checkSamples(t, filepath.Join(output, "warmup.csv"), "warmup", 3, paced)
				if text, err := execute(args); err == nil || !strings.Contains(string(text), "file exists") {
					t.Fatalf("existing output not rejected: err=%v output=%s", err, text)
				}
				if name == "getset100" {
					lingerArgs := append(slices.Clone(args), "--adaptive-linger=20us")
					lingerArgs[slices.Index(lingerArgs, "--output")+1] = filepath.Join(t.TempDir(), "linger")
					if arm.native {
						if text, err := execute(lingerArgs); err == nil || !strings.Contains(string(text), "Go client only") {
							t.Fatalf("cgo accepted adaptive-linger: err=%v output=%s", err, text)
						}
					} else {
						if text, err := execute(lingerArgs); err != nil {
							t.Fatalf("go adaptive-linger failed: %v\n%s", err, text)
						}
						readJSON(t, filepath.Join(lingerArgs[slices.Index(lingerArgs, "--output")+1], "manifest.json"), &manifest)
						if manifest.Config.AdaptiveLinger != 20*time.Microsecond || manifest.Backend.AdaptiveLinger != int64(20*time.Microsecond) || manifest.Backend.EffectiveMinNS != 0 || manifest.Backend.EffectiveMaxNS != 0 {
							t.Fatalf("adaptive-linger manifest = %+v", manifest)
						}
						fixedArgs := append(slices.Clone(args), "--coalesce-delay=10us,20us")
						fixedOut := filepath.Join(t.TempDir(), "cd-fixed")
						fixedArgs[slices.Index(fixedArgs, "--output")+1] = fixedOut
						if text, err := execute(fixedArgs); err != nil {
							t.Fatalf("go coalesce-delay=10us,20us failed: %v\n%s", err, text)
						}
						readJSON(t, filepath.Join(fixedOut, "manifest.json"), &manifest)
						if manifest.Backend.EffectiveMinNS != int64(10*time.Microsecond) || manifest.Backend.EffectiveMaxNS != int64(20*time.Microsecond) || manifest.Backend.AdaptiveLinger != 0 {
							t.Fatalf("coalesce-delay=10us,20us effective connection delay = %+v", manifest.Backend)
						}
					}
				}
				t.Logf("CLIENTBENCH_SMOKE %s/%s: measured=5 warmup=3 persisted=ok backend=%s", arm.name, name, manifest.Backend.Name)
			})
		}
	}
}

func readJSON(t *testing.T, path string, out any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatal(err)
	}
}

func checkSamples(t *testing.T, path, phase string, count int, paced bool) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != count+1 || strings.Join(rows[0], ",") != "phase,index,worker,start_offset_ns,latency_ns,attempts,error,check_status,scheduled_offset_ns" {
		t.Fatalf("wrong CSV population/schema: %v", rows)
	}
	for i, row := range rows[1:] {
		latency, err := strconv.ParseInt(row[4], 10, 64)
		if err != nil || latency <= 0 || row[0] != phase || row[1] != strconv.Itoa(i) || row[2] != strconv.Itoa(i%2) || row[6] != "" || row[7] != "ok" {
			t.Fatalf("invalid %s row: %v", phase, row)
		}
		start, err1 := strconv.ParseInt(row[3], 10, 64)
		scheduled, err2 := strconv.ParseInt(row[8], 10, 64)
		if err1 != nil || err2 != nil || (!paced && scheduled != -1) || (paced && (scheduled < 0 || start < scheduled)) {
			t.Fatalf("invalid %s schedule (paced=%t): %v", phase, paced, row)
		}
	}
}
