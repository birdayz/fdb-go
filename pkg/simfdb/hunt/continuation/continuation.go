// Package continuation is RFC-199 Tier 2's continuation-under-fault replay driver.
//
// A cursor scan that exceeds a row/byte/time limit does not return everything — it mints a
// CONTINUATION token and the caller resumes the scan, in a fresh transaction at a new read
// version, from that token. Every byte of that token is a place a bug can hide: an off-by-one at a
// page boundary, a plan hash baked into (or missing from) the token, a resume that re-scans or
// skips a row, a version-path-dependence when the data changed underneath the token. The record
// layer has hand-written point tests for a handful of these (delete-before-continuation,
// insert-around-continuation, index-delete-past-continuation); this driver is the seeded sweep that
// turns those point samples into a loop-until-bug fuzzer over the deterministic SimFDB backend.
//
// The oracle is an INDEPENDENT Go model, not the engine judging itself. For a seeded record set the
// exact scan order is known a priori — records come back in primary-key (tuple) order, a VALUE index
// comes back in (indexed-value, primary-key) order — so the model is computed directly from the
// seeded (pk, price) pairs. Four checks, all airtight:
//
//   - PAGINATION EQUIVALENCE — paginate each scan (record / index, forward / reverse) at every page
//     size, resuming from the continuation each page in a FRESH transaction, and assert the
//     concatenation equals the model. Page size 1 is the hard case: every single row round-trips
//     through a continuation token, so any token-encoding defect diverges immediately.
//   - PREFIX-DELETE INVARIANCE — mint a continuation mid-scan, then in a separate committed
//     transaction (version bump) delete a record the scan ALREADY passed, resume, and assert the
//     un-scanned tail is untouched: a change to the scanned prefix must not perturb the resume.
//   - TAIL-DELETE REFLECTED — mint a continuation mid-scan, delete a record the scan has NOT yet
//     reached, resume, and assert the tail equals the model with exactly that row removed: a delete
//     past the continuation point must be skipped, not double-counted or lost.
//   - TAIL-DELETE UNDER AN INJECTED FAULT — the same tail delete, but committed through a targeted
//     fault (InjectOnce) in a raw single-commit transaction (db.Run would retry the fault away): the
//     resume must reflect the fault's TRUE outcome — not_committed(1020) rolls the delete back (tail
//     unchanged), and commit_unknown(1021) leaves it durable or not depending on which of its two
//     real branches fired (tail minus the key, or tail unchanged).
//
// The continuation itself is a fault (each page boundary is a "the transaction ended, resume from
// bytes" event) as is the between-page version bump; the fourth check adds a targeted commit fault on
// top. Fully deterministic over SimFDB: same seed ⇒ same pages, same verdict.
package continuation

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"

	"fdb.dev/gen"
	fdb "fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/simfdb"
	"fdb.dev/pkg/simfdb/hunt"

	"google.golang.org/protobuf/proto"
)

// contStream is this workload's PCG stream, distinct from the record workload's (7) and the
// interleave driver's (11).
const contStream = uint64(17)

// Workload is the continuation-replay driver, a hunt.Workload. It ignores the record-oriented
// hunt.Config. The scans run fault-free; the fourth oracle injects a targeted commit fault on the
// between-page write only (via a raw single-commit transaction), so the scan model stays exact.
type Workload struct {
	Records  int   // records seeded per run (default 40)
	PKDomain int64 // primary keys drawn from [0,PKDomain); > Records leaves gaps (default 200)
}

func (Workload) Name() string { return "continuation" }

func (w Workload) Run(seed uint64, _ hunt.Config) *hunt.Report { return w.run(seed).Report }

func (w Workload) withDefaults() Workload {
	if w.Records <= 0 {
		w.Records = 40
	}
	if w.PKDomain <= 0 {
		w.PKDomain = 200
	}
	if w.PKDomain < int64(w.Records) {
		w.PKDomain = int64(w.Records) // need at least Records distinct keys
	}
	return w
}

// priceDomain keeps prices in a small band so many records share a price — exercising the
// (price, primary-key) tie-break in the index continuation.
func (w Workload) priceDomain() int64 {
	if d := int64(w.Records / 3); d >= 2 {
		return d
	}
	return 2
}

