package client

import (
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
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

// validClusterKeyPart ports C++ ClusterConnectionString::parseKey char validation
// (MonitorLeader.actor.cpp:420-432): the description allows [a-zA-Z0-9_], the id
// allows [a-zA-Z0-9]. Rejecting here keeps Go-accept a subset of C++-accept, so a
// persisted cluster key is always parseable by a C++/Java client (RFC-111 §8).
func validClusterKeyPart(s string, allowUnderscore bool) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case allowUnderscore && c == '_':
		default:
			return false
		}
	}
	return true
}

// coordDedupKey returns the canonical duplicate-detection key for a coordinator
// (host:port, ":tls" already stripped). For an IP coordinator it normalizes the
// parsed IP and port so leading-zero ports and compressed/expanded IPv6 collide
// the way C++ ClusterConnectionString's parsed-NetworkAddress set does
// (MonitorLeader.actor.cpp:115-121) — a raw-string map would miss those. For a
// hostname it is the verbatim string (C++ Hostname equality is string-based).
func coordDedupKey(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return addr // hostname — not an IP
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		return addr
	}
	return ip.String() + "/" + strconv.Itoa(p) // canonical IP + port
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
	// dirty is the C++ IClusterConnectionRecord.connectionStringNeedsPersisted
	// flag: set when a forward was adopted in memory but not yet written to disk,
	// cleared once persistIfDirty has run (whether or not the write succeeded —
	// best-effort, matches C++ persist's setPersisted()). Persisting is DEFERRED
	// until the new coordinators are confirmed reachable (a successful non-forward
	// connect), so a forward to a dead set never overwrites the shared cluster file
	// (C++ makeIntermediateRecord is in-memory; setAndPersistConnectionString runs
	// only after a normal reply — MonitorLeader.actor.cpp:944-959).
	dirty bool
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

// setInMemory swaps the in-memory connection string and marks it needs-persist
// (Path A — forward adopt). The file is NOT written here: persistence is deferred
// to persistIfDirty after the new coordinators answer, so an unreachable forwarded
// set never clobbers the cross-tool shared cluster file (C++ makeIntermediateRecord,
// MonitorLeader.actor.cpp:944).
func (r *connRecord) setInMemory(cf *ClusterFile) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cf = cf
	r.dirty = true
}

// persistIfDirty writes the in-memory connection string to the cluster file when a
// previously-adopted forward has not yet been persisted — called only after a
// successful non-forward connect, so the new coordinators are known reachable
// (C++ setAndPersistConnectionString at MonitorLeader.actor.cpp:957, gated on
// connRecord != intermediateConnRecord). Best-effort: the dirty flag is cleared
// regardless of the write outcome (matches C++ persist's setPersisted()), and a
// write error (EROFS/EPERM/...) is logged + swallowed — the in-memory string is
// already correct and the connection is never failed by a file-write error
// (ClusterConnectionFile.actor.cpp:148-182). The write is skipped when the file is
// already up-to-date.
func (r *connRecord) persistIfDirty() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.dirty {
		return
	}
	r.dirty = false
	if r.filename == "" {
		return // memory-only (ClusterConnectionMemoryRecord)
	}
	if stored, err := ParseClusterFile(r.filename); err == nil && stored.String() == r.cf.String() {
		return // already up-to-date
	}
	if err := persistClusterFile(r.filename, r.cf); err != nil && r.logger != nil {
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
// (port of flow's atomicReplace, Platform.actor.cpp) so a concurrent reader never
// sees a torn file. The existing file's mode (and best-effort uid/gid) are carried
// onto the replacement: a co-located C++/Java client or tool may read this shared
// file under a different user, and os.CreateTemp's default 0600 would otherwise
// silently tighten a 0644 cluster file and lock them out.
func atomicReplace(filename string, content []byte) error {
	dir := filepath.Dir(filename)

	mode := os.FileMode(0o644)
	uid, gid := -1, -1
	if fi, err := os.Stat(filename); err == nil {
		mode = fi.Mode().Perm()
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			uid, gid = int(st.Uid), int(st.Gid)
		}
	}

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
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	if uid >= 0 {
		// Best-effort, and a deliberate divergence: C++ atomicReplace hard-fails the
		// whole replace on a chown error (Platform.actor.cpp), abandoning the write so
		// the original stays intact. We keep the write — the mode (which governs
		// readability) is already preserved, so the file stays parseable by every
		// client; only ownership may differ in the rare cross-user case. chown-to-self
		// (the common single-service-user deployment) always succeeds, so they match.
		_ = os.Chown(tmpName, uid, gid)
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
