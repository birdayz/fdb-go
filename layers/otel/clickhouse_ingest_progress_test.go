package otel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type chProgressCancelBody struct {
	io.ReadCloser
	eof            atomic.Bool
	once           sync.Once
	cancel         context.CancelFunc
	closedAfterEOF *atomic.Bool
}

func (b *chProgressCancelBody) Read(wire []byte) (int, error) {
	n, err := b.ReadCloser.Read(wire)
	if err == io.EOF {
		b.eof.Store(true)
	}
	return n, err
}

func (b *chProgressCancelBody) Close() error {
	err := b.ReadCloser.Close()
	if b.eof.Load() {
		b.once.Do(func() { b.closedAfterEOF.Store(true); b.cancel() })
	}
	return err
}

type chProgressCancelTransport struct {
	next           http.RoundTripper
	sourceSQL      string
	responses      atomic.Int64
	closedAfterEOF atomic.Bool
	cancel         context.CancelFunc
}

func (t *chProgressCancelTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.next.RoundTrip(request)
	if err == nil && response != nil && request.URL.Query().Get("query") == t.sourceSQL && response.StatusCode == http.StatusOK && t.responses.Add(1) == 1 {
		response.Body = &chProgressCancelBody{ReadCloser: response.Body, cancel: t.cancel, closedAfterEOF: &t.closedAfterEOF}
	}
	return response, err
}

type chProgressReadErrorBody struct {
	io.ReadCloser
	cause  error
	closed *atomic.Bool
}

func (b *chProgressReadErrorBody) Read([]byte) (int, error) { return 0, b.cause }
func (b *chProgressReadErrorBody) Close() error             { b.closed.Store(true); return b.ReadCloser.Close() }

type chProgressReadErrorTransport struct {
	next      http.RoundTripper
	sourceSQL string
	responses atomic.Int64
	cause     error
	closed    atomic.Bool
}

func (t *chProgressReadErrorTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.next.RoundTrip(request)
	if err == nil && response != nil && request.URL.Query().Get("query") == t.sourceSQL && response.StatusCode == http.StatusOK && t.responses.Add(1) == 2 {
		response.Body = &chProgressReadErrorBody{ReadCloser: response.Body, cause: t.cause, closed: &t.closed}
	}
	return response, err
}

func chProgressPayload(t *testing.T, c Config, first, next int) []byte {
	t.Helper()
	var payload bytes.Buffer
	for ordinal := first; ordinal < next; ordinal++ {
		wire, err := json.Marshal(chRow(Generate(c, ordinal)))
		if err != nil {
			t.Fatal(err)
		}
		payload.Write(wire)
		payload.WriteByte('\n')
	}
	return payload.Bytes()
}

func assertCHProgressDetails(t *testing.T, stats Statistics, c Config) {
	t.Helper()
	const progress = "rows, attributes and logical_payload_bytes count only source INSERT batches with successful HTTP responses; a failed or canceled batch may have applied additional physical rows; ready publication is not inferred"
	if len(stats.Details) != 4 || stats.Details["progress"] != progress || stats.Details["ingest"] == "" || stats.Details["payload_bytes"] == "" || stats.Details["ch_index_granularity"] != strconv.Itoa(c.CHIndexGranularity) {
		t.Fatalf("missing/dishonest CH acknowledgment progress labels: %+v", stats.Details)
	}
}

