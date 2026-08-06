package sqldriver

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
)

// Scheme is the DSN scheme accepted by this driver. It matches Java's
// JDBC scheme (minus the "jdbc:" prefix, which Go's database/sql does
// not use).
const Scheme = "fdbsql"

// Mode selects embedded (in-process FDB client) or remote (gRPC) execution.
type Mode int

const (
	// ModeEmbedded is in-process: the driver talks directly to FDB.
	ModeEmbedded Mode = iota
	// ModeRemote connects to a fdb-relational-server via gRPC.
	// Not yet implemented.
	ModeRemote
)

// DSN is a parsed connection string.
//
// Accepted forms:
//
//	fdbsql:///PATH                             — embedded, default cluster file
//	fdbsql:///PATH?cluster_file=/path          — embedded, explicit cluster file
//	fdbsql://HOST:PORT/PATH                    — remote (gRPC), NOT YET IMPLEMENTED
//
// The path component is the database path (Java's
// RelationalConnection.getPath()) and is mandatory.
type DSN struct {
	// Mode selects embedded vs. remote.
	Mode Mode
	// Path is the database path (corresponds to Java's
	// RelationalConnection.getPath()). Always starts with "/".
	Path string
	// Schema is the initial schema name set on the connection.
	// Corresponds to the ?schema= query option.
	Schema string
	// Host is the gRPC host:port for remote mode. Empty for embedded.
	Host string
	// Options are raw query-string options. Empty values are kept as "".
	Options map[string]string
}

// Clone returns a deep copy of d, including its Options map. Callers may
// mutate the result without affecting the original.
//
// This is what makes a Connector's DSN a snapshot rather than a shared handle.
// Every field a Connector reads after construction — Path, Schema, Mode, Host
// and the Options map that carries cluster_file — must come from the same
// frozen value as the connection options decoded at OpenConnector time.
// Anything else is a split brain: the security option validated and frozen up
// front while the fields around it stay live and mutable, so a mutation between
// OpenConnector and the first Connect would be honoured by some reads and
// ignored by others.
func (d *DSN) Clone() *DSN {
	if d == nil {
		return nil
	}
	out := *d
	out.Options = make(map[string]string, len(d.Options))
	for k, v := range d.Options {
		out.Options[k] = v
	}
	return &out
}

// RestrictDDLToSessionDatabaseParam is the DSN query parameter that sets
// api.OptRestrictDDLToSessionDatabase on every connection the DSN opens. It is
// spelled as the lower-cased option name so the DSN parameter and the option
// stay obviously the same knob.
const RestrictDDLToSessionDatabaseParam = "restrict_ddl_to_session_database"

// ConnectionOptions converts the DSN's recognised query parameters into the
// api.Options installed on each connection.
//
// Only options that must be decided before the first statement belong here.
// Unrecognised parameters stay in the raw Options map and are ignored, matching
// how cluster_file and schema are handled.
func (d *DSN) ConnectionOptions() (*api.Options, error) {
	opts := api.NoOptions()
	if raw, present := d.Options[RestrictDDLToSessionDatabaseParam]; present {
		v, err := parseDSNBool(RestrictDDLToSessionDatabaseParam, raw)
		if err != nil {
			return nil, err
		}
		opts = opts.With(api.OptRestrictDDLToSessionDatabase, v)
	}
	if raw, present := d.Options[TransactionTagsParam]; present {
		tags, err := parseDSNTags(raw)
		if err != nil {
			return nil, err
		}
		opts = opts.With(api.OptTransactionTags, tags)
	}
	return opts, nil
}

// TransactionTagsParam is the DSN query parameter that sets
// api.OptTransactionTags on every connection the DSN opens. Multiple tags are
// comma-separated: `?transaction_tags=tenant-a,bulk`.
const TransactionTagsParam = "transaction_tags"

