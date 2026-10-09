//go:build bazelrunfiles

package conformance_test

// The suite's FDB plumbing, kept free of the Go engine (no recordlayer or
// relational import): rfc257_guardiann_java_test compiles only this, the
// suite and the Java invoker, so Bazel reruns those target-only probes when
// the Java server or the FDB client changes, not on every engine change.

import (
	"context"
	"fmt"

	gofdb "fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
)

func openGoDatabase(ctx context.Context, container *foundationdbtc.Container) (gofdb.Database, error) {
	path, err := container.ClusterFilePath(ctx)
	if err != nil {
		return gofdb.Database{}, err
	}
	gofdb.MustAPIVersion(730)
	return gofdb.OpenDatabase(path)
}

func createGoTenant(ctx context.Context, container *foundationdbtc.Container, db gofdb.Database, name string) (gofdb.Tenant, error) {
	// Create tenant via native system key CRUD (no fdbcli).
	if err := db.CreateTenant(gofdb.Key(name)); err != nil {
		return gofdb.Tenant{}, fmt.Errorf("create tenant %q: %w", name, err)
	}

	// Open tenant — this reads the tenant ID from system keys.
	tenant, err := db.OpenTenant(gofdb.Key(name))
	if err != nil {
		return gofdb.Tenant{}, fmt.Errorf("open tenant %q: %w", name, err)
	}
	fmt.Printf("[TENANT] %s → id=%d\n", name, tenant.ID())

	// Smoke test: write + read through the tenant. Use Set+Get (point ops)
	// to verify tenant mapping works. GetRange hangs — known issue under investigation.
	_, err = tenant.Transact(func(tr gofdb.WritableTransaction) (any, error) {
		tr.Set(gofdb.Key("_init"), []byte("1"))
		v := tr.Get(gofdb.Key("_init")).MustGet()
		if string(v) != "1" {
			return nil, fmt.Errorf("smoke test: got %q, want %q", v, "1")
		}
		return nil, nil
	})
	if err != nil {
		return gofdb.Tenant{}, fmt.Errorf("tenant %q smoke test: %w", name, err)
	}

	return tenant, nil
}

// BytesToIntArray converts a byte slice to an int array for JSON serialization.
// Go's json.Marshal encodes []byte as base64, but Gson expects [1,2,3,...]
func BytesToIntArray(b []byte) []int {
	ints := make([]int, len(b))
	for i, v := range b {
		ints[i] = int(v)
	}
	return ints
}

// JavaTenant is a tenant on the shared container for a spec that drives only
// the Java server: TenantEnvironment without the Go record layer.
type JavaTenant struct {
	DB          gofdb.Database
	Keyspace    subspace.Subspace
	ClusterFile string
	TenantName  string
}

// SetupJavaTenant creates a tenant on the shared container for a Java-only spec.
func SetupJavaTenant(ctx context.Context, container *foundationdbtc.Container, tenantName string) (*JavaTenant, error) {
	if _, err := createGoTenant(ctx, container, sharedDB, tenantName); err != nil {
		return nil, fmt.Errorf("failed to create tenant: %w", err)
	}
	clusterFile, err := container.ClusterFile(ctx)
	if err != nil {
		deleteTenant(sharedDB, tenantName)
		return nil, fmt.Errorf("failed to get cluster file: %w", err)
	}
	return &JavaTenant{DB: sharedDB, Keyspace: subspace.Sub(tuple.Tuple{}), ClusterFile: clusterFile, TenantName: tenantName}, nil
}

// Cleanup deletes the tenant (not the container).
func (env *JavaTenant) Cleanup(_ context.Context) error {
	deleteTenant(env.DB, env.TenantName)
	return nil
}

// deleteTenant clears a tenant (delete refuses a non-empty one) and deletes
// it, best effort.
func deleteTenant(db gofdb.Database, name string) {
	if name == "" {
		return
	}
	if tenant, err := db.OpenTenant(gofdb.Key(name)); err == nil {
		_, _ = tenant.Transact(func(tr gofdb.WritableTransaction) (any, error) {
			tr.ClearRange(gofdb.KeyRange{Begin: gofdb.Key(""), End: gofdb.Key("\xff")})
			return nil, nil
		})
	}
	_ = db.DeleteTenant(gofdb.Key(name))
}
