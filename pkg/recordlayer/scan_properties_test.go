package recordlayer

import (
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
)

// ---------------------------------------------------------------------------
// IsolationLevel
// ---------------------------------------------------------------------------

func TestIsolationLevel_IsSnapshot(t *testing.T) {
	t.Parallel()
	tests := []struct {
		level IsolationLevel
		want  bool
	}{
		{IsolationLevelSnapshot, true},
		{IsolationLevelSerializable, false},
		{IsolationLevel(99), false},
	}
	for _, tt := range tests {
		if got := tt.level.IsSnapshot(); got != tt.want {
			t.Errorf("IsolationLevel(%d).IsSnapshot() = %v, want %v", int(tt.level), got, tt.want)
		}
	}
}

func TestIsolationLevel_String(t *testing.T) {
	t.Parallel()
	tests := []struct {
		level IsolationLevel
		want  string
	}{
		{IsolationLevelSnapshot, "Snapshot"},
		{IsolationLevelSerializable, "Serializable"},
		{IsolationLevel(42), "Unknown"},
	}
	for _, tt := range tests {
		if got := tt.level.String(); got != tt.want {
			t.Errorf("IsolationLevel(%d).String() = %q, want %q", int(tt.level), got, tt.want)
		}
	}
}

func TestIsolationLevel_LegacyAliases(t *testing.T) {
	t.Parallel()
	if SnapshotIsolation != IsolationLevelSnapshot {
		t.Fatal("SnapshotIsolation != IsolationLevelSnapshot")
	}
	if SerializableIsolation != IsolationLevelSerializable {
		t.Fatal("SerializableIsolation != IsolationLevelSerializable")
	}
}

// ---------------------------------------------------------------------------
// CursorStreamingMode
// ---------------------------------------------------------------------------

func TestCursorStreamingMode_ToFDB(t *testing.T) {
	t.Parallel()
	tests := []struct {
		mode CursorStreamingMode
		want fdb.StreamingMode
	}{
		{StreamingModeSmall, fdb.StreamingModeSmall},
		{StreamingModeMedium, fdb.StreamingModeMedium},
		{StreamingModeLarge, fdb.StreamingModeLarge},
		{StreamingModeSerial, fdb.StreamingModeSerial},
		{StreamingModeWantAll, fdb.StreamingModeWantAll},
		{StreamingModeIterator, fdb.StreamingModeIterator},
	}
	for _, tt := range tests {
		if got := tt.mode.ToFDB(); got != tt.want {
			t.Errorf("CursorStreamingMode(%d).ToFDB() = %d, want %d", int(tt.mode), int(got), int(tt.want))
		}
	}
}

func TestCursorStreamingMode_ToFDB_UnknownDefaultsToIterator(t *testing.T) {
	t.Parallel()
	if got := CursorStreamingMode(99).ToFDB(); got != fdb.StreamingModeIterator {
		t.Fatalf("unknown mode mapped to %d, want Iterator (%d)", int(got), int(fdb.StreamingModeIterator))
	}
}

// ---------------------------------------------------------------------------
// ExecuteProperties — defaults
// ---------------------------------------------------------------------------

func TestDefaultExecuteProperties(t *testing.T) {
	t.Parallel()
	p := DefaultExecuteProperties()
	if p.IsolationLevel != IsolationLevelSerializable {
		t.Fatalf("IsolationLevel: got %v, want Serializable", p.IsolationLevel)
	}
	if p.ReturnedRowLimit != 0 {
		t.Fatalf("ReturnedRowLimit: got %d, want 0", p.ReturnedRowLimit)
	}
	if p.ScannedRecordsLimit != 0 {
		t.Fatalf("ScannedRecordsLimit: got %d", p.ScannedRecordsLimit)
	}
	if p.ScannedBytesLimit != 0 {
		t.Fatalf("ScannedBytesLimit: got %d", p.ScannedBytesLimit)
	}
	if p.TimeLimit != 0 {
		t.Fatalf("TimeLimit: got %v", p.TimeLimit)
	}
	if p.DefaultCursorStreamingMode != StreamingModeIterator {
		t.Fatalf("DefaultCursorStreamingMode: got %d", p.DefaultCursorStreamingMode)
	}
	if p.FailOnScanLimitReached {
		t.Fatal("FailOnScanLimitReached should be false")
	}
	if p.Skip != 0 {
		t.Fatalf("Skip: got %d", p.Skip)
	}
}

// ---------------------------------------------------------------------------
// ExecuteProperties — With* methods (value semantics)
// ---------------------------------------------------------------------------

func TestExecuteProperties_WithReturnedRowLimit(t *testing.T) {
	t.Parallel()
	orig := DefaultExecuteProperties()
	updated := orig.WithReturnedRowLimit(42)
	if updated.ReturnedRowLimit != 42 {
		t.Fatalf("updated: got %d, want 42", updated.ReturnedRowLimit)
	}
	if orig.ReturnedRowLimit != 0 {
		t.Fatal("original was mutated")
	}
}

