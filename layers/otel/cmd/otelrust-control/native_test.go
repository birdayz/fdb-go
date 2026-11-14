package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
)

func TestInheritedNetworkOptionsAreRefused(t *testing.T) {
	t.Parallel()
	for _, env := range [][]string{
		{"FDB_NETWORK_OPTION_TRACE_ENABLE=/tmp/secret-trace-dir"},
		{"PATH=/bin", "FDB_NETWORK_OPTION_EXTERNAL_CLIENT_DIRECTORY="},
		{"FDB_NETWORK_OPTION_KNOB"},
	} {
		err := rejectNetworkEnv(env)
		if err == nil || err.Error() != envRefused || strings.Contains(err.Error(), "secret") {
			t.Fatalf("%v: %v", env, err)
		}
	}
	if err := rejectNetworkEnv([]string{"PATH=/bin", "FDB_CLUSTER_FILE=/x", "XFDB_NETWORK_OPTION_A=1"}); err != nil {
		t.Fatalf("an unrelated environment was refused: %v", err)
	}
}

// identity is a file's device and inode as the kernel reports them.
func identity(t *testing.T, path string) (device, uint64) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st := info.Sys().(*syscall.Stat_t)
	return statDevice(uint64(st.Dev)), st.Ino
}

func mapping(dev, inode, path string) string {
	return fmt.Sprintf("7f0000000000-7f0000001000 r-xp 00000000 %s %s %s", dev, inode, path)
}

// mapsLine is the maps line of path mapped now, with its real device and inode.
func mapsLine(t *testing.T, path string) string {
	t.Helper()
	d, ino := identity(t, path)
	return mapping(fmt.Sprintf("%02x:%02x", d.major, d.minor), fmt.Sprint(ino), path)
}

