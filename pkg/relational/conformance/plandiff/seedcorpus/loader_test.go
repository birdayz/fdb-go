package seedcorpus

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"fdb.dev/pkg/relational/conformance/plandiff"
)

func TestSeedRunCorpus_JSONBitwiseRoundTrip(t *testing.T) {
	t.Parallel()

	orig := SeedRunCorpus()
	if len(orig) < 1645 {
		t.Fatalf("SeedRunCorpus() returned %d entries, want at least 1645", len(orig))
	}

	raw, err := EncodeJSON(orig)
	if err != nil {
		t.Fatalf("EncodeJSON(SeedRunCorpus()) failed: %v", err)
	}

	// Single-file round-trip.
	singleFS := fstest.MapFS{
		"seed.json": &fstest.MapFile{Data: raw},
	}
	gotSingle, err := LoadFromFS(singleFS, ".")
	if err != nil {
		t.Fatalf("LoadFromFS(single) failed: %v", err)
	}
	negZeroCount := assertRunQueriesBitwiseEqual(t, gotSingle, orig)
	if negZeroCount == 0 {
		t.Fatal("expected at least one -0.0 cell in SeedRunCorpus() to verify sign-bit preservation")
	}

	// Multi-shard round-trip across nested directories in lexical order.
	shardedFS := make(fstest.MapFS)
	const shardSize = 200
	shardIdx := 0
	for start := 0; start < len(orig); start += shardSize {
		end := start + shardSize
		if end > len(orig) {
			end = len(orig)
		}
		shardBytes, err := EncodeJSON(orig[start:end])
		if err != nil {
			t.Fatalf("EncodeJSON(shard %d) failed: %v", shardIdx, err)
		}
		var shardPath string
		if shardIdx%2 == 0 {
			shardPath = fmt.Sprintf("shards/part_%02d.json", shardIdx)
		} else {
			shardPath = fmt.Sprintf("shards/part_%02d/nested.json", shardIdx)
		}
		shardedFS[shardPath] = &fstest.MapFile{Data: shardBytes}
		shardIdx++
	}
	// Non-JSON files in the directory must be ignored cleanly.
	shardedFS["shards/README.txt"] = &fstest.MapFile{Data: []byte("ignored")}

	gotSharded, err := LoadFromFS(shardedFS, "shards")
	if err != nil {
		t.Fatalf("LoadFromFS(sharded) failed: %v", err)
	}
	assertRunQueriesBitwiseEqual(t, gotSharded, orig)
}

func TestSeedRunCorpusWithRFC082Divergences_JSONBitwiseRoundTrip(t *testing.T) {
	t.Parallel()

	orig := SeedRunCorpus()
	ApplyRFC082Divergences(orig)

	raw, err := EncodeJSON(orig)
	if err != nil {
		t.Fatalf("EncodeJSON failed: %v", err)
	}

	got, err := LoadFromFS(fstest.MapFS{
		"corpus/all.json": &fstest.MapFile{Data: raw},
	}, "corpus")
	if err != nil {
		t.Fatalf("LoadFromFS failed: %v", err)
	}
	assertRunQueriesBitwiseEqual(t, got, orig)
}

