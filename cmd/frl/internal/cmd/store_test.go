package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	configv1 "github.com/birdayz/fdb-record-layer-go/cmd/frl/gen/frl/config/v1"
	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
)

func TestParseKeyspacePath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in      string
		wantErr bool
		// wantLen is the expected number of tuple elements. The subspace
		// bytes themselves aren't asserted — that's the tuple package's
		// contract; here we only verify segmentation.
		wantLen int
		// wantErrMsg, if non-empty, is a substring the error message must
		// contain — guards both the error branch (by case) and that the
		// message stays operator-friendly after banner capitalization.
		wantErrMsg string
	}{
		{"/myapp/prod/orders", false, 3, ""},
		{"myapp/prod/orders", false, 3, ""}, // no leading slash
		{"/myapp/", false, 1, ""},           // trailing slash stripped
		{"/single", false, 1, ""},
		{"", true, 0, "empty"},              // empty → empty-tuple branch
		{"/", true, 0, "empty"},             // slash-only → empty tuple
		{"/a//b", true, 0, "empty segment"}, // double slash → empty segment branch
		{"//", true, 0, "empty"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			ss, err := parseKeyspacePath(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseKeyspacePath(%q) succeeded, want error", tc.in)
				}
				if tc.wantErrMsg != "" && !strings.Contains(err.Error(), tc.wantErrMsg) {
					t.Errorf("parseKeyspacePath(%q) err = %v; want substring %q",
						tc.in, err, tc.wantErrMsg)
				}
				// Both error branches name the config key so operators can
				// fix the YAML without digging into the source.
				if !strings.Contains(err.Error(), "keyspace_path") {
					t.Errorf("parseKeyspacePath(%q) err = %v; should name keyspace_path",
						tc.in, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseKeyspacePath(%q): %v", tc.in, err)
			}
			// Unpack the subspace prefix back to a tuple and verify the
			// element count matches the input segmentation. Catches silent
			// segmentation bugs (empty-bytes check isn't enough — must be
			// exactly wantLen elements).
			unpacked, err := tuple.Unpack(ss.Bytes())
			if err != nil {
				t.Fatalf("tuple.Unpack(%x): %v", ss.Bytes(), err)
			}
			if len(unpacked) != tc.wantLen {
				t.Errorf("parseKeyspacePath(%q) → %d elements, want %d",
					tc.in, len(unpacked), tc.wantLen)
			}
		})
	}
}

func TestRecordCountStateName(t *testing.T) {
	t.Parallel()
	cases := map[gen.DataStoreInfo_RecordCountState]string{
		gen.DataStoreInfo_READABLE:   "readable",
		gen.DataStoreInfo_WRITE_ONLY: "write-only",
		gen.DataStoreInfo_DISABLED:   "disabled",
	}
	for state, want := range cases {
		if got := recordCountStateName(state); got != want {
			t.Errorf("recordCountStateName(%v) = %q, want %q", state, got, want)
		}
	}
	// Unknown value falls into default branch.
	if got := recordCountStateName(gen.DataStoreInfo_RecordCountState(99)); !strings.HasPrefix(got, "unknown") {
		t.Errorf("unknown state rendered as %q, want unknown(...)", got)
	}
}

func TestLockStateDescription(t *testing.T) {
	t.Parallel()

	// Nil and UNSPECIFIED both render as "unlocked".
	if got := lockStateDescription(nil); got != "unlocked" {
		t.Errorf("nil lock = %q, want unlocked", got)
	}
	if got := lockStateDescription(&gen.DataStoreInfo_StoreLockState{}); got != "unlocked" {
		t.Errorf("empty StoreLockState = %q, want unlocked", got)
	}

	// With reason appended.
	locked := &gen.DataStoreInfo_StoreLockState{
		LockState: gen.DataStoreInfo_StoreLockState_FORBID_RECORD_UPDATE.Enum(),
		Reason:    proto_string("maintenance window"),
	}
	got := lockStateDescription(locked)
	if !strings.Contains(got, "FORBID_RECORD_UPDATE") || !strings.Contains(got, "maintenance window") {
		t.Errorf("lock description = %q; missing state or reason", got)
	}
}