// Result is the driver's rich outcome: the hunt.Report plus the number of pages scanned (work done).
type Result struct {
	Report *hunt.Report
	Pages  int
}

type rec struct {
	pk    int64
	price int64
}

func (w Workload) run(seed uint64) *Result {
	w = w.withDefaults()
	rng := rand.New(rand.NewPCG(seed, contStream))
	recs := w.seedData(rng)

	rep := &hunt.Report{Seed: seed}
	res := &Result{Report: rep}

	// Reference orders, computed independently from the seeded set.
	model := buildModel(recs)

	// The mutating oracles draw their parameters up-front so the whole run is a pure function of
	// the seed regardless of which oracle short-circuits first.
	prefixL := 1 + rng.IntN(w.Records-1)            // page-1 size for the prefix/tail oracles
	tailL := 1 + rng.IntN(w.Records-1)              // page-1 size for the tail-delete oracle
	prefixDel := rng.IntN(prefixL)                  // index (in model order) of the already-scanned pk to delete
	tailDel := tailL + rng.IntN(w.Records-tailL)    // index of the not-yet-scanned pk to delete
	faultL := 1 + rng.IntN(w.Records-1)             // page-1 size for the fault oracle
	faultDel := faultL + rng.IntN(w.Records-faultL) // index of the not-yet-scanned pk deleted under fault

	ctx := context.Background()

	// ---- Oracle 1: pagination equivalence vs the independent model ----
	_, db1, sub, md, index, err := w.setup(seed, recs)
	if err != nil {
		rep.Err = fmt.Sprintf("setup: %v", err)
		return res
	}
	for _, sc := range scanCases(md, index) {
		want := model[sc.key]
		for _, limit := range pageLimits(len(want)) {
			got, pages, err := w.drainWithLimit(ctx, db1, md, sub, sc, nil, len(want), limit)
			res.Pages += pages
			if err != nil {
				rep.Err = fmt.Sprintf("%s limit=%d: %v", sc.key, limit, err)
				return res
			}
			if d := diff(want, got); d != "" {
				rep.Violations = append(rep.Violations, fmt.Sprintf(
					"pagination equivalence: %s limit=%d diverged from the model — %s", sc.key, limit, d,
				))
				return res
			}
		}
	}
	rep.Fingerprint = hunt.Fingerprint(db1)

	// ---- Oracle 2: prefix-delete invariance (record forward) ----
	if v, pages, err := w.prefixDeleteInvariance(ctx, seed, recs, model["record-fwd"], prefixL, prefixDel); err != nil {
		rep.Err = err.Error()
		return res
	} else {
		res.Pages += pages
		if v != "" {
			rep.Violations = append(rep.Violations, v)
			return res
		}
	}

	// ---- Oracle 3: tail-delete reflected (record forward) ----
	if v, pages, err := w.tailDeleteReflected(ctx, seed, recs, model["record-fwd"], tailL, tailDel); err != nil {
		rep.Err = err.Error()
		return res
	} else {
		res.Pages += pages
		if v != "" {
			rep.Violations = append(rep.Violations, v)
			return res
		}
	}

	// ---- Oracle 4: tail-delete under an INJECTED between-page fault (record forward) ----
	// The between-page delete commits through a targeted fault: not_committed(1020) rolls it back
	// (tail unchanged), and commit_unknown(1021) does one or the other depending on which of its two
	// real branches was injected. Pairs the continuation resume with true-rollback fault injection
	// across the seeded sweep.
	for _, inject := range []int{1020, simfdb.CommitUnknownApplied, simfdb.CommitUnknownDiscarded} {
		v, pages, err := w.tailDeleteUnderFault(ctx, seed, recs, model["record-fwd"], faultL, faultDel, inject)
		if err != nil {
			rep.Err = err.Error()
			return res
		}
		res.Pages += pages
		if v != "" {
			rep.Violations = append(rep.Violations, v)
			return res
		}
	}

	rep.Ops = res.Pages
	return res
}