func TestCellEncoding_NegativeAndPositiveZeroBitwise(t *testing.T) {
	t.Parallel()

	negZero64 := math.Copysign(0, -1)
	posZero64 := 0.0
	negZero32 := float32(math.Copysign(0, -1))
	posZero32 := float32(0)

	entries := []plandiff.RunQuery{
		{
			Name:           "signed_zeros_and_special_floats",
			SchemaTemplate: "CREATE TABLE T (id BIGINT, v DOUBLE, PRIMARY KEY (id))",
			SetupSqls:      []string{"INSERT INTO T VALUES (1, -0.0)"},
			Query:          "SELECT id, v FROM T ORDER BY id",
			Divergence: &plandiff.Divergence{
				Direction: plandiff.DivergenceJavaWrongRowsGoCorrect,
				Reason:    "verify IEEE-754 float64 and float32 bit preservation",
				GoExpectedRows: [][]any{
					{posZero64, negZero64},
					{posZero32, negZero32},
					{math.NaN(), math.Inf(1), math.Inf(-1)},
					{math.MaxFloat64, math.SmallestNonzeroFloat64},
					{float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1))},
				},
			},
		},
	}

	raw, err := EncodeJSON(entries)
	if err != nil {
		t.Fatalf("EncodeJSON failed: %v", err)
	}
	if !bytes.Contains(raw, []byte("0x8000000000000000")) {
		t.Fatalf("expected encoded JSON to carry explicit -0.0 bits 0x8000000000000000, got:\n%s", string(raw))
	}

	got, err := LoadFromFS(fstest.MapFS{
		"zeros.json": &fstest.MapFile{Data: raw},
	}, ".")
	if err != nil {
		t.Fatalf("LoadFromFS failed: %v", err)
	}
	assertRunQueriesBitwiseEqual(t, got, entries)

	gotPos := got[0].Divergence.GoExpectedRows[0][0].(float64)
	gotNeg := got[0].Divergence.GoExpectedRows[0][1].(float64)
	if math.Float64bits(gotPos) == math.Float64bits(gotNeg) {
		t.Fatalf("expected +0.0 bits != -0.0 bits, both were 0x%016x", math.Float64bits(gotPos))
	}
	if !math.Signbit(gotNeg) || math.Signbit(gotPos) {
		t.Fatalf("signbit mismatch: pos=%v neg=%v", math.Signbit(gotPos), math.Signbit(gotNeg))
	}
}

func TestCellEncoding_DynamicTypesDistinguished(t *testing.T) {
	t.Parallel()

	entries := []plandiff.RunQuery{
		{
			Name:           "dynamic_types",
			SchemaTemplate: "CREATE TABLE T (id BIGINT, PRIMARY KEY (id))",
			SetupSqls:      []string{},
			Query:          "SELECT id FROM T",
			Divergence: &plandiff.Divergence{
				Direction: plandiff.DivergenceJavaErrorsGoCorrect,
				Reason:    "verify every cell Go dynamic type round-trips without collapsing",
				GoExpectedRows: [][]any{
					{
						int64(1),
						float64(1),
						int32(1),
						int(1),
						uint64(1),
						uint32(1),
						float32(1),
						"1",
						true,
						false,
						[]byte("hi"),
						[]byte{},
						[]byte(nil),
						[]any{int64(2), float64(2), "two", nil},
						[]any{},
						[]any(nil),
						nil,
					},
					{
						int64(0),
						int64(-9223372036854775808),
						int64(9223372036854775807),
						uint64(18446744073709551615),
						float64(0),
						float32(0),
					},
				},
			},
		},
	}

	raw, err := EncodeJSON(entries)
	if err != nil {
		t.Fatalf("EncodeJSON failed: %v", err)
	}
	got, err := LoadFromFS(fstest.MapFS{
		"types.json": &fstest.MapFile{Data: raw},
	}, ".")
	if err != nil {
		t.Fatalf("LoadFromFS failed: %v", err)
	}
	assertRunQueriesBitwiseEqual(t, got, entries)
}