func TestMappedClientMustBeTheSealedArtifact(t *testing.T) {
	t.Parallel()
	// The kernel names a mapping by its resolved path, which the mount points contain.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	client := filepath.Join(dir, "libfdb_c.so")
	pin := writeFile(t, client, []byte("pinned client"))
	other := filepath.Join(dir, "other", "libfdb_c.so")
	if err := os.MkdirAll(filepath.Dir(other), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, other, []byte("pinned client"))
	link := filepath.Join(dir, "link.so")
	if err := os.Symlink(client, link); err != nil {
		t.Fatal(err)
	}
	dev, ino := identity(t, client)
	devText, inoText := fmt.Sprintf("%02x:%02x", dev.major, dev.minor), fmt.Sprint(ino)
	// A btrfs subvolume's mappings name the superblock device, never the file's st_dev.
	superblock := device{0, 0x1d}
	if superblock == dev {
		superblock.minor++
	}
	sbText := fmt.Sprintf("%02x:%02x", superblock.major, superblock.minor)
	root := "1 0 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw,errors=remount-ro\n"
	mountAt := func(point, fstype string, d device) string {
		return fmt.Sprintf("36 1 %d:%d /@home %s rw,relatime shared:2 - %s /dev/nvme0n1p2 rw,subvol=/@home\n", d.major, d.minor, point, fstype)
	}
	btrfs := root + mountAt(dir, "btrfs", superblock)
	// The same btrfs mount with a relative root: no superblock proof, exact stat only.
	badRoot := root + fmt.Sprintf("36 1 %d:%d @home %s rw,relatime shared:2 - btrfs /dev/nvme0n1p2 rw,subvol=/@home\n", superblock.major, superblock.minor, dir)
	libc := mapping("08:01", "77", "/usr/lib/libc.so.6")
	sbMapping := mapping(sbText, inoText, client)
	for name, c := range map[string]struct {
		maps, mounts, library, pin string
		// rule is the accepting rule, empty for a refusal.
		rule string
	}{
		"mapped once":                       {libc + "\n" + mapsLine(t, client), root, client, pin, exactStat},
		"mapped segments":                   {mapsLine(t, client) + "\n" + mapsLine(t, client) + "\n" + libc, root, client, pin, exactStat},
		"job names a link to it":            {mapsLine(t, client), root, link, pin, exactStat},
		"exact device on btrfs":             {mapsLine(t, client), btrfs, client, pin, exactStat},
		"exact device, no mounts":           {mapsLine(t, client), "", client, pin, exactStat},
		"btrfs subvolume":                   {sbMapping, btrfs, client, pin, btrfsSuperblock},
		"malformed root, exact device":      {mapsLine(t, client), badRoot, client, pin, exactStat},
		"malformed root, superblock device": {sbMapping, badRoot, client, pin, ""},
		"btrfs ancestor mount":              {sbMapping, root + mountAt(filepath.Dir(dir), "btrfs", superblock), client, pin, btrfsSuperblock},
		"btrfs, another device":             {mapping("00:fe", inoText, client), btrfs, client, pin, ""},
		"ext4, mismatching device":          {sbMapping, root + mountAt(dir, "ext4", device{8, 2}), client, pin, ""},
		"ext4, matching device":             {sbMapping, root + mountAt(dir, "ext4", superblock), client, pin, ""},
		"ext4 nested under btrfs":           {sbMapping, root + mountAt(filepath.Dir(dir), "btrfs", superblock) + mountAt(dir, "ext4", superblock), client, pin, ""},
		"ambiguous equal mounts":            {sbMapping, btrfs + mountAt(dir, "btrfs", superblock), client, pin, ""},
		"escaped mount point":               {sbMapping, btrfs + mountAt(`/mnt/a\040b`, "tmpfs", device{0, 40}), client, pin, ""},
		"unclean mount point":               {sbMapping, root + mountAt(dir+"/", "btrfs", superblock), client, pin, ""},
		"malformed mountinfo line":          {sbMapping, btrfs + "37 1 0:30 / /mnt rw btrfs /dev/x rw\n", client, pin, ""},
		"not a component boundary":          {sbMapping, root + mountAt(dir[:len(dir)-1], "btrfs", superblock), client, pin, ""},
		"no mountinfo":                      {sbMapping, "", client, pin, ""},
		"mixed rules":                       {mapsLine(t, client) + "\n" + sbMapping, btrfs, client, pin, ""},
		"absent":                            {libc, btrfs, client, pin, ""},
		"another inode":                     {mapsLine(t, other), btrfs, client, pin, ""},
		"another inode, superblock":         {mapping(sbText, fmt.Sprint(identityInode(t, other)), other), btrfs, client, pin, ""},
		"a second client":                   {mapsLine(t, client) + "\n" + mapsLine(t, other), btrfs, client, pin, ""},
		"a renamed second client":           {mapsLine(t, client) + "\n" + mapping(devText, inoText, filepath.Join(dir, "libfdb_c_7.1.so")), btrfs, client, pin, ""},
		"forged inode":                      {mapping(devText, fmt.Sprint(ino+1), client), btrfs, client, pin, ""},
		"forged inode, superblock":          {mapping(sbText, fmt.Sprint(ino+1), client), btrfs, client, pin, ""},
		"wrong device":                      {mapping("fe:fe", inoText, client), root, client, pin, ""},
		"malformed device":                  {mapping("0x08:01", inoText, client), btrfs, client, pin, ""},
		"device without a minor":            {mapping("08:", inoText, client), btrfs, client, pin, ""},
		"malformed inode":                   {mapping(devText, "12a", client), btrfs, client, pin, ""},
		"zero inode":                        {mapping(devText, "0", client), btrfs, client, pin, ""},
		"deleted":                           {mapsLine(t, client) + " (deleted)", btrfs, client, pin, ""},
		"ambiguous path":                    {mapping(devText, inoText, filepath.Join(dir, "a b", "libfdb_c.so")), btrfs, client, pin, ""},
		"artifact differs from pin":         {mapsLine(t, client), btrfs, client, strings.Repeat("0", 64), ""},
		"superblock, pin differs":           {sbMapping, btrfs, client, strings.Repeat("0", 64), ""},
		"job library absent":                {mapsLine(t, client), btrfs, filepath.Join(dir, "absent.so"), pin, ""},
	} {
		rule, err := checkMappedClient(c.maps, c.mounts, c.library, c.pin)
		switch {
		case c.rule != "" && (err != nil || rule != c.rule):
			t.Errorf("%s: rule %q, %v; want %q", name, rule, err, c.rule)
		case c.rule == "" && (err == nil || rule != ""):
			t.Errorf("%s: accepted by %q", name, rule)
		case err != nil && strings.Contains(err.Error(), dir):
			t.Errorf("%s: the error names a path", name)
		}
	}
	// A path replaced by a new file with the same pinned bytes no longer names the
	// mapped inode, though the path, the bytes and the hash all still match, under
	// either rule.
	oldExact, oldSuperblock := mapsLine(t, client), sbMapping
	_, before := identity(t, client)
	replacement := filepath.Join(dir, "replacement")
	writeFile(t, replacement, []byte("pinned client"))
	if err := os.Rename(replacement, client); err != nil {
		t.Fatal(err)
	}
	if _, after := identity(t, client); after == before {
		t.Fatal("the replacement kept the inode")
	}
	for _, old := range []string{oldExact, oldSuperblock} {
		if rule, err := checkMappedClient(old, btrfs, client, pin); err == nil {
			t.Fatalf("a replaced artifact's old mapping was accepted by %q", rule)
		}
	}
	if rule, err := checkMappedClient(mapsLine(t, client), btrfs, client, pin); err != nil || rule != exactStat {
		t.Fatalf("the new file's own mapping: %q, %v", rule, err)
	}
}

