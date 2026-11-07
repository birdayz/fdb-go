package embedded

// Regression tests for the connection's schema witnesses (connection.go
// cachedLoadSchema): an auto-commit load keeps the catalog bytes it was built
// from, ResetSession keeps them, and the next statement reuses the loaded
// schema only while the catalog still holds those bytes. Harness:
// statement_read_test.go.

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/session"
)

var srSchemaKey = session.SchemaCacheKey(srDBPath, srSchema)

// TestSchemaWitness_ReusedAcrossResetSession: ResetSession drops the session
// schema cache and keeps the witness, and the next statement over an
// unchanged catalog reuses the witnessed schema instance without opening the
// catalog store. The control drops the witness: the statement then reloads,
// which opens the catalog store and builds a new instance, so both
// observations do tell a reuse from a reload.
func TestSchemaWitness_ReusedAcrossResetSession(t *testing.T) {
	t.Parallel()
	h := newSRHarness(t, 307)
	h.bootstrap(t)
	c, p := h.open(t)
	srInsertRows(t, c, 3)
	// The connection's first QueryContext bootstraps the catalog (ensureCatalogInit), opening catalog stores; warm it outside the count.
	srMustQuery(t, c, "SELECT PK FROM T WHERE PK = 1")
	const q = "SELECT PK, V FROM T WHERE PK = 3"
	w, ok := c.schemaWitnesses[srSchemaKey]
	if !ok || w.witness == nil || w.schema == nil {
		t.Fatal("the auto-commit schema load kept no witness")
	}

	srResetSession(t, c)
	if _, cached := c.sess.SchemaCache[srSchemaKey]; cached {
		t.Fatal("ResetSession kept the session schema cache: the case would not reach the witness")
	}
	if got, ok := c.schemaWitnesses[srSchemaKey]; !ok || got.witness != w.witness {
		t.Fatal("ResetSession dropped the schema witness")
	}
	p.reset()
	if got := srMustQuery(t, c, q); got != "[[3 30]]" {
		t.Fatalf("rows %s, want [[3 30]]", got)
	}
	if s := c.sess.SchemaCache[srSchemaKey]; s != w.schema {
		t.Fatal("the statement over an unchanged catalog loaded a new schema instead of reusing the witnessed one")
	}
	if n := p.timer.GetCount(recordlayer.EventOpenStore); n != 1 {
		t.Fatalf("the statement opened %d stores, want 1 (T's): the witness check opens no catalog store", n)
	}

	c.invalidateSchemaCache(srDBPath, srSchema)
	if _, ok := c.schemaWitnesses[srSchemaKey]; ok {
		t.Fatal("invalidateSchemaCache kept the schema witness")
	}
	p.reset()
	if got := srMustQuery(t, c, q); got != "[[3 30]]" {
		t.Fatalf("rows after the reload %s, want [[3 30]]", got)
	}
	if n := p.timer.GetCount(recordlayer.EventOpenStore); n < 2 {
		t.Fatalf("a full load opened %d stores: the count cannot tell a reload from a reuse", n)
	}
	if s := c.sess.SchemaCache[srSchemaKey]; s == w.schema {
		t.Fatal("a full load returned the witnessed instance: identity cannot tell a reload from a reuse")
	}
	if nw, ok := c.schemaWitnesses[srSchemaKey]; !ok || nw.witness == w.witness {
		t.Fatal("the full load kept no fresh witness")
	}
}

