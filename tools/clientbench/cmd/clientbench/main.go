package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	"fdb.dev/tools/clientbench/internal/bench"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	var cfg bench.Config
	flag.StringVar(&cfg.ClusterFile, "cluster-file", "", "existing FDB cluster file")
	flag.StringVar(&cfg.Workload, "workload", "get100", "get100|get1000|get10000|read1000|grv|set100|range10|range100|ryw|batchset50|batchset50x1000")
	flag.IntVar(&cfg.Iterations, "iterations", 2000, "total measured operations across all workers")
	flag.IntVar(&cfg.Warmup, "warmup", 500, "total warmup operations across all workers")
	flag.IntVar(&cfg.Workers, "workers", 1, "closed-loop workers, deterministic round-robin assignment")
	flag.StringVar(&cfg.Output, "output", "", "output directory; refuses existing manifest/samples")
	flag.StringVar(&cfg.RunID, "run-id", "", "campaign run ID")
	flag.IntVar(&cfg.Block, "block", -1, "paired campaign block")
	flag.StringVar(&cfg.SourceRevision, "source-revision", "", "immutable parent source revision")
	flag.StringVar(&cfg.HarnessID, "harness-id", "", "harness source artifact SHA256")
	flag.BoolVar(&cfg.AllowWrites, "allow-writes", false, "authorize fixture overwrites under prefix (required for every workload)")
	flag.StringVar(&cfg.Prefix, "prefix", "clientbench/v1/", "reserved benchmark prefix")
	flag.Int64Var(&cfg.TimeoutMS, "transaction-timeout-ms", 30000, "database transaction timeout")
	flag.Int64Var(&cfg.RetryLimit, "retry-limit", 10, "database transaction retry limit")
	flag.StringVar(&cfg.Profile, "profile", "none", "none|cpu|heap|mutex|block|trace; profiling runs are separate")
	flag.Parse()
	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %v", flag.Args())
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	db, meta, err := openBackend(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	if info, ok := debug.ReadBuildInfo(); ok {
		meta["go_build_info"] = info.String()
	}
	if exe, err := os.Executable(); err == nil {
		meta["executable"] = exe
		digest, err := hashFile(exe)
		if err != nil {
			return err
		}
		meta["executable_sha256"] = digest
	}
	maps, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		return err
	}
	libraries := map[string]string{}
	for _, line := range strings.Split(string(maps), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 || !strings.Contains(fields[5], "libfdb_c") {
			continue
		}
		path, err := filepath.EvalSymlinks(fields[5])
		if err != nil {
			return err
		}
		if _, ok := libraries[path]; !ok {
			digest, err := hashFile(path)
			if err != nil {
				return err
			}
			libraries[path] = digest
		}
	}
	meta["loaded_fdb_libraries_sha256"] = libraries
	if native, _ := meta["native_library"].(bool); native != (len(libraries) > 0) {
		return fmt.Errorf("native linkage assertion failed: native=%t mapped=%v", native, libraries)
	}
	return bench.Run(cfg, db, meta)
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
