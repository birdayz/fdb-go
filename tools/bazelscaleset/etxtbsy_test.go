package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestFakeSSHIsPreexistingAndNeverWrittenPerTest pins the STRUCTURE that keeps
// ETXTBSY out of this package: the fake ssh binaries exist before any test runs
// and no test ever writes one.
//
// A per-test os.WriteFile of the binary would re-arm the race — a fork from any
// of this package's parallel tests, landing between that open and its close,
// duplicates the write fd, and execve on the file fails "text file busy" until
// the child execs. The failure below is worded for whoever trips it: if these
// assertions go red because a helper went back to writing its own binary, the
// intermittent ETXTBSY flake in every remote test is back with it.
func TestFakeSSHIsPreexistingAndNeverWrittenPerTest(t *testing.T) {
	t.Parallel()

	if fakeSSHReadyAt.IsZero() {
		t.Fatal("setupFakeSSHBinaries did not run before m.Run; the shared fake ssh binaries are what keep the per-test write (and its ETXTBSY window) out of this package")
	}

	for _, tc := range []struct {
		name   string
		make   func(*testing.T) (string, string)
		target string
	}{
		{"fakeSSH", fakeSSH, sharedFakeSSH},
		{"fakeSSHFailing", fakeSSHFailing, sharedFakeSSHFailing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bin, argvLog := tc.make(t)

			// The per-test artefact must be a SYMLINK. A regular file here means
			// the helper wrote the executable itself, which is exactly the
			// ETXTBSY window this design removed.
			fi, err := os.Lstat(bin)
			if err != nil {
				t.Fatalf("lstat %s: %v", bin, err)
			}
			if fi.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("%s produced a regular file at %s; it must produce a symlink to the shared binary written in TestMain. A per-test write of an exec'd file re-arms the ETXTBSY fork race across every parallel test in this package", tc.name, bin)
			}

			got, err := os.Readlink(bin)
			if err != nil {
				t.Fatalf("readlink %s: %v", bin, err)
			}
			if got != tc.target {
				t.Fatalf("%s links to %q, want the shared binary %q", tc.name, got, tc.target)
			}

			// The argv log must live beside the symlink, not beside the shared
			// target — otherwise parallel tests would interleave their argv into
			// one file. The script derives it from "$0".
			if want := bin + ".log"; argvLog != want {
				t.Fatalf("%s returned argv log %q, want %q (derived from \"$0\", so each test gets its own)", tc.name, argvLog, want)
			}
			if filepath.Dir(argvLog) == fakeSSHDir {
				t.Fatalf("%s put its argv log in the shared dir %s; parallel tests would interleave into it", tc.name, fakeSSHDir)
			}

			// The target must not have been rewritten after setup stamped its
			// clock. A rewrite mid-run is a write racing live forks.
			tfi, err := os.Stat(tc.target)
			if err != nil {
				t.Fatalf("stat %s: %v", tc.target, err)
			}
			if tfi.ModTime().After(fakeSSHReadyAt) {
				t.Fatalf("shared binary %s was modified at %s, after setup finished at %s; nothing may write it once tests (and therefore forks) are running", tc.target, tfi.ModTime(), fakeSSHReadyAt)
			}
		})
	}
}

// TestFakeSSHSymlinkIsExecutableAndLogsPerTest proves the shared-binary +
// per-test-symlink arrangement actually works end to end: the symlink execs,
// and its argv lands in this test's own log rather than a shared one.
func TestFakeSSHSymlinkIsExecutableAndLogsPerTest(t *testing.T) {
	t.Parallel()
	bin, argvLog := fakeSSH(t)

	if err := exec.Command(bin, "some-host", "echo marker").Run(); err != nil {
		t.Fatalf("exec'ing the fake ssh symlink %s failed: %v", bin, err)
	}
	b, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("the fake ssh did not write its argv log at %s (derived from \"$0\"): %v", argvLog, err)
	}
	if !strings.Contains(string(b), "some-host") {
		t.Fatalf("argv log %s = %q, want it to record the invocation", argvLog, string(b))
	}
}