func TestEncodeDecode_NilVsEmptySlices(t *testing.T) {
	t.Parallel()

	entries := []plandiff.RunQuery{
		{
			Name:           "nil_setup_and_nil_expected",
			SchemaTemplate: "CREATE TABLE T1 (id BIGINT, PRIMARY KEY (id))",
			SetupSqls:      nil,
			Query:          "SELECT id FROM T1",
			Divergence: &plandiff.Divergence{
				Direction:       plandiff.DivergenceBothErrorMessagesDrift,
				Reason:          "nil GoExpectedRows",
				GoExpectedRows:  nil,
				GoErrorContains: "boom",
			},
		},
		{
			Name:           "empty_setup_and_empty_expected",
			SchemaTemplate: "CREATE TABLE T2 (id BIGINT, PRIMARY KEY (id))",
			SetupSqls:      []string{},
			Query:          "SELECT id FROM T2",
			Divergence: &plandiff.Divergence{
				Direction:      plandiff.DivergenceJavaErrorsGoCorrect,
				Reason:         "empty non-nil GoExpectedRows",
				GoExpectedRows: [][]any{},
			},
		},
		{
			Name:           "row_nil_vs_empty_row",
			SchemaTemplate: "CREATE TABLE T3 (id BIGINT, PRIMARY KEY (id))",
			SetupSqls:      []string{"INSERT INTO T3 VALUES (1)"},
			Query:          "SELECT id FROM T3",
			Divergence: &plandiff.Divergence{
				Direction:      plandiff.DivergenceJavaErrorsGoCorrect,
				Reason:         "nil row vs empty non-nil row",
				GoExpectedRows: [][]any{nil, {}},
			},
		},
	}

	raw, err := EncodeJSON(entries)
	if err != nil {
		t.Fatalf("EncodeJSON failed: %v", err)
	}
	got, err := LoadFromFS(fstest.MapFS{
		"slices.json": &fstest.MapFile{Data: raw},
	}, ".")
	if err != nil {
		t.Fatalf("LoadFromFS failed: %v", err)
	}
	assertRunQueriesBitwiseEqual(t, got, entries)

	if got[0].SetupSqls != nil {
		t.Fatalf("got[0].SetupSqls = %#v, want nil", got[0].SetupSqls)
	}
	if got[1].SetupSqls == nil || len(got[1].SetupSqls) != 0 {
		t.Fatalf("got[1].SetupSqls = %#v, want non-nil empty []string{}", got[1].SetupSqls)
	}
	if got[0].Divergence.GoExpectedRows != nil {
		t.Fatalf("got[0].Divergence.GoExpectedRows = %#v, want nil", got[0].Divergence.GoExpectedRows)
	}
	if got[1].Divergence.GoExpectedRows == nil || len(got[1].Divergence.GoExpectedRows) != 0 {
		t.Fatalf("got[1].Divergence.GoExpectedRows = %#v, want non-nil empty [][]any{}", got[1].Divergence.GoExpectedRows)
	}
	if got[2].Divergence.GoExpectedRows[0] != nil {
		t.Fatalf("got[2].GoExpectedRows[0] = %#v, want nil", got[2].Divergence.GoExpectedRows[0])
	}
	if got[2].Divergence.GoExpectedRows[1] == nil || len(got[2].Divergence.GoExpectedRows[1]) != 0 {
		t.Fatalf("got[2].GoExpectedRows[1] = %#v, want non-nil empty []any{}", got[2].Divergence.GoExpectedRows[1])
	}
}

