package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
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