// TestETXTBSYIsRealForAWriterHeldOpen is the mechanism pin — a negative result
// made permanent. It demonstrates, deterministically, the kernel behaviour the
// shared-binary design exists to dodge: execve fails with ETXTBSY while ANY
// process holds the target inode open for writing, including a forked child
// that merely inherited the fd, and including when the exec goes through a
// symlink.
//
// If this ever stops reproducing, the fake-ssh arrangement above has become
// belt-and-braces rather than load-bearing — say so before relaxing it, because
// the reverse reading (that the design was never needed) is how the flake comes
// back.
func TestETXTBSYIsRealForAWriterHeldOpen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bin := filepath.Join(dir, "victim")

	f, err := os.OpenFile(bin, os.O_CREATE|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatalf("create %s: %v", bin, err)
	}
	if _, err := f.WriteString("#!/bin/sh\nexit 0\n"); err != nil {
		t.Fatalf("write %s: %v", bin, err)
	}

	// (a) This process holds the write fd.
	err = exec.Command(bin).Run()
	if !errors.Is(err, syscall.ETXTBSY) {
		t.Fatalf("exec while holding our own write fd: got %v, want ETXTBSY", err)
	}

	// (b) Through a symlink: ETXTBSY tracks the TARGET inode, which is why a
	// per-test symlink to an unwritten shared binary is safe and a per-test
	// WRITE would not be.
	link := filepath.Join(dir, "victim-link")
	if err := os.Symlink(bin, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := exec.Command(link).Run(); !errors.Is(err, syscall.ETXTBSY) {
		t.Fatalf("exec via symlink while the target has a writer: got %v, want ETXTBSY", err)
	}

	// (c) The actual race: a forked child inherits the write fd, and the file
	// stays busy after THIS process closes it. ExtraFiles hands the fd over
	// explicitly, which is what a plain fork does implicitly in the window
	// before the child execs.
	child := exec.Command("sleep", "30")
	child.ExtraFiles = []*os.File{f}
	if err := child.Start(); err != nil {
		t.Fatalf("start fd-holding child: %v", err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_, _ = child.Process.Wait()
	})
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := exec.Command(bin).Run(); !errors.Is(err, syscall.ETXTBSY) {
		t.Fatalf("exec while only a forked child holds the inherited write fd: got %v, want ETXTBSY", err)
	}

	// (d) Once no writer remains, the same file execs fine — so (a)-(c) are
	// about writers, not about the file being broken.
	_ = child.Process.Kill()
	_, _ = child.Process.Wait()
	if err := execUntilNotBusy(bin); err != nil {
		t.Fatalf("exec with no writer left: %v", err)
	}
}

// TestWriteExecutableSerialisesAgainstForks pins the cure for the run.sh write,
// which cannot use the shared-inode trick the fake ssh binaries use.
//
// The property: writeExecutable holds syscall.ForkLock exclusively, the same
// lock os/exec takes around forkExec, so no fork can be in flight while its
// write fd is open and no child can inherit that fd. The test proves it by
// holding the lock and showing writeExecutable cannot proceed — if the lock is
// dropped from writeExecutable, it sails through and this goes red.
//
// The remote launch path has no ETXTBSY retry anywhere, so nothing downstream
// would absorb the regression.
func TestWriteExecutableSerialisesAgainstForks(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "victim.sh")

	held := make(chan struct{})
	released := make(chan struct{})
	go func() {
		syscall.ForkLock.Lock()
		close(held)
		// Short: this blocks every fork in the process, including other
		// parallel tests', so it is held only long enough to observe the block.
		time.Sleep(150 * time.Millisecond)
		syscall.ForkLock.Unlock()
		close(released)
	}()
	<-held

	done := make(chan error, 1)
	go func() { done <- writeExecutable(path, []byte("#!/bin/sh\nexit 0\n")) }()

	select {
	case err := <-done:
		t.Fatalf("writeExecutable completed (err=%v) while syscall.ForkLock was held exclusively; it is not taking the lock, so a fork can still duplicate its write fd and re-arm ETXTBSY on the remote run.sh — which has no retry behind it", err)
	case <-time.After(40 * time.Millisecond):
	}

	<-released
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("writeExecutable: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("writeExecutable never completed after ForkLock was released")
	}
	if err := exec.Command(path).Run(); err != nil {
		t.Fatalf("the file writeExecutable produced does not exec: %v", err)
	}
}

