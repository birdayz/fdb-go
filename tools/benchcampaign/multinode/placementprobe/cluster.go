package placementprobe

import (
	"errors"
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb"
)

// Op is a mutation kind.
type Op int

// Mutation kinds.
const (
	OpSet Op = iota
	OpClear
	OpClearRange
)

// Mutation is one write applied by Cluster.Commit.
type Mutation struct {
	Op    Op
	Key   []byte
	End   []byte // OpClearRange only
	Value []byte // OpSet only
}

// Reader reads a key inside the committing transaction (non-snapshot, so the
// key joins the read-conflict set; system keys are readable).
type Reader func(key []byte) (value []byte, present bool, err error)

// Cluster is everything the probe and the ballast tool need from
// FoundationDB. The real implementation is FDBCluster; tests use a fake.
type Cluster interface {
	// ReadVersion returns a fresh read version.
	ReadVersion() (int64, error)
	// BoundaryKeys returns every shard boundary in [begin, end) at version,
	// without a limit.
	BoundaryKeys(begin, end []byte, version int64) ([][]byte, error)
	// Keys returns every key in [begin, end) at version (snapshot read).
	Keys(begin, end []byte, version int64) ([][]byte, error)
	// Addresses returns the storage addresses holding key (location cache).
	Addresses(key []byte) ([]string, error)
	// Get reads one key at version; system permits \xff keys.
	Get(key []byte, version int64, system bool) (value []byte, present bool, err error)
	// Commit runs guard and then applies writes in one transaction, retrying
	// retryable errors; guard errors abort without retry. It returns the
	// committed version.
	Commit(guard func(read Reader) error, writes []Mutation) (int64, error)
}

// FDBCluster implements Cluster over the pure-Go client.
type FDBCluster struct {
	DB fdb.Database
	// TimeoutMillis bounds each transaction (FDB transaction timeout option).
	TimeoutMillis int64
	// MaxAttempts bounds Commit retries.
	MaxAttempts int
}

var _ Cluster = FDBCluster{}

// ReadVersion implements Cluster.
func (c FDBCluster) ReadVersion() (int64, error) {
	tr, err := c.DB.CreateTransaction()
	if err != nil {
		return 0, err
	}
	return tr.GetReadVersion().Get()
}

func (c FDBCluster) at(version int64, system bool) (fdb.Transaction, error) {
	tr, err := c.DB.CreateTransaction()
	if err != nil {
		return fdb.Transaction{}, err
	}
	if c.TimeoutMillis > 0 {
		if err := tr.Options().SetTimeout(c.TimeoutMillis); err != nil {
			return fdb.Transaction{}, err
		}
	}
	if system {
		if err := tr.Options().SetReadSystemKeys(); err != nil {
			return fdb.Transaction{}, err
		}
	}
	tr.SetReadVersion(version)
	return tr, nil
}

// BoundaryKeys implements Cluster: LocalityGetBoundaryKeys reads
// \xff/keyServers at the given read version; limit 0 means unlimited.
func (c FDBCluster) BoundaryKeys(begin, end []byte, version int64) ([][]byte, error) {
	keys, err := c.DB.LocalityGetBoundaryKeys(fdb.KeyRange{Begin: fdb.Key(begin), End: fdb.Key(end)}, 0, version)
	if err != nil {
		return nil, err
	}
	out := make([][]byte, len(keys))
	for i, k := range keys {
		out[i] = append([]byte(nil), k...)
	}
	return out, nil
}

// Keys implements Cluster. The whole range is read in one transaction at
// version, so it must finish within the MVCC window (about 5 s); a
// transaction_too_old fails the probe rather than mixing versions.
func (c FDBCluster) Keys(begin, end []byte, version int64) ([][]byte, error) {
	tr, err := c.at(version, false)
	if err != nil {
		return nil, err
	}
	kvs, err := tr.Snapshot().GetRange(fdb.KeyRange{Begin: fdb.Key(begin), End: fdb.Key(end)},
		fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
	if err != nil {
		return nil, err
	}
	out := make([][]byte, len(kvs))
	for i, kv := range kvs {
		out[i] = append([]byte(nil), kv.Key...)
	}
	return out, nil
}

// Addresses implements Cluster. LocalityGetAddressesForKey ignores the read
// version (see the package comment); a fresh transaction per key costs no GRV.
func (c FDBCluster) Addresses(key []byte) ([]string, error) {
	tr, err := c.DB.CreateTransaction()
	if err != nil {
		return nil, err
	}
	return tr.LocalityGetAddressesForKey(fdb.Key(key)).Get()
}

// Get implements Cluster.
func (c FDBCluster) Get(key []byte, version int64, system bool) ([]byte, bool, error) {
	tr, err := c.at(version, system)
	if err != nil {
		return nil, false, err
	}
	v, err := tr.Get(fdb.Key(key)).Get()
	if err != nil {
		return nil, false, err
	}
	return v, v != nil, nil
}

// Commit implements Cluster with an explicit OnError loop so the committed
// version of the successful attempt is available.
func (c FDBCluster) Commit(guard func(read Reader) error, writes []Mutation) (int64, error) {
	tr, err := c.DB.CreateTransaction()
	if err != nil {
		return 0, err
	}
	attempts := c.MaxAttempts
	if attempts <= 0 {
		attempts = 10
	}
	for attempt := 1; ; attempt++ {
		err := c.attempt(tr, guard, writes)
		if err == nil {
			return tr.GetCommittedVersion()
		}
		var fe fdb.Error
		if !errors.As(err, &fe) || attempt >= attempts {
			return 0, err
		}
		if err := tr.OnError(fe).Get(); err != nil {
			return 0, fmt.Errorf("after attempt %d: %w", attempt, err)
		}
	}
}

func (c FDBCluster) attempt(tr fdb.Transaction, guard func(read Reader) error, writes []Mutation) error {
	if c.TimeoutMillis > 0 {
		if err := tr.Options().SetTimeout(c.TimeoutMillis); err != nil {
			return err
		}
	}
	if err := tr.Options().SetReadSystemKeys(); err != nil {
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
		case OpSet:
			tr.Set(fdb.Key(m.Key), m.Value)
		case OpClear:
			tr.Clear(fdb.Key(m.Key))
		case OpClearRange:
			tr.ClearRange(fdb.KeyRange{Begin: fdb.Key(m.Key), End: fdb.Key(m.End)})
		default:
			return fmt.Errorf("unknown mutation op %d", m.Op)
		}
	}
	return tr.Commit().Get()
}