func TestExecuteProperties_WithTimeLimit(t *testing.T) {
	t.Parallel()
	orig := DefaultExecuteProperties()
	updated := orig.WithTimeLimit(5 * time.Second)
	if updated.TimeLimit != 5*time.Second {
		t.Fatalf("updated: got %v", updated.TimeLimit)
	}
	if orig.TimeLimit != 0 {
		t.Fatal("original was mutated")
	}
}

func TestExecuteProperties_WithIsolationLevel(t *testing.T) {
	t.Parallel()
	orig := DefaultExecuteProperties()
	updated := orig.WithIsolationLevel(IsolationLevelSnapshot)
	if updated.IsolationLevel != IsolationLevelSnapshot {
		t.Fatalf("updated: got %v", updated.IsolationLevel)
	}
	if orig.IsolationLevel != IsolationLevelSerializable {
		t.Fatal("original was mutated")
	}
}

func TestExecuteProperties_WithScannedRecordsLimit(t *testing.T) {
	t.Parallel()
	orig := DefaultExecuteProperties()
	updated := orig.WithScannedRecordsLimit(1000)
	if updated.ScannedRecordsLimit != 1000 {
		t.Fatalf("updated: got %d", updated.ScannedRecordsLimit)
	}
	if orig.ScannedRecordsLimit != 0 {
		t.Fatal("original was mutated")
	}
}

func TestExecuteProperties_WithScannedBytesLimit(t *testing.T) {
	t.Parallel()
	orig := DefaultExecuteProperties()
	updated := orig.WithScannedBytesLimit(1 << 20)
	if updated.ScannedBytesLimit != 1<<20 {
		t.Fatalf("updated: got %d", updated.ScannedBytesLimit)
	}
	if orig.ScannedBytesLimit != 0 {
		t.Fatal("original was mutated")
	}
}

func TestExecuteProperties_WithSkip(t *testing.T) {
	t.Parallel()
	orig := DefaultExecuteProperties()
	updated := orig.WithSkip(10)
	if updated.Skip != 10 {
		t.Fatalf("updated: got %d", updated.Skip)
	}
	if orig.Skip != 0 {
		t.Fatal("original was mutated")
	}
}

// ---------------------------------------------------------------------------
// ExecuteProperties — ClearRowAndTimeLimits
// ---------------------------------------------------------------------------

func TestExecuteProperties_ClearRowAndTimeLimits(t *testing.T) {
	t.Parallel()
	p := DefaultExecuteProperties().
		WithReturnedRowLimit(50).
		WithTimeLimit(3 * time.Second).
		WithScannedRecordsLimit(200).
		WithSkip(5)
	cleared := p.ClearRowAndTimeLimits()
	if cleared.ReturnedRowLimit != 0 {
		t.Fatalf("ReturnedRowLimit: got %d, want 0", cleared.ReturnedRowLimit)
	}
	if cleared.TimeLimit != 0 {
		t.Fatalf("TimeLimit: got %v, want 0", cleared.TimeLimit)
	}
	// Preserved fields.
	if cleared.ScannedRecordsLimit != 200 {
		t.Fatalf("ScannedRecordsLimit should be preserved: got %d", cleared.ScannedRecordsLimit)
	}
	if cleared.Skip != 5 {
		t.Fatalf("Skip should be preserved: got %d", cleared.Skip)
	}
	// Original unchanged.
	if p.ReturnedRowLimit != 50 {
		t.Fatal("original ReturnedRowLimit was mutated")
	}
	if p.TimeLimit != 3*time.Second {
		t.Fatal("original TimeLimit was mutated")
	}
}

// ---------------------------------------------------------------------------
// ExecuteProperties — ClearSkipAndLimit
// ---------------------------------------------------------------------------

func TestExecuteProperties_ClearSkipAndLimit(t *testing.T) {
	t.Parallel()
	p := DefaultExecuteProperties().
		WithReturnedRowLimit(100).
		WithSkip(20).
		WithTimeLimit(2 * time.Second).
		WithScannedRecordsLimit(500)
	cleared := p.ClearSkipAndLimit()
	if cleared.Skip != 0 {
		t.Fatalf("Skip: got %d, want 0", cleared.Skip)
	}
	if cleared.ReturnedRowLimit != 0 {
		t.Fatalf("ReturnedRowLimit: got %d, want 0", cleared.ReturnedRowLimit)
	}
	// Preserved fields.
	if cleared.TimeLimit != 2*time.Second {
		t.Fatalf("TimeLimit should be preserved: got %v", cleared.TimeLimit)
	}
	if cleared.ScannedRecordsLimit != 500 {
		t.Fatalf("ScannedRecordsLimit should be preserved: got %d", cleared.ScannedRecordsLimit)
	}
	// Original unchanged.
	if p.Skip != 20 {
		t.Fatal("original Skip was mutated")
	}
	if p.ReturnedRowLimit != 100 {
		t.Fatal("original ReturnedRowLimit was mutated")
	}
}