func identityInode(t *testing.T, path string) uint64 {
	t.Helper()
	_, ino := identity(t, path)
	return ino
}

// btrfsDevice proves a superblock device only from a strictly parsed mountinfo whose
// longest mount containing the path is unique and btrfs, and whose mounts containing
// the path all have clean roots.
func TestBtrfsDevice(t *testing.T) {
	t.Parallel()
	root := "1 0 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw,errors=remount-ro\n"
	home := "36 1 0:29 /@home /home rw,relatime shared:2 - btrfs /dev/nvme0n1p2 rw,ssd,space_cache=v2,subvolid=257,subvol=/@home\n"
	proc := "22 1 0:21 / /proc rw,nosuid,nodev,noexec,relatime shared:12 - proc proc rw\n"
	subvolume := device{0, 29}
	for name, c := range map[string]struct {
		info, path string
		ok         bool
	}{
		"btrfs subvolume":              {root + proc + home, "/home/birdy/lib.so", true},
		"a /@home subvolume root":      {root + "36 1 0:29 /@home /home rw,relatime shared:2 - btrfs /dev/nvme0n1p2 rw,subvol=/@home\n", "/home/x", true},
		"a nested subvolume root":      {root + "36 1 0:29 /@home/.snapshots/1/snapshot /home rw - btrfs /dev/x rw\n", "/home/x", true},
		"a namespace root elsewhere":   {root + home + "60 25 0:4 net:[4026533080] /run/netns/a rw shared:9 - nsfs nsfs rw\n", "/home/x", true},
		"a relative root":              {root + "36 1 0:29 @home /home rw - btrfs /dev/x rw\n", "/home/x", false},
		"a dotted root":                {root + "36 1 0:29 /@home/../@ /home rw - btrfs /dev/x rw\n", "/home/x", false},
		"an unclean root":              {root + "36 1 0:29 /@home/ /home rw - btrfs /dev/x rw\n", "/home/x", false},
		"an escaped root":              {root + `36 1 0:29 /@ho\040me /home rw - btrfs /dev/x rw` + "\n", "/home/x", false},
		"a malformed ancestor root":    {"1 0 8:1 rel / rw - ext4 /dev/sda1 rw\n" + home, "/home/x", false},
		"no trailing newline":          {root + strings.TrimSuffix(home, "\n"), "/home/birdy/lib.so", true},
		"the mount point itself":       {root + home, "/home", true},
		"no optional fields":           {root + "36 1 0:29 /@home /home rw - btrfs /dev/x rw\n", "/home/x", true},
		"several optional fields":      {root + "36 1 0:29 /@home /home rw shared:2 master:1 - btrfs /dev/x rw\n", "/home/x", true},
		"btrfs nested in btrfs":        {root + home + "41 36 0:29 /@home/x /home/x rw shared:4 - btrfs /dev/nvme0n1p2 rw\n", "/home/x/lib.so", true},
		"sibling of a nested ext4":     {root + home + "40 36 259:3 / /home/data rw shared:3 - ext4 /dev/nvme1n1p1 rw\n", "/home/birdy/lib.so", true},
		"unrelated equal mount points": {root + home + "50 1 0:50 / /mnt/media rw - ext4 /dev/sdb1 rw\n51 1 0:51 / /mnt/media rw - ext4 /dev/sdc1 rw\n", "/home/x", true},
		"ext4 root":                    {root + home, "/usr/lib/x", false},
		"not a component boundary":     {root + home, "/homework/x", false},
		"ext4 nested under btrfs":      {root + home + "40 36 259:3 / /home/data rw shared:3 - ext4 /dev/nvme1n1p1 rw\n", "/home/data/lib.so", false},
		"tmpfs over the btrfs mount":   {root + home + "37 36 0:40 / /home rw - tmpfs t rw\n", "/home/x", false},
		"ambiguous equal btrfs mounts": {root + home + "37 1 0:29 /@home /home rw shared:5 - btrfs /dev/nvme0n1p2 rw\n", "/home/x", false},
		"an escaped mount point":       {root + home + `31 1 0:40 / /mnt/a\040b rw - tmpfs t rw` + "\n", "/home/x", false},
		"an escaped path in the point": {root + `36 1 0:29 /@home /ho\155e rw - btrfs /dev/x rw` + "\n", "/home/x", false},
		"an unclean mount point":       {root + "36 1 0:29 /@home /home/ rw - btrfs /dev/x rw\n", "/home/x", false},
		"a dotted mount point":         {root + "36 1 0:29 /@home /mnt/../home rw - btrfs /dev/x rw\n", "/home/x", false},
		"a relative mount point":       {root + "36 1 0:29 /@home home rw - btrfs /dev/x rw\n", "home/x", false},
		"no separator":                 {root + "36 1 0:29 /@home /home rw btrfs /dev/x rw\n", "/home/x", false},
		"separator before the options": {root + "36 1 0:29 /@home /home - btrfs /dev/x rw\n", "/home/x", false},
		"an extra field after it":      {root + "36 1 0:29 /@home /home rw - btrfs /dev/x rw extra\n", "/home/x", false},
		"a missing field after it":     {root + "36 1 0:29 /@home /home rw - btrfs /dev/x\n", "/home/x", false},
		"a doubled space":              {root + "36 1 0:29 /@home /home rw  - btrfs /dev/x rw\n", "/home/x", false},
		"a hexadecimal device":         {root + "36 1 0:1d /@home /home rw - btrfs /dev/x rw\n", "/home/x", false},
		"a device without a minor":     {root + "36 1 0: /@home /home rw - btrfs /dev/x rw\n", "/home/x", false},
		"a non-numeric mount ID":       {root + "x 1 0:29 /@home /home rw - btrfs /dev/x rw\n", "/home/x", false},
		"a blank line":                 {root + "\n" + home, "/home/x", false},
		"empty":                        {"", "/home/x", false},
	} {
		got, ok := btrfsDevice(c.info, c.path)
		if ok != c.ok || (ok && got != subvolume) {
			t.Errorf("%s: %v %v, want %v", name, got, ok, c.ok)
		}
	}
}