func TestWriteStoreInfoRendersAllFields(t *testing.T) {
	t.Parallel()
	ctx := &configv1.Context{
		Name:         "prod",
		ClusterFile:  "/etc/fdb/prod.cluster",
		KeyspacePath: "/myapp/prod",
	}
	info := &gen.DataStoreInfo{
		FormatVersion:   proto_int32(12),
		MetaDataversion: proto_int32(17),
		UserVersion:     proto_int32(3),
		Cacheable:       proto_bool(true),
	}

	var buf bytes.Buffer
	if err := writeStoreInfo(&buf, ctx, info, nil); err != nil {
		t.Fatalf("writeStoreInfo: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"Context:           prod",
		"Cluster file:      /etc/fdb/prod.cluster",
		"Keyspace path:     /myapp/prod",
		"Format version:    12",
		"Metadata version:  17",
		"User version:      3",
		"Cacheable:         true",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("writeStoreInfo output missing %q\nfull output:\n%s", want, out)
		}
	}
}

func TestWriteStoreInfo_RendersRFC3339Timestamp(t *testing.T) {
	t.Parallel()
	// 2026-04-19T08:00:00Z as ms-since-epoch (verified via `date -u -d`).
	const ts = 1776585600000
	info := &gen.DataStoreInfo{LastUpdateTime: proto_uint64(ts)}
	ctx := &configv1.Context{Name: "test", KeyspacePath: "/x"}
	var buf bytes.Buffer
	if err := writeStoreInfo(&buf, ctx, info, nil); err != nil {
		t.Fatalf("writeStoreInfo: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"Last updated:", "2026-04-19T08:00:00Z", "1776585600000 ms epoch"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\nfull output:\n%s", want, out)
		}
	}
}

func TestWriteStoreInfo_RendersFDBPrefix(t *testing.T) {
	t.Parallel()
	info := &gen.DataStoreInfo{}
	ctx := &configv1.Context{Name: "test", KeyspacePath: "/myapp"}
	prefix := []byte{0x02, 'a', 'b', 0x00}
	var buf bytes.Buffer
	if err := writeStoreInfo(&buf, ctx, info, prefix); err != nil {
		t.Fatalf("writeStoreInfo: %v", err)
	}
	if !strings.Contains(buf.String(), "FDB prefix (hex):  02616200") {
		t.Errorf("expected hex-rendered prefix in output:\n%s", buf.String())
	}
}

func TestWriteStoreInfo_OmitsPrefixWhenNil(t *testing.T) {
	t.Parallel()
	info := &gen.DataStoreInfo{}
	ctx := &configv1.Context{Name: "test", KeyspacePath: "/myapp"}
	var buf bytes.Buffer
	if err := writeStoreInfo(&buf, ctx, info, nil); err != nil {
		t.Fatalf("writeStoreInfo: %v", err)
	}
	if strings.Contains(buf.String(), "FDB prefix") {
		t.Errorf("FDB prefix should be omitted when nil:\n%s", buf.String())
	}
}

// TestWriteStoreInfo_RendersIncarnation verifies the cluster-migration
// marker is shown when non-zero. Incarnation is the only defense
// against `version()` indexes colliding across clusters — silently
// hiding it would leave operators blind to whether a store has been
// migrated.
func TestWriteStoreInfo_RendersIncarnation(t *testing.T) {
	t.Parallel()
	ctx := &configv1.Context{Name: "migrated", KeyspacePath: "/x"}
	info := &gen.DataStoreInfo{Incarnation: proto_int32(7)}

	var buf bytes.Buffer
	if err := writeStoreInfo(&buf, ctx, info, nil); err != nil {
		t.Fatalf("writeStoreInfo: %v", err)
	}
	if !strings.Contains(buf.String(), "Incarnation:       7") {
		t.Errorf("expected Incarnation: 7 in output:\n%s", buf.String())
	}
}

// TestWriteStoreInfo_OmitsZeroIncarnation — stores that were never
// migrated should not show the Incarnation line (counterpart to
// RendersIncarnation).
func TestWriteStoreInfo_OmitsZeroIncarnation(t *testing.T) {
	t.Parallel()
	ctx := &configv1.Context{Name: "pristine", KeyspacePath: "/x"}
	info := &gen.DataStoreInfo{}
	var buf bytes.Buffer
	if err := writeStoreInfo(&buf, ctx, info, nil); err != nil {
		t.Fatalf("writeStoreInfo: %v", err)
	}
	if strings.Contains(buf.String(), "Incarnation") {
		t.Errorf("Incarnation should be omitted when zero:\n%s", buf.String())
	}
}