// ---------------------------------------------------------------------------
// ExecuteProperties — chaining preserves all fields
// ---------------------------------------------------------------------------

func TestExecuteProperties_Chaining(t *testing.T) {
	t.Parallel()
	p := DefaultExecuteProperties().
		WithReturnedRowLimit(10).
		WithSkip(5).
		WithIsolationLevel(IsolationLevelSnapshot).
		WithTimeLimit(1 * time.Second).
		WithScannedRecordsLimit(300).
		WithScannedBytesLimit(4096)
	if p.ReturnedRowLimit != 10 {
		t.Fatalf("ReturnedRowLimit: %d", p.ReturnedRowLimit)
	}
	if p.Skip != 5 {
		t.Fatalf("Skip: %d", p.Skip)
	}
	if p.IsolationLevel != IsolationLevelSnapshot {
		t.Fatalf("IsolationLevel: %v", p.IsolationLevel)
	}
	if p.TimeLimit != 1*time.Second {
		t.Fatalf("TimeLimit: %v", p.TimeLimit)
	}
	if p.ScannedRecordsLimit != 300 {
		t.Fatalf("ScannedRecordsLimit: %d", p.ScannedRecordsLimit)
	}
	if p.ScannedBytesLimit != 4096 {
		t.Fatalf("ScannedBytesLimit: %d", p.ScannedBytesLimit)
	}
}

// ---------------------------------------------------------------------------
// ScanProperties — ForwardScan
// ---------------------------------------------------------------------------

func TestForwardScan(t *testing.T) {
	t.Parallel()
	s := ForwardScan()
	if s.IsReverse() {
		t.Fatal("ForwardScan should not be reverse")
	}
	if s.CursorStreamingMode != StreamingModeIterator {
		t.Fatalf("streaming mode: got %d, want Iterator", s.CursorStreamingMode)
	}
	if s.GetExecuteProperties() != DefaultExecuteProperties() {
		t.Fatal("ForwardScan should have default execute properties")
	}
}

func TestForwardScan_IndependentCopies(t *testing.T) {
	t.Parallel()
	a := ForwardScan()
	b := ForwardScan()
	mutated := a.WithReverse(true).WithStreamingMode(StreamingModeLarge)
	if mutated.IsReverse() != true {
		t.Fatal("mutated should be reverse")
	}
	if b.IsReverse() {
		t.Fatal("b should be unaffected by a's mutation")
	}
	if b.CursorStreamingMode != StreamingModeIterator {
		t.Fatal("b streaming mode should be unaffected")
	}
}

// ---------------------------------------------------------------------------
// ScanProperties — ReverseScan
// ---------------------------------------------------------------------------

func TestReverseScan(t *testing.T) {
	t.Parallel()
	s := ReverseScan()
	if !s.IsReverse() {
		t.Fatal("ReverseScan should be reverse")
	}
	if s.CursorStreamingMode != StreamingModeIterator {
		t.Fatalf("streaming mode: got %d", s.CursorStreamingMode)
	}
	if s.GetExecuteProperties() != DefaultExecuteProperties() {
		t.Fatal("ReverseScan should have default execute properties")
	}
}

// ---------------------------------------------------------------------------
// ScanProperties — NewScanProperties
// ---------------------------------------------------------------------------

func TestNewScanProperties(t *testing.T) {
	t.Parallel()
	ep := DefaultExecuteProperties()
	ep.DefaultCursorStreamingMode = StreamingModeWantAll
	s := NewScanProperties(ep)
	if s.IsReverse() {
		t.Fatal("NewScanProperties should default to forward")
	}
	if s.CursorStreamingMode != StreamingModeWantAll {
		t.Fatalf("should inherit streaming mode from execute props: got %d", s.CursorStreamingMode)
	}
	if s.GetExecuteProperties() != ep {
		t.Fatal("execute properties mismatch")
	}
}

func TestNewScanProperties_DefaultStreamingMode(t *testing.T) {
	t.Parallel()
	// Default execute properties use StreamingModeIterator.
	s := NewScanProperties(DefaultExecuteProperties())
	if s.CursorStreamingMode != StreamingModeIterator {
		t.Fatalf("got %d, want Iterator", s.CursorStreamingMode)
	}
}

// ---------------------------------------------------------------------------
// ScanProperties — With* methods (value semantics)
// ---------------------------------------------------------------------------