// TestSchemaWitness_SeesAnotherConnectionsChanges: a change another
// connection commits between two statements of a pooled connection is the
// next statement's schema. Before each change the schema has no column W, so
// a stale schema cannot answer the probe query.
func TestSchemaWitness_SeesAnotherConnectionsChanges(t *testing.T) {
	t.Parallel()
	const q = "SELECT PK, V FROM T WHERE PK = 3"
	const probe = "SELECT PK, W FROM T WHERE PK = 3"
	setup := func(t *testing.T, seed uint64) (*srHarness, *EmbeddedConnection) {
		h := newSRHarness(t, seed)
		h.bootstrap(t)
		c, _ := h.open(t)
		srInsertRows(t, c, 5)
		if got := srMustQuery(t, c, q); got != "[[3 30]]" {
			t.Fatalf("rows %s, want [[3 30]]", got)
		}
		if _, err := srQuery(c, probe); err == nil {
			t.Fatal("SRTMPL@1 answered a query of column W: the probe cannot tell the schemas apart")
		}
		if _, ok := c.schemaWitnesses[srSchemaKey]; !ok {
			t.Fatal("no witness before the change")
		}
		return h, c
	}
	// wantEvolved asserts the next statement planned over the changed schema.
	wantEvolved := func(t *testing.T, c *EmbeddedConnection, before witnessedSchema, version int) {
		t.Helper()
		if got := srMustQuery(t, c, probe); got != "[[3 <nil>]]" {
			t.Fatalf("rows %s, want [[3 <nil>]]: the statement did not see the change", got)
		}
		s := c.sess.SchemaCache[srSchemaKey]
		if s == before.schema {
			t.Fatal("the statement reused the witnessed schema over a changed catalog")
		}
		if v := s.SchemaTemplate().Version(); v != version {
			t.Fatalf("loaded SRTMPL@%d, want @%d", v, version)
		}
		if md := c.cachedMetaData(); md == nil || md.GetIndex("T_BY_W") == nil {
			t.Fatal("the loaded metadata has no index T_BY_W")
		}
		if w, ok := c.schemaWitnesses[srSchemaKey]; !ok || w.witness == before.witness || w.schema != s {
			t.Fatal("the reload did not replace the witness with its own")
		}
		if got := srMustQuery(t, c, q); got != "[[3 30]]" {
			t.Fatalf("rows %s after the change, want [[3 30]]", got)
		}
	}

	t.Run("schema re-pointed to a new template version", func(t *testing.T) {
		t.Parallel()
		h, c := setup(t, 311)
		before := c.schemaWitnesses[srSchemaKey]
		h.evolve(t)
		srResetSession(t, c)
		wantEvolved(t, c, before, 2)
	})

	t.Run("template row rewritten in place", func(t *testing.T) {
		t.Parallel()
		h, c := setup(t, 313)
		before := c.schemaWitnesses[srSchemaKey]
		// Only SRTMPL@1's row changes: the schema row still names version 1
		// and the catalog header is untouched.
		h.rewriteTemplateRow(t, 1, srTemplate(t, 1, true))
		srResetSession(t, c)
		wantEvolved(t, c, before, 1)
	})

	t.Run("schema dropped", func(t *testing.T) {
		t.Parallel()
		h, c := setup(t, 317)
		admin, _ := h.open(t)
		if _, err := admin.ExecContext(context.Background(), "DROP SCHEMA /FRL/SIMDB/S", nil); err != nil {
			t.Fatal(err)
		}
		srResetSession(t, c)
		_, err := srQuery(c, q)
		var ae *api.Error
		if !errors.As(err, &ae) || ae.Code != api.ErrCodeUndefinedSchema {
			t.Fatalf("a statement on the dropped schema = %v, want %s", err, api.ErrCodeUndefinedSchema)
		}
		// As before witnesses: a connection that never loaded the schema
		// fails the same way.
		fresh, _ := h.open(t)
		_, freshErr := srQuery(fresh, q)
		if freshErr == nil || freshErr.Error() != err.Error() {
			t.Fatalf("the pooled connection's error %q, a fresh connection's %v", err, freshErr)
		}
		if _, ok := c.schemaWitnesses[srSchemaKey]; ok {
			t.Fatal("the witness of the dropped schema was kept")
		}
		if n := h.be.OpenTransactions(); n != 0 {
			t.Fatalf("%d transactions left open by the failed statements", n)
		}
	})
}

// TestSchemaWitness_DroppedWithTheConnectionsOwnDDL: the connection's own
// DDL drops what it may have changed (CREATE SCHEMA TEMPLATE every witness,
// DROP SCHEMA the schema's), and an explicit transaction's schema load
// records no witness.
func TestSchemaWitness_DroppedWithTheConnectionsOwnDDL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("CREATE SCHEMA TEMPLATE", func(t *testing.T) {
		t.Parallel()
		h := newSRHarness(t, 331)
		h.bootstrap(t)
		c, _ := h.open(t)
		srInsertRows(t, c, 1)
		if len(c.schemaWitnesses) == 0 {
			t.Fatal("no witness before the DDL")
		}
		if _, err := c.ExecContext(ctx, "CREATE SCHEMA TEMPLATE sr_other CREATE TABLE x (id BIGINT, PRIMARY KEY (id))", nil); err != nil {
			t.Fatal(err)
		}
		if len(c.schemaWitnesses) != 0 {
			t.Fatalf("CREATE SCHEMA TEMPLATE kept %d witnesses", len(c.schemaWitnesses))
		}
	})

	t.Run("DROP SCHEMA", func(t *testing.T) {
		t.Parallel()
		h := newSRHarness(t, 337)
		h.bootstrap(t)
		c, _ := h.open(t)
		srInsertRows(t, c, 1)
		if _, err := c.ExecContext(ctx, "DROP SCHEMA /FRL/SIMDB/S", nil); err != nil {
			t.Fatal(err)
		}
		if _, ok := c.schemaWitnesses[srSchemaKey]; ok {
			t.Fatal("DROP SCHEMA kept the dropped schema's witness")
		}
	})

	t.Run("an explicit transaction records none", func(t *testing.T) {
		t.Parallel()
		h := newSRHarness(t, 347)
		h.bootstrap(t)
		c, _ := h.open(t)
		tx, err := c.BeginTx(ctx, driver.TxOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if got := srMustQuery(t, c, "SELECT PK FROM T"); got != "[]" {
			t.Fatalf("rows %s, want none", got)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		if _, ok := c.schemaWitnesses[srSchemaKey]; ok {
			t.Fatal("a schema loaded inside an explicit transaction left a witness")
		}
	})
}
