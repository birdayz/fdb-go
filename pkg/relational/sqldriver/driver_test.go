package sqldriver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
)

func TestParseDSN_Embedded(t *testing.T) {
	t.Parallel()
	dsn, err := ParseDSN("fdbsql:///mydb")
	if err != nil {
		t.Fatalf("ParseDSN: %v", err)
	}
	if dsn.Mode != ModeEmbedded {
		t.Errorf("Mode = %v, want ModeEmbedded", dsn.Mode)
	}
	if dsn.Path != "/mydb" {
		t.Errorf("Path = %q, want %q", dsn.Path, "/mydb")
	}
	if dsn.Host != "" {
		t.Errorf("Host = %q, want empty", dsn.Host)
	}
	if len(dsn.Options) != 0 {
		t.Errorf("Options = %v, want empty", dsn.Options)
	}
}

func TestParseDSN_EmbeddedWithOptions(t *testing.T) {
	t.Parallel()
	dsn, err := ParseDSN("fdbsql:///mydb?cluster_file=/etc/fdb.cluster&foo=bar")
	if err != nil {
		t.Fatalf("ParseDSN: %v", err)
	}
	if dsn.Mode != ModeEmbedded {
		t.Errorf("Mode = %v, want ModeEmbedded", dsn.Mode)
	}
	if dsn.Options["cluster_file"] != "/etc/fdb.cluster" {
		t.Errorf("cluster_file option: %v", dsn.Options)
	}
	if dsn.Options["foo"] != "bar" {
		t.Errorf("foo option: %v", dsn.Options)
	}
}

func TestParseDSN_EngineOption(t *testing.T) {
	t.Parallel()
	dsn, err := ParseDSN("fdbsql:///mydb?schema=myschema&engine=cascades")
	if err != nil {
		t.Fatalf("ParseDSN: %v", err)
	}
	if dsn.Options["engine"] != "cascades" {
		t.Errorf("engine option = %q, want cascades", dsn.Options["engine"])
	}
	if dsn.Schema != "myschema" {
		t.Errorf("Schema = %q, want myschema", dsn.Schema)
	}
}

func TestParseDSN_Remote(t *testing.T) {
	t.Parallel()
	dsn, err := ParseDSN("fdbsql://sqlserver.example.com:50051/mydb")
	if err != nil {
		t.Fatalf("ParseDSN: %v", err)
	}
	if dsn.Mode != ModeRemote {
		t.Errorf("Mode = %v, want ModeRemote", dsn.Mode)
	}
	if dsn.Host != "sqlserver.example.com:50051" {
		t.Errorf("Host = %q", dsn.Host)
	}
	if dsn.Path != "/mydb" {
		t.Errorf("Path = %q", dsn.Path)
	}
}

func TestParseDSN_Errors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		dsn  string
	}{
		{"empty", ""},
		{"wrong scheme", "postgres:///mydb"},
		{"missing path embedded", "fdbsql:///"},
		{"missing path remote", "fdbsql://host:123"},
		{"no scheme", "///mydb"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseDSN(c.dsn)
			if err == nil {
				t.Fatalf("ParseDSN(%q) = nil error, want InvalidPath", c.dsn)
			}
			e := api.AsError(err)
			if e == nil {
				t.Fatalf("error is not *api.Error: %v", err)
			}
			if e.Code != api.ErrCodeInvalidPath {
				t.Errorf("code = %q, want %q", e.Code, api.ErrCodeInvalidPath)
			}
		})
	}
}

func TestParseDSN_URLEncodedOptions(t *testing.T) {
	t.Parallel()
	// cluster_file paths on Windows / with spaces need URL encoding.
	// Verify the parser unescapes correctly.
	dsn, err := ParseDSN("fdbsql:///mydb?cluster_file=%2Fetc%2Ffdb%2Ffdb.cluster&comment=hello%20world")
	if err != nil {
		t.Fatalf("ParseDSN: %v", err)
	}
	if dsn.Options["cluster_file"] != "/etc/fdb/fdb.cluster" {
		t.Errorf("cluster_file not decoded: %v", dsn.Options)
	}
	if dsn.Options["comment"] != "hello world" {
		t.Errorf("comment not decoded: %v", dsn.Options)
	}
}

func TestParseDSN_EmptyOptionValue(t *testing.T) {
	t.Parallel()
	// Query param with no value (e.g. ?debug&other=1) has empty string.
	dsn, err := ParseDSN("fdbsql:///mydb?debug")
	if err != nil {
		t.Fatalf("ParseDSN: %v", err)
	}
	if v, ok := dsn.Options["debug"]; !ok || v != "" {
		t.Errorf("debug option missing or wrong: (%v, %v)", v, ok)
	}
}

func TestParseDSN_OptionWithEquals(t *testing.T) {
	t.Parallel()
	// URL-encoded "=" in a value.
	dsn, err := ParseDSN("fdbsql:///mydb?x=a%3Db")
	if err != nil {
		t.Fatalf("ParseDSN: %v", err)
	}
	if dsn.Options["x"] != "a=b" {
		t.Errorf("value with = not preserved: %v", dsn.Options)
	}
}

func TestParseDSN_DuplicateOption(t *testing.T) {
	t.Parallel()
	// Duplicate keys — first value wins (matches Java's
	// JDBCURI.getFirstValue behavior).
	dsn, err := ParseDSN("fdbsql:///mydb?x=first&x=second")
	if err != nil {
		t.Fatalf("ParseDSN: %v", err)
	}
	if dsn.Options["x"] != "first" {
		t.Errorf("duplicate: first value should win, got %q", dsn.Options["x"])
	}
}