// TestRenameAndChmodDoNotCureETXTBSY is a negative result made permanent, and it
// exists because both of these read as obvious fixes and neither works.
//
// Writing to a temp name and rename(2)-ing it into place is the standard cure
// for a torn read; it does nothing here, because rename moves a NAME and the
// exec'd path still resolves to the very inode the writer had open. Writing
// 0644 and chmod +x only after the close is equally tempting; it does nothing
// either, because ETXTBSY is decided by the inode's writer count, not its mode.
//
// The cures that do work: never execve a freshly written inode (link a pristine
// one, as the fake ssh helpers do; or hand the script to an interpreter as data,
// as lifecycle_test.go does), or keep forks out of the write window entirely
// (writeExecutable). If this test ever goes red, one of the two below started
// working and the design has cheaper options than it does today — check the
// kernel, not the test.
func TestRenameAndChmodDoNotCureETXTBSY(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const body = "#!/bin/sh\nexit 0\n"

	// A fork that caught the write fd keeps the inode busy no matter what name
	// it is later reachable under, or what mode it is later given.
	hold := func(t *testing.T, f *os.File) {
		t.Helper()
		child := exec.Command("sleep", "30")
		child.ExtraFiles = []*os.File{f}
		if err := child.Start(); err != nil {
			t.Fatalf("start fd-holding child: %v", err)
		}
		t.Cleanup(func() { _ = child.Process.Kill(); _, _ = child.Process.Wait() })
	}

	t.Run("rename into place", func(t *testing.T) {
		tmp, final := filepath.Join(dir, "r.tmp"), filepath.Join(dir, "r.sh")
		f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY, 0o755)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := f.WriteString(body); err != nil {
			t.Fatalf("write: %v", err)
		}
		hold(t, f)
		if err := f.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		if err := os.Rename(tmp, final); err != nil {
			t.Fatalf("rename: %v", err)
		}
		if err := exec.Command(final).Run(); !errors.Is(err, syscall.ETXTBSY) {
			t.Fatalf("exec after rename-into-place: got %v, want ETXTBSY — rename moves a name, it does not give the exec'd path a writer-free inode", err)
		}
	})

	t.Run("chmod after close", func(t *testing.T) {
		p := filepath.Join(dir, "c.sh")
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := f.WriteString(body); err != nil {
			t.Fatalf("write: %v", err)
		}
		hold(t, f)
		if err := f.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		if err := os.Chmod(p, 0o755); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		if err := exec.Command(p).Run(); !errors.Is(err, syscall.ETXTBSY) {
			t.Fatalf("exec after close-then-chmod: got %v, want ETXTBSY — the executable bit is not what ETXTBSY tests, the inode's writer count is", err)
		}
	})

	t.Run("interpreter reads it as data", func(t *testing.T) {
		p := filepath.Join(dir, "d.sh")
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o755)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := f.WriteString(body); err != nil {
			t.Fatalf("write: %v", err)
		}
		hold(t, f)
		if err := f.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		// No execve on the script's inode at all, so a writer cannot matter.
		if err := exec.Command("/bin/sh", p).Run(); err != nil {
			t.Fatalf("running a held-open script through /bin/sh: %v; reading it as data is supposed to sidestep ETXTBSY entirely", err)
		}
	})
}

// TestSharedInodeTrickIsUnsafeForRunSh pins WHY run.sh uses the lock instead of
// the link the fake ssh binaries use. A link shares the inode in both
// directions: helpers that legitimately rewrite a runner dir's run.sh would
// write through to every other test's copy. This is not hypothetical — it is
// what an earlier attempt at this fix did, and the suite failed with "exec
// format error" once one test's body landed on the shared inode.
func TestSharedInodeTrickIsUnsafeForRunSh(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pristine := filepath.Join(dir, "pristine-run.sh")
	if err := os.WriteFile(pristine, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(dir, "run.sh")
	if err := os.Link(pristine, linked); err != nil {
		t.Fatalf("hard link within one temp dir failed: %v", err)
	}
	// A helper rewriting "its own" run.sh through the link.
	if err := os.WriteFile(linked, []byte("#!/bin/sh\nexit 4\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(pristine)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "exit 4") {
		t.Fatalf("writing through a hard link did NOT reach the shared inode (%q); if that is now true, the link trick would be usable for run.sh and writeExecutable's lock could be reconsidered", string(b))
	}
}

// execUntilNotBusy runs bin, tolerating the brief ETXTBSY that lingers while the
// killed fd-holder is reaped. Only the no-writer case uses this; the assertions
// above deliberately do not retry.
func execUntilNotBusy(bin string) error {
	var err error
	for i := 0; i < 200; i++ {
		if err = exec.Command(bin).Run(); !errors.Is(err, syscall.ETXTBSY) {
			return err
		}
	}
	return err
}
