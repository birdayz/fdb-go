package sqldriver

// A Connector's DSN is an immutable snapshot taken at OpenConnector.
//
// The hazard is a split brain rather than any single field. The connection
// options are decoded and frozen at OpenConnector (so a malformed value is a
// DSN error, not a connect-time failure), while Path, Schema, Mode and the
// Options map that carries cluster_file were read live from a *DSN that
// Connector.DSN() handed out by pointer. A caller could therefore flip
// restrict_ddl_to_session_database after OpenConnector and have Connect honour
// the STALE decode — a security restriction silently not matching what the DSN
// now says — while a newly-malformed value bypassed validation entirely, and
// meanwhile Path/cluster_file mutations WOULD take effect. Some reads live,
// some frozen, is the defect.
//
// These tests pin the freeze WHOLESALE: every field, not just the option that
// motivated it. No FDB or Docker required.

import (
	"testing"

	"fdb.dev/pkg/relational/api"
)

const freezeDSN = "fdbsql:///tenant-a" +
	"?cluster_file=/tmp/original.cluster" +
	"&schema=orig_schema" +
	"&restrict_ddl_to_session_database=true"

func openFreezeConnector(t *testing.T) *Connector {
	t.Helper()
	d := &Driver{}
	c, err := d.OpenConnector(freezeDSN)
	if err != nil {
		t.Fatalf("OpenConnector: %v", err)
	}
	return c.(*Connector)
}

// TestConnectorDSNIsDefensiveCopy proves DSN() does not expose the internal
// snapshot: mutating what it returns changes nothing, on any field.
func TestConnectorDSNIsDefensiveCopy(t *testing.T) {
	t.Parallel()
	conn := openFreezeConnector(t)

	handed := conn.DSN()
	if handed == conn.dsn {
		t.Fatal("DSN() returned the internal pointer; callers can mutate the Connector's snapshot")
	}

	// Mutate every exported field, including the map.
	handed.Path = "/tenant-b"
	handed.Schema = "attacker_schema"
	handed.Mode = ModeRemote
	handed.Host = "evil.example.com:1234"
	handed.Options["cluster_file"] = "/tmp/attacker.cluster"
	handed.Options[RestrictDDLToSessionDatabaseParam] = "false"
	handed.Options["injected"] = "x"

	after := conn.DSN()
	if after.Path != "/tenant-a" {
		t.Errorf("Path leaked: %q", after.Path)
	}
	if after.Schema != "orig_schema" {
		t.Errorf("Schema leaked: %q", after.Schema)
	}
	if after.Mode != ModeEmbedded {
		t.Errorf("Mode leaked: %v", after.Mode)
	}
	if after.Host != "" {
		t.Errorf("Host leaked: %q", after.Host)
	}
	if got := after.Options["cluster_file"]; got != "/tmp/original.cluster" {
		t.Errorf("cluster_file leaked: %q", got)
	}
	if got := after.Options[RestrictDDLToSessionDatabaseParam]; got != "true" {
		t.Errorf("restrict option leaked: %q", got)
	}
	if _, injected := after.Options["injected"]; injected {
		t.Error("injected key reached the Connector's snapshot")
	}
}

// TestConnectorFieldsReadFromFrozenSnapshot proves the fields Connect and
// initialize consult are the frozen ones. Reading c.dsn directly is the point:
// these are exactly the values Connect passes to embedded.New / SetDefaultSchema
// and initialize uses for the cluster file, and they must be unaffected by a
// mutation of anything the caller can reach.
func TestConnectorFieldsReadFromFrozenSnapshot(t *testing.T) {
	t.Parallel()
	conn := openFreezeConnector(t)

	handed := conn.DSN()
	handed.Path = "/tenant-b"
	handed.Schema = "attacker_schema"
	handed.Options["cluster_file"] = "/tmp/attacker.cluster"
	handed.Options[RestrictDDLToSessionDatabaseParam] = "false"

	if conn.dsn.Path != "/tenant-a" {
		t.Errorf("Connect would open database %q, want %q", conn.dsn.Path, "/tenant-a")
	}
	if conn.dsn.Schema != "orig_schema" {
		t.Errorf("Connect would set schema %q, want %q", conn.dsn.Schema, "orig_schema")
	}
	if got := conn.dsn.Options["cluster_file"]; got != "/tmp/original.cluster" {
		t.Errorf("initialize would use cluster file %q, want %q", got, "/tmp/original.cluster")
	}

	// The decoded option is what Connect installs on the connection. It must
	// still be the OpenConnector-time value — the restriction stays ON.
	if v := conn.connOpts.Get(api.OptRestrictDDLToSessionDatabase); v != true {
		t.Errorf("connOpts restriction = %v, want true — a post-OpenConnector "+
			"mutation must not disable the restriction", v)
	}

	// And the snapshot the options were decoded FROM agrees with the decode, so
	// there is no field that says one thing while the decode says another.
	if got := conn.dsn.Options[RestrictDDLToSessionDatabaseParam]; got != "true" {
		t.Errorf("snapshot option string = %q but decode says true: split brain", got)
	}
}

// TestDSNCloneIsDeep pins Clone itself — the primitive the freeze rests on.
func TestDSNCloneIsDeep(t *testing.T) {
	t.Parallel()

	orig, err := ParseDSN(freezeDSN)
	if err != nil {
		t.Fatalf("ParseDSN: %v", err)
	}
	cp := orig.Clone()

	if &cp.Options == &orig.Options {
		t.Fatal("Clone shared the Options map header")
	}
	cp.Options["cluster_file"] = "/tmp/other.cluster"
	cp.Path = "/other"
	if orig.Options["cluster_file"] != "/tmp/original.cluster" {
		t.Errorf("Clone shared the Options map: %q", orig.Options["cluster_file"])
	}
	if orig.Path != "/tenant-a" {
		t.Errorf("Clone shared Path: %q", orig.Path)
	}

	// Mutating the ORIGINAL must not reach the clone either.
	orig.Options["cluster_file"] = "/tmp/mutated.cluster"
	if cp.Options["cluster_file"] != "/tmp/other.cluster" {
		t.Errorf("original mutation reached the clone: %q", cp.Options["cluster_file"])
	}

	if Clone := (*DSN)(nil).Clone(); Clone != nil {
		t.Errorf("nil.Clone() = %v, want nil", Clone)
	}
}