// nativeRecorder records the native opener's steps in order.
type nativeRecorder struct {
	steps []string
	fail  string
}

type closedBackend struct {
	fdb.BackendDatabase
	closed *bool
}

func (b closedBackend) Close() { *b.closed = true }

func (r *nativeRecorder) opener(t *testing.T, env []string, client, pin string) *nativeOpener {
	step := func(name string) error {
		r.steps = append(r.steps, name)
		if r.fail == name {
			return errors.New("injected " + name + " failure")
		}
		return nil
	}
	return &nativeOpener{nativeSteps: nativeSteps{
		environ: func() []string {
			r.steps = append(r.steps, "env")
			return env
		},
		maps: func() ([]byte, error) {
			r.steps = append(r.steps, "maps")
			return []byte(mapsLine(t, client)), nil
		},
		mountinfo: func() ([]byte, error) {
			r.steps = append(r.steps, "mounts")
			return nil, nil
		},
		apiVersion: func(v int) error {
			if v != 730 {
				t.Errorf("API version %d", v)
			}
			return step("api730")
		},
		disableMVC: func() error { return step("option60") },
		open: func(string) (fdb.BackendDatabase, error) {
			if err := step("open"); err != nil {
				return nil, err
			}
			return closedBackend{closed: new(bool)}, nil
		},
		pin: pin,
	}}
}