func TestLoadFromFS_AndEncodeJSON_Errors(t *testing.T) {
	t.Parallel()

	loadCases := []struct {
		name      string
		fsys      fs.FS
		dir       string
		wantCell  bool
		wantInMsg string
	}{
		{
			name:      "nil_fs",
			fsys:      nil,
			dir:       "testdata",
			wantInMsg: "nil filesystem",
		},
		{
			name:      "missing_dir",
			fsys:      fstest.MapFS{},
			dir:       "missing",
			wantInMsg: "walk corpus directory",
		},
		{
			name: "no_json_files",
			fsys: fstest.MapFS{
				"dir/readme.md": &fstest.MapFile{Data: []byte("hello")},
			},
			dir:       "dir",
			wantInMsg: "no JSON corpus files found",
		},
		{
			name: "malformed_json_syntax",
			fsys: fstest.MapFS{
				"bad.json": &fstest.MapFile{Data: []byte("{not json")},
			},
			dir:       ".",
			wantInMsg: "decode JSON",
		},
		{
			name: "trailing_tokens",
			fsys: fstest.MapFS{
				"bad.json": &fstest.MapFile{Data: []byte(`{"schema_version":1,"entries":[]}{}`)},
			},
			dir:       ".",
			wantInMsg: "trailing data",
		},
		{
			name: "wrong_schema_version",
			fsys: fstest.MapFS{
				"bad.json": &fstest.MapFile{Data: []byte(`{"schema_version":2,"entries":[]}`)},
			},
			dir:       ".",
			wantInMsg: "unsupported schema_version",
		},
		{
			name: "null_entries",
			fsys: fstest.MapFS{
				"bad.json": &fstest.MapFile{Data: []byte(`{"schema_version":1,"entries":null}`)},
			},
			dir:       ".",
			wantInMsg: "missing or null",
		},
		{
			name: "unknown_top_field",
			fsys: fstest.MapFS{
				"bad.json": &fstest.MapFile{Data: []byte(`{"schema_version":1,"entries":[],"extra":1}`)},
			},
			dir:       ".",
			wantInMsg: "decode JSON",
		},
		{
			name: "missing_entry_name",
			fsys: fstest.MapFS{
				"bad.json": &fstest.MapFile{Data: []byte(`{"schema_version":1,"entries":[{"name":"","query":"SELECT 1"}]}`)},
			},
			dir:       ".",
			wantInMsg: `missing required "name"`,
		},
		{
			name: "missing_entry_query",
			fsys: fstest.MapFS{
				"bad.json": &fstest.MapFile{Data: []byte(`{"schema_version":1,"entries":[{"name":"q1","query":""}]}`)},
			},
			dir:       ".",
			wantInMsg: `missing required "query"`,
		},
		{
			name: "invalid_divergence_direction",
			fsys: fstest.MapFS{
				"bad.json": &fstest.MapFile{Data: []byte(`{"schema_version":1,"entries":[{"name":"q1","query":"SELECT 1","divergence":{"direction":"Bogus","reason":"r"}}]}`)},
			},
			dir:       ".",
			wantInMsg: "unknown divergence direction",
		},
		{
			name: "bare_json_number_rejected",
			fsys: fstest.MapFS{
				"bad.json": &fstest.MapFile{Data: []byte(`{"schema_version":1,"entries":[{"name":"q1","query":"SELECT 1","divergence":{"direction":"JavaErrorsGoCorrect","reason":"r","go_expected_rows":[[1]]}}]}`)},
			},
			dir:       ".",
			wantCell:  true,
			wantInMsg: "numbers must use typed wrapper",
		},
		{
			name: "unknown_cell_type",
			fsys: fstest.MapFS{
				"bad.json": &fstest.MapFile{Data: []byte(`{"schema_version":1,"entries":[{"name":"q1","query":"SELECT 1","divergence":{"direction":"JavaErrorsGoCorrect","reason":"r","go_expected_rows":[[{"type":"decimal","value":"1.2"}]]}}]}`)},
			},
			dir:       ".",
			wantCell:  true,
			wantInMsg: "unknown typed cell type",
		},
		{
			name: "null_numeric_value",
			fsys: fstest.MapFS{
				"bad.json": &fstest.MapFile{Data: []byte(`{"schema_version":1,"entries":[{"name":"q1","query":"SELECT 1","divergence":{"direction":"JavaErrorsGoCorrect","reason":"r","go_expected_rows":[[{"type":"float64","value":null}]]}}]}`)},
			},
			dir:       ".",
			wantCell:  true,
			wantInMsg: "unexpected null",
		},
		{
			name: "invalid_bits_hex",
			fsys: fstest.MapFS{
				"bad.json": &fstest.MapFile{Data: []byte(`{"schema_version":1,"entries":[{"name":"q1","query":"SELECT 1","divergence":{"direction":"JavaErrorsGoCorrect","reason":"r","go_expected_rows":[[{"type":"float64","bits":"0xzzzz"}]]}}]}`)},
			},
			dir:       ".",
			wantCell:  true,
			wantInMsg: "must have 16 hex digits",
		},
		{
			name: "both_value_and_bits",
			fsys: fstest.MapFS{
				"bad.json": &fstest.MapFile{Data: []byte(`{"schema_version":1,"entries":[{"name":"q1","query":"SELECT 1","divergence":{"direction":"JavaErrorsGoCorrect","reason":"r","go_expected_rows":[[{"type":"float64","value":1,"bits":"0x0000000000000000"}]]}}]}`)},
			},
			dir:       ".",
			wantCell:  true,
			wantInMsg: "not both",
		},
		{
			name: "invalid_base64_bytes",
			fsys: fstest.MapFS{
				"bad.json": &fstest.MapFile{Data: []byte(`{"schema_version":1,"entries":[{"name":"q1","query":"SELECT 1","divergence":{"direction":"JavaErrorsGoCorrect","reason":"r","go_expected_rows":[[{"type":"bytes","value":"!!!not-b64!!!"}]]}}]}`)},
			},
			dir:       ".",
			wantCell:  true,
			wantInMsg: "invalid base64",
		},
	}

	for _, tc := range loadCases {
		tc := tc
		t.Run("load_"+tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := LoadFromFS(tc.fsys, tc.dir)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantInMsg)
			}
			var loadErr *CorpusLoadError
			if !errors.As(err, &loadErr) {
				t.Fatalf("expected *CorpusLoadError, got %T (%v)", err, err)
			}
			if tc.wantCell {
				var cellErr *CellCodecError
				if !errors.As(err, &cellErr) {
					t.Fatalf("expected wrapped *CellCodecError, got %v", err)
				}
			}
			if !strings.Contains(err.Error(), tc.wantInMsg) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantInMsg)
			}
		})
	}

	t.Run("encode_invalid_direction", func(t *testing.T) {
		t.Parallel()
		_, err := EncodeJSON([]plandiff.RunQuery{
			{
				Name:  "bad_dir",
				Query: "SELECT 1",
				Divergence: &plandiff.Divergence{
					Direction: plandiff.DivergenceDirection("Bogus"),
				},
			},
		})
		if err == nil {
			t.Fatal("expected error for invalid divergence direction")
		}
		var encErr *CorpusEncodeError
		if !errors.As(err, &encErr) {
			t.Fatalf("expected *CorpusEncodeError, got %T (%v)", err, err)
		}
	})

	t.Run("encode_unsupported_cell_type", func(t *testing.T) {
		t.Parallel()
		_, err := EncodeJSON([]plandiff.RunQuery{
			{
				Name:  "bad_cell",
				Query: "SELECT 1",
				Divergence: &plandiff.Divergence{
					Direction:      plandiff.DivergenceJavaErrorsGoCorrect,
					GoExpectedRows: [][]any{{struct{}{}}},
				},
			},
		})
		if err == nil {
			t.Fatal("expected error for unsupported cell type")
		}
		var cellErr *CellCodecError
		if !errors.As(err, &cellErr) {
			t.Fatalf("expected *CellCodecError, got %T (%v)", err, err)
		}
	})
}

