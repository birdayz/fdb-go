package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestDecideMirrorBoundsStaleness pins the two thresholds against the clock.
// The soft bound decides whether to touch the network; the hard bound decides
// whether a verdict is allowed to exist at all.
func TestDecideMirrorBoundsStaleness(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		present bool
		age     time.Duration
		want    MirrorDecision
	}{
		{"absent", false, 0, MirrorMustRefresh},
		{"just fetched", true, time.Minute, MirrorUseAsIs},
		{"inside the refresh window", true, refreshAfter - time.Minute, MirrorUseAsIs},
		{"past the refresh window", true, refreshAfter + time.Minute, MirrorRefresh},
		{"inside the hard bound", true, hardStaleAfter - time.Hour, MirrorRefresh},
		{"past the hard bound", true, hardStaleAfter + time.Hour, MirrorMustRefresh},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := DecideMirror(tc.present, now.Add(-tc.age), now)
			if got != tc.want {
				t.Fatalf("DecideMirror(present=%t, age=%s) = %s, want %s", tc.present, tc.age, got, tc.want)
			}
		})
	}
}

// TestStalenessBoundIsEnforcedNotJustRecorded is requirement 3 in one test: a
// bound that exists but never refuses is decoration. Past the hard bound with
// the network down, the gate must decline to proceed.
func TestStalenessBoundIsEnforcedNotJustRecorded(t *testing.T) {
	t.Parallel()

	t.Run("inside the bound a failed refresh is survivable", func(t *testing.T) {
		t.Parallel()
		proceed, reason := MayProceedAfterFailedRefresh(true, now.Add(-(hardStaleAfter - time.Hour)), now)
		if !proceed {
			t.Fatalf("MayProceedAfterFailedRefresh(inside bound) = false (%s); a transient upstream "+
				"failure with a usable mirror must NOT present as a failed security gate", reason)
		}
	})

	t.Run("past the bound a failed refresh is fatal", func(t *testing.T) {
		t.Parallel()
		proceed, reason := MayProceedAfterFailedRefresh(true, now.Add(-(hardStaleAfter + time.Hour)), now)
		if proceed {
			t.Fatalf("MayProceedAfterFailedRefresh(past bound) = true; a gate that keeps reporting from an " +
				"unboundedly stale database is the silent-pass failure mode wearing a cache for a disguise")
		}
		if reason == "" {
			t.Fatal("refusal came with no reason")
		}
	})

	t.Run("with no mirror at all a failed refresh is fatal", func(t *testing.T) {
		t.Parallel()
		if proceed, _ := MayProceedAfterFailedRefresh(false, time.Time{}, now); proceed {
			t.Fatal("MayProceedAfterFailedRefresh(no mirror) = true; there is no database to scan against, " +
				"so there is no verdict to render")
		}
	})
}

// TestValidateDBRejectsDegenerateDatabases guards the mirror against the shape
// that measurably produces a silent pass: govulncheck aimed at an empty or
// half-extracted directory exits 0 and finds nothing.
func TestValidateDBRejectsDegenerateDatabases(t *testing.T) {
	t.Parallel()

	build := func(t *testing.T, f func(dir string)) string {
		t.Helper()
		dir := t.TempDir()
		f(dir)
		return dir
	}

	cases := map[string]func(dir string){
		"empty directory": func(dir string) {},
		"index only, no advisories": func(dir string) {
			writeIndex(dir, "2026-07-27T20:14:16Z")
		},
		"zero modified time": func(dir string) {
			writeIndex(dir, "0001-01-01T00:00:00Z")
			writeAdvisory(dir)
		},
		"missing modified field": func(dir string) {
			os.MkdirAll(filepath.Join(dir, "index"), 0o755)
			os.WriteFile(filepath.Join(dir, "index", "db.json"), []byte(`{}`), 0o644)
			os.WriteFile(filepath.Join(dir, "index", "modules.json"), []byte(`[]`), 0o644)
			writeAdvisory(dir)
		},
		"corrupt index": func(dir string) {
			os.MkdirAll(filepath.Join(dir, "index"), 0o755)
			os.WriteFile(filepath.Join(dir, "index", "db.json"), []byte(`{not json`), 0o644)
			writeAdvisory(dir)
		},
	}

	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := ValidateDB(build(t, f)); err == nil {
				t.Fatalf("ValidateDB(%s) = nil; a database in this state yields a zero-finding scan "+
					"that would be indistinguishable from a clean one", name)
			}
		})
	}

	t.Run("a well-formed database validates", func(t *testing.T) {
		t.Parallel()
		dir := build(t, func(dir string) {
			writeIndex(dir, "2026-07-27T20:14:16Z")
			writeAdvisory(dir)
		})
		if err := ValidateDB(dir); err != nil {
			t.Fatalf("ValidateDB(good) = %v, want nil", err)
		}
	})
}

