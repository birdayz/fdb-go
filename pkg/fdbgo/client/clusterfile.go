package client

import (
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Coordinator-token validation, ported 1:1 from C++ (RFC-111 §8). A token is a
// valid coordinator iff isHostnameToken || isNetworkAddressToken — exactly C++
// `Hostname::isHostname(tok) || NetworkAddress::parse(tok)`
// (fdbclient/MonitorLeader.actor.cpp:106-118). The two regexes are C++'s verbatim
// (flow/Hostname.actor.cpp:31-44); Go `\w` == ECMAScript `\w` for these inputs. The
// `(:tls)?` tail is optional so the helpers accept a token whether or not the
// caller already stripped the ":tls" suffix.
var (
	// validation: a hostname coordinator — dotted `[\w\-]` labels then `:port`.
	hostnameTokenRe = regexp.MustCompile(`^([\w\-]+\.?)+:[\d]+(:tls)?$`)
	// ipv4Validation: 4+ dotted 1-3 digit groups then `:port` — used only to
	// EXCLUDE IP-shaped tokens from the hostname class (matches C++ isHostname).
	ipv4TokenRe = regexp.MustCompile(`^([\d]{1,3}\.?){4,}:[\d]+(:tls)?$`)
)

// isHostnameToken mirrors C++ Hostname::isHostname: not IP-shaped, but matching the
// hostname regex.
func isHostnameToken(s string) bool {
	return !ipv4TokenRe.MatchString(s) && hostnameTokenRe.MatchString(s)
}

// isNetworkAddressToken reports whether s (a host:port with any ":tls" suffix
// already stripped) is a valid IP NetworkAddress. This is a deliberate one-way
// SAFE tightening over C++'s NetworkAddress::parse (RFC-111 §8): C++'s
// sscanf/std::stoi accept-and-truncate out-of-range octets and trailing-junk ports
// (999.999.999.999 → 234.234.234.231); net.ParseIP + an all-digits port reject
// them. Go-accept is a strict subset of C++-accept, so the persist path can never
// write a token C++ can't parse; the over-rejections are unreachable because real
// forward/file strings are always toString()-normalized (octets 0-255).
func isNetworkAddressToken(s string) bool {
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return false
	}
	if net.ParseIP(host) == nil {
		return false
	}
	return allDigits(port)
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// String renders the connection string in C++ ClusterConnectionString::toString
// byte-order (MonitorLeader.actor.cpp:438-453): `description:id@`, then all IP
// coordinators in original order, then all hostname coordinators in original order,
// comma-joined, each with ":tls" re-appended when UseTLS. This is the
// cross-tool-shared on-disk form; it must be byte-identical to what a C++/Java
// client would write so they can read the file back. Equality between two
// ClusterFiles is defined as a.String() == b.String() (matches C++ upToDate's
// toString() compare) — the serialization is the single source of truth.
func (cf *ClusterFile) String() string {
	var sb strings.Builder
	sb.WriteString(cf.Description)
	sb.WriteByte(':')
	sb.WriteString(cf.ID)
	sb.WriteByte('@')
	first := true
	emit := func(addr string) {
		if !first {
			sb.WriteByte(',')
		}
		first = false
		sb.WriteString(addr)
		if cf.UseTLS {
			sb.WriteString(":tls")
		}
	}
	// IP coordinators first (C++ emits `coords` before `hostnames`).
	for _, c := range cf.Coordinators {
		if !isHostnameToken(c) {
			emit(c)
		}
	}
	for _, c := range cf.Coordinators {
		if isHostnameToken(c) {
			emit(c)
		}
	}
	return sb.String()
}

// connRecord owns the mutable active connection string and the file-vs-memory
// distinction — the Go analog of C++ IClusterConnectionRecord
// (ClusterConnectionFile when filename != "", ClusterConnectionMemoryRecord
// otherwise). All mutation goes through the locked methods; the mutex serializes
// the coordinator-fan-out readers against the single topology writer and makes
// Path B's read-compare-swap atomic (RFC-111 §1).
type connRecord struct {
	mu       sync.Mutex
	cf       *ClusterFile
	filename string // "" => memory-only (no persistence)
	logger   *slog.Logger
}

func newConnRecord(cf *ClusterFile, filename string, logger *slog.Logger) *connRecord {
	return &connRecord{cf: cf, filename: filename, logger: logger}
}

// get snapshots the active connection string under the lock.
func (r *connRecord) get() *ClusterFile {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cf
}

// setAndPersist swaps the in-memory connection string UNCONDITIONALLY, then
// best-effort persists it to the cluster file (Path A — forward adopt). A persist
// error (EROFS/EPERM/...) is logged and swallowed: in-memory adoption already took
// effect and the connection must never be failed by a file-write error — matching
// C++ ClusterConnectionFile::persist, which catches the error and returns false,
// and setAndPersistConnectionString, which discards the bool
// (ClusterConnectionFile.actor.cpp:54-58, 148-182). The on-disk write is skipped
// when the file already equals cf (idempotent, matching C++ persist's up-to-date
// short-circuit).
func (r *connRecord) setAndPersist(cf *ClusterFile) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cf = cf
	if r.filename == "" {
		return // memory-only (ClusterConnectionMemoryRecord)
	}
	if stored, err := ParseClusterFile(r.filename); err == nil && stored.String() == cf.String() {
		return // already up-to-date
	}
	if err := persistClusterFile(r.filename, cf); err != nil && r.logger != nil {
		r.logger.Warn("fdbgo: failed to persist cluster file (coordinator change still applied in memory)",
			"filename", r.filename, "error", err)
	}
}

// adoptStoredIfChanged is Path B (file re-read) as ONE critical section: re-read
// the on-disk file and, if it changed and has >=1 coordinator, swap the in-memory
// connection string. The file is already authoritative — no re-persist. Returns
// whether it changed. Parse/read errors are logged and treated as "no change"
// (C++ upToDate swallows file errors and returns false,
// ClusterConnectionFile.actor.cpp:80-83). Doing the read-compare-swap under a
// single lock hold eliminates the TOCTOU window between the topology and bootstrap
// writers.
func (r *connRecord) adoptStoredIfChanged() (changed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.filename == "" {
		return false
	}
	stored, err := ParseClusterFile(r.filename)
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("fdbgo: cluster file unreadable on re-read", "filename", r.filename, "error", err)
		}
		return false
	}
	if len(stored.Coordinators) == 0 || stored.String() == r.cf.String() {
		return false
	}
	r.cf = stored
	return true
}

// persistClusterFile writes cf to the cluster file in the exact C++
// ClusterConnectionFile::persist byte layout (ClusterConnectionFile.actor.cpp:153):
// the two `#` header lines + cs.toString() + a trailing newline.
func persistClusterFile(filename string, cf *ClusterFile) error {
	content := "# DO NOT EDIT!\n# This file is auto-generated, it is not to be edited by hand\n" +
		cf.String() + "\n"
	return atomicReplace(filename, []byte(content))
}

// atomicReplace writes content to filename via a temp file + durable rename
// (port of flow's atomicReplace) so a concurrent reader never sees a torn file.
func atomicReplace(filename string, content []byte) error {
	dir := filepath.Dir(filename)
	tmp, err := os.CreateTemp(dir, ".fdb.cluster.tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, filename); err != nil {
		return err
	}
	// fsync the directory so the rename survives a crash.
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
