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

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"google.golang.org/protobuf/proto"
)

func TestCHIndexGranularityConfigBounds(t *testing.T) {
	t.Parallel()
	if got := DefaultConfig().CHIndexGranularity; got != 8192 {
		t.Fatalf("default CH index granularity=%d, want 8192", got)
	}
	for _, value := range []int{0, 8193} {
		c := testConfig()
		c.CHIndexGranularity = value
		var invalid *ConfigError
		if err := c.Validate(); !errors.As(err, &invalid) {
			t.Fatalf("granularity %d accepted/wrong error: %v", value, err)
		}
	}
	for _, value := range []int{1, 64, 512, 8192} {
		c := testConfig()
		c.CHIndexGranularity = value
		if err := c.Validate(); err != nil {
			t.Fatalf("valid granularity %d: %v", value, err)
		}
	}
}

func TestCHGranularityDoesNotChangeFDBOrSourceBytes(t *testing.T) {
	t.Parallel()
	baseline := testConfig()
	baseline.Run, baseline.Backend, baseline.Count = "granularity_fdb_fixture", "fdb", 7
	baseline.CHIndexGranularity = 8192
	baseIdentity := corpusIdentity(baseline)
	baseManifest := manifest(baseline, "ready")
	baseJSON := make([][]byte, baseline.Count)
	baseRecords := make([][]byte, baseline.Count)
	for i := 0; i < baseline.Count; i++ {
		span := Generate(baseline, i)
		var err error
		baseJSON[i], err = json.Marshal(span)
		if err != nil {
			t.Fatal(err)
		}
		record, err := toRecord(span)
		if err != nil {
			t.Fatal(err)
		}
		baseRecords[i], err = (proto.MarshalOptions{Deterministic: true}).Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range []int{1, 64, 512, 8192} {
		c := baseline
		c.CHIndexGranularity = value
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
		if corpusIdentity(c) != baseIdentity || !reflect.DeepEqual(manifest(c, "ready"), baseManifest) {
			t.Fatalf("CH granularity %d changed FDB ownership/corpus", value)
		}
		if err := checkManifest(c, baseManifest, true); err != nil {
			t.Fatalf("CH-only setting invalidated FDB ownership: %v", err)
		}
		m := manifest(c, "ready")
		if m.CHIndexGranularity != 0 {
			t.Fatalf("FDB manifest acquired CH layout field: %+v", m)
		}
		// Source identity is checked above; replace only the fingerprint with
		// a literal to pin the legacy FDB manifest's exact JSON field layout.
		m.Corpus = "legacy_corpus"
		wire, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		const legacy = `{"tool":"otelbench-v2","run":"granularity_fdb_fixture","corpus":"legacy_corpus","state":"ready","order":"trace","hash_bytes":32,"backend":"fdb"}`
		if string(wire) != legacy {
			t.Fatalf("legacy FDB manifest JSON changed: got %s want %s", wire, legacy)
		}
		foreign := baseManifest
		foreign.CHIndexGranularity = 64
		if err := checkManifest(c, foreign, true); err != nil {
			t.Fatalf("FDB checker interpreted irrelevant CH field: %v", err)
		}
		for i := 0; i < c.Count; i++ {
			span := Generate(c, i)
			wire, err := json.Marshal(span)
			if err != nil {
				t.Fatal(err)
			}
			record, err := toRecord(span)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := (proto.MarshalOptions{Deterministic: true}).Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(wire, baseJSON[i]) || !bytes.Equal(encoded, baseRecords[i]) {
				t.Fatalf("granularity %d changed source/FDB wire bytes at ordinal %d", value, i)
			}
		}
		for _, backend := range []string{"ch-map", "ch-attrs"} {
			c.Backend = backend
			if corpusIdentity(c) != baseIdentity {
				t.Fatalf("CH layout %s/%d changed source fingerprint", backend, value)
			}
		}
	}
}

func TestCHGranularityManifestOwnership(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{"ch-map", "ch-attrs"} {
		for _, state := range []string{"loading", "ready"} {
			for _, value := range []int{1, 64, 512, 8192} {
				c := testConfig()
				c.Backend, c.CHIndexGranularity = backend, value
				m := manifest(c, state)
				if m.CHIndexGranularity != value {
					t.Fatalf("CH layout missing from manifest: %+v", m)
				}
				if err := checkManifest(c, m, state == "ready"); err != nil {
					t.Fatalf("matching CH layout rejected: %v", err)
				}
				for _, stored := range []int{0, value%8192 + 1} {
					wrong := m
					wrong.CHIndexGranularity = stored
					wire, err := json.Marshal(wrong)
					if err != nil {
						t.Fatal(err)
					}
					if stored == 0 && bytes.Contains(wire, []byte("ch_index_granularity")) {
						t.Fatalf("missing-field fixture serialized a CH setting: %s", wire)
					}
					var reopened Manifest
					if err := json.Unmarshal(wire, &reopened); err != nil {
						t.Fatal(err)
					}
					var ownership *OwnershipError
					if err := checkManifest(c, reopened, false); !errors.As(err, &ownership) {
						t.Fatalf("CH %s/%s configured=%d stored=%d accepted or wrong error: %v", backend, state, value, stored, err)
					}
				}
			}
		}
	}
}

func TestCHGranularityPublicOwnershipGuards(t *testing.T) {
	t.Parallel()
	for _, transport := range []string{"http", "native"} {
		for _, attrs := range []bool{false, true} {
			for _, order := range []string{"trace", "time"} {
				for _, state := range []string{"loading", "ready"} {
					for _, problem := range []string{"missing", "mismatch", "matching"} {
						for _, op := range []string{"query", "trace", "span", "time", "empty_time", "audit", "drop"} {
							if problem == "matching" && op != "drop" {
								continue
							}
							t.Run(fmt.Sprintf("%s/attrs_%t/%s/%s/%s/%s", transport, attrs, order, state, problem, op), func(t *testing.T) {
								t.Parallel()
								c := testConfig()
								c.Backend, c.Order, c.CHIndexGranularity = "ch-map", order, 8192
								if attrs {
									c.Backend = "ch-attrs"
								}
								if transport == "native" {
									c.CHQueryTransport, c.CHNativeAddr = "native", "127.0.0.1:9000"
								}
								owner := manifest(c, state)
								owner.CHIndexGranularity = 64
								if problem == "matching" {
									owner.CHIndexGranularity = c.CHIndexGranularity
								}
								if problem == "missing" {
									owner.CHIndexGranularity = 0
								}
								payload, err := json.Marshal(owner)
								if err != nil {
									t.Fatal(err)
								}
								if problem == "missing" && bytes.Contains(payload, []byte("ch_index_granularity")) {
									t.Fatalf("old CH fixture is not actually missing its setting: %s", payload)
								}
								var httpOwners, nativeOwners, otherCalls atomic.Int64
								var sentinelDeleted atomic.Bool
								b := chHTTPBackend(t, attrs, func(w http.ResponseWriter, r *http.Request) {
									sql := r.URL.Query().Get("query")
									if chIsManifest(sql) {
										httpOwners.Add(1)
										fmt.Fprint(w, chManifestWire(t, owner))
										return
									}
									otherCalls.Add(1)
									if strings.HasPrefix(sql, "DROP DATABASE ") {
										sentinelDeleted.Store(true)
										if problem == "matching" && sql == "DROP DATABASE otel_"+c.Run+" SYNC" {
											return
										}
									}
									http.Error(w, "granularity guard bypassed", http.StatusInternalServerError)
								})
								b.cfg = c
								if transport == "native" {
									conn := &fakeCHNativeConn{}
									conn.query = func(_ int, _ context.Context, sql string) (driver.Rows, error) {
										if chIsManifest(sql) {
											nativeOwners.Add(1)
											return &fakeCHNativeRows{values: [][]any{{string(payload)}}}, nil
										}
										otherCalls.Add(1)
										return nil, errors.New("native granularity guard bypassed")
									}
									b.queries = &chNativeQueries{conn: conn, cfg: c}
								}
								b.lastWork = QueryResult{QueryID: "old_data", ServerReadRows: 37, ServerSummary: true}
								err = chPublicOperation(t.Context(), b, op)
								if problem == "matching" {
									if err != nil || httpOwners.Load() != 1 || nativeOwners.Load() != 0 || otherCalls.Load() != 1 || !sentinelDeleted.Load() {
										t.Fatalf("matching %s CH run not publicly droppable: error=%v HTTP=%d native=%d other=%d deleted=%t", state, err, httpOwners.Load(), nativeOwners.Load(), otherCalls.Load(), sentinelDeleted.Load())
									}
									return
								}
								var ownership *OwnershipError
								if !errors.As(err, &ownership) {
									t.Fatalf("CH granularity %s allowed %s or returned wrong error: %v", problem, op, err)
								}
								if httpOwners.Load()+nativeOwners.Load() != 1 || otherCalls.Load() != 0 || sentinelDeleted.Load() {
									t.Fatalf("guard bypassed ownership: HTTP=%d native=%d other=%d sentinelDeleted=%t", httpOwners.Load(), nativeOwners.Load(), otherCalls.Load(), sentinelDeleted.Load())
								}
								usesNative := transport == "native" && op != "audit" && op != "drop"
								if usesNative && nativeOwners.Load() != 1 || !usesNative && httpOwners.Load() != 1 {
									t.Fatalf("wrong ownership transport HTTP=%d native=%d", httpOwners.Load(), nativeOwners.Load())
								}
								if op != "audit" && op != "drop" && !reflect.DeepEqual(b.LastQueryWork(), QueryResult{}) {
									t.Fatalf("granularity ownership failure retained data provenance: %+v", b.LastQueryWork())
								}
							})
						}
					}
				}
			}
		}
	}
}

func TestCHGranularityActualIngestDDL(t *testing.T) {
	t.Parallel()
	for _, attrs := range []bool{false, true} {
		for _, order := range []string{"trace", "time"} {
			for _, value := range []int{8192, 512, 64, 1} {
				t.Run(fmt.Sprintf("attrs_%t/%s/%d", attrs, order, value), func(t *testing.T) {
					t.Parallel()
					c := testConfig()
					c.Count, c.Tenants, c.Attributes, c.CHBatch, c.Order, c.CHIndexGranularity = 3, 1, 4, 2, order, value
					var mu sync.Mutex
					ddl := map[string]string{}
					var states []string
					var sequences []int
					var ordinals []int
					var b *CHBackend
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						mu.Lock()
						defer mu.Unlock()
						sql := r.URL.Query().Get("query")
						body, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
							return
						}
						if r.Method != http.MethodPost {
							t.Errorf("Ingest DDL method=%s", r.Method)
						}
						switch {
						case sql == "CREATE DATABASE "+b.database:
						case strings.HasPrefix(sql, "CREATE TABLE "+b.database+".owner "):
							if strings.Contains(sql, "index_granularity") {
								t.Error("MergeTree setting applied to TinyLog owner")
							}
						case strings.HasPrefix(sql, "CREATE TABLE "+b.database+".spans "):
							ddl["spans"] = sql
						case strings.HasPrefix(sql, "CREATE TABLE "+b.database+".attr_lookup "):
							ddl["attr_lookup"] = sql
						case strings.HasPrefix(sql, "CREATE MATERIALIZED VIEW "+b.database+".attr_mv "):
						case sql == "INSERT INTO "+b.database+".owner FORMAT JSONEachRow":
							var row struct {
								Sequence int    `json:"sequence"`
								Payload  string `json:"payload"`
							}
							if err := json.Unmarshal(body, &row); err != nil {
								t.Error(err)
								return
							}
							var owner Manifest
							if err := json.Unmarshal([]byte(row.Payload), &owner); err != nil {
								t.Error(err)
								return
							}
							if owner.CHIndexGranularity != value || owner.Backend != b.cfg.Backend {
								t.Errorf("ingested ownership manifest lacks physical setting: %+v", owner)
							}
							if owner.State == "ready" && len(ordinals) != 3 {
								t.Error("ready manifest preceded complete source insertion")
							}
							states, sequences = append(states, owner.State), append(sequences, row.Sequence)
						case sql == "INSERT INTO "+b.database+".spans SETTINGS async_insert=0 FORMAT JSONEachRow":
							if ddl["spans"] == "" || attrs && ddl["attr_lookup"] == "" {
								t.Error("source INSERT preceded configured table creation")
							}
							for _, line := range bytes.Split(bytes.TrimSpace(body), []byte{'\n'}) {
								var row struct {
									Ordinal int `json:"ordinal"`
								}
								if err := json.Unmarshal(line, &row); err != nil {
									t.Error(err)
									continue
								}
								ordinals = append(ordinals, row.Ordinal)
							}
						default:
							t.Errorf("unexpected public Ingest SQL: %s", sql)
							http.Error(w, "unexpected SQL", http.StatusBadRequest)
						}
					}))
					t.Cleanup(srv.Close)
					var err error
					b, err = OpenCH(c, srv.URL, attrs)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(b.Close)
					stats, err := b.Ingest(t.Context())
					if err != nil || stats.Rows != 3 || stats.Attributes != 12 {
						t.Fatalf("configured DDL Ingest: stats=%+v error=%v", stats, err)
					}
					mu.Lock()
					defer mu.Unlock()
					wantTables := 1
					if attrs {
						wantTables = 2
					}
					if len(ddl) != wantTables {
						t.Fatalf("MergeTree table census=%v want=%d", ddl, wantTables)
					}
					for name, sql := range ddl {
						if !strings.HasSuffix(sql, " SETTINGS index_granularity="+strconv.Itoa(value)) || strings.Contains(sql, "ALTER TABLE") {
							t.Fatalf("initial %s table omitted/exchanged configured setting %d: %s", name, value, sql)
						}
					}
					if !reflect.DeepEqual(ordinals, []int{0, 1, 2}) || !reflect.DeepEqual(states, []string{"loading", "ready"}) || !reflect.DeepEqual(sequences, []int{1, 2}) {
						t.Fatalf("DDL test did not complete public ingestion: ordinals=%v states=%v sequences=%v", ordinals, states, sequences)
					}
				})
			}
		}
	}
}