// prefixDeleteInvariance mints a continuation after a page of prefixL records, deletes an
// already-scanned primary key (recordFwd[delIdx], delIdx < prefixL) in a committed transaction, and
// asserts the resumed tail equals recordFwd[prefixL:]. A change to the scanned prefix must not
// perturb the resume.
func (w Workload) prefixDeleteInvariance(ctx context.Context, seed uint64, recs []rec, recordFwd []string, prefixL, delIdx int) (string, int, error) {
	_, db, sub, md, _, err := w.setup(seed, recs)
	if err != nil {
		return "", 0, fmt.Errorf("prefix-delete setup: %w", err)
	}
	sc := recordScan(false)
	page1, cont, err := w.scanPage(ctx, db, md, sub, sc, nil, prefixL)
	if err != nil {
		return "", 1, fmt.Errorf("prefix-delete page1: %w", err)
	}
	if d := diff(recordFwd[:prefixL], page1); d != "" {
		return fmt.Sprintf("prefix-delete: page1 (limit=%d) diverged from model — %s", prefixL, d), 1, nil
	}
	delPK := pkFromRow(recordFwd[delIdx])
	if err := deleteRecord(ctx, db, md, sub, delPK); err != nil {
		return "", 1, fmt.Errorf("prefix-delete delete pk=%d: %w", delPK, err)
	}
	tail, pages, err := w.drainWithLimit(ctx, db, md, sub, sc, cont, len(recordFwd), 3)
	if err != nil {
		return "", 1 + pages, fmt.Errorf("prefix-delete resume: %w", err)
	}
	if d := diff(recordFwd[prefixL:], tail); d != "" {
		return fmt.Sprintf("prefix-delete invariance: deleting already-scanned pk=%d (idx %d) perturbed the tail — %s",
			delPK, delIdx, d), 1 + pages, nil
	}
	return "", 1 + pages, nil
}

// tailDeleteReflected mints a continuation after a page of tailL records, deletes a not-yet-scanned
// primary key (recordFwd[delIdx], delIdx >= tailL), resumes, and asserts the tail equals
// recordFwd[tailL:] with exactly that key removed.
func (w Workload) tailDeleteReflected(ctx context.Context, seed uint64, recs []rec, recordFwd []string, tailL, delIdx int) (string, int, error) {
	_, db, sub, md, _, err := w.setup(seed, recs)
	if err != nil {
		return "", 0, fmt.Errorf("tail-delete setup: %w", err)
	}
	sc := recordScan(false)
	_, cont, err := w.scanPage(ctx, db, md, sub, sc, nil, tailL)
	if err != nil {
		return "", 1, fmt.Errorf("tail-delete page1: %w", err)
	}
	delPK := pkFromRow(recordFwd[delIdx])
	if err := deleteRecord(ctx, db, md, sub, delPK); err != nil {
		return "", 1, fmt.Errorf("tail-delete delete pk=%d: %w", delPK, err)
	}
	tail, pages, err := w.drainWithLimit(ctx, db, md, sub, sc, cont, len(recordFwd), 3)
	if err != nil {
		return "", 1 + pages, fmt.Errorf("tail-delete resume: %w", err)
	}
	want := removeRow(recordFwd[tailL:], recordFwd[delIdx])
	if d := diff(want, tail); d != "" {
		return fmt.Sprintf("tail-delete reflected: deleting un-scanned pk=%d (idx %d) not correctly reflected in the resume — %s",
			delPK, delIdx, d), 1 + pages, nil
	}
	return "", 1 + pages, nil
}

