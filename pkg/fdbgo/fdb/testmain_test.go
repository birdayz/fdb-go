package fdb_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/client"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	tcfdb "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
)

// Shared FDB container for all tests in this package.
var (
	sharedContainer   *tcfdb.Container
	sharedClusterFile *client.ClusterFile
)

func TestMain(m *testing.M) {
	fdb.MustAPIVersion(730)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	container, err := tcfdb.Run(ctx, "", tcfdb.WithStorageEngine("ssd"), tcfdb.WithDirectIP())
	if err != nil {
		fmt.Fprintf(os.Stderr, "start FDB container: %v\n", err)
		os.Exit(1)
	}

	connStr, err := container.ClusterFile(ctx)
	if err != nil {
		container.Terminate(ctx)
		fmt.Fprintf(os.Stderr, "get cluster file: %v\n", err)
		os.Exit(1)
	}

	cf, err := client.ParseClusterString(connStr)
	if err != nil {
		container.Terminate(ctx)
		fmt.Fprintf(os.Stderr, "parse cluster string: %v\n", err)
		os.Exit(1)
	}
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

	_, internalReader, err := container.Exec(ctx, []string{"cat", "/var/fdb/fdb.cluster"})
	if err != nil {
		container.Terminate(ctx)
		fmt.Fprintf(os.Stderr, "read internal cluster file: %v\n", err)
		os.Exit(1)
	}
	internalBytes, _ := io.ReadAll(internalReader)
	internalStr := string(internalBytes)
	if idx := strings.Index(internalStr, cf.Description); idx >= 0 {
		internalStr = internalStr[idx:]
	}
	internalCF, err := client.ParseClusterString(strings.TrimSpace(internalStr))
	if err != nil {
		container.Terminate(ctx)
		fmt.Fprintf(os.Stderr, "parse internal cluster: %v\n", err)
		os.Exit(1)
	}

	connectCF := &client.ClusterFile{
		Description:  internalCF.Description,
		ID:           internalCF.ID,
		Coordinators: cf.Coordinators,
	}

	sharedContainer = container
	sharedClusterFile = connectCF

	code := m.Run()

	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cleanupCancel()
	container.Terminate(cleanupCtx)

	os.Exit(code)
}
