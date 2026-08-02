package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The mirror exists because govulncheck's default behaviour is structurally
// incompatible with a multi-runner self-hosted fleet.
//
// Pointed at https://vuln.go.dev, govulncheck fetches per-advisory documents
// (ID/GO-YYYY-NNNN.json.gz) on every invocation. That is hundreds of requests
// per job. With several runners serving concurrently from a small block of
// addresses, the fleet reads as a scraper and upstream answers 403, which the
// job then reports as a failed security gate. Nothing about that failure has
// anything to do with the code under test, and it recurs by construction.
//
// Mirroring collapses the request count for a full refresh from hundreds to
// ONE (vulndb.zip, about 3 MB, carrying the same v1 schema layout govulncheck
// reads directly), and in the steady state to ZERO, because a mirror inside the
// refresh window is used without touching the network at all.
//
// The mirror is per-runner rather than fleet-shared on purpose: /mnt/ci-data is
// an attached volume owned by a single pool box (infra/migrate-ci-volume.sh),
// so a scheduled job that refreshed "the" mirror would warm exactly one runner
// and leave the rest cold. Self-healing on use needs no scheduled job, no
// shared storage, and no assumption about which box picks up a given PR.
const vulnDBZipURL = "https://vuln.go.dev/vulndb.zip"

const (
	// refreshAfter is the SOFT bound: past this age the mirror is still used if
	// the network refuses, but a refresh is attempted first. Six hours puts the
	// fleet-wide refresh traffic at a handful of 3 MB requests per day, which
	// is not a rate that any upstream objects to.
	refreshAfter = 6 * time.Hour

	// hardStaleAfter is the HARD bound, and it is the reason a cache is
	// allowed to exist here at all. Inside it, a failed refresh is survivable
	// and the gate keeps reporting on data that is still meaningful. Outside
	// it, the gate REFUSES to report — a stale-enough database cannot
	// distinguish "no vulnerabilities" from "no recent advisories fetched",
	// and a gate quietly answering the second question while appearing to
	// answer the first is worse than one that fails.
	hardStaleAfter = 7 * 24 * time.Hour
)

// MirrorDecision is what to do about the on-disk mirror before scanning.
type MirrorDecision int

const (
	// MirrorUseAsIs means the mirror is inside the refresh window: scan
	// offline, touch nothing.
	MirrorUseAsIs MirrorDecision = iota

	// MirrorRefresh means the mirror is past the soft bound. Try to refresh,
	// but a failure is not fatal — the existing copy is still inside the hard
	// bound.
	MirrorRefresh

	// MirrorMustRefresh means there is no usable mirror: either none exists or
	// the one that does is past the hard bound. A failed refresh here is fatal,
	// because there is no database to render a verdict from.
	MirrorMustRefresh
)

func (d MirrorDecision) String() string {
	switch d {
	case MirrorUseAsIs:
		return "use-as-is"
	case MirrorRefresh:
		return "refresh-preferred"
	case MirrorMustRefresh:
		return "refresh-required"
	}
	return fmt.Sprintf("MirrorDecision(%d)", int(d))
}

// DecideMirror chooses what to do given the mirror's presence and fetch age.
//
// Age is measured from when WE last successfully synced, not from the
// database's own publication stamp. Those measure different things and only
// the first is under this repo's control: upstream can legitimately publish
// nothing for days, which must not be indistinguishable from a fleet that has
// stopped fetching. The publication stamp is bounded separately, and much more
// loosely, in classify.go.
func DecideMirror(present bool, fetchedAt, now time.Time) MirrorDecision {
	if !present || fetchedAt.IsZero() {
		return MirrorMustRefresh
	}
	age := now.Sub(fetchedAt)
	switch {
	case age > hardStaleAfter:
		return MirrorMustRefresh
	case age > refreshAfter:
		return MirrorRefresh
	default:
		return MirrorUseAsIs
	}
}