// tailDeleteUnderFault mints a continuation after a page of tailL records, then deletes a not-yet-
// scanned key (recordFwd[delIdx], delIdx >= tailL) in a RAW single-commit transaction that an injected
// fault hits, and resumes. The tail must reflect the fault's TRUE outcome: not_committed(1020) rolls
// the delete back so the tail is unchanged; commit_unknown(1021) leaves it applied so the tail loses
// that key. The raw transaction is required — db.Run would retry the retryable fault away.
func (w Workload) tailDeleteUnderFault(ctx context.Context, seed uint64, recs []rec, recordFwd []string, tailL, delIdx, inject int) (string, int, error) {
	sim, db, sub, md, _, err := w.setup(seed, recs)
	if err != nil {
		return "", 0, fmt.Errorf("tail-fault setup: %w", err)
	}
	sc := recordScan(false)
	_, cont, err := w.scanPage(ctx, db, md, sub, sc, nil, tailL)
	if err != nil {
		return "", 1, fmt.Errorf("tail-fault page1: %w", err)
	}
	delPK := pkFromRow(recordFwd[delIdx])
	applied, err := deleteUnderFault(ctx, db, sim, md, sub, delPK, inject)
	if err != nil {
		return "", 1, fmt.Errorf("tail-fault delete pk=%d inject=%d: %w", delPK, inject, err)
	}
	tail, pages, err := w.drainWithLimit(ctx, db, md, sub, sc, cont, len(recordFwd), 3)
	if err != nil {
		return "", 1 + pages, fmt.Errorf("tail-fault resume: %w", err)
	}
	want := recordFwd[tailL:]
	if applied {
		want = removeRow(recordFwd[tailL:], recordFwd[delIdx])
	}
	if d := diff(want, tail); d != "" {
		return fmt.Sprintf("tail-delete under injected fault %d (applied=%v): resumed tail wrong for pk=%d (idx %d) — %s",
			inject, applied, delPK, delIdx, d), 1 + pages, nil
	}
	return "", 1 + pages, nil
}

// deleteUnderFault deletes pk in a RAW single-commit transaction with an injected fault, returning
// whether the delete APPLIED.
//
// The fault is injected by NAMED branch, not by bare error code: commit_unknown_result has two real
// outcomes (durable-but-unacknowledged, and never-reached-the-proxy) and the oracle has to know
// which one it asked for in order to predict the tail. not_committed(1020) and
// transaction_too_old(1007) roll back before apply and have only one outcome. The code that
// surfaces must be the one the injected branch maps to.
func deleteUnderFault(ctx context.Context, db *recordlayer.FDBDatabase, sim *simfdb.SimDB, md *recordlayer.RecordMetaData, sub subspace.Subspace, pk int64, inject int) (bool, error) {
	tx, err := db.CreateWritableTransaction()
	if err != nil {
		return false, err
	}
	store, err := openStore(recordlayer.NewFDBRecordContext(tx, db.Env()), md, sub)
	if err != nil {
		return false, err
	}
	if _, err := store.DeleteRecord(tuple.Tuple{pk}); err != nil {
		return false, err
	}
	sim.InjectOnce(inject)
	wantCode, applied := faultSurface(inject)
	var fe fdb.Error
	if cerr := tx.Commit().Get(); !errors.As(cerr, &fe) || fe.Code != wantCode {
		return false, fmt.Errorf("commit = %v, want code %d", cerr, wantCode)
	}
	return applied, nil
}

// faultSurface maps an injection request to the error code the caller sees and whether the
// mutations landed.
func faultSurface(inject int) (code int, applied bool) {
	switch inject {
	case simfdb.CommitUnknownApplied:
		return 1021, true
	case simfdb.CommitUnknownDiscarded:
		return 1021, false
	default:
		return inject, false
	}
}

// ---- scan plumbing --------------------------------------------------------------------------

// scanCase names one scan target + direction and carries the closures to page it.
type scanCase struct {
	key     string
	reverse bool
	index   *recordlayer.Index // nil ⇒ record scan
}

func recordScan(reverse bool) scanCase {
	key := "record-fwd"
	if reverse {
		key = "record-rev"
	}
	return scanCase{key: key, reverse: reverse}
}

func indexScan(index *recordlayer.Index, reverse bool) scanCase {
	key := "index-fwd"
	if reverse {
		key = "index-rev"
	}
	return scanCase{key: key, reverse: reverse, index: index}
}

func scanCases(md *recordlayer.RecordMetaData, index *recordlayer.Index) []scanCase {
	return []scanCase{recordScan(false), recordScan(true), indexScan(index, false), indexScan(index, true)}
}