// TestRefreshIsAtomic proves a failed or invalid download cannot replace a good
// mirror with a broken one. Extracting in place would leave a partial database
// behind, and a partial database scans clean.
func TestRefreshIsAtomic(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	m := Mirror{Root: root}

	good := func(dest string) error {
		writeIndex(dest, "2026-07-27T20:14:16Z")
		writeAdvisory(dest)
		return nil
	}
	if err := m.Refresh(now, good); err != nil {
		t.Fatalf("first Refresh: %v", err)
	}
	present, fetchedAt := m.State()
	if !present || !fetchedAt.Equal(now.UTC().Truncate(time.Second)) {
		t.Fatalf("State() after refresh = (%t, %s), want a present mirror stamped %s", present, fetchedAt, now)
	}

	for name, bad := range map[string]func(string) error{
		"download error":     func(string) error { return fmt.Errorf("403 Forbidden") },
		"empty extraction":   func(string) error { return nil },
		"partial extraction": func(dest string) error { writeIndex(dest, "2026-07-28T00:00:00Z"); return nil },
	} {
		t.Run(name, func(t *testing.T) {
			if err := m.Refresh(now.Add(time.Hour), bad); err == nil {
				t.Fatalf("Refresh(%s) = nil, want an error", name)
			}
			if err := ValidateDB(m.DBDir()); err != nil {
				t.Fatalf("after a failed %s the previously good mirror no longer validates: %v; "+
					"a failed refresh must never degrade a working database", name, err)
			}
		})
	}
}

// TestFetchRetriesTransientFailuresThenSucceeds pins the retry behaviour on the
// exact upstream response that motivated this work.
func TestFetchRetriesTransientFailuresThenSucceeds(t *testing.T) {
	t.Parallel()

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Write(zipFixture(t))
	}))
	defer srv.Close()

	dest := t.TempDir()
	fetch := FetchVulnDBZip(srv.Client(), srv.URL, 4, func(time.Duration) {})
	if err := fetch(dest); err != nil {
		t.Fatalf("fetch after two 403s = %v, want success", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
	if err := ValidateDB(dest); err != nil {
		t.Fatalf("extracted database does not validate: %v", err)
	}
}

// TestFetchGivesUpAndReportsRatherThanReturningEmpty pins that exhausting the
// retries is an ERROR. Returning nil with an empty destination would hand the
// caller a directory that scans clean.
func TestFetchGivesUpAndReportsRatherThanReturningEmpty(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	dest := t.TempDir()
	fetch := FetchVulnDBZip(srv.Client(), srv.URL, 3, func(time.Duration) {})
	err := fetch(dest)
	if err == nil {
		t.Fatal("fetch against a permanently refusing server = nil; an exhausted retry budget must be " +
			"an error, never a silently empty database")
	}
	if err := ValidateDB(dest); err == nil {
		t.Fatal("the destination validated after a wholly failed fetch")
	}
}

// TestExtractRejectsPathTraversal — the mirror is written into a persistent CI
// volume that also holds the Bazel caches.
func TestExtractRejectsPathTraversal(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("../escaped.json")
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("{}"))
	zw.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(buf.Bytes())
	}))
	defer srv.Close()

	root := t.TempDir()
	dest := filepath.Join(root, "dest")
	os.MkdirAll(dest, 0o755)
	fetch := FetchVulnDBZip(srv.Client(), srv.URL, 1, func(time.Duration) {})
	if err := fetch(dest); err == nil {
		t.Fatal("an archive entry escaping the destination was accepted")
	}
	if _, err := os.Stat(filepath.Join(root, "escaped.json")); err == nil {
		t.Fatal("the archive wrote outside the destination directory")
	}
}

func writeIndex(dir, modified string) {
	os.MkdirAll(filepath.Join(dir, "index"), 0o755)
	b, _ := json.Marshal(map[string]string{"modified": modified})
	os.WriteFile(filepath.Join(dir, "index", "db.json"), b, 0o644)
	os.WriteFile(filepath.Join(dir, "index", "modules.json"), []byte(`[]`), 0o644)
}

func writeAdvisory(dir string) {
	os.MkdirAll(filepath.Join(dir, "ID"), 0o755)
	os.WriteFile(filepath.Join(dir, "ID", "GO-2021-0113.json"), []byte(`{"id":"GO-2021-0113"}`), 0o644)
}

func zipFixture(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name, body string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	add("index/db.json", `{"modified":"2026-07-27T20:14:16Z"}`)
	add("index/modules.json", `[]`)
	add("ID/GO-2021-0113.json", `{"id":"GO-2021-0113"}`)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
