package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"fdb.dev/pkg/fdbgo/fdb"
)

const envRefused = "inherited FDB_NETWORK_OPTION_* environment variables are not allowed"

// The rule that bound every mapping of the client to the sealed artifact.
const (
	// exactStat: each mapping names the artifact's own st_dev.
	exactStat = "exact_stat"
	// btrfsSuperblock: mountinfo proves the artifact's mount is btrfs, whose mappings
	// name the superblock device rather than the subvolume's st_dev.
	btrfsSuperblock = "btrfs_superblock"
)

// nativeSteps are the native opener's checks and calls, as seams for its order tests.
type nativeSteps struct {
	environ    func() []string
	maps       func() ([]byte, error)
	mountinfo  func() ([]byte, error)
	apiVersion func(int) error
	disableMVC func() error
	open       func(clusterFile string) (fdb.BackendDatabase, error)
	pin        string
}

// nativeOpener opens libfdb_c handles: the environment and the mapped client are
// checked on every open; API 730 and network option 60 are set once, before the
// first Open starts the process's one network.
type nativeOpener struct {
	nativeSteps
	mu         sync.Mutex
	configured bool
}

// openBackend returns the handle and the rule that proved this open's mapped client.
func (o *nativeOpener) openBackend(ctx context.Context, library, clusterFile string) (fdb.BackendDatabase, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if err := rejectNetworkEnv(o.environ()); err != nil {
		return nil, "", err
	}
	maps, err := o.maps()
	if err != nil {
		return nil, "", fault("client", "cannot read the process mappings")
	}
	mounts, err := o.mountinfo()
	if err != nil {
		return nil, "", fault("client", "cannot read the process mounts")
	}
	rule, err := checkMappedClient(string(maps), string(mounts), library, o.pin)
	if err != nil {
		return nil, "", err
	}
	if err := o.configure(); err != nil {
		return nil, "", err
	}
	db, err := o.open(clusterFile)
	if err != nil {
		return nil, "", err
	}
	// Open takes no context: a cancellation it outlived still releases the handle.
	if err := ctx.Err(); err != nil {
		db.Close()
		return nil, "", err
	}
	return db, rule, nil
}

func (o *nativeOpener) configure() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.configured {
		return nil
	}
	if err := o.apiVersion(730); err != nil {
		return err
	}
	if err := o.disableMVC(); err != nil {
		return err
	}
	o.configured = true
	return nil
}

// rejectNetworkEnv refuses any inherited FDB_NETWORK_OPTION_ key, empty-valued too:
// libfdb_c would apply it at network setup. The error names no key or value.
func rejectNetworkEnv(env []string) error {
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(key, "FDB_NETWORK_OPTION_") {
			return refuse(envRefused)
		}
	}
	return nil
}

// device is a Linux device number as major:minor.
type device struct{ major, minor uint64 }

// statDevice splits st_dev the way glibc's gnu_dev_major and gnu_dev_minor do.
func statDevice(dev uint64) device {
	return device{major: (dev>>8)&0xfff | (dev>>32)&^uint64(0xfff), minor: dev&0xff | (dev>>12)&^uint64(0xff)}
}