// scanPage scans one page of sc from continuation cont with the given row limit, in a fresh
// transaction, and returns the page's rows (in model row format) and the next continuation.
func (w Workload) scanPage(ctx context.Context, db *recordlayer.FDBDatabase, md *recordlayer.RecordMetaData, sub subspace.Subspace, sc scanCase, cont []byte, limit int) (rows []string, next []byte, err error) {
	_, e := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := openStore(rtx, md, sub)
		if err != nil {
			return nil, err
		}
		props := recordlayer.ScanProperties{
			ExecuteProperties:   recordlayer.DefaultExecuteProperties().WithReturnedRowLimit(limit),
			CursorStreamingMode: recordlayer.StreamingModeIterator,
			Reverse:             sc.reverse,
		}
		if sc.index == nil {
			cur := store.ScanRecords(cont, props)
			list, c, err := recordlayer.AsListWithContinuation(ctx, cur)
			if err != nil {
				return nil, err
			}
			for _, r := range list {
				rows = append(rows, fmt.Sprintf("%d", r.Record.(*gen.Order).GetOrderId()))
			}
			next = c
		} else {
			cur := store.ScanIndex(sc.index, recordlayer.TupleRangeAll, cont, props)
			list, c, err := recordlayer.AsListWithContinuation(ctx, cur)
			if err != nil {
				return nil, err
			}
			for _, e := range list {
				rows = append(rows, fmt.Sprintf("%d:%d", e.IndexValues()[0].(int64), e.PrimaryKey()[0].(int64)))
			}
			next = c
		}
		return nil, nil
	})
	return rows, next, e
}

// drainWithLimit resumes sc from continuation start until exhausted at the given page limit,
// resuming each page in a fresh transaction, and returns the full ordered row list. expected bounds
// the page count so a non-advancing continuation is caught rather than looping forever.
func (w Workload) drainWithLimit(ctx context.Context, db *recordlayer.FDBDatabase, md *recordlayer.RecordMetaData, sub subspace.Subspace, sc scanCase, start []byte, expected, limit int) (rows []string, pages int, err error) {
	cont := start
	maxPages := 4*(expected+1) + 16
	for {
		page, next, err := w.scanPage(ctx, db, md, sub, sc, cont, limit)
		pages++
		if err != nil {
			return rows, pages, err
		}
		rows = append(rows, page...)
		if len(next) == 0 {
			return rows, pages, nil
		}
		cont = next
		if pages > maxPages {
			return rows, pages, fmt.Errorf("continuation did not terminate: %d pages, %d rows (expected ~%d) — non-advancing token", pages, len(rows), expected)
		}
	}
}

// paginate's page-size sweep: 1 (max token churn) up through the full length.
func pageLimits(n int) []int {
	base := []int{1, 2, 3, 5, 8, 13}
	var out []int
	for _, l := range base {
		if l <= n {
			out = append(out, l)
		}
	}
	if n > 0 {
		out = append(out, n) // one shot
	}
	if len(out) == 0 {
		out = []int{1}
	}
	return out
}

// ---- setup + model --------------------------------------------------------------------------

func (w Workload) seedData(rng *rand.Rand) []rec {
	used := make(map[int64]bool, w.Records)
	recs := make([]rec, 0, w.Records)
	pd := w.priceDomain()
	for len(recs) < w.Records {
		pk := rng.Int64N(w.PKDomain)
		if used[pk] {
			continue
		}
		used[pk] = true
		recs = append(recs, rec{pk: pk, price: rng.Int64N(pd)})
	}
	return recs
}

// setup builds a fresh SimFDB-backed store and seeds recs. Returns the db, subspace, metadata, and
// the price index. Fault-free env (the continuation/version-bump is the fault under test).
func (w Workload) setup(seed uint64, recs []rec) (*simfdb.SimDB, *recordlayer.FDBDatabase, subspace.Subspace, *recordlayer.RecordMetaData, *recordlayer.Index, error) {
	env := hunt.NewSimEnv(seed, 0)
	backend := simfdb.New(env)
	db := recordlayer.NewFDBDatabaseWithBackend(backend).SetEnv(env)
	sub := subspace.FromBytes(tuple.Tuple{"cont"}.Pack())
	md, index := buildMetadata()
	ctx := context.Background()
	_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := openStore(rtx, md, sub)
		if err != nil {
			return nil, err
		}
		for _, r := range recs {
			if _, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(r.pk), Price: proto.Int32(int32(r.price))}); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	if err != nil {
		return backend, nil, sub, md, index, err
	}
	return backend, db, sub, md, index, nil
}