// parseDSNTags splits and validates the comma-separated tag list. Validation
// happens here, at DSN parse time, so a malformed tag fails when the pool is
// opened rather than on the first statement of some later request — the same
// reasoning that makes parseDSNBool reject an unrecognised boolean instead of
// reading it as false.
//
// Empty elements are dropped rather than becoming empty tags, so a trailing
// comma is not an error; an entirely empty value yields no tags at all, which
// is indistinguishable from omitting the parameter.
func parseDSNTags(raw string) ([]string, error) {
	var tags []string
	for _, part := range strings.Split(raw, ",") {
		if t := strings.TrimSpace(part); t != "" {
			tags = append(tags, t)
		}
	}
	if len(tags) == 0 {
		return nil, nil
	}
	// The record layer's limits are the binding ones — stricter than the FDB
	// client's own 5/255 — so applying them here means the DSN and a
	// programmatic RecordContextConfig reject exactly the same sets.
	uniq, err := recordlayer.ValidateTags(tags)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", TransactionTagsParam, err)
	}
	sort.Strings(uniq)
	return uniq, nil
}

// parseDSNBool reads a boolean DSN parameter. A bare `?name` (empty value)
// means true, the usual URL-flag convention; anything not recognisable as a
// boolean is an error rather than a silent false, because silently reading a
// misspelled security flag as "off" is the failure mode that matters.
func parseDSNBool(name, raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "1", "t", "true", "yes", "on":
		return true, nil
	case "0", "f", "false", "no", "off":
		return false, nil
	default:
		return false, api.NewErrorf(api.ErrCodeInvalidParameter,
			"DSN option %q must be a boolean, got %q", name, raw)
	}
}

// ParseDSN parses a DSN string into a DSN.
//
// Returns a relational Error with code InvalidPath if the DSN is
// malformed or uses an unsupported scheme. Matches Java's behavior
// (JDBCRelationalDriver.acceptsURL + connect).
func ParseDSN(s string) (*DSN, error) {
	if s == "" {
		return nil, api.NewError(api.ErrCodeInvalidPath, "empty DSN")
	}

	u, err := url.Parse(s)
	if err != nil {
		return nil, api.WrapError(api.ErrCodeInvalidPath, "malformed DSN: "+s, err)
	}

	if u.Scheme != Scheme {
		return nil, api.NewErrorf(api.ErrCodeInvalidPath,
			"unsupported scheme %q, expected %q", u.Scheme, Scheme)
	}

	// Path must be present and non-empty.
	if u.Path == "" || u.Path == "/" {
		return nil, api.NewError(api.ErrCodeInvalidPath,
			"DSN is missing database path (expected fdbsql:///PATH)")
	}

	dsn := &DSN{Path: u.Path, Options: make(map[string]string)}

	// Extract reserved options that have typed fields.
	if schema := u.Query().Get("schema"); schema != "" {
		dsn.Schema = schema
	}

	// url.Parse gives us Host = "" for fdbsql:///path and Host = "h:p"
	// for fdbsql://h:p/path. That matches Java's JDBC URI behavior.
	if u.Host == "" {
		dsn.Mode = ModeEmbedded
	} else {
		dsn.Mode = ModeRemote
		dsn.Host = u.Host
	}

	// Flatten query values (first value wins, matches Java's
	// JDBCURI.getFirstValue pattern).
	for key, values := range u.Query() {
		if len(values) == 0 {
			dsn.Options[key] = ""
		} else {
			dsn.Options[key] = values[0]
		}
	}

	return dsn, nil
}

// String renders the DSN back to its canonical URI form. Always
// includes the scheme and a valid path. Query options are sorted
// for deterministic output.
func (d *DSN) String() string {
	var b strings.Builder
	b.WriteString(Scheme)
	b.WriteString("://")
	if d.Mode == ModeRemote && d.Host != "" {
		b.WriteString(d.Host)
	}
	b.WriteString(d.Path)
	if len(d.Options) > 0 {
		b.WriteByte('?')
		first := true
		// Sort keys for determinism — useful for logging and tests.
		keys := make([]string, 0, len(d.Options))
		for k := range d.Options {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if !first {
				b.WriteByte('&')
			}
			first = false
			b.WriteString(url.QueryEscape(k))
			b.WriteByte('=')
			b.WriteString(url.QueryEscape(d.Options[k]))
		}
	}
	return b.String()
}