func TestCHPublicIngestAcknowledgedProgress(t *testing.T) {
	t.Parallel()
	for _, layout := range []struct {
		name  string
		attrs bool
		order string
	}{{"map", false, "trace"}, {"attrs_trace", true, "trace"}, {"attrs_time", true, "time"}} {
		for _, diagnostics := range []bool{false, true} {
			for _, tc := range []struct {
				name                                           string
				ackRows, ackAttrs, physicalRows, physicalAttrs int
				sourceCalls, ackBatches, readyAttempts         int
			}{
				{"second_batch_error", 2, 8, 2, 8, 2, 1, 0},
				{"cancel_after_first_ack", 2, 8, 2, 8, 1, 1, 0},
				{"first_batch_error", 0, 0, 0, 0, 1, 0, 0},
				{"failed_second_batch_applied_one_row", 2, 8, 3, 12, 2, 1, 0},
				{"second_response_body_error", 2, 8, 3, 12, 2, 1, 0},
				{"admission_after_two_acks", 2, 8, 2, 8, 2, 2, 0},
				{"ready_publication_error", 5, 20, 5, 20, 3, 3, 1},
				{"success", 5, 20, 5, 20, 3, 3, 1},
			} {
				t.Run(fmt.Sprintf("%s/diagnostics_%t/%s", layout.name, diagnostics, tc.name), func(t *testing.T) {
					t.Parallel()
					c := testConfig()
					c.Run, c.Count, c.Tenants, c.Attributes, c.Batch, c.CHBatch = "ack_progress_fixture", 5, 1, 4, 2, 2
					c.Order, c.Diagnostics = layout.order, diagnostics
					if tc.name == "admission_after_two_acks" {
						c.Batch, c.CHBatch, c.MatchEvery = 1, 1, 3
						first := chProgressPayload(t, c, 0, 1)
						second := chProgressPayload(t, c, 1, 2)
						third := chProgressPayload(t, c, 2, 3)
						c.CHBatchBytes = len(first)
						if len(second) != len(first) || len(first) > c.CHBatchBytes || len(second) > c.CHBatchBytes || len(third) <= c.CHBatchBytes {
							t.Fatalf("admission fixture does not cross actual row+newline boundary: row sizes=%d/%d/%d cap=%d", len(first), len(second), len(third), c.CHBatchBytes)
						}
					}
					var mu sync.Mutex
					created := false
					var createCalls, sourceCalls, readyAttempts, ownerReads, otherCalls int
					var persistedOwner Manifest
					var ownerAttempts, ownerACKs []string
					var sourcePayloads, acknowledgedPayloads [][]byte
					var physical []Span
					var b *CHBackend
					sourceSQL := "INSERT INTO otel_" + c.Run + ".spans SETTINGS async_insert=0 FORMAT JSONEachRow"
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
						mu.Lock()
						defer mu.Unlock()
						sql := request.URL.Query().Get("query")
						body, err := io.ReadAll(request.Body)
						if err != nil {
							t.Error(err)
							http.Error(w, "body read failed", http.StatusBadRequest)
							return
						}
						if request.Method != http.MethodPost {
							t.Errorf("CH ingest/read method=%s", request.Method)
						}
						switch {
						case sql == "CREATE DATABASE "+b.database:
							createCalls++
							if created {
								http.Error(w, "existing database must not be adopted", http.StatusConflict)
								return
							}
							created = true
						case strings.HasPrefix(sql, "CREATE TABLE "+b.database+".owner "), strings.HasPrefix(sql, "CREATE TABLE "+b.database+".spans "), strings.HasPrefix(sql, "CREATE TABLE "+b.database+".attr_lookup "), strings.HasPrefix(sql, "CREATE MATERIALIZED VIEW "+b.database+".attr_mv "):
						case sql == "INSERT INTO "+b.database+".owner FORMAT JSONEachRow":
							var row struct {
								Sequence int    `json:"sequence"`
								Payload  string `json:"payload"`
							}
							if err := json.Unmarshal(body, &row); err != nil {
								t.Error(err)
								http.Error(w, "bad manifest row", http.StatusBadRequest)
								return
							}
							var owner Manifest
							if err := json.Unmarshal([]byte(row.Payload), &owner); err != nil {
								t.Error(err)
								http.Error(w, "bad manifest", http.StatusBadRequest)
								return
							}
							ownerAttempts = append(ownerAttempts, owner.State)
							if owner.State == "ready" {
								readyAttempts++
								if tc.name == "ready_publication_error" {
									http.Error(w, "ready publication rejected", http.StatusServiceUnavailable)
									return
								}
								if row.Sequence != 2 || sourceCalls != (c.Count+c.CHBatch-1)/c.CHBatch {
									t.Errorf("premature/wrong ready publication: sequence=%d batches=%d", row.Sequence, sourceCalls)
								}
							} else if owner.State != "loading" || row.Sequence != 1 {
								t.Errorf("unexpected initial owner state/sequence: %+v/%d", owner, row.Sequence)
							}
							persistedOwner = owner
							ownerACKs = append(ownerACKs, owner.State)
						case sql == sourceSQL:
							sourceCalls++
							sourcePayloads = append(sourcePayloads, append([]byte(nil), body...))
							if persistedOwner.State != "loading" {
								t.Errorf("source insertion without loading owner: %+v", persistedOwner)
							}
							if len(body) == 0 || body[len(body)-1] != '\n' {
								t.Error("source payload missing final JSONEachRow newline")
							}
							var incoming []Span
							for _, line := range bytes.Split(bytes.TrimSpace(body), []byte{'\n'}) {
								var row struct {
									Ordinal int    `json:"ordinal"`
									Source  string `json:"source"`
								}
								if err := json.Unmarshal(line, &row); err != nil {
									t.Error(err)
									http.Error(w, "bad source row", http.StatusBadRequest)
									return
								}
								var source Span
								if err := json.Unmarshal([]byte(row.Source), &source); err != nil {
									t.Error(err)
									http.Error(w, "bad source", http.StatusBadRequest)
									return
								}
								if source.Ordinal != row.Ordinal || !equalSpan(source, Generate(b.cfg, row.Ordinal)) {
									t.Errorf("transport/source ordinal mismatch: %d %+v", row.Ordinal, source)
								}
								incoming = append(incoming, source)
							}
							if tc.name == "first_batch_error" && sourceCalls == 1 {
								http.Error(w, "first source batch rejected", http.StatusInternalServerError)
								return
							}
							if tc.name == "second_batch_error" && sourceCalls == 2 {
								http.Error(w, "second source batch rejected", http.StatusInternalServerError)
								return
							}
							if (tc.name == "failed_second_batch_applied_one_row" || tc.name == "second_response_body_error") && sourceCalls == 2 {
								if len(incoming) != 2 {
									t.Errorf("partial failed batch population=%d, want 2", len(incoming))
								} else {
									physical = append(physical, incoming[0])
								}
								if tc.name == "second_response_body_error" {
									fmt.Fprint(w, "HTTP200 but response body will fail before completion\n")
								} else {
									http.Error(w, "one of two rows applied before failed response", http.StatusInternalServerError)
								}
								return
							}
							physical = append(physical, incoming...)
							acknowledgedPayloads = append(acknowledgedPayloads, append([]byte(nil), body...))
							fmt.Fprint(w, "source INSERT acknowledged\n")
						case chIsManifest(sql):
							ownerReads++
							fmt.Fprint(w, chManifestWire(t, persistedOwner))
						default:
							otherCalls++
							t.Errorf("unexpected/adopting CH SQL: %s", sql)
							http.Error(w, "unexpected query", http.StatusBadRequest)
						}
					}))
					t.Cleanup(srv.Close)
					var err error
					b, err = OpenCH(c, srv.URL, layout.attrs)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(b.Close)
					runCtx, cancel := context.WithCancel(t.Context())
					defer cancel()
					var cancelTransport *chProgressCancelTransport
					var readErrorTransport *chProgressReadErrorTransport
					readCause := errors.New("controlled successful-status response body read failure")
					if tc.name == "cancel_after_first_ack" {
						cancelTransport = &chProgressCancelTransport{next: b.client.Transport, sourceSQL: sourceSQL, cancel: cancel}
						b.client.Transport = cancelTransport
					}
					if tc.name == "second_response_body_error" {
						readErrorTransport = &chProgressReadErrorTransport{next: b.client.Transport, sourceSQL: sourceSQL, cause: readCause}
						b.client.Transport = readErrorTransport
					}
					stats, ingestError := b.Ingest(runCtx)
					if tc.name == "success" {
						if ingestError != nil {
							t.Fatal(ingestError)
						}
					} else if ingestError == nil {
						t.Fatal("controlled ingestion failure returned success")
					}
					if tc.name == "cancel_after_first_ack" {
						if !errors.Is(ingestError, context.Canceled) || !cancelTransport.closedAfterEOF.Load() {
							t.Fatalf("cancellation did not follow a fully consumed successful response: error=%v EOF-close=%t", ingestError, cancelTransport.closedAfterEOF.Load())
						}
					}
					if tc.name == "second_response_body_error" && (!errors.Is(ingestError, readCause) || !readErrorTransport.closed.Load()) {
						t.Fatalf("HTTP200 body failure counted as ACK or leaked body/cause: error=%v closed=%t", ingestError, readErrorTransport.closed.Load())
					}
					if tc.name == "admission_after_two_acks" {
						var admission *AdmissionError
						if !errors.As(ingestError, &admission) {
							t.Fatalf("oversized third row lost typed admission failure: %v", ingestError)
						}
					}
					assertCHProgressDetails(t, stats, b.cfg)
					mu.Lock()
					var acknowledgedBytes int64
					for _, body := range acknowledgedPayloads {
						acknowledgedBytes += int64(len(body))
					}
					var physicalAttrs int
					for _, source := range physical {
						physicalAttrs += len(source.Attributes)
					}
					if sourceCalls != tc.sourceCalls || len(acknowledgedPayloads) != tc.ackBatches || readyAttempts != tc.readyAttempts || len(physical) != tc.physicalRows || physicalAttrs != tc.physicalAttrs {
						t.Errorf("actual source population calls=%d ACK-batches=%d ready-attempts=%d physical=%d attrs=%d, want %+v", sourceCalls, len(acknowledgedPayloads), readyAttempts, len(physical), physicalAttrs, tc)
					}
					for i, payload := range sourcePayloads {
						first, next := c.CHBatch*i, min(c.CHBatch*i+c.CHBatch, 5)
						if first >= next || !bytes.Equal(payload, chProgressPayload(t, b.cfg, first, next)) {
							t.Errorf("source INSERT %d retried/changed/duplicated its literal batch", i)
						}
					}
					wantState := "loading"
					wantOwnerACKs := []string{"loading"}
					if tc.name == "success" {
						wantState, wantOwnerACKs = "ready", []string{"loading", "ready"}
					}
					wantOwnerAttempts := []string{"loading"}
					if tc.readyAttempts != 0 {
						wantOwnerAttempts = []string{"loading", "ready"}
					}
					if persistedOwner != manifest(b.cfg, wantState) || !reflect.DeepEqual(ownerAttempts, wantOwnerAttempts) || !reflect.DeepEqual(ownerACKs, wantOwnerACKs) || createCalls != 1 || otherCalls != 0 {
						t.Errorf("owner/admission lifecycle owner=%+v attempts=%v ACKs=%v creates=%d other=%d", persistedOwner, ownerAttempts, ownerACKs, createCalls, otherCalls)
					}
					physicalBefore := append([]Span(nil), physical...)
					ownerBefore, sourcesBefore, ownerWritesBefore := persistedOwner, sourceCalls, len(ownerAttempts)
					mu.Unlock()
					wantBytes := int64(len(chProgressPayload(t, b.cfg, 0, tc.ackRows)))
					if stats.Rows != int64(tc.ackRows) || stats.Attributes != int64(tc.ackAttrs) || stats.LogicalPayloadBytes != acknowledgedBytes || stats.LogicalPayloadBytes != wantBytes {
						t.Fatalf("progress must count successful source responses only: stats=%+v ACK-request-bytes=%d wantRows=%d attrs=%d bytes=%d", stats, acknowledgedBytes, tc.ackRows, tc.ackAttrs, wantBytes)
					}
					if (tc.name == "failed_second_batch_applied_one_row" || tc.name == "second_response_body_error") && (stats.Rows != 2 || stats.Attributes != 8 || len(physicalBefore) != 3 || physicalAttrs != 12) {
						t.Fatalf("ambiguous partial physical tail was guessed as ACK: stats=%+v physical=%d attrs=%d", stats, len(physicalBefore), physicalAttrs)
					}
					if tc.name != "success" {
						for _, op := range []string{"query", "audit"} {
							var ownership *OwnershipError
							if err := chPublicOperation(t.Context(), b, op); !errors.As(err, &ownership) {
								t.Fatalf("loading run authorized %s after partial/error receipt: %v", op, err)
							}
						}
						mu.Lock()
						if ownerReads != 2 || sourceCalls != sourcesBefore || otherCalls != 0 {
							t.Errorf("loading query/audit did more than owner reads: owners=%d sources=%d other=%d", ownerReads, sourceCalls, otherCalls)
						}
						mu.Unlock()
					}
					// A new live-context Ingest is not recovery/adoption. CREATE
					// DATABASE must fail without touching existing owner or sources.
					retried, retryError := b.Ingest(t.Context())
					if retryError == nil || retried.Rows != 0 || retried.Attributes != 0 || retried.LogicalPayloadBytes != 0 {
						t.Fatalf("reused database was adopted or inherited previous ACK counts: stats=%+v error=%v", retried, retryError)
					}
					assertCHProgressDetails(t, retried, b.cfg)
					mu.Lock()
					if createCalls != 2 || sourceCalls != sourcesBefore || len(ownerAttempts) != ownerWritesBefore || persistedOwner != ownerBefore || !reflect.DeepEqual(physical, physicalBefore) || otherCalls != 0 {
						t.Errorf("rejected retry modified owned/ambiguous load: creates=%d sources=%d/%d ownerWrites=%d/%d owner=%+v physical=%+v other=%d", createCalls, sourceCalls, sourcesBefore, len(ownerAttempts), ownerWritesBefore, persistedOwner, physical, otherCalls)
					}
					mu.Unlock()
				})
			}
		}
	}
}

