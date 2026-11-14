package main

import (
	"bytes"
	"context"

	"fdb.dev/layers/otel/otelsql"
	"fdb.dev/layers/otel/otelsql/otelv1pb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
	"google.golang.org/protobuf/proto"
)

const (
	verifyBoundary = "independent Go verification through the pure-Go client for any arm: every source row loaded by the standard serializer and compared as full compiled row and canonical OTLP bytes, an exact full-tenant KV census of baseline, records and all SPANS index entries, a full standard record scan and the layout seals; the query oracle runs only when requested"
	loadBatch      = 256
	censusPage     = 2000
)

type oracleClass struct {
	Expected int `json:"expected"`
	Executed int `json:"executed"`
	Passed   int `json:"passed"`
}

type oracleReport struct {
	Kind            string                 `json:"kind"`
	RequiredQueries int                    `json:"required_queries"`
	ExecutedQueries int                    `json:"executed_queries"`
	PassedQueries   int                    `json:"passed_queries"`
	FailedQueries   int                    `json:"failed_queries"`
	Classes         map[string]oracleClass `json:"classes"`
	ResultsFile     *string                `json:"results_file"`
	ResultsSHA256   *string                `json:"results_sha256"`
	Status          string                 `json:"status"`
}

type verifyReport struct {
	reportCommon
	DatabasePath                        string       `json:"database_path"`
	SourceRecordsExpected               int64        `json:"source_records_expected"`
	SourceRecordsReadStandardSerializer int64        `json:"source_records_read_standard_serializer"`
	SourceRecordsExactMatch             int64        `json:"source_records_exact_match"`
	ScannedDistinctSpans                int64        `json:"scanned_distinct_spans"`
	OTLPBytesEqual                      bool         `json:"otlp_bytes_equal"`
	FullRowsEqual                       bool         `json:"full_rows_equal"`
	LayoutSealsEqual                    bool         `json:"layout_seals_equal"`
	Census                              censusResult `json:"census"`
	DistinctSpans                       int64        `json:"distinct_spans"`
	Oracle                              oracleReport `json:"oracle"`
}

func newVerifyReport(c reportCommon) *verifyReport {
	return &verifyReport{
		reportCommon: c,
		Census:       censusResult{Expected: newCounts(), Actual: newCounts(), Problems: []string{}},
		// A local run may leave the oracle out; that is not_requested, never passed.
		Oracle: oracleReport{Kind: "none", Classes: map[string]oracleClass{}, Status: "not_requested"},
	}
}