func TestEmbeddedFixture_Completeness(t *testing.T) {
	t.Parallel()

	rawFixture, err := fs.ReadFile(embeddedCorpusFS, "testdata/sample_roundtrip.json")
	if err != nil {
		t.Fatalf("embedded testdata/sample_roundtrip.json missing: %v", err)
	}
	if len(rawFixture) == 0 {
		t.Fatal("embedded testdata/sample_roundtrip.json is empty")
	}

	fromEmbedded, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded() failed: %v", err)
	}
	fromFS, err := LoadFromFS(embeddedCorpusFS, "testdata")
	if err != nil {
		t.Fatalf("LoadFromFS(embeddedCorpusFS, testdata) failed: %v", err)
	}
	assertRunQueriesBitwiseEqual(t, fromEmbedded, fromFS)

	want := []plandiff.RunQuery{
		{
			Name:           "single_row_bigint",
			SchemaTemplate: "CREATE TABLE T1 (id BIGINT, PRIMARY KEY (id))",
			SetupSqls:      []string{"INSERT INTO T1 VALUES (42)"},
			Query:          "SELECT id FROM T1 ORDER BY id",
		},
		{
			Name:           "empty_table",
			SchemaTemplate: "CREATE TABLE T4 (id BIGINT, PRIMARY KEY (id))",
			SetupSqls:      nil,
			Query:          "SELECT id FROM T4",
		},
		{
			Name:           "double_negative_zero_ge_predicate",
			SchemaTemplate: "CREATE TABLE T_E7B (id BIGINT, v DOUBLE, PRIMARY KEY (id))",
			SetupSqls:      []string{"INSERT INTO T_E7B VALUES (1, 0.0), (2, -0.0), (3, 1.5), (4, -1.5)"},
			Query:          "SELECT id, v FROM T_E7B WHERE v >= 0.0 ORDER BY id",
			Divergence: &plandiff.Divergence{
				Direction: plandiff.DivergenceJavaWrongRowsGoCorrect,
				Reason:    "Java drops the -0.0 row from `v >= 0.0` despite IEEE 754 saying -0.0 == +0.0; Go correctly keeps it.",
				GoExpectedRows: [][]any{
					{float64(1), float64(0)},
					{float64(2), math.Copysign(0, -1)},
					{float64(3), float64(1.5)},
				},
			},
		},
		{
			Name:           "groupby_empty_table",
			SchemaTemplate: "CREATE TABLE T_GBY_05 (id BIGINT, grp BIGINT, v BIGINT, PRIMARY KEY (id))",
			SetupSqls:      nil,
			Query:          "SELECT grp, COUNT(*) FROM T_GBY_05 GROUP BY grp ORDER BY grp",
			Divergence: &plandiff.Divergence{
				Direction:      plandiff.DivergenceJavaErrorsGoCorrect,
				Reason:         "Java cannot plan a GROUP BY no index orders (UnableToPlanException, TODO #39). Go correctly returns zero groups for empty table per SQL spec.",
				GoExpectedRows: [][]any{},
			},
		},
		{
			Name:           "typed_cells_sample",
			SchemaTemplate: "CREATE TABLE T_SAMPLE (id BIGINT, PRIMARY KEY (id))",
			SetupSqls:      []string{},
			Query:          "SELECT id FROM T_SAMPLE",
			Divergence: &plandiff.Divergence{
				Direction: plandiff.DivergenceBothErrorMessagesDrift,
				Reason:    "Sample fixture exercising all typed cell encodings.",
				GoExpectedRows: [][]any{
					{int64(1), float64(1), true, "ok", []byte{1, 2}, nil},
				},
				GoErrorContains: "sample",
			},
		},
	}

	negZeros := assertRunQueriesBitwiseEqual(t, fromEmbedded, want)
	if negZeros != 1 {
		t.Fatalf("expected 1 negative zero in embedded fixture, got %d", negZeros)
	}

	reencoded, err := EncodeJSON(fromEmbedded)
	if err != nil {
		t.Fatalf("EncodeJSON(fromEmbedded) failed: %v", err)
	}
	roundTripped, err := LoadFromFS(fstest.MapFS{
		"reencoded.json": &fstest.MapFile{Data: reencoded},
	}, ".")
	if err != nil {
		t.Fatalf("LoadFromFS(reencoded) failed: %v", err)
	}
	assertRunQueriesBitwiseEqual(t, roundTripped, want)
}

