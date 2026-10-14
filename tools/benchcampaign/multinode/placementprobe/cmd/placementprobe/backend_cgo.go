//go:build libfdbc

package main

import (
	"errors"
	"fmt"

	"fdb.dev/tools/benchcampaign/multinode/placementprobe"
	"github.com/apple/foundationdb/bindings/go/src/fdb"
)

// The libfdb_c backend, the production probe: libfdb_c 7.3.77's
// getAddressesForKey reads \xff/serverTag, \xff/keyServers and \xff/serverList
// in the transaction (NativeAPI.actor.cpp getAddressesForKeyActor), so every
// address is the shard map at the snapshot's read version.
type cgoCluster struct {
	db            fdb.Database
	timeoutMillis int64
	maxAttempts   int
}

func openCluster(clusterFile string, timeoutMillis int64) (placementprobe.Cluster, func(), error) {
	if err := fdb.APIVersion(apiVersion); err != nil {
		return nil, nil, err
	}
	db, err := fdb.OpenDatabase(clusterFile)
	if err != nil {
		return nil, nil, err
	}
	return cgoCluster{db: db, timeoutMillis: timeoutMillis, maxAttempts: 10}, db.Close, nil
}

func (c cgoCluster) AddressBasis() string { return placementprobe.AddressTransactional }

func (c cgoCluster) ReadVersion() (int64, error) {
	tr, err := c.db.CreateTransaction()
	if err != nil {
		return 0, err
	}
	return tr.GetReadVersion().Get()
}

func (c cgoCluster) options(tr fdb.Transaction) error {
	if c.timeoutMillis > 0 {
		if err := tr.Options().SetTimeout(c.timeoutMillis); err != nil {
			return err
		}
	}
	return tr.Options().SetReadSystemKeys()
}

func (c cgoCluster) At(version int64) (placementprobe.Txn, error) {
	tr, err := c.db.CreateTransaction()
	if err != nil {
		return nil, err
	}
	if err := c.options(tr); err != nil {
		return nil, err
	}
	tr.SetReadVersion(version)
	return cgoTxn{tr: tr}, nil
}

type cgoTxn struct{ tr fdb.Transaction }

func (t cgoTxn) Get(key []byte) ([]byte, bool, error) {
	v, err := t.tr.Get(fdb.Key(key)).Get()
	if err != nil {
		return nil, false, err
	}
	return v, v != nil, nil
}

func (t cgoTxn) Range(begin, end []byte, fromLastLessOrEqual bool) ([]placementprobe.KV, error) {
	var r fdb.Range = fdb.KeyRange{Begin: fdb.Key(begin), End: fdb.Key(end)}
	if fromLastLessOrEqual {
		r = fdb.SelectorRange{Begin: fdb.LastLessOrEqual(fdb.Key(begin)), End: fdb.FirstGreaterOrEqual(fdb.Key(end))}
	}
	kvs, err := t.tr.Snapshot().GetRange(r, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
	if err != nil {
		return nil, err
	}
	out := make([]placementprobe.KV, len(kvs))
	for i, kv := range kvs {
		out[i] = placementprobe.KV{Key: append([]byte(nil), kv.Key...), Value: append([]byte(nil), kv.Value...)}
	}
	return out, nil
}

// addressBatch bounds the concurrent getAddressesForKey actors per round.
const addressBatch = 512

func (t cgoTxn) Addresses(keys [][]byte) ([][]string, error) {
	out := make([][]string, len(keys))
	for start := 0; start < len(keys); start += addressBatch {
		stop := min(start+addressBatch, len(keys))
		futures := make([]fdb.FutureStringSlice, stop-start)
		for i := range futures {
			futures[i] = t.tr.LocalityGetAddressesForKey(fdb.Key(keys[start+i]))
		}
		for i, f := range futures {
			a, err := f.Get()
			if err != nil {
				return nil, err
			}
			out[start+i] = a
		}
	}
	return out, nil
}

func (c cgoCluster) Commit(guard func(read placementprobe.Reader) error, writes []placementprobe.Mutation) (int64, error) {
	tr, err := c.db.CreateTransaction()
	if err != nil {
		return 0, err
	}
	for attempt := 1; ; attempt++ {
		err := c.attempt(tr, guard, writes)
		if err == nil {
			return tr.GetCommittedVersion()
		}
		var fe fdb.Error
		if !errors.As(err, &fe) || attempt >= c.maxAttempts {
			return 0, err
		}
		if err := tr.OnError(fe).Get(); err != nil {
			return 0, fmt.Errorf("after attempt %d: %w", attempt, err)
		}
	}
}

func (c cgoCluster) attempt(tr fdb.Transaction, guard func(read placementprobe.Reader) error, writes []placementprobe.Mutation) error {
	if err := c.options(tr); err != nil {
		return err
	}
	read := func(key []byte) ([]byte, bool, error) {
		v, err := tr.Get(fdb.Key(key)).Get()
		if err != nil {
			return nil, false, err
		}
		return v, v != nil, nil
	}
	if guard != nil {
		if err := guard(read); err != nil {
			return err
		}
	}
	for _, m := range writes {
		switch m.Op {
		case placementprobe.OpSet:
			tr.Set(fdb.Key(m.Key), m.Value)
		case placementprobe.OpClear:
			tr.Clear(fdb.Key(m.Key))
		case placementprobe.OpClearRange:
			tr.ClearRange(fdb.KeyRange{Begin: fdb.Key(m.Key), End: fdb.Key(m.End)})
		default:
			return fmt.Errorf("unknown mutation op %d", m.Op)
		}
	}
	return tr.Commit().Get()
}
