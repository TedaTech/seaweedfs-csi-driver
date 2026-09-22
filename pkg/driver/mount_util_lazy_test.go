package driver

import (
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"k8s.io/mount-utils"
)

func resolvedTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	return dir
}

// TestCleanupCorruptedStagingPathLazyFallback verifies that cleanupCorruptedStagingPath
// falls back to a lazy unmount when the regular umount fails (EBUSY from bind mounts),
// then removes the path so a later NodeStageVolume can recreate the mount.
// Regression test for seaweedfs/seaweedfs-csi-driver#305.
func TestCleanupCorruptedStagingPathLazyFallback(t *testing.T) {
	stagingPath := filepath.Join(resolvedTempDir(t), "globalmount")
	if err := os.MkdirAll(stagingPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	origMountutil := mountutil
	origLazyUnmount := lazyUnmount
	defer func() {
		mountutil = origMountutil
		lazyUnmount = origLazyUnmount
	}()

	fake := mount.NewFakeMounter([]mount.MountPoint{{Device: "fuse", Path: stagingPath, Type: "fuse.seaweedfs"}})
	fake.UnmountFunc = func(path string) error { return errors.New("umount: target is busy") }
	mountutil = fake

	var lazyCalls int32
	lazyUnmount = func(p string) error {
		atomic.StoreInt32(&lazyCalls, 1)
		if p != stagingPath {
			t.Errorf("lazy unmount called with %q, want %q", p, stagingPath)
		}
		return nil
	}

	if err := cleanupCorruptedStagingPath(stagingPath); err != nil {
		t.Fatalf("cleanupCorruptedStagingPath: %v", err)
	}
	if atomic.LoadInt32(&lazyCalls) != 1 {
		t.Fatal("expected lazy unmount to be invoked after CleanupMountPoint failed")
	}
	if _, err := os.Stat(stagingPath); !os.IsNotExist(err) {
		t.Fatalf("expected staging path removed, got err=%v", err)
	}
}

// TestCleanupCorruptedStagingPathFailsWhenBothUnmountsFail ensures an error
// is returned when neither the standard cleanup nor the lazy unmount succeeds.
func TestCleanupCorruptedStagingPathFailsWhenBothUnmountsFail(t *testing.T) {
	stagingPath := filepath.Join(resolvedTempDir(t), "globalmount")
	if err := os.MkdirAll(stagingPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	origMountutil := mountutil
	origLazyUnmount := lazyUnmount
	defer func() {
		mountutil = origMountutil
		lazyUnmount = origLazyUnmount
	}()

	fake := mount.NewFakeMounter([]mount.MountPoint{{Device: "fuse", Path: stagingPath, Type: "fuse.seaweedfs"}})
	fake.UnmountFunc = func(path string) error { return errors.New("umount: target is busy") }
	mountutil = fake
	lazyUnmount = func(p string) error { return errors.New("lazy unmount: invalid argument") }

	if err := cleanupCorruptedStagingPath(stagingPath); err == nil {
		t.Fatal("expected error when both standard cleanup and lazy unmount fail")
	}
}
