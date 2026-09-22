package mountmanager

import (
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"

	"k8s.io/mount-utils"
)

func withFakeMounter(target string, fake *mount.FakeMounter, lazy func(string) error) func() {
	origKubeMounter := kubeMounter
	origLazyUnmount := lazyUnmount
	kubeMounter = fake
	lazyUnmount = lazy
	return func() {
		kubeMounter = origKubeMounter
		lazyUnmount = origLazyUnmount
	}
}

// TestEnsureTargetCleanLazyFallbackOnCorruptedMount verifies that ensureTargetClean
// falls back to a lazy unmount when a corrupted mount's regular umount fails.
// Regression test for seaweedfs/seaweedfs-csi-driver#305.
func TestEnsureTargetCleanLazyFallbackOnCorruptedMount(t *testing.T) {
	target := filepath.Join(t.TempDir(), "staging")

	fake := mount.NewFakeMounter([]mount.MountPoint{{Device: "fuse", Path: target, Type: "fuse.seaweedfs"}})
	fake.MountCheckErrors = map[string]error{target: syscall.ENOTCONN}
	fake.UnmountFunc = func(path string) error { return errors.New("umount: target is busy") }

	var lazyCalls int32
	restore := withFakeMounter(target, fake, func(p string) error {
		atomic.StoreInt32(&lazyCalls, 1)
		if p != target {
			t.Errorf("lazy unmount called with %q, want %q", p, target)
		}
		return nil
	})
	defer restore()

	if err := ensureTargetClean(target); err != nil {
		t.Fatalf("ensureTargetClean: %v", err)
	}
	if atomic.LoadInt32(&lazyCalls) != 1 {
		t.Fatal("expected lazy unmount to be invoked after regular unmount failed")
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("target not recreated after cleanup: %v", err)
	}
}

// TestEnsureTargetCleanSkipsLazyWhenRegularUnmountSucceeds ensures the lazy
// fallback is not invoked when the regular unmount succeeds.
func TestEnsureTargetCleanSkipsLazyWhenRegularUnmountSucceeds(t *testing.T) {
	target := filepath.Join(t.TempDir(), "staging")

	fake := mount.NewFakeMounter([]mount.MountPoint{{Device: "fuse", Path: target, Type: "fuse.seaweedfs"}})
	fake.MountCheckErrors = map[string]error{target: syscall.ENOTCONN}
	fake.UnmountFunc = nil // regular unmount succeeds

	var lazyCalls int32
	restore := withFakeMounter(target, fake, func(p string) error {
		atomic.StoreInt32(&lazyCalls, 1)
		return nil
	})
	defer restore()

	if err := ensureTargetClean(target); err != nil {
		t.Fatalf("ensureTargetClean: %v", err)
	}
	if atomic.LoadInt32(&lazyCalls) != 0 {
		t.Fatal("lazy unmount must not run when regular unmount succeeds")
	}
}

// TestEnsureTargetCleanFailsWhenBothUnmountsFail ensures an error is returned
// when neither the regular nor the lazy unmount can detach the mount.
func TestEnsureTargetCleanFailsWhenBothUnmountsFail(t *testing.T) {
	target := filepath.Join(t.TempDir(), "staging")

	fake := mount.NewFakeMounter([]mount.MountPoint{{Device: "fuse", Path: target, Type: "fuse.seaweedfs"}})
	fake.MountCheckErrors = map[string]error{target: syscall.ENOTCONN}
	fake.UnmountFunc = func(path string) error { return errors.New("umount: target is busy") }

	restore := withFakeMounter(target, fake, func(p string) error {
		return errors.New("lazy unmount: invalid argument")
	})
	defer restore()

	if err := ensureTargetClean(target); err == nil {
		t.Fatal("expected error when both unmounts fail")
	}
}
