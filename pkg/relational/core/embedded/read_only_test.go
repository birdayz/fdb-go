package embedded

import (
	"bytes"
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"slices"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	apiddl "fdb.dev/pkg/relational/api/ddl"
	"fdb.dev/pkg/relational/core/keyspace"
	"fdb.dev/pkg/relational/core/rowstruct"
)

// readOnlyIDs returns the ids of t, read on a writable connection of g.
func readOnlyIDs(t *testing.T, g *gatedSim) []int64 {
	t.Helper()
	rows, err := g.connect(t, "S").QueryContext(context.Background(), "SELECT id FROM t ORDER BY id", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []int64
	dest := make([]driver.Value, len(rows.Columns()))
	for {
		if err := rows.Next(dest); errors.Is(err, io.EOF) {
			return ids
		} else if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, dest[0].(int64))
	}
}

// The Go APIs that write refuse on a read-only connection before doing
// anything, while a direct-access read still runs.
func TestReadOnly_WritingGoAPIsRefuse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	g := newGatedSim(t, 531)
	mustExec(t, g.connect(t, "S"), "INSERT INTO t (id, v) VALUES (1, 10)")
	c := g.connect(t, "S")
	c.SetReadOnly()

	pk, err := api.NewKeySet().SetKeyColumn("ID", int64(1))
	if err != nil {
		t.Fatal(err)
	}
	row := rowstruct.NewStructBuilder().AddLong("ID", 2).AddLong("V", 20).Build()
	opRan := false
	for _, op := range []struct {
		name, what string
		call       func() error
	}{
		{"ExecuteInsert", "a direct-access insert", func() error {
			_, err := c.DirectAccess().ExecuteInsert(ctx, "T", []api.Struct{row}, nil)
			return err
		}},
		{"ExecuteDelete", "a direct-access delete", func() error {
			_, err := c.DirectAccess().ExecuteDelete(ctx, "T", pk, nil)
			return err
		}},
		{"ExecuteDeleteRange", "a direct-access delete", func() error {
			_, err := c.DirectAccess().ExecuteDeleteRange(ctx, "T", api.EmptyKeySet(), nil)
			return err
		}},
		{"CollectStatistics", "collecting statistics", func() error {
			_, err := c.CollectStatistics(ctx, recordlayer.CollectOptions{})
			return err
		}},
		{"ClearStatistics", "clearing statistics", func() error { return c.ClearStatistics(ctx) }},
		{"ApplyMetadataOperation", "a metadata operation", func() error {
			return c.ApplyMetadataOperation(ctx, func(apiddl.MetadataOperationsFactory, api.Transaction) error {
				opRan = true
				return nil
			})
		}},
	} {
		want := "read-only connection: " + op.what + " is not allowed"
		var ae *api.Error
		if err := op.call(); !errors.As(err, &ae) || ae.Code != api.ErrCodeUnsupportedOperation || ae.Message != want {
			t.Errorf("%s on a read-only connection = %v; want 0A000 %q", op.name, err, want)
		}
	}
	if opRan {
		t.Error("ApplyMetadataOperation ran its operation on a read-only connection")
	}

	rs, err := c.DirectAccess().ExecuteGet(ctx, "T", pk, nil)
	if err != nil {
		t.Fatalf("ExecuteGet on a read-only connection: %v", err)
	}
	if !rs.Next() {
		t.Fatalf("ExecuteGet on a read-only connection found no row 1: %v", rs.Err())
	}
	rs.Close()
	if got := readOnlyIDs(t, g); !slices.Equal(got, []int64{1}) {
		t.Errorf("t after the refusals = %v; want [1]", got)
	}
}

// A read-only connection stays read-only across ResetSession, the reset a
// pooled connection gets between borrowers.
func TestReadOnly_OutlivesResetSession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	g := newGatedSim(t, 532)
	c := g.connect(t, "S")
	c.SetReadOnly()
	if err := c.ResetSession(ctx); err != nil {
		t.Fatal(err)
	}
	var ae *api.Error
	if _, err := c.ExecContext(ctx, "INSERT INTO t (id, v) VALUES (1, 10)", nil); !errors.As(err, &ae) ||
		ae.Code != api.ErrCodeUnsupportedOperation {
		t.Fatalf("INSERT after ResetSession = %v; want the read-only 0A000", err)
	}
	if got := readOnlyIDs(t, g); len(got) != 0 {
		t.Errorf("t after the refused INSERT = %v; want no rows", got)
	}
}

// A read-only connection looks its schema's store up and never interns a
// name, which a writable one does in a transaction of its own, committed
// whatever the statement's transaction does.
func TestReadOnly_SchemaSubspaceInternsNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	g := newGatedSim(t, 533)

	ro := g.connect(t, "NOPE_RO")
	ro.SetReadOnly()
	var ae *api.Error
	if _, err := ro.sessionSchemaSubspace(ctx); !errors.As(err, &ae) || ae.Code != api.ErrCodeUndefinedSchema {
		t.Errorf("read-only resolution of an unknown schema = %v; want 42F51 (undefined schema)", err)
	}
	if _, err := g.connect(t, "NOPE_RW").sessionSchemaSubspace(ctx); err != nil {
		t.Fatalf("writable resolution of an unknown schema: %v", err)
	}

	// What persisted, through a database and keyspace with no cached names.
	fresh := recordlayer.NewFDBDatabaseWithBackend(g.gate).SetEnv(g.env)
	ks := keyspace.New(subspace.Sub())
	runs := 0
	counted := countingRunner{r: ReadOnlyRunner(fresh), runs: &runs}
	ss, err := ks.LookupSchemaSubspace(ctx, counted, "/FRL/SIMDB", "NOPE_RW")
	if err != nil {
		t.Fatalf("the writable resolution interned nothing (%v), so the read-only half proves nothing", err)
	}
	ss2, err := ks.LookupSchemaSubspace(ctx, counted, "/FRL/SIMDB", "NOPE_RW")
	if !bytes.Equal(ss2.Bytes(), ss.Bytes()) || err != nil || runs != 1 {
		t.Fatalf("second lookup = (%v, %v), runs = %d; want cached subspace after 1 run", ss2, err, runs)
	}
	if cached, ok := ks.ResolvedSchemaSubspace("/FRL/SIMDB", "NOPE_RW"); !ok || !bytes.Equal(cached.Bytes(), ss.Bytes()) {
		t.Fatalf("ResolvedSchemaSubspace after lookup = (%v, %v); want (%v, true)", cached, ok, ss)
	}
	if _, err := ks.LookupSchemaSubspace(ctx, fresh, "/FRL/SIMDB", "NOPE_RO"); !errors.As(err, &ae) ||
		ae.Code != api.ErrCodeUndefinedSchema {
		t.Errorf("lookup of the name the read-only connection resolved = %v; want UNDEFINED_SCHEMA (not interned)", err)
	}
}

type countingRunner struct {
	r    keyspace.Runner
	runs *int
}

func (c countingRunner) Run(ctx context.Context, fn func(*recordlayer.FDBRecordContext) (any, error)) (any, error) {
	*c.runs++
	return c.r.Run(ctx, fn)
}