func TestScanProperties_WithReverse(t *testing.T) {
	t.Parallel()
	orig := ForwardScan()
	rev := orig.WithReverse(true)
	if !rev.IsReverse() {
		t.Fatal("WithReverse(true) should be reverse")
	}
	if orig.IsReverse() {
		t.Fatal("original should be unchanged")
	}
	// And back to false.
	fwd := rev.WithReverse(false)
	if fwd.IsReverse() {
		t.Fatal("WithReverse(false) should not be reverse")
	}
}

func TestScanProperties_WithStreamingMode(t *testing.T) {
	t.Parallel()
	orig := ForwardScan()
	updated := orig.WithStreamingMode(StreamingModeSerial)
	if updated.CursorStreamingMode != StreamingModeSerial {
		t.Fatalf("got %d", updated.CursorStreamingMode)
	}
	if orig.CursorStreamingMode != StreamingModeIterator {
		t.Fatal("original was mutated")
	}
}

func TestScanProperties_WithExecuteProperties(t *testing.T) {
	t.Parallel()
	orig := ForwardScan()
	ep := DefaultExecuteProperties().WithReturnedRowLimit(99)
	updated := orig.WithExecuteProperties(ep)
	if updated.GetExecuteProperties().ReturnedRowLimit != 99 {
		t.Fatalf("got %d", updated.GetExecuteProperties().ReturnedRowLimit)
	}
	if orig.GetExecuteProperties().ReturnedRowLimit != 0 {
		t.Fatal("original was mutated")
	}
}

// ---------------------------------------------------------------------------
// ScanProperties — IsReverse / GetExecuteProperties accessors
// ---------------------------------------------------------------------------

func TestScanProperties_IsReverse_MatchesField(t *testing.T) {
	t.Parallel()
	s := ScanProperties{Reverse: true}
	if !s.IsReverse() {
		t.Fatal("IsReverse should return true when Reverse=true")
	}
	s.Reverse = false
	if s.IsReverse() {
		t.Fatal("IsReverse should return false when Reverse=false")
	}
}

func TestScanProperties_GetExecuteProperties(t *testing.T) {
	t.Parallel()
	ep := DefaultExecuteProperties().WithReturnedRowLimit(7).WithSkip(3)
	s := NewScanProperties(ep)
	got := s.GetExecuteProperties()
	if got.ReturnedRowLimit != 7 {
		t.Fatalf("ReturnedRowLimit: %d", got.ReturnedRowLimit)
	}
	if got.Skip != 3 {
		t.Fatalf("Skip: %d", got.Skip)
	}
}

// ---------------------------------------------------------------------------
// Benchmarks
// ---------------------------------------------------------------------------

func BenchmarkDefaultExecuteProperties(b *testing.B) {
	var p ExecuteProperties
	for b.Loop() {
		p = DefaultExecuteProperties()
	}
	_ = p
}

func BenchmarkForwardScan(b *testing.B) {
	var s ScanProperties
	for b.Loop() {
		s = ForwardScan()
	}
	_ = s
}

func BenchmarkReverseScan(b *testing.B) {
	var s ScanProperties
	for b.Loop() {
		s = ReverseScan()
	}
	_ = s
}

func BenchmarkExecuteProperties_BuilderChain(b *testing.B) {
	var p ExecuteProperties
	for b.Loop() {
		p = DefaultExecuteProperties().
			WithReturnedRowLimit(100).
			WithSkip(10).
			WithIsolationLevel(IsolationLevelSnapshot).
			WithTimeLimit(5 * time.Second).
			WithScannedRecordsLimit(1000).
			WithScannedBytesLimit(1 << 20)
	}
	_ = p
}

func BenchmarkScanProperties_BuilderChain(b *testing.B) {
	var s ScanProperties
	for b.Loop() {
		s = ForwardScan().
			WithReverse(true).
			WithStreamingMode(StreamingModeWantAll).
			WithExecuteProperties(
				DefaultExecuteProperties().WithReturnedRowLimit(50),
			)
	}
	_ = s
}

func BenchmarkCursorStreamingMode_ToFDB(b *testing.B) {
	var m fdb.StreamingMode
	for b.Loop() {
		m = StreamingModeLarge.ToFDB()
	}
	_ = m
}

func BenchmarkIsolationLevel_IsSnapshot(b *testing.B) {
	var v bool
	for b.Loop() {
		v = IsolationLevelSnapshot.IsSnapshot()
	}
	_ = v
}

func BenchmarkExecuteProperties_ClearRowAndTimeLimits(b *testing.B) {
	base := DefaultExecuteProperties().
		WithReturnedRowLimit(50).
		WithTimeLimit(3 * time.Second)
	var p ExecuteProperties
	for b.Loop() {
		p = base.ClearRowAndTimeLimits()
	}
	_ = p
}
