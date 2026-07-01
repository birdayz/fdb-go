package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	configv1 "fdb.dev/cmd/frl/gen/frl/config/v1"
	"fdb.dev/cmd/frl/internal/config"
	"fdb.dev/cmd/frl/internal/meta"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

// storeAddressFlags is the shared flag set for every store-touching
// command. Two addressing modes (RFC-174 §3.1 — layered addressing):
//
//   - record-layer: context keyspace_path + meta_file / --meta-file
//   - relational:   --database /path --schema name — keyspace resolved
//     via the relational keyspace layout, metadata from the catalog's
//     schema-pinned template version (catalogSource)
//
// The modes are mutually exclusive per invocation; --database/--schema
// override the context's keyspace_path + metadata entirely.
type storeAddressFlags struct {
	contextName   string
	metaFile      string
	database      string
	schema        string
	clusterFile   string
	keyspaceTuple string // JSON array (--keyspace-tuple)
}

// register declares the flags on c. withMetaFile is false for commands
// that never load metadata from files (store info / store dump).
func (f *storeAddressFlags) register(c *cobra.Command, withMetaFile bool) {
	c.Flags().StringVar(&f.contextName, "context", "", "context name to use")
	if withMetaFile {
		c.Flags().StringVar(&f.metaFile, "meta-file", "", "path to MetaData.pb; overrides context.metadata")
	}
	c.Flags().StringVar(&f.database, "database", "", "relational database URI (with --schema; overrides keyspace_path)")
	c.Flags().StringVar(&f.schema, "schema", "", "relational schema name (with --database)")
	c.Flags().StringVar(&f.clusterFile, "cluster-file", "", "FDB cluster file; overrides the context's cluster_file — chains with `frl fdb up`")
	c.Flags().StringVar(&f.keyspaceTuple, "keyspace-tuple", "", `typed keyspace as a JSON array, e.g. '["myapp", 42, {"uuid": "…"}]'`)
}

// relational reports whether the relational addressing mode is active.
func (f *storeAddressFlags) relational() bool {
	return f.database != "" || f.schema != ""
}

// validate enforces the mode rules: --database and --schema come as a
// pair, don't mix with --meta-file (two competing metadata sources) or
// --keyspace-tuple (two competing keyspaces).
func (f *storeAddressFlags) validate() error {
	if (f.database == "") != (f.schema == "") {
		return fmt.Errorf("relational addressing needs both flags — pass --database AND --schema")
	}
	if f.relational() && f.metaFile != "" {
		return fmt.Errorf("conflicting metadata sources: --meta-file cannot be combined with --database/--schema (the catalog is the metadata source for relational stores)")
	}
	if f.relational() && f.keyspaceTuple != "" {
		return fmt.Errorf("conflicting keyspaces: --keyspace-tuple cannot be combined with --database/--schema")
	}
	return nil
}