func assertRunQueriesBitwiseEqual(t *testing.T, got, want []plandiff.RunQuery) int {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("entry count mismatch: got %d, want %d", len(got), len(want))
	}
	negZeroCount := 0
	for i := range want {
		g, w := got[i], want[i]
		if g.Name != w.Name {
			t.Fatalf("entry[%d].Name: got %q, want %q", i, g.Name, w.Name)
		}
		if g.SchemaTemplate != w.SchemaTemplate {
			t.Fatalf("entry[%d] (%s).SchemaTemplate mismatch", i, w.Name)
		}
		if g.Query != w.Query {
			t.Fatalf("entry[%d] (%s).Query mismatch", i, w.Name)
		}
		if (g.SetupSqls == nil) != (w.SetupSqls == nil) {
			t.Fatalf("entry[%d] (%s).SetupSqls nilness mismatch: got nil=%v, want nil=%v", i, w.Name, g.SetupSqls == nil, w.SetupSqls == nil)
		}
		if !reflect.DeepEqual(g.SetupSqls, w.SetupSqls) {
			t.Fatalf("entry[%d] (%s).SetupSqls: got %#v, want %#v", i, w.Name, g.SetupSqls, w.SetupSqls)
		}
		if (g.Divergence == nil) != (w.Divergence == nil) {
			t.Fatalf("entry[%d] (%s).Divergence nilness mismatch: got %v, want %v", i, w.Name, g.Divergence, w.Divergence)
		}
		if w.Divergence == nil {
			continue
		}
		gd, wd := g.Divergence, w.Divergence
		if gd.Direction != wd.Direction {
			t.Fatalf("entry[%d] (%s).Divergence.Direction: got %q, want %q", i, w.Name, gd.Direction, wd.Direction)
		}
		if gd.Reason != wd.Reason {
			t.Fatalf("entry[%d] (%s).Divergence.Reason: got %q, want %q", i, w.Name, gd.Reason, wd.Reason)
		}
		if gd.GoErrorContains != wd.GoErrorContains {
			t.Fatalf("entry[%d] (%s).Divergence.GoErrorContains: got %q, want %q", i, w.Name, gd.GoErrorContains, wd.GoErrorContains)
		}
		if (gd.GoExpectedRows == nil) != (wd.GoExpectedRows == nil) {
			t.Fatalf("entry[%d] (%s).GoExpectedRows nilness mismatch: got nil=%v, want nil=%v", i, w.Name, gd.GoExpectedRows == nil, wd.GoExpectedRows == nil)
		}
		if len(gd.GoExpectedRows) != len(wd.GoExpectedRows) {
			t.Fatalf("entry[%d] (%s).GoExpectedRows len: got %d, want %d", i, w.Name, len(gd.GoExpectedRows), len(wd.GoExpectedRows))
		}
		for r := range wd.GoExpectedRows {
			gr, wr := gd.GoExpectedRows[r], wd.GoExpectedRows[r]
			if (gr == nil) != (wr == nil) {
				t.Fatalf("entry[%d] (%s) row[%d] nilness mismatch: got nil=%v, want nil=%v", i, w.Name, r, gr == nil, wr == nil)
			}
			if len(gr) != len(wr) {
				t.Fatalf("entry[%d] (%s) row[%d] len: got %d, want %d", i, w.Name, r, len(gr), len(wr))
			}
			for c := range wr {
				negZeroCount += assertCellBitwiseEqual(t, fmt.Sprintf("entry[%d] (%s) row[%d][%d]", i, w.Name, r, c), gr[c], wr[c])
			}
		}
	}
	return negZeroCount
}