// MayProceedAfterFailedRefresh decides whether a refresh failure is survivable.
//
// This single function is what keeps a transient upstream 403 from presenting
// as a failed security gate: with a mirror inside the hard bound there is still
// a real database to scan against, so the scan proceeds and the fetch failure
// is reported as a warning rather than an error. It is also what stops that
// leniency from becoming a silent pass, because outside the hard bound — or
// with no mirror at all — it says no, and the caller has nothing to report a
// clean verdict from.
func MayProceedAfterFailedRefresh(present bool, fetchedAt, now time.Time) (bool, string) {
	if !present || fetchedAt.IsZero() {
		return false, "no local vulnerability database mirror exists and it could not be fetched, so there is nothing to scan against"
	}
	age := now.Sub(fetchedAt)
	if age > hardStaleAfter {
		return false, fmt.Sprintf("the local vulnerability database mirror was last synced %s ago, beyond the %s bound, and it could not be refreshed",
			age.Round(time.Minute), hardStaleAfter)
	}
	return true, fmt.Sprintf("the vulnerability database could not be refreshed; scanning against the local mirror synced %s ago (within the %s bound)",
		age.Round(time.Minute), hardStaleAfter)
}

// Mirror is an on-disk copy of the Go vulnerability database.
type Mirror struct {
	Root string
}

// DBDir is the directory govulncheck's -db flag points at.
func (m Mirror) DBDir() string { return filepath.Join(m.Root, "db") }

// stampPath records when the mirror was last SUCCESSFULLY synced. It is written
// only after a downloaded copy has been validated and swapped into place, so
// its presence is proof of a complete mirror rather than of an attempt.
func (m Mirror) stampPath() string { return filepath.Join(m.Root, "fetched-at") }

// State reports whether a usable mirror exists and when it was synced.
func (m Mirror) State() (present bool, fetchedAt time.Time) {
	if err := ValidateDB(m.DBDir()); err != nil {
		return false, time.Time{}
	}
	raw, err := os.ReadFile(m.stampPath())
	if err != nil {
		return false, time.Time{}
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(raw)))
	if err != nil {
		return false, time.Time{}
	}
	return true, t
}