// resolve validates the flags and loads the config context. A missing
// context is tolerated when the invocation is self-contained — a
// relational address or an explicit --meta-file — so `frl record scan
// --database /x --schema y` works with zero config (cluster from the
// default cluster-file discovery).
func (f *storeAddressFlags) resolve() (*storeTarget, error) {
	if err := f.validate(); err != nil {
		return nil, err
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	cfgCtx, err := config.ResolveContext(cfg, f.contextName)
	if err != nil {
		selfContained := f.metaFile != "" || f.relational() || f.clusterFile != ""
		if !selfContained {
			if errors.Is(err, config.ErrNoContext) {
				path, _ := config.Path()
				return nil, fmt.Errorf("%w (config: %s)", err, path)
			}
			return nil, err
		}
		cfgCtx = &configv1.Context{Name: "(cli-flag)"}
	}
	if err := validateContextAddressing(cfgCtx); err != nil {
		return nil, err
	}
	target := &storeTarget{
		cfgCtx:          cfgCtx,
		metaFile:        f.metaFile,
		database:        f.database,
		schema:          f.schema,
		clusterFileFlag: f.clusterFile,
	}
	if f.keyspaceTuple != "" {
		t, err := tupleFromJSON(f.keyspaceTuple)
		if err != nil {
			return nil, err
		}
		target.keyspaceTuple = t
	}
	// Adopt the context's addressing when no flag overrides it: a
	// context may carry database/schema (relational) or keyspace_tuple
	// (typed path) instead of keyspace_path.
	if !target.relational() && target.metaFile == "" && target.keyspaceTuple == nil {
		if cfgCtx.GetDatabase() != "" {
			target.database = cfgCtx.GetDatabase()
			target.schema = cfgCtx.GetSchema()
		} else if cfgCtx.GetKeyspaceTuple() != nil {
			t, err := tupleFromListValue(cfgCtx.GetKeyspaceTuple())
			if err != nil {
				return nil, fmt.Errorf("context %q keyspace_tuple: %w", cfgCtx.GetName(), err)
			}
			target.keyspaceTuple = t
		}
	}
	return target, nil
}

// validateContextAddressing enforces that a context declares exactly one
// addressing mode: keyspace_path, keyspace_tuple, or database+schema.
func validateContextAddressing(cfgCtx *configv1.Context) error {
	if (cfgCtx.GetDatabase() == "") != (cfgCtx.GetSchema() == "") {
		return fmt.Errorf("context %q sets only one of database/schema — they come as a pair", cfgCtx.GetName())
	}
	modes := 0
	if cfgCtx.GetKeyspacePath() != "" {
		modes++
	}
	if cfgCtx.GetKeyspaceTuple() != nil {
		modes++
	}
	if cfgCtx.GetDatabase() != "" {
		modes++
	}
	if modes > 1 {
		return fmt.Errorf("context %q is ambiguous — set exactly one of keyspace_path, keyspace_tuple, or database+schema", cfgCtx.GetName())
	}
	return nil
}

// storeTarget is a fully-resolved store address: the config context
// (cluster file + record-layer keyspace/metadata defaults) plus any
// flag overrides. withStore turns it into an open FDBRecordStore.
type storeTarget struct {
	cfgCtx          *configv1.Context
	metaFile        string
	database        string
	schema          string
	clusterFileFlag string
	keyspaceTuple   tuple.Tuple // typed keyspace (flag or context); nil → keyspace_path
}

func (t *storeTarget) relational() bool { return t.database != "" }

// clusterFile is the effective cluster file: --cluster-file wins over
// the context's, empty means the client's default discovery.
func (t *storeTarget) clusterFile() string {
	if t.clusterFileFlag != "" {
		return t.clusterFileFlag
	}
	return t.cfgCtx.GetClusterFile()
}

// describe renders the target for error messages — the relational
// address or the context's keyspace path, whichever is active.
func (t *storeTarget) describe() string {
	if t.relational() {
		return t.database + "/" + t.schema
	}
	return t.cfgCtx.GetKeyspacePath()
}

// subspace resolves the store's FDB subspace per the addressing mode.
func (t *storeTarget) subspace() (subspace.Subspace, error) {
	if t.relational() {
		return relationalStoreSubspace(t.database, t.schema)
	}
	if t.keyspaceTuple != nil {
		return subspaceFromTuple(t.keyspaceTuple), nil
	}
	if t.cfgCtx.GetKeyspacePath() == "" {
		return nil, fmt.Errorf("context %q has empty keyspace_path", t.cfgCtx.GetName())
	}
	return parseKeyspacePath(t.cfgCtx.GetKeyspacePath())
}

// resolveContextAndOverride is the legacy prelude retained for commands
// that only take --context/--meta-file (sql, meta catalog). Store-
// touching commands use storeAddressFlags.resolve instead.
func resolveContextAndOverride(contextName, metaFile string) (*configv1.Context, meta.Source, error) {
	f := storeAddressFlags{contextName: contextName, metaFile: metaFile}
	target, err := f.resolve()
	if err != nil {
		return nil, nil, err
	}
	var override meta.Source
	if metaFile != "" {
		override = &meta.FileSource{Path: metaFile}
	}
	return target.cfgCtx, override, nil
}

// lookupRecordType resolves name against md, returning the RecordType on
// hit and a "not found — available: A, B, C" error on miss. Shared by
// meta types describe / record scan / record count so typos always
// produce the same user-facing message.
func lookupRecordType(md *recordlayer.RecordMetaData, name string) (*recordlayer.RecordType, error) {
	if rt := md.GetRecordType(name); rt != nil {
		return rt, nil
	}
	return nil, fmt.Errorf("record type %q not found — available: %s",
		name, strings.Join(sortedRecordTypeNames(md), ", "))
}

// validateRecordType is the error-only form of lookupRecordType — for
// callers (record scan / count) that only need the presence check, not
// the RecordType itself.
func validateRecordType(md *recordlayer.RecordMetaData, name string) error {
	_, err := lookupRecordType(md, name)
	return err
}

// lookupIndex resolves an index name against md, returning the Index
// on hit and a "not found — available: A, B, C" error on miss. Shared
// by index describe / index scan so typos always produce the same
// user-facing message.
func lookupIndex(md *recordlayer.RecordMetaData, name string) (*recordlayer.Index, error) {
	if idx := md.GetIndex(name); idx != nil {
		return idx, nil
	}
	return nil, fmt.Errorf("index %q not found — available: %s",
		name, strings.Join(sortedIndexNames(md), ", "))
}

// resolveMetaSourceFile returns `override` when non-nil, otherwise
// invokes meta.FromContext(cfgCtx, nil, nil) — i.e. the FDB-store-
// unsupported resolution used by every metadata-reading command in v1.
// Wraps the two well-known sentinels (ErrMissingSource,
// ErrFDBStoreNotAvailable) with the context name so operators can tell
// which context the message is about when they have several.
func resolveMetaSourceFile(cfgCtx *configv1.Context, override meta.Source) (meta.Source, error) {
	if override != nil {
		return override, nil
	}
	src, err := meta.FromContext(cfgCtx, nil, nil)
	if err != nil {
		if errors.Is(err, meta.ErrMissingSource) ||
			errors.Is(err, meta.ErrFDBStoreNotAvailable) {
			return nil, fmt.Errorf("%w (context %q)", err, cfgCtx.GetName())
		}
		return nil, err
	}
	return src, nil
}

// withStoreE is the ergonomic twin of withStore for commands whose store
// closure doesn't need a return value. Most `record scan` / `index ls` /
// `index scan` style commands stream output directly to the writer and
// would otherwise thread a sentinel struct{} through their withStore
// calls — that's the boilerplate this wrapper eliminates.
func withStoreE(
	ctx context.Context,
	target *storeTarget,
	fn func(store *recordlayer.FDBRecordStore) error,
) error {
	_, err := withStore(ctx, target,
		func(s *recordlayer.FDBRecordStore) (struct{}, error) {
			return struct{}{}, fn(s)
		})
	return err
}

// withStore wires the plumbing that every data command needs: open the FDB
// connection from the context's cluster_file, resolve the store subspace
// and metadata source per the target's addressing mode, open the record
// store inside a transaction, and hand the ready-to-use store to the
// caller. Returns the caller's result + error.
//
// Metadata resolution order: relational addressing (catalogSource at the
// schema-pinned template version) → --meta-file → the context's
// metadata source.
func withStore[T any](
	ctx context.Context,
	target *storeTarget,
	fn func(store *recordlayer.FDBRecordStore) (T, error),
) (T, error) {
	var zero T
	cfgCtx := target.cfgCtx

	ss, err := target.subspace()
	if err != nil {
		return zero, err
	}

	db, err := openDatabase(cfgCtx.GetClusterFile())
	if err != nil {
		return zero, err
	}
	rec := recordlayer.NewFDBDatabase(db)

	var src meta.Source
	switch {
	case target.relational():
		src = &catalogSource{db: rec, database: target.database, schema: target.schema}
	case target.metaFile != "":
		src = &meta.FileSource{Path: target.metaFile}
	default:
		src, err = meta.FromContext(cfgCtx, rec, parseKeyspacePath)
		if err != nil {
			if errors.Is(err, meta.ErrMissingSource) {
				return zero, fmt.Errorf("%w (context %q)", err, cfgCtx.GetName())
			}
			return zero, err
		}
	}
	md, err := src.Load(ctx)
	if err != nil {
		return zero, err
	}

	result, err := rec.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		// SetSkipPossiblyRebuild: every withStore caller is a read-only
		// command, and Open() without it runs checkPossiblyRebuild —
		// which WRITES (header version bump, index clears/rebuild marks)
		// whenever the provided metadata is newer than the store header.
		// A `record scan --meta-file newer.pb` must never mutate the
		// store it inspects. Write commands (RFC-174 Slice 4) make this
		// decision explicitly on their own open path.
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(md).
			SetSubspace(ss).
			SetSkipPossiblyRebuild(true).
			Open()
		if err != nil {
			return nil, fmt.Errorf("open store at %s: %w", target.describe(), err)
		}
		return fn(store)
	})
	if err != nil {
		return zero, err
	}
	v, _ := result.(T)
	return v, nil
}