func assertCellBitwiseEqual(t *testing.T, loc string, got, want any) int {
	t.Helper()
	if (got == nil) != (want == nil) {
		t.Fatalf("%s nilness mismatch: got %#v (%T), want %#v (%T)", loc, got, got, want, want)
	}
	if want == nil {
		return 0
	}
	if reflect.TypeOf(got) != reflect.TypeOf(want) {
		t.Fatalf("%s dynamic type mismatch: got %T (%#v), want %T (%#v)", loc, got, got, want, want)
	}

	switch w := want.(type) {
	case float64:
		g := got.(float64)
		gb, wb := math.Float64bits(g), math.Float64bits(w)
		if gb != wb {
			t.Fatalf("%s float64 bits mismatch: got 0x%016x (%v), want 0x%016x (%v)", loc, gb, g, wb, w)
		}
		if w == 0 && math.Signbit(w) {
			return 1
		}
	case float32:
		g := got.(float32)
		gb, wb := math.Float32bits(g), math.Float32bits(w)
		if gb != wb {
			t.Fatalf("%s float32 bits mismatch: got 0x%08x (%v), want 0x%08x (%v)", loc, gb, g, wb, w)
		}
	case []byte:
		g := got.([]byte)
		if (g == nil) != (w == nil) {
			t.Fatalf("%s []byte nilness mismatch: got nil=%v, want nil=%v", loc, g == nil, w == nil)
		}
		if !bytes.Equal(g, w) {
			t.Fatalf("%s []byte content mismatch: got %x, want %x", loc, g, w)
		}
	case []any:
		g := got.([]any)
		if (g == nil) != (w == nil) {
			t.Fatalf("%s []any nilness mismatch: got nil=%v, want nil=%v", loc, g == nil, w == nil)
		}
		if len(g) != len(w) {
			t.Fatalf("%s []any len mismatch: got %d, want %d", loc, len(g), len(w))
		}
		negZeros := 0
		for i := range w {
			negZeros += assertCellBitwiseEqual(t, fmt.Sprintf("%s[%d]", loc, i), g[i], w[i])
		}
		return negZeros
	default:
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s value mismatch: got %#v, want %#v", loc, got, want)
		}
	}
	return 0
}