// ValidateDB checks that a directory really is a v1-schema vulnerability
// database, and is the guard that makes the atomic swap below worth doing.
//
// Without it the mirror has a silent-pass shape of its own: govulncheck aimed
// at an empty or half-extracted directory exits 0 and finds nothing. Classify
// catches that after the fact via the zero-time attestation, but a mirror that
// never becomes corrupt in the first place is the stronger guarantee, so a
// download is validated BEFORE it is allowed to replace a good copy.
func ValidateDB(dir string) error {
	idx := filepath.Join(dir, "index", "db.json")
	raw, err := os.ReadFile(idx)
	if err != nil {
		return fmt.Errorf("reading %s: %w", idx, err)
	}
	var meta struct {
		Modified string `json:"modified"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return fmt.Errorf("parsing %s: %w", idx, err)
	}
	if meta.Modified == "" {
		return fmt.Errorf("%s carries no modified timestamp", idx)
	}
	t, err := time.Parse(time.RFC3339, meta.Modified)
	if err != nil {
		return fmt.Errorf("parsing modified %q in %s: %w", meta.Modified, idx, err)
	}
	if t.IsZero() {
		return fmt.Errorf("%s reports the zero time as modified", idx)
	}
	if _, err := os.Stat(filepath.Join(dir, "index", "modules.json")); err != nil {
		return fmt.Errorf("missing index/modules.json: %w", err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "ID"))
	if err != nil {
		return fmt.Errorf("reading ID directory: %w", err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("the ID directory is empty, so the database holds no advisories")
	}
	return nil
}

// Refresh downloads, validates and atomically installs a new mirror.
//
// The staging-then-rename shape matters: extracting in place would leave a
// partially written database behind on any interruption, and that is a
// database govulncheck will happily scan against and report nothing from.
func (m Mirror) Refresh(now time.Time, fetch func(dest string) error) error {
	if err := os.MkdirAll(m.Root, 0o755); err != nil {
		return fmt.Errorf("creating mirror root: %w", err)
	}
	staging, err := os.MkdirTemp(m.Root, "staging-")
	if err != nil {
		return fmt.Errorf("creating staging dir: %w", err)
	}
	defer os.RemoveAll(staging)

	if err := fetch(staging); err != nil {
		return err
	}
	if err := ValidateDB(staging); err != nil {
		return fmt.Errorf("the downloaded database did not validate, refusing to install it: %w", err)
	}

	// Drop the stamp first, then swap. A stamp that briefly predates its
	// database only makes the mirror look older than it is, which errs toward
	// refreshing again; the reverse would let a stale database claim freshness.
	if err := os.WriteFile(m.stampPath(), []byte(now.UTC().Format(time.RFC3339)), 0o644); err != nil {
		return fmt.Errorf("writing fetch stamp: %w", err)
	}

	old := m.DBDir() + ".old"
	_ = os.RemoveAll(old)
	if _, err := os.Stat(m.DBDir()); err == nil {
		if err := os.Rename(m.DBDir(), old); err != nil {
			return fmt.Errorf("moving previous mirror aside: %w", err)
		}
	}
	if err := os.Rename(staging, m.DBDir()); err != nil {
		_ = os.Rename(old, m.DBDir())
		return fmt.Errorf("installing new mirror: %w", err)
	}
	_ = os.RemoveAll(old)
	return nil
}

// FetchVulnDBZip downloads and extracts the published database archive.
//
// Retries use exponential backoff because the failure this whole file exists to
// absorb — upstream rate limiting — is time-dependent in a way a real finding
// never is. Retrying is safe precisely because it happens BEFORE any scan: this
// code never retries a govulncheck verdict, only the fetch that feeds one.
func FetchVulnDBZip(client *http.Client, url string, attempts int, sleep func(time.Duration)) func(string) error {
	return func(dest string) error {
		var lastErr error
		backoff := time.Second
		for i := 0; i < attempts; i++ {
			if i > 0 {
				sleep(backoff)
				backoff *= 4
			}
			err := downloadAndExtract(client, url, dest)
			if err == nil {
				return nil
			}
			lastErr = err
		}
		return fmt.Errorf("fetching %s failed after %d attempts: %w", url, attempts, lastErr)
	}
}

func downloadAndExtract(client *http.Client, url, dest string) error {
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s returned %s", url, resp.Status)
	}

	tmp, err := os.CreateTemp("", "vulndb-*.zip")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	size, err := io.Copy(tmp, resp.Body)
	if err != nil {
		return fmt.Errorf("reading archive body: %w", err)
	}
	zr, err := zip.NewReader(tmp, size)
	if err != nil {
		return fmt.Errorf("opening archive: %w", err)
	}
	for _, f := range zr.File {
		if err := extractOne(f, dest); err != nil {
			return err
		}
	}
	return nil
}

func extractOne(f *zip.File, dest string) error {
	// Reject path traversal rather than sanitising it: this writes into a
	// persistent CI volume shared with the Bazel caches, and an archive
	// carrying such an entry is not one to trust the rest of. Checking the
	// joined path alone is not enough — joining a cleaned "/../x" lands back
	// inside the destination, so the escape attempt disappears instead of
	// being reported. The raw name is what has to be judged.
	// Directory entries legitimately carry a trailing slash, which Clean strips.
	name := strings.TrimSuffix(f.Name, "/")
	if name == "" || name != filepath.Clean(name) || filepath.IsAbs(name) || strings.HasPrefix(name, "..") {
		return fmt.Errorf("archive entry %q is not a plain relative path", f.Name)
	}
	target := filepath.Join(dest, name)
	if !strings.HasPrefix(target, filepath.Clean(dest)+string(os.PathSeparator)) {
		return fmt.Errorf("archive entry %q escapes the destination directory", f.Name)
	}
	if f.FileInfo().IsDir() {
		return os.MkdirAll(target, 0o755)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, rc); err != nil {
		return err
	}
	return nil
}
