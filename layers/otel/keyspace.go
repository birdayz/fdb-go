package otel

// Key layouts of an FDB run.
//
// A run's owner manifest is always at ("otel_otelbench_v2", run, "owner") and
// records the run's key layout. In the tuple layout (runs created before
// directory_v1) every key of the run starts with ("otel_otelbench_v2", run),
// about 30 bytes. In directory_v1 the run's data lives under a Record Layer
// KeySpace path whose application and run directories are Java's
// DirectoryLayerDirectory, resolved through the FDB directory layer
// (ExtendedDirectoryLayer.global) to small integers, and the run's
// subdirectories are integer constants: a key starts with about six bytes.

import (
	"context"
	"encoding/json"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/keyspace"
)

const (
	keyLayoutTuple     = ""
	keyLayoutDirectory = "directory_v1"

	// keyApplication is the application directory's name in the directory
	// layer and the tuple layout's first key element.
	keyApplication = "otel_otelbench_v2"
)

// Integer subdirectories of a directory_v1 run (keySpace).
const (
	keyStore int64 = iota + 1
	keyLogs
	keyLogTrace
	keyLogEq
	keyMessageWriters
	keyQuery
)

// runKeys are the subspaces and keys of one FDB run.
type runKeys struct {
	layout string
	// legacy is ("otel_otelbench_v2", run): the owner manifest, and all of a
	// tuple-layout run's data.
	legacy subspace.Subspace
	// data holds the run's data: legacy, or the KeySpace path of the run.
	data                     subspace.Subspace
	store                    subspace.Subspace
	logs, logTrace, logEq    subspace.Subspace
	messageWriters, queryKey fdb.Key
}

func (k runKeys) owner() fdb.Key { return k.legacy.Sub("owner").FDBKey() }

// runKeySpace is the directory_v1 KeySpace: application / run / one integer
// constant per kind of data. The application and run directories are Java's
// DirectoryLayerDirectory: names resolved through the directory layer of db.
func runKeySpace(db *recordlayer.FDBDatabase) (*keyspace.KeySpace, error) {
	run := recordlayer.DirectoryLayerDirectory("run", db)
	for _, d := range []struct {
		name  string
		value int64
	}{{"store", keyStore}, {"logs", keyLogs}, {"log_trace", keyLogTrace}, {"log_eq", keyLogEq}, {"message_writers", keyMessageWriters}, {"query", keyQuery}} {
		if err := run.AddSubdirectory(keyspace.NewConstantDirectory(d.name, keyspace.KeyTypeLong, d.value)); err != nil {
			return nil, err
		}
	}
	app := recordlayer.DirectoryLayerDirectory("application", db)
	if err := app.AddSubdirectory(run); err != nil {
		return nil, err
	}
	root := keyspace.NewDirectory("root", keyspace.KeyTypeNull)
	if err := root.AddSubdirectory(app); err != nil {
		return nil, err
	}
	ks := keyspace.NewKeySpace(root)
	return ks, ks.Validate()
}

// storedKeyLayout reads the key layout an existing run's manifest records;
// found is false for a fresh run.
func storedKeyLayout(ctx context.Context, db *recordlayer.FDBDatabase, legacy subspace.Subspace) (layout string, found bool, err error) {
	_, err = db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		if err := configureRecordTimeout(rc); err != nil {
			return nil, err
		}
		wire, err := rc.Transaction().Get(legacy.Sub("owner").FDBKey()).Get()
		if err != nil || wire == nil {
			found = false
			return nil, err
		}
		var m Manifest
		if err := json.Unmarshal(wire, &m); err != nil {
			return nil, wrap("corruption", "owner manifest", err)
		}
		layout, found = m.KeyLayout, true
		return nil, nil
	})
	return layout, found, err
}

// openRunKeys returns the keys of run in layout.
func openRunKeys(ctx context.Context, db *recordlayer.FDBDatabase, run, layout string) (runKeys, error) {
	legacy := subspace.Sub(keyApplication, run)
	k := runKeys{layout: layout, legacy: legacy}
	if layout == keyLayoutTuple {
		k.data = legacy
		k.store = legacy.Sub("store")
		k.logs, k.logTrace, k.logEq = legacy.Sub(logRecordSubspace), legacy.Sub(logTraceSubspace), legacy.Sub(logEqSubspace)
		k.messageWriters = legacy.Sub("message_writers").FDBKey()
		k.queryKey = legacy.Sub("query_v1", "continuation_hmac_v1").FDBKey()
		return k, nil
	}
	ks, err := runKeySpace(db)
	if err != nil {
		return k, err
	}
	app, err := ks.Path(ctx, "application", keyApplication)
	if err != nil {
		return k, wrap("operation", "run key space", err)
	}
	path, err := app.Add(ctx, "run", run)
	if err != nil {
		return k, wrap("operation", "run key space", err)
	}
	sub := func(name string) (subspace.Subspace, error) {
		p, err := path.Add(ctx, name, nil)
		if err != nil {
			return nil, wrap("operation", "run key space", err)
		}
		return p.ToSubspace(), nil
	}
	k.data = path.ToSubspace()
	spaces := []*subspace.Subspace{&k.store, &k.logs, &k.logTrace, &k.logEq}
	for i, name := range []string{"store", "logs", "log_trace", "log_eq"} {
		if *spaces[i], err = sub(name); err != nil {
			return k, err
		}
	}
	writers, err := sub("message_writers")
	if err != nil {
		return k, err
	}
	query, err := sub("query")
	if err != nil {
		return k, err
	}
	k.messageWriters = writers.FDBKey()
	k.queryKey = query.Pack(tuple.Tuple{"continuation_hmac_v1"})
	return k, nil
}
