// Package benchcluster gives benchmark binaries one FoundationDB cluster file.
//
// With FDB_BENCH_CLUSTER_FILE set, every benchmark in the process uses that
// existing cluster and no container is started; the cluster is never written
// to here. Unset, the first caller starts one FoundationDB container (API 730),
// as the classic benchmarks do, and Shutdown stops it.
package benchcluster

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
)

// EnvClusterFile names an existing cluster file for every benchmark in the process.
const EnvClusterFile = "FDB_BENCH_CLUSTER_FILE"

var (
	once      sync.Once
	path      string
	external  bool
	container *foundationdbtc.Container
	tempFile  string
	startErr  error
)

// ClusterFile returns the process's cluster file, starting the fallback
// container on first use when EnvClusterFile is unset.
func ClusterFile(tb testing.TB) string {
	tb.Helper()
	once.Do(func() {
		if env := os.Getenv(EnvClusterFile); env != "" {
			path, startErr = externalClusterFile(env)
			external = startErr == nil
			return
		}
		path, startErr = startContainer()
	})
	if startErr != nil {
		tb.Fatalf("benchmark cluster: %v", startErr)
	}
	return path
}

// External reports whether the process uses EnvClusterFile. It is meaningful
// after ClusterFile.
func External() bool { return external }

// externalClusterFile validates an EnvClusterFile value: a readable,
// nonempty regular file.
func externalClusterFile(env string) (string, error) {
	info, err := os.Stat(env)
	if err != nil {
		return "", fmt.Errorf("%s: %w", EnvClusterFile, err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return "", fmt.Errorf("%s=%q is not a nonempty regular file", EnvClusterFile, env)
	}
	return env, nil
}

func startContainer() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c, err := foundationdbtc.Run(ctx, "", foundationdbtc.WithAPIVersion(730))
	if err != nil {
		return "", fmt.Errorf("start FDB container: %w", err)
	}
	container = c
	content, err := c.ClusterFile(ctx)
	if err != nil {
		return "", fmt.Errorf("container cluster file: %w", err)
	}
	f, err := os.CreateTemp("", "fdb_bench_*.cluster")
	if err != nil {
		return "", err
	}
	tempFile = f.Name()
	_, writeErr := f.WriteString(content)
	if err := errors.Join(writeErr, f.Close()); err != nil {
		return "", err
	}
	return tempFile, nil
}

// Shutdown stops a container ClusterFile started and removes its cluster
// file. An external cluster is left untouched. Call it from TestMain after
// m.Run; it returns the cleanup failure, if any.
func Shutdown() error {
	var errs []error
	if container != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		errs = append(errs, container.Terminate(ctx))
		cancel()
		container = nil
	}
	if tempFile != "" {
		errs = append(errs, os.Remove(tempFile))
		tempFile = ""
	}
	return errors.Join(errs...)
}

// Main runs m and then Shutdown, reporting a cleanup failure as a failure.
func Main(m *testing.M) int {
	code := m.Run()
	if err := Shutdown(); err != nil {
		fmt.Fprintf(os.Stderr, "benchmark cluster cleanup: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}