// API 730, then network option 60, then the Open that starts the network, each once;
// any refusal stops every later step.
func TestNativeOpenerOrder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	client := filepath.Join(dir, "libfdb_c.so")
	pin := writeFile(t, client, []byte("pinned client"))
	rec := &nativeRecorder{}
	o := rec.opener(t, nil, client, pin)
	// Every open validates the mapping again and returns the rule that proved it.
	for range 2 {
		if _, rule, err := o.openBackend(t.Context(), client, "/cluster"); err != nil || rule != exactStat {
			t.Fatalf("rule %q, %v", rule, err)
		}
	}
	if want := []string{"env", "maps", "mounts", "api730", "option60", "open", "env", "maps", "mounts", "open"}; !slices.Equal(rec.steps, want) {
		t.Fatalf("steps %v, want %v", rec.steps, want)
	}
	for _, c := range []struct {
		name string
		env  []string
		pin  string
		fail string
		want []string
	}{
		{"inherited option", []string{"FDB_NETWORK_OPTION_TRACE_ENABLE="}, pin, "", []string{"env"}},
		{"unpinned artifact", nil, strings.Repeat("0", 64), "", []string{"env", "maps", "mounts"}},
		{"API version refused", nil, pin, "api730", []string{"env", "maps", "mounts", "api730"}},
		{"option refused", nil, pin, "option60", []string{"env", "maps", "mounts", "api730", "option60"}},
		{"open refused", nil, pin, "open", []string{"env", "maps", "mounts", "api730", "option60", "open"}},
	} {
		rec := &nativeRecorder{fail: c.fail}
		if _, rule, err := rec.opener(t, c.env, client, c.pin).openBackend(t.Context(), client, "/cluster"); err == nil || rule != "" {
			t.Fatalf("%s: opened by %q", c.name, rule)
		}
		if !slices.Equal(rec.steps, c.want) {
			t.Fatalf("%s: steps %v, want %v", c.name, rec.steps, c.want)
		}
	}
	// A cancellation Open outlived still releases the handle.
	closed := false
	o = rec.opener(t, nil, client, pin)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	o.open = func(string) (fdb.BackendDatabase, error) {
		cancel()
		return closedBackend{closed: &closed}, nil
	}
	if db, rule, err := o.openBackend(ctx, client, "/cluster"); err == nil || db != nil || rule != "" || !closed {
		t.Fatalf("a canceled open returned %v, %q, %v, closed %v", db, rule, err, closed)
	}
}

// A native open must return the rule that proved its mapping; a reopen proved by
// another rule than the preflight's fails rather than reporting one of them.
func TestNativeMappingRuleIsPropagated(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for _, c := range []struct {
		rule string
		ok   bool
	}{{exactStat, true}, {btrfsSuperblock, true}, {"", false}, {"mount_device", false}} {
		closed := false
		rt := runtime{
			openPure: func(context.Context, string) (fdb.BackendDatabase, error) {
				return nil, errors.New("a native job opened the pure client")
			},
			openNative: func(context.Context, string, string) (fdb.BackendDatabase, string, error) {
				return closedBackend{closed: &closed}, c.rule, nil
			},
		}
		db, rule, err := rt.ingestBackend(t.Context(), parseTestJob(t, f.job("ingest", "go_native", point2)))
		if c.ok != (err == nil) || (c.ok && (rule != c.rule || db == nil || closed)) || (!c.ok && (db != nil || rule != "" || !closed)) {
			t.Errorf("rule %q: %v, %q, %v, closed %v", c.rule, db, rule, err, closed)
		}
	}
	pure := runtime{
		openPure: func(context.Context, string) (fdb.BackendDatabase, error) {
			return closedBackend{closed: new(bool)}, nil
		},
		openNative: func(context.Context, string, string) (fdb.BackendDatabase, string, error) {
			return nil, "", errors.New("a pure job opened the native client")
		},
	}
	if db, rule, err := pure.ingestBackend(t.Context(), parseTestJob(t, f.job("ingest", "go_pure", point2))); err != nil || db == nil || rule != "" {
		t.Fatalf("a pure open: %v, %q, %v", db, rule, err)
	}
	for _, c := range []struct {
		preflight, rule string
		ok              bool
	}{
		{exactStat, exactStat, true},
		{btrfsSuperblock, btrfsSuperblock, true},
		{"", "", true},
		{exactStat, btrfsSuperblock, false},
		{btrfsSuperblock, exactStat, false},
		{exactStat, "", false},
	} {
		if err := sameMapping(c.preflight, c.rule); (err == nil) != c.ok {
			t.Errorf("preflight %q, reopen %q: %v", c.preflight, c.rule, err)
		}
	}
	// Only a measured native handle is claimed; a pure open claims nothing.
	r := &ingestReport{}
	r.claimMapping("")
	if r.NativeMappingRule != nil {
		t.Fatal("a pure open claimed a mapping rule")
	}
	r.claimMapping(btrfsSuperblock)
	if r.NativeMappingRule == nil || *r.NativeMappingRule != btrfsSuperblock {
		t.Fatalf("claimed %v", r.NativeMappingRule)
	}
}