func TestDSN_StringRoundTrip(t *testing.T) {
	t.Parallel()
	cases := []string{
		"fdbsql:///mydb",
		"fdbsql:///mydb?a=1&b=2&c=3",
		"fdbsql://host:1234/mydb",
	}
	for _, in := range cases {
		in := in
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			dsn, err := ParseDSN(in)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			roundTrip, err := ParseDSN(dsn.String())
			if err != nil {
				t.Fatalf("reparse %q: %v", dsn.String(), err)
			}
			if roundTrip.Path != dsn.Path || roundTrip.Mode != dsn.Mode ||
				roundTrip.Host != dsn.Host || len(roundTrip.Options) != len(dsn.Options) {
				t.Errorf("round-trip mismatch: %+v vs %+v", dsn, roundTrip)
			}
		})
	}
}

func TestDSN_StringExact(t *testing.T) {
	t.Parallel()
	// Pin the exact output shape so the DSN form is part of the
	// public API contract — accidental reformats become test failures.
	cases := []struct {
		name string
		dsn  *DSN
		want string
	}{
		{
			name: "embedded no options",
			dsn:  &DSN{Mode: ModeEmbedded, Path: "/mydb"},
			want: "fdbsql:///mydb",
		},
		{
			name: "embedded with options (sorted)",
			dsn: &DSN{
				Mode: ModeEmbedded, Path: "/mydb",
				Options: map[string]string{"z": "1", "a": "2"},
			},
			want: "fdbsql:///mydb?a=2&z=1",
		},
		{
			name: "remote host:port",
			dsn:  &DSN{Mode: ModeRemote, Host: "h:1234", Path: "/mydb"},
			want: "fdbsql://h:1234/mydb",
		},
		{
			name: "remote with options",
			dsn: &DSN{
				Mode: ModeRemote, Host: "h:1234", Path: "/mydb",
				Options: map[string]string{"tls": "true"},
			},
			want: "fdbsql://h:1234/mydb?tls=true",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := c.dsn.String(); got != c.want {
				t.Errorf("String() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestDSN_StringDeterministicOrder(t *testing.T) {
	t.Parallel()
	// Options map iteration is randomized; String() must sort keys.
	dsn := &DSN{Mode: ModeEmbedded, Path: "/x", Options: map[string]string{
		"z": "1", "a": "2", "m": "3",
	}}
	s1 := dsn.String()
	for i := 0; i < 20; i++ {
		if got := dsn.String(); got != s1 {
			t.Fatalf("DSN.String() nondeterministic: %q vs %q", s1, got)
		}
	}
	// And the sort is actually alphabetical.
	want := "fdbsql:///x?a=2&m=3&z=1"
	if s1 != want {
		t.Errorf("String() = %q, want %q", s1, want)
	}
}

func TestDriverRegistration(t *testing.T) {
	t.Parallel()
	// database/sql.Register panics on duplicate; running this test
	// confirms the init() ran and registered the driver.
	names := sql.Drivers()
	found := false
	for _, n := range names {
		if n == DriverName {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("driver %q not registered (drivers: %v)", DriverName, names)
	}
}

func TestDriverOpenLegacy(t *testing.T) {
	t.Parallel()
	// driver.Driver.Open delegates to OpenConnector(name) + Connect.
	// Without FDB, Connect fails with an FDB initialization error.
	d := &Driver{}
	_, err := d.Open("fdbsql:///mydb")
	if err == nil {
		t.Fatal("expected Open to fail (no FDB available)")
	}
	// Bad DSN must surface at Open time (via OpenConnector).
	_, err = d.Open("not a valid dsn")
	if err == nil {
		t.Fatal("expected Open to fail on bad DSN")
	}
}

func TestConnectorAccessors(t *testing.T) {
	t.Parallel()
	d := &Driver{}
	c, err := d.OpenConnector("fdbsql:///mydb?cluster_file=/tmp/fdb.cluster")
	if err != nil {
		t.Fatalf("OpenConnector: %v", err)
	}
	conn := c.(*Connector)
	if conn.Driver() != d {
		t.Error("Driver() should return the parent driver")
	}
	dsn := conn.DSN()
	if dsn == nil || dsn.Path != "/mydb" {
		t.Errorf("DSN() returned unexpected value: %+v", dsn)
	}
	if dsn.Options["cluster_file"] != "/tmp/fdb.cluster" {
		t.Errorf("DSN options not preserved: %+v", dsn.Options)
	}
}

func TestDriverOpenConnector_BadDSN(t *testing.T) {
	t.Parallel()
	d := &Driver{}
	_, err := d.OpenConnector("not a valid dsn")
	if err == nil {
		t.Fatal("expected error for bad DSN")
	}
}

func TestDriverOpenConnector_GoodDSN(t *testing.T) {
	t.Parallel()
	d := &Driver{}
	c, err := d.OpenConnector("fdbsql:///mydb")
	if err != nil {
		t.Fatalf("OpenConnector: %v", err)
	}
	if _, ok := c.(driver.Connector); !ok {
		t.Fatal("returned value is not driver.Connector")
	}
	// Without FDB cluster, Connect fails at initialization.
	_, err = c.Connect(context.Background())
	if err == nil {
		t.Fatal("Connect should fail (no FDB available)")
	}
}

func TestConnectRespectsCtxCancel(t *testing.T) {
	t.Parallel()
	d := &Driver{}
	c, err := d.OpenConnector("fdbsql:///mydb")
	if err != nil {
		t.Fatalf("OpenConnector: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.Connect(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestConnectRespectsCtxDeadline(t *testing.T) {
	t.Parallel()
	d := &Driver{}
	c, err := d.OpenConnector("fdbsql:///mydb")
	if err != nil {
		t.Fatalf("OpenConnector: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()
	time.Sleep(1 * time.Millisecond)
	_, err = c.Connect(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected DeadlineExceeded, got %v", err)
	}
}
