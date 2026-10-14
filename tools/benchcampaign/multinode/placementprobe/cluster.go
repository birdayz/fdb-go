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

// KV is one key-value pair.
type KV struct {
	Key, Value []byte
}

// Reader reads a key inside the committing transaction (non-snapshot, so the
// key joins the read-conflict set; system keys are readable).
type Reader func(key []byte) (value []byte, present bool, err error)

// Txn is ONE backend transaction pinned at a read version with
// READ_SYSTEM_KEYS. Every method reads at that version.
type Txn interface {
	// Get reads one key.
	Get(key []byte) (value []byte, present bool, err error)
	// Range returns every pair of [begin, end), without a limit. With
	// fromLastLessOrEqual it starts at the last key <= begin instead (the
	// keyServers entry covering begin).
	Range(begin, end []byte, fromLastLessOrEqual bool) ([]KV, error)
	// Addresses returns each key's storage addresses (LocalityGetAddressesForKey
	// in this transaction), in the order of keys.
	Addresses(keys [][]byte) ([][]string, error)
}

// Cluster is everything the probe and the ballast tool need from
// FoundationDB. Production backends: cmd/placementprobe backend_cgo.go
// (libfdb_c, transactional addresses) and GoCluster (pure Go, diagnostic
// addresses). Tests use a fake.
type Cluster interface {
	// ReadVersion returns a fresh read version.
	ReadVersion() (int64, error)
	// At opens one transaction at version.
	At(version int64) (Txn, error)
	// AddressBasis is AddressTransactional or AddressLocationCache.
	AddressBasis() string
	// Commit runs guard and then applies writes in one transaction, retrying
	// retryable errors; guard errors abort without retry. It returns the
	// committed version.
	Commit(guard func(read Reader) error, writes []Mutation) (int64, error)
}

// GoCluster implements Cluster over the pure-Go client. Its
// LocalityGetAddressesForKey answers from the client location cache, unlike
// libfdb_c's transactional getAddressesForKey, so its address basis is
// diagnostic: probes from it are refused without --diagnostic and never pass
// placement_receipt integrity. Ballast and control operations are exact.
type GoCluster struct {
	DB fdb.Database
	// TimeoutMillis bounds each transaction (FDB transaction timeout option).
	TimeoutMillis int64
	// MaxAttempts bounds Commit retries.
	MaxAttempts int
}

var _ Cluster = GoCluster{}

// AddressBasis implements Cluster.
func (c GoCluster) AddressBasis() string { return AddressLocationCache }

// ReadVersion implements Cluster.
func (c GoCluster) ReadVersion() (int64, error) {
	tr, err := c.DB.CreateTransaction()
	if err != nil {
		return 0, err
	}
	return tr.GetReadVersion().Get()
}

// At implements Cluster.
func (c GoCluster) At(version int64) (Txn, error) {
	tr, err := c.DB.CreateTransaction()
	if err != nil {
		return nil, err
	}
	if c.TimeoutMillis > 0 {
		if err := tr.Options().SetTimeout(c.TimeoutMillis); err != nil {
			return nil, err
		}
	}
	if err := tr.Options().SetReadSystemKeys(); err != nil {
		return nil, err
	}
	tr.SetReadVersion(version)
	return goTxn{tr: tr}, nil
}

type goTxn struct{ tr fdb.Transaction }

func (t goTxn) Get(key []byte) ([]byte, bool, error) {
	v, err := t.tr.Get(fdb.Key(key)).Get()
	if err != nil {
		return nil, false, err
	}
	return v, v != nil, nil
}

func (t goTxn) Range(begin, end []byte, fromLastLessOrEqual bool) ([]KV, error) {
	var r fdb.Range = fdb.KeyRange{Begin: fdb.Key(begin), End: fdb.Key(end)}
	if fromLastLessOrEqual {
		r = fdb.SelectorRange{Begin: fdb.LastLessOrEqual(fdb.Key(begin)), End: fdb.FirstGreaterOrEqual(fdb.Key(end))}
	}
	kvs, err := t.tr.Snapshot().GetRange(r, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
	if err != nil {
		return nil, err
	}
	out := make([]KV, len(kvs))
	for i, kv := range kvs {
		out[i] = KV{Key: append([]byte(nil), kv.Key...), Value: append([]byte(nil), kv.Value...)}
	}
	return out, nil
}

func (t goTxn) Addresses(keys [][]byte) ([][]string, error) {
	futures := make([]fdb.FutureStringSlice, len(keys))
	for i, k := range keys {
		futures[i] = t.tr.LocalityGetAddressesForKey(fdb.Key(k))
	}
	out := make([][]string, len(keys))
	for i, f := range futures {
		a, err := f.Get()
		if err != nil {
			return nil, err
		}
		out[i] = a
	}
	return out, nil
}

// Commit implements Cluster with an explicit OnError loop so the committed
// version of the successful attempt is available.
func (c GoCluster) Commit(guard func(read Reader) error, writes []Mutation) (int64, error) {
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

func (c GoCluster) attempt(tr fdb.Transaction, guard func(read Reader) error, writes []Mutation) error {
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
