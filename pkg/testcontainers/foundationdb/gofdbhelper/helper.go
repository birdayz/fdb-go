// Package gofdbhelper provides a pure Go FDB database from a testcontainer.
// Separated from the main foundationdb testcontainer package to avoid
// import cycles (testcontainer ← fdbgo/fdb ← fdbgo/client ← testcontainer).
package gofdbhelper

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	gofdb "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	tc "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
)

// OpenDatabase returns a pure Go FDB database from a testcontainer.
// Uses the native Go wire protocol — no CGo/libfdb_c required.
func OpenDatabase(ctx context.Context, c *tc.Container) (gofdb.Database, error) {
	clusterContent, err := c.ClusterFile(ctx)
	if err != nil {
		return gofdb.Database{}, fmt.Errorf("get cluster file: %w", err)
	}

	_, internalReader, err := c.Exec(ctx, []string{"cat", "/var/fdb/fdb.cluster"})
	if err != nil {
		return gofdb.Database{}, fmt.Errorf("read internal cluster file: %w", err)
	}
	internalBytes, _ := io.ReadAll(internalReader)
	internalStr := strings.TrimSpace(string(internalBytes))

	// Hybrid cluster: internal desc:id + external coordinator address.
	atIdx := strings.Index(internalStr, "@")
	if atIdx < 0 {
		return gofdb.Database{}, fmt.Errorf("invalid internal cluster file: %q", internalStr)
	}
	prefix := internalStr[:atIdx+1]

	externalAtIdx := strings.Index(clusterContent, "@")
	if externalAtIdx < 0 {
		return gofdb.Database{}, fmt.Errorf("invalid external cluster file: %q", clusterContent)
	}
	coords := clusterContent[externalAtIdx+1:]

	tmpFile, err := os.CreateTemp("", "fdb_gocluster_*.txt")
	if err != nil {
		return gofdb.Database{}, fmt.Errorf("create temp file: %w", err)
	}
	if _, err := io.WriteString(tmpFile, prefix+coords); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpFile.Name())
		return gofdb.Database{}, fmt.Errorf("write cluster file: %w", err)
	}
	_ = tmpFile.Close()

	gofdb.MustAPIVersion(c.APIVersion())
	db, err := gofdb.OpenDatabase(tmpFile.Name())
	if err != nil {
		_ = os.Remove(tmpFile.Name())
		return gofdb.Database{}, fmt.Errorf("open pure Go database: %w", err)
	}
	return db, nil
}

// CreateTenant creates and returns a pure Go FDB tenant from a testcontainer.
func CreateTenant(ctx context.Context, c *tc.Container, db gofdb.Database, name string) (gofdb.Tenant, error) {
	// Create tenant via fdbcli (works regardless of client type)
	exitCode, output, err := c.Exec(ctx, []string{
		"/usr/bin/fdbcli", "--exec", fmt.Sprintf("createtenant %s", name),
	})
	if err != nil {
		return gofdb.Tenant{}, fmt.Errorf("fdbcli createtenant: %w", err)
	}
	outputBytes, _ := io.ReadAll(output)
	if exitCode != 0 {
		return gofdb.Tenant{}, fmt.Errorf("fdbcli createtenant exit %d: %s", exitCode, outputBytes)
	}

	tenant, err := db.OpenTenant(gofdb.Key(name))
	if err != nil {
		return gofdb.Tenant{}, fmt.Errorf("open tenant %q: %w", name, err)
	}
	return tenant, nil
}

// DeleteTenant deletes a tenant via the database client.
func DeleteTenant(db gofdb.Database, name string) error {
	return db.DeleteTenant(gofdb.Key(name))
}