// runVerify independently checks an arm's tenant after its ingest exited, through the
// pure-Go client whatever the arm wrote with.
func runVerify(ctx context.Context, rt runtime, j *job, m *layoutManifest, r *verifyReport) error {
	path, _ := tenantPaths(j.Run)
	r.DatabasePath = path
	backend, err := rt.openPure(ctx, j.ClusterFile)
	if err != nil {
		return err
	}
	defer backend.Close()
	db := recordlayer.NewFDBDatabaseWithBackend(backend)
	// Seals first; nothing below commits, so verification never repairs what it checks.
	ts, base, err := openSealed(ctx, backend, j, m, false)
	if err != nil {
		return err
	}
	exp := newJobExpectation(j, ts, base)
	// Pass 1: every source identity, union and index entry, before any stored record is
	// decoded. Assumes the arm's ingest has exited: nothing writes while this runs.
	frames, err := eachSourceBatch(ctx, j, ts, func(rows []*sourceRow) error {
		for _, row := range rows {
			if err := exp.add(row); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	r.SourceRecordsExpected = frames
	// The bounded physical census runs before the standard serializer reads anything, so
	// its compatibility decoder never sees a value this census did not validate.
	c := newCensus(exp)
	if err := pageThrough(ctx, fetchRange(db), prefixRange(ts.Subspace), censusPage, c.add); err != nil {
		return err
	}
	c.finish()
	r.Census = c.result()
	if !r.Census.Matched {
		return fault("verify", "the tenant census differs from the expected population")
	}
	// Pass 2: every row through the standard serializer, rebuilt per batch.
	r.FullRowsEqual, r.OTLPBytesEqual = true, true
	again, err := eachSourceBatch(ctx, j, ts, func(rows []*sourceRow) error {
		return loadRows(ctx, backend, ts, rows, r)
	})
	if err != nil {
		return err
	}
	if again != frames {
		return fault("verify", "the corpus changed between verification passes")
	}
	scanned, err := scanRecords(ctx, backend, ts, exp)
	if err != nil {
		return err
	}
	r.ScannedDistinctSpans, r.DistinctSpans = scanned, scanned
	if err := sealHolds(ctx, db, m); err != nil {
		return err
	}
	r.LayoutSealsEqual = true
	switch {
	case !r.FullRowsEqual || !r.OTLPBytesEqual || r.SourceRecordsExactMatch != r.SourceRecordsExpected:
		return fault("verify", "a source row read through the standard serializer differs from its Go-built row")
	case scanned != r.SourceRecordsExpected:
		return fault("verify", "the standard record scan differs from the source population")
	}
	return nil
}

// eachSourceBatch reads the whole sealed corpus, building each frame's expected row,
// and returns its frame count, which must be expected_frames.
func eachSourceBatch(ctx context.Context, j *job, ts *otelsql.TenantStore, fn func([]*sourceRow) error) (int64, error) {
	in, err := openCorpus(j.CorpusFile, j.RowBudget)
	if err != nil {
		return 0, err
	}
	defer in.close()
	var frames int64
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		wires, err := in.next(loadBatch)
		if err != nil {
			return 0, err
		}
		if len(wires) == 0 {
			break
		}
		rows := make([]*sourceRow, 0, len(wires))
		for _, wire := range wires {
			row, err := buildRow(ts, wire)
			if err != nil {
				return 0, err
			}
			rows = append(rows, row)
		}
		if err := fn(rows); err != nil {
			return 0, err
		}
		frames += int64(len(rows))
	}
	if frames != int64(j.ExpectedFrames) || !in.ended {
		return 0, fault("verify", "the source population differs from expected_frames")
	}
	return frames, nil
}

func sealHolds(ctx context.Context, db *recordlayer.FDBDatabase, m *layoutManifest) error {
	_, err := db.RunRead(ctx, func(tx fdb.ReadTransaction) (any, error) { return nil, checkSeal(tx, *m) })
	return err
}

// loadRows loads each row's primary key through the standard serializer and compares
// the complete compiled row and its canonical OTLP bytes.
func loadRows(ctx context.Context, backend fdb.BackendDatabase, ts *otelsql.TenantStore, rows []*sourceRow, r *verifyReport) error {
	var got []rowOutcome
	err := inStore(ctx, backend, ts, func(store *recordlayer.FDBRecordStore) error {
		pending := make([]recordlayer.PendingRead[*recordlayer.FDBStoredRecord[proto.Message]], len(rows))
		for i, row := range rows {
			pending[i] = store.LoadRecordAsync(row.pk, nil)
		}
		for i, p := range pending {
			rec, err := p.Get()
			if err != nil {
				return err
			}
			got = append(got, compareRow(rec, rows[i]))
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, o := range got {
		if o.read {
			r.SourceRecordsReadStandardSerializer++
		}
		r.FullRowsEqual = r.FullRowsEqual && o.rowEqual
		r.OTLPBytesEqual = r.OTLPBytesEqual && o.otlpEqual
		if o.rowEqual && o.otlpEqual {
			r.SourceRecordsExactMatch++
		}
	}
	return nil
}

type rowOutcome struct{ read, rowEqual, otlpEqual bool }

func compareRow(rec *recordlayer.FDBStoredRecord[proto.Message], want *sourceRow) rowOutcome {
	var o rowOutcome
	if rec == nil {
		return o
	}
	o.read = true
	row, ok := rec.Record.(*otelv1pb.SPANS)
	if !ok || rec.RecordType.Name != "SPANS" || !bytes.Equal(rec.PrimaryKey.Pack(), want.pk.Pack()) {
		return o
	}
	wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(row)
	o.rowEqual = err == nil && bytes.Equal(wire, want.rowWire)
	o.otlpEqual = bytes.Equal(row.GetOTLP(), want.canonical)
	return o
}

// scanRecords scans every record through the standard serializer: exactly each source
// identity once and the sealed layout row once, through a progressing continuation.
func scanRecords(ctx context.Context, backend fdb.BackendDatabase, ts *otelsql.TenantStore, exp *expectation) (int64, error) {
	type scanned struct {
		typ  string
		pk   string
		meta []byte
	}
	seen := map[string]bool{}
	baseSeen := false
	basePK := string(exp.base.layoutPK.Pack())
	layout, err := proto.MarshalOptions{Deterministic: true}.Marshal(exp.base.layout)
	if err != nil {
		return 0, err
	}
	var continuation []byte
	for {
		var recs []scanned
		var next []byte
		exhausted := false
		err := inStore(ctx, backend, ts, func(store *recordlayer.FDBRecordStore) error {
			scan := recordlayer.ForwardScan()
			scan.ExecuteProperties = scan.ExecuteProperties.WithReturnedRowLimit(500).WithScannedBytesLimit(4 << 20)
			cursor := store.ScanRecords(continuation, scan)
			defer func() { _ = cursor.Close() }()
			for {
				result, err := cursor.OnNext(ctx)
				if err != nil {
					return err
				}
				if !result.HasNext() {
					exhausted = result.GetNoNextReason() == recordlayer.SourceExhausted
					next, err = result.GetContinuation().ToBytes()
					return err
				}
				rec := result.GetValue()
				s := scanned{typ: rec.RecordType.Name, pk: string(rec.PrimaryKey.Pack())}
				if meta, ok := rec.Record.(*otelv1pb.STORE_META); ok {
					if s.meta, err = (proto.MarshalOptions{Deterministic: true}).Marshal(meta); err != nil {
						return err
					}
				}
				recs = append(recs, s)
			}
		})
		if err != nil {
			return 0, err
		}
		for _, s := range recs {
			switch {
			case s.typ == "SPANS" && exp.spans[s.pk] != nil && !seen[s.pk]:
				seen[s.pk] = true
			case s.typ == "STORE_META" && s.pk == basePK && !baseSeen && bytes.Equal(s.meta, layout):
				baseSeen = true
			default:
				return 0, fault("verify", "the record scan found an extra, duplicate or substituted record")
			}
		}
		if exhausted {
			if len(seen) != len(exp.spans) || !baseSeen {
				return 0, fault("verify", "the record scan missed a source identity or the layout row")
			}
			return int64(len(seen)), nil
		}
		if len(next) == 0 || bytes.Equal(next, continuation) {
			return 0, fault("verify", "the record scan stopped without a progressing continuation")
		}
		continuation = next
	}
}