// TestWriteStoreInfo_LegacyUnsplitSuffix: legacy stores with
// omit_unsplit_record_suffix=true can't enable split-long-records —
// operators debugging "why is my large record failing to save" need
// to see this flag. Shown with the constraint explanation baked in.
func TestWriteStoreInfo_LegacyUnsplitSuffix(t *testing.T) {
	t.Parallel()
	ctx := &configv1.Context{Name: "legacy", KeyspacePath: "/x"}
	info := &gen.DataStoreInfo{OmitUnsplitRecordSuffix: proto_bool(true)}

	var buf bytes.Buffer
	if err := writeStoreInfo(&buf, ctx, info, nil); err != nil {
		t.Fatalf("writeStoreInfo: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Unsplit suffix:") {
		t.Errorf("expected Unsplit suffix line when omit_unsplit_record_suffix=true:\n%s", out)
	}
	if !strings.Contains(out, "legacy store") {
		t.Errorf("expected 'legacy store' hint in the unsplit line:\n%s", out)
	}
}

// TestWriteStoreInfo_ModernNoUnsplitLine — fresh stores (the default,
// where the flag is unset / false) should NOT get the legacy line.
func TestWriteStoreInfo_ModernNoUnsplitLine(t *testing.T) {
	t.Parallel()
	ctx := &configv1.Context{Name: "modern", KeyspacePath: "/x"}
	info := &gen.DataStoreInfo{}
	var buf bytes.Buffer
	if err := writeStoreInfo(&buf, ctx, info, nil); err != nil {
		t.Fatalf("writeStoreInfo: %v", err)
	}
	if strings.Contains(buf.String(), "Unsplit suffix") {
		t.Errorf("Unsplit suffix line should be omitted on modern stores:\n%s", buf.String())
	}
}

func TestWriteStoreInfo_OmitsZeroTimestamp(t *testing.T) {
	t.Parallel()
	info := &gen.DataStoreInfo{} // LastUpdateTime unset → 0
	ctx := &configv1.Context{Name: "test", KeyspacePath: "/x"}
	var buf bytes.Buffer
	if err := writeStoreInfo(&buf, ctx, info, nil); err != nil {
		t.Fatalf("writeStoreInfo: %v", err)
	}
	if strings.Contains(buf.String(), "Last updated:") {
		t.Errorf("expected Last updated to be omitted for zero ts, got:\n%s", buf.String())
	}
}

func proto_uint64(v uint64) *uint64 { return &v }

func TestWriteStoreInfoJSON_RendersProtoFields(t *testing.T) {
	t.Parallel()
	info := &gen.DataStoreInfo{
		FormatVersion:   proto_int32(12),
		MetaDataversion: proto_int32(17),
		Cacheable:       proto_bool(true),
	}
	var buf bytes.Buffer
	if err := writeStoreInfoJSON(&buf, info); err != nil {
		t.Fatalf("writeStoreInfoJSON: %v", err)
	}
	out := buf.String()
	// protojson uses camelCase keys matching the proto field names;
	// int32 fields render as bare numbers, bool as true/false.
	// Whitespace between ':' and the value isn't guaranteed by the
	// protojson contract — assert key and value separately to stay
	// resilient to any formatter changes.
	for _, want := range []string{
		`"formatVersion"`,
		` 12`,
		`"metaDataversion"`,
		` 17`,
		`"cacheable"`,
		` true`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("JSON output missing %q:\n%s", want, out)
		}
	}
}

func TestRunStoreInfo_EmptyKeyspaceErrors(t *testing.T) {
	t.Parallel()
	ctx := &configv1.Context{Name: "bad"} // keyspace_path left empty
	var buf bytes.Buffer
	err := runStoreInfo(context.Background(), &buf, ctx, "text")
	if err == nil {
		t.Fatal("runStoreInfo with empty keyspace succeeded, want error")
	}
	if !strings.Contains(err.Error(), "empty keyspace_path") {
		t.Errorf("error = %v; want mention of empty keyspace_path", err)
	}
}

// proto_string / proto_int32 / proto_bool are tiny pointer helpers for
// proto2 optional fields. Kept local to this test file to avoid pulling
// in proto.String etc. over isolated-extension Bazel repo boundaries.
func proto_string(s string) *string { return &s }
func proto_int32(v int32) *int32    { return &v }
func proto_bool(v bool) *bool       { return &v }
