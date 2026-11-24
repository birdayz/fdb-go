package keyspace_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/keyspace"
	"fdb.dev/pkg/simfdb"
)

func TestSchemaSubspace_EmptyPath(t *testing.T) {
	t.Parallel()
	ks := keyspace.New(subspace.Sub())
	if _, err := ks.SchemaSubspace(nil, "", "s1"); err == nil {
		t.Fatal("expected error for empty dbPath, got nil")
	}
}

func TestSchemaSubspace_EmptySchema(t *testing.T) {
	t.Parallel()
	ks := keyspace.New(subspace.Sub())
	if _, err := ks.SchemaSubspace(nil, "/FRL/db", ""); err == nil {
		t.Fatal("expected error for empty schemaName, got nil")
	}
}

// The catalog store is Java's __SYS (NULL) / __SYS (NULL) / CATALOG (LONG 0):
// the tuple (null, null, 0).
func TestCatalogSubspace_IsJavasSystemPath(t *testing.T) {
	t.Parallel()
	if got := keyspace.New(subspace.Sub()).CatalogSubspace().Bytes(); string(got) != "\x00\x00\x14" {
		t.Fatalf("catalog subspace %x, want 000014", got)
	}
}

func TestParseDBPath_Valid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		input string
		want  []string
	}{
		{"/db", []string{"db"}},
		{"/domain/db", []string{"domain", "db"}},
	}
	for _, tc := range cases {
		parts, err := keyspace.ParseDBPath(tc.input)
		if err != nil {
			t.Errorf("ParseDBPath(%q): unexpected error %v", tc.input, err)
			continue
		}
		if len(parts) != len(tc.want) {
			t.Errorf("ParseDBPath(%q): got %v, want %v", tc.input, parts, tc.want)
		}
	}
}

func TestParseDBPath_Invalid(t *testing.T) {
	t.Parallel()
	cases := []string{"", "noslash", "/", "//double", "/a//b"}
	for _, c := range cases {
		if _, err := keyspace.ParseDBPath(c); err == nil {
			t.Errorf("ParseDBPath(%q): expected error, got nil", c)
		}
	}
}

func TestRelationalKeyspace_InstanceDomainsIsolation(t *testing.T) {
	t.Parallel()
	keyspace.RegisterDomainIfNotExists("KS_ISO_GLOBAL")

	ks1 := keyspace.NewRelationalKeyspace(subspace.Sub(), keyspace.WithDomains("KS_DOM_A"))
	ks2 := keyspace.NewRelationalKeyspace(subspace.Sub(), keyspace.WithDomains("KS_DOM_B"))
	ksEmpty := keyspace.NewRelationalKeyspace(subspace.Sub())

	got, err := ks1.ToDatabasePath("/KS_DOM_A/db1")
	if err != nil || got.Domain != "KS_DOM_A" || got.Database != "db1" {
		t.Fatalf("ks1.ToDatabasePath(/KS_DOM_A/db1) = %+v, %v", got, err)
	}
	for _, rejected := range []string{"/KS_DOM_B/db1", "/KS_ISO_GLOBAL/db1"} {
		var ae *api.Error
		if _, err := ks1.ToDatabasePath(rejected); !errors.As(err, &ae) || ae.Code != api.ErrCodeInvalidPath {
			t.Errorf("ks1.ToDatabasePath(%q) = %v, want ErrCodeInvalidPath", rejected, err)
		}
	}

	got2, err := ks2.ToDatabasePath("/KS_DOM_B/db2")
	if err != nil || got2.Domain != "KS_DOM_B" || got2.Database != "db2" {
		t.Fatalf("ks2.ToDatabasePath(/KS_DOM_B/db2) = %+v, %v", got2, err)
	}
	var ae *api.Error
	if _, err := ks2.ToDatabasePath("/KS_DOM_A/db1"); !errors.As(err, &ae) || ae.Code != api.ErrCodeInvalidPath {
		t.Errorf("ks2.ToDatabasePath(/KS_DOM_A/db1) = %v, want ErrCodeInvalidPath", err)
	}

	// Empty instance keyspace accepts /__SYS but rejects user domains until registered.
	if sysPath, err := ksEmpty.ToDatabasePath("/__SYS"); err != nil || !sysPath.System {
		t.Fatalf("ksEmpty.ToDatabasePath(/__SYS) = %+v, %v", sysPath, err)
	}
	if _, err := ksEmpty.ToDatabasePath("/KS_DOM_A/db1"); !errors.As(err, &ae) || ae.Code != api.ErrCodeInvalidPath {
		t.Errorf("ksEmpty.ToDatabasePath(/KS_DOM_A/db1) = %v, want ErrCodeInvalidPath", err)
	}

	ksEmpty.RegisterDomain("KS_DOM_C")
	ksEmpty.RegisterDomain("KS_DOM_C") // idempotent
	ksEmpty.RegisterDomain("")         // ignored
	if gotC, err := ksEmpty.ToDatabasePath("/KS_DOM_C/db3"); err != nil || gotC.Domain != "KS_DOM_C" {
		t.Fatalf("ksEmpty.ToDatabasePath(/KS_DOM_C/db3) = %+v, %v", gotC, err)
	}
	if gotDoms := ksEmpty.RegisteredDomains(); !slices.Equal(gotDoms, []string{"KS_DOM_C"}) {
		t.Fatalf("ksEmpty.RegisteredDomains() = %v, want [KS_DOM_C]", gotDoms)
	}
	// Registering on ksEmpty must not leak to ks1, ks2, or global registry.
	if _, err := ks1.ToDatabasePath("/KS_DOM_C/db3"); !errors.As(err, &ae) || ae.Code != api.ErrCodeInvalidPath {
		t.Errorf("ks1 saw KS_DOM_C: %v", err)
	}
	if _, err := keyspace.ToDatabasePath("/KS_DOM_C/db3"); !errors.As(err, &ae) || ae.Code != api.ErrCodeInvalidPath {
		t.Errorf("global ToDatabasePath saw KS_DOM_C: %v", err)
	}
	// Mutating RegisteredDomains slice must not affect keyspace state.
	doms := ks1.RegisteredDomains()
	doms[0] = "MUTATED"
	if !slices.Equal(ks1.RegisteredDomains(), []string{"KS_DOM_A"}) {
		t.Fatalf("RegisteredDomains aliased internal slice: %v", ks1.RegisteredDomains())
	}
}