func TestCHPublicIngestInitializationFailureProgress(t *testing.T) {
	t.Parallel()
	for _, layout := range []struct {
		name  string
		attrs bool
		order string
	}{{"map", false, "trace"}, {"attrs_trace", true, "trace"}, {"attrs_time", true, "time"}} {
		for _, diagnostics := range []bool{false, true} {
			for _, tc := range []struct {
				stage                                           string
				attrsOnly, beforeInitialized                    bool
				loadingAttempts, loadingApplied, retainedTables int
				steps                                           []string
			}{
				{"create_owner", false, true, 0, 0, 0, []string{"create_database", "create_owner", "drop_database"}},
				{"loading_manifest", false, true, 1, 1, 0, []string{"create_database", "create_owner", "loading_manifest", "drop_database"}},
				{"create_spans", false, false, 1, 1, 0, []string{"create_database", "create_owner", "loading_manifest", "create_spans"}},
				{"create_attr_lookup", true, false, 1, 1, 1, []string{"create_database", "create_owner", "loading_manifest", "create_spans", "create_attr_lookup"}},
				{"create_attr_mv", true, false, 1, 1, 2, []string{"create_database", "create_owner", "loading_manifest", "create_spans", "create_attr_lookup", "create_attr_mv"}},
			} {
				if tc.attrsOnly && !layout.attrs {
					continue
				}
				t.Run(fmt.Sprintf("%s/diagnostics_%t/%s", layout.name, diagnostics, tc.stage), func(t *testing.T) {
					t.Parallel()
					c := testConfig()
					c.Run, c.Count, c.Tenants, c.Attributes, c.CHBatch = "init_progress_fixture", 5, 1, 4, 2
					c.Order, c.Diagnostics = layout.order, diagnostics
					var mu sync.Mutex
					var steps []string
					var created, ownerTable bool
					var owner Manifest
					var failures, drops, sourceInserts, loadingAttempts, loadingApplied, loadingACKs, ownerReads int
					tables := map[string]bool{}
					marker := "controlled initialization failure at " + tc.stage
					var b *CHBackend
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
						mu.Lock()
						defer mu.Unlock()
						sql := request.URL.Query().Get("query")
						body, err := io.ReadAll(request.Body)
						if err != nil {
							t.Error(err)
							http.Error(w, "body read failed", http.StatusBadRequest)
							return
						}
						if request.Method != http.MethodPost {
							t.Errorf("initialization/read method=%s", request.Method)
						}
						step := "unexpected"
						switch {
						case sql == "CREATE DATABASE "+b.database:
							step = "create_database"
						case strings.HasPrefix(sql, "CREATE TABLE "+b.database+".owner "):
							step = "create_owner"
						case sql == "INSERT INTO "+b.database+".owner FORMAT JSONEachRow":
							step = "loading_manifest"
						case strings.HasPrefix(sql, "CREATE TABLE "+b.database+".spans "):
							step = "create_spans"
						case strings.HasPrefix(sql, "CREATE TABLE "+b.database+".attr_lookup "):
							step = "create_attr_lookup"
						case strings.HasPrefix(sql, "CREATE MATERIALIZED VIEW "+b.database+".attr_mv "):
							step = "create_attr_mv"
						case sql == "DROP DATABASE "+b.database+" SYNC":
							step = "drop_database"
						case chIsManifest(sql):
							step = "owner_read"
						case strings.HasPrefix(sql, "INSERT INTO "+b.database+".spans "):
							step = "source_insert"
						}
						steps = append(steps, step)
						if step == "create_database" {
							if created {
								http.Error(w, "existing owned database cannot be adopted", http.StatusConflict)
								return
							}
							created = true
							return
						}
						if step == "loading_manifest" {
							loadingAttempts++
							var row struct {
								Sequence int    `json:"sequence"`
								Payload  string `json:"payload"`
							}
							var loading Manifest
							if err := json.Unmarshal(body, &row); err != nil {
								t.Error(err)
								http.Error(w, "bad owner row", http.StatusBadRequest)
								return
							}
							if err := json.Unmarshal([]byte(row.Payload), &loading); err != nil {
								t.Error(err)
								http.Error(w, "bad owner payload", http.StatusBadRequest)
								return
							}
							if !ownerTable || row.Sequence != 1 || loading != manifest(b.cfg, "loading") {
								t.Errorf("initial owner protocol table=%t sequence=%d payload=%+v", ownerTable, row.Sequence, loading)
							}
							// Model the ambiguous initialization response: the owner
							// row is applied even when its HTTP acknowledgment fails.
							owner, loadingApplied = loading, loadingApplied+1
						}
						if step == tc.stage {
							failures++
							http.Error(w, marker, http.StatusServiceUnavailable)
							return
						}
						switch step {
						case "create_owner":
							ownerTable = true
						case "loading_manifest":
							loadingACKs++
						case "create_spans", "create_attr_lookup", "create_attr_mv":
							tables[step] = true
						case "drop_database":
							if !created {
								t.Error("cleanup dropped a database this attempt did not create")
							}
							drops++
							created, ownerTable, owner = false, false, Manifest{}
							tables = map[string]bool{}
						case "owner_read":
							ownerReads++
							fmt.Fprint(w, chManifestWire(t, owner))
						case "source_insert":
							sourceInserts++
							t.Error("initialization failure reached a source INSERT")
							http.Error(w, "unexpected source INSERT", http.StatusInternalServerError)
						default:
							t.Errorf("unexpected initialization SQL: %s", sql)
							http.Error(w, "unexpected SQL", http.StatusBadRequest)
						}
					}))
					t.Cleanup(srv.Close)
					var err error
					b, err = OpenCH(c, srv.URL, layout.attrs)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(b.Close)
					stats, ingestError := b.Ingest(t.Context())
					var requestFailure *HTTPError
					if !errors.As(ingestError, &requestFailure) || !strings.Contains(ingestError.Error(), marker) {
						t.Fatalf("wrong initialization failure for %s: %v", tc.stage, ingestError)
					}
					if stats.Rows != 0 || stats.Attributes != 0 || stats.LogicalPayloadBytes != 0 {
						t.Fatalf("initialization failure reported source ACKs: %+v", stats)
					}
					assertCHProgressDetails(t, stats, b.cfg)
					mu.Lock()
					if !reflect.DeepEqual(steps, tc.steps) || failures != 1 || sourceInserts != 0 || ownerReads != 0 || loadingAttempts != tc.loadingAttempts || loadingApplied != tc.loadingApplied {
						t.Errorf("initialization protocol stage=%s actual=%v want=%v failures=%d source=%d reads=%d ownerAttempt/applied=%d/%d", tc.stage, steps, tc.steps, failures, sourceInserts, ownerReads, loadingAttempts, loadingApplied)
					}
					if tc.beforeInitialized {
						if created || ownerTable || owner != (Manifest{}) || drops != 1 || loadingACKs != 0 || len(tables) != 0 {
							t.Errorf("pre-owner failure leaked fresh database/ambiguous owner: created=%t ownerTable=%t owner=%+v drops=%d ACKs=%d tables=%v", created, ownerTable, owner, drops, loadingACKs, tables)
						}
						mu.Unlock()
						return
					}
					if !created || !ownerTable || owner != manifest(b.cfg, "loading") || drops != 0 || loadingACKs != 1 || len(tables) != tc.retainedTables {
						t.Errorf("post-owner failure did not retain exact loading state: created=%t ownerTable=%t owner=%+v drops=%d ACKs=%d tables=%v", created, ownerTable, owner, drops, loadingACKs, tables)
					}
					ownerBefore := owner
					tablesBefore := make(map[string]bool, len(tables))
					for table, exists := range tables {
						tablesBefore[table] = exists
					}
					mu.Unlock()
					for _, op := range []string{"query", "audit"} {
						var ownership *OwnershipError
						if err := chPublicOperation(t.Context(), b, op); !errors.As(err, &ownership) {
							t.Fatalf("owned loading initialization authorized %s: %v", op, err)
						}
					}
					mu.Lock()
					if ownerReads != 2 || len(steps) != len(tc.steps)+2 || sourceInserts != 0 || drops != 0 {
						t.Errorf("loading query/audit bypassed owner-only guard: steps=%v reads=%d source=%d drops=%d", steps, ownerReads, sourceInserts, drops)
					}
					mu.Unlock()
					retried, retryError := b.Ingest(t.Context())
					if retryError == nil || retried.Rows != 0 || retried.Attributes != 0 || retried.LogicalPayloadBytes != 0 {
						t.Fatalf("retained initialization was adopted: stats=%+v error=%v", retried, retryError)
					}
					assertCHProgressDetails(t, retried, b.cfg)
					mu.Lock()
					if len(steps) != len(tc.steps)+3 || steps[len(steps)-1] != "create_database" || !created || owner != ownerBefore || !reflect.DeepEqual(tables, tablesBefore) || loadingAttempts != 1 || loadingACKs != 1 || sourceInserts != 0 || drops != 0 {
						t.Errorf("rejected retry changed retained ownership/schema: steps=%v owner=%+v tables=%v source=%d drops=%d", steps, owner, tables, sourceInserts, drops)
					}
					mu.Unlock()
					if err := b.Drop(t.Context()); err != nil {
						t.Fatalf("matching owned loading run was not explicitly droppable: %v", err)
					}
					mu.Lock()
					if created || ownerTable || owner != (Manifest{}) || len(tables) != 0 || drops != 1 || sourceInserts != 0 || ownerReads != 3 {
						t.Errorf("explicit owned Drop failed: created=%t owner=%+v tables=%v drops=%d source=%d ownerReads=%d", created, owner, tables, drops, sourceInserts, ownerReads)
					}
					mu.Unlock()
				})
			}
		}
	}
}
