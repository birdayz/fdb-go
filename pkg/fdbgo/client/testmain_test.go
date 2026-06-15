package client

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	tcfdb "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
)

// Shared FDB container for all tests in this package.
// Started once in TestMain, used by openTestDB to create per-test Database connections.
var (
	sharedContainer   *tcfdb.Container
	sharedClusterFile *ClusterFile
)

// startFDBContainer starts an FDB testcontainer, waits for health, and
// resolves the connectable ClusterFile (external coordinators + the
// container-internal cluster key). Used by TestMain for the shared container
// and by tests that need a DEDICATED instance (e.g. database-lock tests —
// a lock is database-global and would break every parallel test on the
// shared container). The caller owns termination.
func startFDBContainer(ctx context.Context) (*tcfdb.Container, *ClusterFile, error) {
	container, err := tcfdb.Run(ctx, "", tcfdb.WithStorageEngine("ssd"), tcfdb.WithDirectIP())
	if err != nil {
		return nil, nil, fmt.Errorf("start FDB container: %w", err)
	}

	connStr, err := container.ClusterFile(ctx)
	if err != nil {
		container.Terminate(ctx)
		return nil, nil, fmt.Errorf("get cluster file: %w", err)
	}

	cf, err := ParseClusterString(connStr)
	if err != nil {
		container.Terminate(ctx)
		return nil, nil, fmt.Errorf("parse cluster string: %w", err)
	}

	// Wait for health.
	for i := 0; i < 30; i++ {
		time.Sleep(1 * time.Second)
		code, reader, execErr := container.Exec(ctx, []string{"fdbcli", "--exec", "status minimal"})
		if execErr != nil || reader == nil {
			continue
		}
		if code == 0 {
			out, _ := io.ReadAll(reader)
			if strings.Contains(string(out), "Healthy") {
				break
			}
		}
	}

	// Read internal cluster file for correct cluster key.
	_, internalReader, err := container.Exec(ctx, []string{"cat", "/var/fdb/fdb.cluster"})
	if err != nil {
		container.Terminate(ctx)
		return nil, nil, fmt.Errorf("read internal cluster file: %w", err)
	}
	internalBytes, _ := io.ReadAll(internalReader)
	internalStr := string(internalBytes)
	if idx := strings.Index(internalStr, cf.Description); idx >= 0 {
		internalStr = internalStr[idx:]
	}
	internalCF, err := ParseClusterString(strings.TrimSpace(internalStr))
	if err != nil {
		container.Terminate(ctx)
		return nil, nil, fmt.Errorf("parse internal cluster: %w", err)
	}

	connectCF := &ClusterFile{
		Description:  internalCF.Description,
		ID:           internalCF.ID,
		Coordinators: cf.Coordinators,
	}

	return container, connectCF, nil
}

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	container, connectCF, err := startFDBContainer(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	sharedContainer = container
	sharedClusterFile = connectCF

	code := m.Run()

	// Cleanup: terminate the shared container.
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cleanupCancel()
	container.Terminate(cleanupCtx)

	os.Exit(code)
}