// mappedIdentity parses a maps line's hex major:minor device and decimal inode.
func mappedIdentity(dev, inode string) (device, uint64, bool) {
	major, minor, ok := strings.Cut(dev, ":")
	if !ok || major == "" || minor == "" {
		return device{}, 0, false
	}
	ma, err1 := strconv.ParseUint(major, 16, 32)
	mi, err2 := strconv.ParseUint(minor, 16, 32)
	ino, err3 := strconv.ParseUint(inode, 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || ino == 0 {
		return device{}, 0, false
	}
	return device{ma, mi}, ino, true
}

// mount is one strictly parsed mountinfo line (proc(5)).
type mount struct {
	device device
	root   string
	point  string
	fstype string
}

// cleanPath is an absolute path in its clean form with no escape (\040 for a space,
// ...): nothing about it is decoded or guessed at.
func cleanPath(p string) bool {
	return !strings.Contains(p, `\`) && filepath.IsAbs(p) && filepath.Clean(p) == p
}

// parseMountinfo parses every line or none: IDs, a decimal major:minor, a root, a
// clean mount point, optional fields, the "-" separator, and exactly the filesystem
// type, source and super options after it. A root need not be a path (nsfs mounts
// show net:[inode]); btrfsDevice requires it to be one wherever it matters.
func parseMountinfo(text string) ([]mount, bool) {
	var out []mount
	for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		fields := strings.Split(line, " ")
		sep := -1
		for i := 6; i < len(fields); i++ {
			if fields[i] == "-" {
				sep = i
				break
			}
		}
		if sep < 0 || len(fields) != sep+4 || slicesContainEmpty(fields) {
			return nil, false
		}
		_, err1 := strconv.ParseUint(fields[0], 10, 64)
		_, err2 := strconv.ParseUint(fields[1], 10, 64)
		major, minor, ok := strings.Cut(fields[2], ":")
		ma, err3 := strconv.ParseUint(major, 10, 32)
		mi, err4 := strconv.ParseUint(minor, 10, 32)
		point := fields[4]
		if err1 != nil || err2 != nil || !ok || err3 != nil || err4 != nil || !cleanPath(point) {
			return nil, false
		}
		out = append(out, mount{device: device{ma, mi}, root: fields[3], point: point, fstype: fields[sep+1]})
	}
	return out, len(out) > 0
}

func slicesContainEmpty(fields []string) bool {
	for _, f := range fields {
		if f == "" {
			return true
		}
	}
	return false
}

// btrfsDevice is the superblock device of path's mount when mountinfo proves that
// mount unambiguously: the longest mount point containing path, appearing once, of
// type btrfs, and every mount containing path has a clean root. Anything else gives
// no proof.
func btrfsDevice(mountinfo, path string) (device, bool) {
	mounts, ok := parseMountinfo(mountinfo)
	if !ok {
		return device{}, false
	}
	var best []mount
	for _, m := range mounts {
		if !(m.point == "/" || path == m.point || strings.HasPrefix(path, m.point+"/")) {
			continue
		}
		if !cleanPath(m.root) {
			return device{}, false
		}
		switch {
		case len(best) == 0 || len(m.point) > len(best[0].point):
			best = []mount{m}
		case len(m.point) == len(best[0].point):
			best = append(best, m)
		}
	}
	if len(best) != 1 || best[0].fstype != "btrfs" {
		return device{}, false
	}
	return best[0].device, true
}

// checkMappedClient requires library to be the pinned artifact and every mapped
// libfdb_c to be that same file, and returns the one rule that proved all of them.
// Each mapping's inode must be the artifact's and its path must still name it; its
// device must be the artifact's st_dev, or, only when mountinfo proves the mapped
// file's mount is btrfs, that mount's superblock device. A wrong, second, replaced,
// deleted, malformed or ambiguous mapping, or mappings proved by different rules,
// fail. The loader ran before main, so this detects a substituted client; it cannot
// undo one.
func checkMappedClient(maps, mountinfo, library, pin string) (string, error) {
	want, err := os.Stat(library)
	if err != nil {
		return "", fault("client", "the native client library is absent")
	}
	st, ok := want.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fault("client", "the native client library has no device and inode")
	}
	if h, err := hashFile(library); err != nil || h != pin {
		return "", fault("client", "the native client artifact differs from the pinned libfdb_c 7.3.77")
	}
	exact := statDevice(uint64(st.Dev))
	rule := ""
	for _, line := range strings.Split(maps, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 || !strings.Contains(filepath.Base(strings.Join(fields[5:], " ")), "libfdb_c") {
			continue
		}
		if len(fields) != 6 {
			return "", fault("client", "a mapped native client is deleted or ambiguous")
		}
		dev, ino, ok := mappedIdentity(fields[3], fields[4])
		if !ok {
			return "", fault("client", "a native client mapping is malformed")
		}
		got, err := os.Stat(fields[5])
		if err != nil || !os.SameFile(want, got) || ino != st.Ino {
			return "", fault("client", "a mapped native client is not the sealed job artifact")
		}
		proved := ""
		if dev == exact {
			proved = exactStat
		} else if sb, ok := btrfsDevice(mountinfo, fields[5]); ok && dev == sb {
			proved = btrfsSuperblock
		}
		if proved == "" {
			return "", fault("client", "a mapped native client is not the sealed job artifact")
		}
		if rule != "" && rule != proved {
			return "", fault("client", "the native client's mappings were proved by different rules")
		}
		rule = proved
	}
	if rule == "" {
		return "", fault("client", "the pinned native client is not mapped")
	}
	return rule, nil
}
