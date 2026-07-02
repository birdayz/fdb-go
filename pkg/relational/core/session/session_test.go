package session

import (
	"testing"
	"time"

	"fdb.dev/pkg/relational/api"
)

// Tests for the Session helpers:
// SchemaCacheKey, InvalidateSchema, ResetSchemaCache, StatementNow,
// BeginStatement. Pure in-memory state — no FDB required.

// The cache stores api.Schema values by key; tests don't inspect the
// stored value, just whether the right keys are present / removed.
// Using nil api.Schema as sentinel keeps the tests free of a fake
// implementation that would drift as the Schema interface evolves.
var nilSchema api.Schema = nil

func TestSchemaCacheKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		dbPath, schema string
		want           string
	}{
		{"", "", "\x00"},
		{"/main", "public", "/main\x00public"},
		{"/a/b", "c", "/a/b\x00c"},
	}
	for _, c := range cases {
		if got := SchemaCacheKey(c.dbPath, c.schema); got != c.want {
			t.Errorf("SchemaCacheKey(%q, %q) = %q, want %q", c.dbPath, c.schema, got, c.want)
		}
	}
}

func TestSchemaCacheKeyDelimiter(t *testing.T) {
	t.Parallel()
	// Keys must not collide when splitting. `dbPath` ends with `\x00` +
	// schema — since FDB paths don't contain `\x00`, `/a\x00` + `b`
	// shouldn't exist as a valid input; test that distinct inputs
	// produce distinct keys regardless.
	k1 := SchemaCacheKey("/a", "bc")
	k2 := SchemaCacheKey("/ab", "c")
	if k1 == k2 {
		t.Errorf("SchemaCacheKey should distinguish /a+bc from /ab+c: both = %q", k1)
	}
}

func TestSession_InvalidateSchema(t *testing.T) {
	t.Parallel()
	s := &Session{SchemaCache: map[string]api.Schema{
		SchemaCacheKey("/main", "public"): nilSchema,
		SchemaCacheKey("/main", "other"):  nilSchema,
	}}
	s.InvalidateSchema("/main", "public")
	if _, ok := s.SchemaCache[SchemaCacheKey("/main", "public")]; ok {
		t.Error("InvalidateSchema did not remove the entry")
	}
	if _, ok := s.SchemaCache[SchemaCacheKey("/main", "other")]; !ok {
		t.Error("InvalidateSchema removed an unrelated entry")
	}
}

func TestSession_InvalidateSchema_MissingIsNoOp(t *testing.T) {
	t.Parallel()
	s := &Session{SchemaCache: map[string]api.Schema{}}
	// Should not panic or error on a missing key.
	s.InvalidateSchema("/nonexistent", "nowhere")
	if len(s.SchemaCache) != 0 {
		t.Errorf("InvalidateSchema on missing key mutated cache: %v", s.SchemaCache)
	}
}

func TestSession_ResetSchemaCache(t *testing.T) {
	t.Parallel()
	s := &Session{SchemaCache: map[string]api.Schema{
		"stale1": nilSchema,
		"stale2": nilSchema,
	}}
	s.ResetSchemaCache()
	if len(s.SchemaCache) != 0 {
		t.Errorf("ResetSchemaCache did not empty the cache: %v", s.SchemaCache)
	}
	// Post-reset cache must still be usable (non-nil map).
	s.SchemaCache[SchemaCacheKey("/a", "b")] = nilSchema
	if len(s.SchemaCache) != 1 {
		t.Errorf("cache not usable after Reset: %v", s.SchemaCache)
	}
}

func TestSession_StatementNow_ZeroFallsBackToWall(t *testing.T) {
	t.Parallel()
	s := &Session{} // StatementTime is zero
	before := time.Now().UTC().Add(-time.Second)
	got := s.StatementNow()
	after := time.Now().UTC().Add(time.Second)
	if got.Before(before) || got.After(after) {
		t.Errorf("StatementNow fallback outside wall-clock window: got %v, window [%v, %v]", got, before, after)
	}
}

func TestSession_StatementNow_UsesPinnedTime(t *testing.T) {
	t.Parallel()
	pinned := time.Date(2024, 1, 15, 12, 30, 0, 0, time.UTC)
	s := &Session{StatementTime: pinned}
	if got := s.StatementNow(); !got.Equal(pinned) {
		t.Errorf("StatementNow returned %v, want pinned %v", got, pinned)
	}
}

func TestSession_StatementNow_NilReceiver(t *testing.T) {
	t.Parallel()
	// Defensive path: evaluators may call StatementNow on a nil *Session
	// (out-of-SQL contexts). Must not panic; return wall-clock.
	var s *Session
	before := time.Now().UTC().Add(-time.Second)
	got := s.StatementNow()
	after := time.Now().UTC().Add(time.Second)
	if got.Before(before) || got.After(after) {
		t.Errorf("StatementNow on nil Session outside wall-clock window: got %v", got)
	}
}

func TestSession_BeginStatement_SetsAndRestores(t *testing.T) {
	t.Parallel()
	s := &Session{}
	if !s.StatementTime.IsZero() {
		t.Fatal("precondition: StatementTime should start zero")
	}
	before := time.Now().UTC()
	pop := s.BeginStatement()
	if s.StatementTime.IsZero() {
		t.Error("BeginStatement did not set StatementTime")
	}
	if s.StatementTime.Before(before) {
		t.Errorf("StatementTime (%v) is before BeginStatement call (%v)", s.StatementTime, before)
	}
	// Calls before pop see the pinned value.
	first := s.StatementNow()
	second := s.StatementNow()
	if !first.Equal(second) {
		t.Errorf("StatementNow returned different values within the same statement: %v vs %v", first, second)
	}
	pop()
	if !s.StatementTime.IsZero() {
		t.Errorf("pop did not restore StatementTime to zero: %v", s.StatementTime)
	}
}

func TestSession_BeginStatement_Nested(t *testing.T) {
	t.Parallel()
	s := &Session{}
	outer := s.BeginStatement()
	outerTime := s.StatementTime
	time.Sleep(2 * time.Millisecond) // guarantee a different inner timestamp
	inner := s.BeginStatement()
	innerTime := s.StatementTime
	if innerTime.Equal(outerTime) {
		t.Error("nested BeginStatement produced the same timestamp — clock resolution?")
	}
	if !s.StatementNow().Equal(innerTime) {
		t.Error("StatementNow should return inner timestamp while inner is active")
	}
	inner()
	if !s.StatementTime.Equal(outerTime) {
		t.Errorf("inner pop did not restore outer timestamp: got %v, want %v", s.StatementTime, outerTime)
	}
	outer()
	if !s.StatementTime.IsZero() {
		t.Error("outer pop did not restore zero")
	}
}

func TestNew_InitializesSchemaCache(t *testing.T) {
	t.Parallel()
	s := New(nil, nil, nil, nil)
	if s.SchemaCache == nil {
		t.Error("New() did not initialize SchemaCache — callers would nil-panic on Set")
	}
	// Must be usable immediately without calling ResetSchemaCache.
	s.SchemaCache[SchemaCacheKey("/a", "b")] = nilSchema
}