func TestRelationalKeyspace_BackwardCompatGlobal(t *testing.T) {
	t.Parallel()
	keyspace.RegisterDomainIfNotExists("KS_COMPAT_GLOBAL")
	ks := keyspace.New(subspace.Sub())
	got, err := ks.ToDatabasePath("/KS_COMPAT_GLOBAL/db")
	if err != nil || got.Domain != "KS_COMPAT_GLOBAL" {
		t.Fatalf("keyspace.New().ToDatabasePath(/KS_COMPAT_GLOBAL/db) = %+v, %v", got, err)
	}

	// RegisterDomain on a New() keyspace switches that instance to instance-scoped.
	ks.RegisterDomain("KS_COMPAT_LOCAL")
	if gotLocal, err := ks.ToDatabasePath("/KS_COMPAT_LOCAL/db"); err != nil || gotLocal.Domain != "KS_COMPAT_LOCAL" {
		t.Fatalf("ks.ToDatabasePath(/KS_COMPAT_LOCAL/db) = %+v, %v", gotLocal, err)
	}
	var ae *api.Error
	if _, err := ks.ToDatabasePath("/KS_COMPAT_GLOBAL/db"); !errors.As(err, &ae) || ae.Code != api.ErrCodeInvalidPath {
		t.Errorf("instance-scoped keyspace still saw global domain: %v", err)
	}
}

func TestRelationalKeyspace_SchemaSubspaceIsolation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := dst.NewSim(91)
	env.Buggify = dst.DisabledBuggifier()
	db := recordlayer.NewFDBDatabaseWithBackend(simfdb.New(env)).SetEnv(env)

	ks1 := keyspace.NewRelationalKeyspace(subspace.Sub(), keyspace.WithDomains("SUB_A"))
	ks2 := keyspace.NewRelationalKeyspace(subspace.Sub(), keyspace.WithDomains("SUB_B"))

	ss, err := ks1.SchemaSubspaceIn(ctx, db, "/SUB_A/db", "S")
	if err != nil {
		t.Fatalf("ks1.SchemaSubspaceIn: %v", err)
	}
	got, err := ks1.LookupSchemaSubspace(ctx, db, "/SUB_A/db", "S")
	if err != nil || !bytes.Equal(got.Bytes(), ss.Bytes()) {
		t.Fatalf("ks1.LookupSchemaSubspace = %v, %v; want %x", got, err, ss.Bytes())
	}

	var ae *api.Error
	if _, err := ks2.SchemaSubspaceIn(ctx, db, "/SUB_A/db", "S"); !errors.As(err, &ae) || ae.Code != api.ErrCodeInvalidPath {
		t.Errorf("ks2.SchemaSubspaceIn(/SUB_A/db) = %v, want ErrCodeInvalidPath", err)
	}
	if _, err := ks2.LookupSchemaSubspace(ctx, db, "/SUB_A/db", "S"); !errors.As(err, &ae) || ae.Code != api.ErrCodeInvalidPath {
		t.Errorf("ks2.LookupSchemaSubspace(/SUB_A/db) = %v, want ErrCodeInvalidPath", err)
	}
}

func TestRelationalKeyspace_ConcurrentRegisterAndResolve(t *testing.T) {
	t.Parallel()
	ks := keyspace.NewRelationalKeyspace(subspace.Sub(), keyspace.WithDomains("CONC_0"))
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			dom := "CONC_" + string(rune('A'+id))
			ks.RegisterDomain(dom)
			_ = ks.RegisteredDomains()
			if _, err := ks.ToDatabasePath("/" + dom + "/db"); err != nil {
				t.Errorf("ToDatabasePath(%s): %v", dom, err)
			}
		}(i)
	}
	wg.Wait()
}
