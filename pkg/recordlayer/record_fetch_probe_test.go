package recordlayer

import (
	"bytes"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
)

// recordFetchProbe observes record-subspace reads: how many distinct records
// had a read in flight when the scan first waited on one.
type recordFetchProbe struct {
	fdb.WritableTransaction
	records        subspace.Subspace
	issued         map[any]bool
	issuedAtWait   int
	versions       int
	versionsAtWait int
	waited         bool
	maxOutstanding int
	resolved       map[any]bool
}

func newRecordFetchProbe(tx fdb.WritableTransaction, records subspace.Subspace) *recordFetchProbe {
	return &recordFetchProbe{WritableTransaction: tx, records: records, issued: map[any]bool{}, resolved: map[any]bool{}}
}

func (p *recordFetchProbe) Get(key fdb.KeyConvertible) fdb.FutureByteSlice {
	future := p.WritableTransaction.Get(key)
	if !bytes.HasPrefix(key.FDBKey(), p.records.Bytes()) {
		return future
	}
	t, err := p.records.Unpack(key.FDBKey())
	if err != nil || len(t) == 0 {
		return future
	}
	p.issued[t[0]] = true
	if t[len(t)-1] == int64(-1) {
		p.versions++
	}
	p.maxOutstanding = max(p.maxOutstanding, len(p.issued)-len(p.resolved))
	return &probedFuture{FutureByteSlice: future, probe: p, record: t[0]}
}

type probedFuture struct {
	fdb.FutureByteSlice
	probe  *recordFetchProbe
	record any
}

func (f *probedFuture) Get() ([]byte, error) {
	if !f.probe.waited {
		f.probe.waited = true
		f.probe.issuedAtWait = len(f.probe.issued)
		f.probe.versionsAtWait = f.probe.versions
	}
	f.probe.resolved[f.record] = true
	return f.FutureByteSlice.Get()
}