func buildMetadata() (*recordlayer.RecordMetaData, *recordlayer.Index) {
	b := recordlayer.NewRecordMetaDataBuilder()
	b.SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	b.AddIndex("Order", recordlayer.NewIndex("cont_price", recordlayer.Field("price")))
	md, err := b.Build()
	if err != nil {
		panic("continuation: metadata build: " + err.Error())
	}
	return md, md.GetIndex("cont_price")
}

func openStore(rtx *recordlayer.FDBRecordContext, md *recordlayer.RecordMetaData, sub subspace.Subspace) (*recordlayer.FDBRecordStore, error) {
	return recordlayer.NewStoreBuilder().
		SetContext(rtx).
		SetMetaDataProvider(md).
		SetSubspace(sub).
		CreateOrOpen()
}

func deleteRecord(ctx context.Context, db *recordlayer.FDBDatabase, md *recordlayer.RecordMetaData, sub subspace.Subspace, pk int64) error {
	_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := openStore(rtx, md, sub)
		if err != nil {
			return nil, err
		}
		_, err = store.DeleteRecord(tuple.Tuple{pk})
		return nil, err
	})
	return err
}

// buildModel computes the independent reference orders from the seeded set: record scans in
// primary-key order, the index in (price, primary-key) order, each with its reverse.
func buildModel(recs []rec) map[string][]string {
	byPK := append([]rec(nil), recs...)
	sort.Slice(byPK, func(i, j int) bool { return byPK[i].pk < byPK[j].pk })
	recordFwd := make([]string, len(byPK))
	for i, r := range byPK {
		recordFwd[i] = fmt.Sprintf("%d", r.pk)
	}

	byIdx := append([]rec(nil), recs...)
	sort.Slice(byIdx, func(i, j int) bool {
		if byIdx[i].price != byIdx[j].price {
			return byIdx[i].price < byIdx[j].price
		}
		return byIdx[i].pk < byIdx[j].pk
	})
	indexFwd := make([]string, len(byIdx))
	for i, r := range byIdx {
		indexFwd[i] = fmt.Sprintf("%d:%d", r.price, r.pk)
	}

	return map[string][]string{
		"record-fwd": recordFwd,
		"record-rev": reversed(recordFwd),
		"index-fwd":  indexFwd,
		"index-rev":  reversed(indexFwd),
	}
}

// ---- small helpers --------------------------------------------------------------------------

func reversed(s []string) []string {
	out := make([]string, len(s))
	for i, v := range s {
		out[len(s)-1-i] = v
	}
	return out
}

// diff returns "" when want == got, else a short description of the first divergence (or a
// length/multiset mismatch). Ordered comparison — scan order is part of the contract.
func diff(want, got []string) string {
	if len(want) != len(got) {
		return fmt.Sprintf("length %d != %d (want %v, got %v)", len(want), len(got), cap10(want), cap10(got))
	}
	for i := range want {
		if want[i] != got[i] {
			return fmt.Sprintf("at index %d: want %q, got %q", i, want[i], got[i])
		}
	}
	return ""
}

func cap10(s []string) []string {
	if len(s) <= 10 {
		return s
	}
	return append(append([]string(nil), s[:10]...), "...")
}

// pkFromRow extracts the primary key from a record-scan row ("%d").
func pkFromRow(row string) int64 {
	var pk int64
	_, _ = fmt.Sscanf(row, "%d", &pk)
	return pk
}

// removeRow returns rows with the first occurrence of target removed.
func removeRow(rows []string, target string) []string {
	out := make([]string, 0, len(rows))
	removed := false
	for _, r := range rows {
		if !removed && r == target {
			removed = true
			continue
		}
		out = append(out, r)
	}
	return out
}

// Profiles is the continuation sweep matrix: a balanced set, a dense keyspace (adjacent PKs + many
// price ties — maximal boundary pressure), and a sparse one (big gaps between keys). cmd/dst-hunt
// appends these to its registry.
func Profiles() []hunt.Profile {
	return []hunt.Profile{
		{Name: "continuation", Cfg: hunt.Config{Workload: Workload{Records: 40, PKDomain: 200}}},
		{Name: "continuation-dense", Cfg: hunt.Config{Workload: Workload{Records: 60, PKDomain: 80}}},
		{Name: "continuation-sparse", Cfg: hunt.Config{Workload: Workload{Records: 25, PKDomain: 1000}}},
	}
}
