//go:build linux && integration

package mountmanager

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func skipIfNotRoot(t *testing.T) {
	t.Helper()
	if os.Getuid() != 0 {
		t.Skip("integration tests require root (CAP_SYS_ADMIN)")
	}
}

// TestIntegrationLazyUnmountDetachesBusyMount reproduces the core mechanism
// behind seaweedfs/seaweedfs-csi-driver#305. After the weed mount FUSE daemon
// dies, a regular umount of the staging path fails with EBUSY (the publish
// bind mounts keep the mount busy), leaving a stale ENOTCONN mount that
// blocks later NodeStageVolume. A lazy unmount (MNT_DETACH) detaches it
// anyway so the staging path can be reused.
//
// A real FUSE daemon is not needed to exercise the mechanism: any condition
// that makes a regular umount return EBUSY while MNT_DETACH still succeeds
// demonstrates the fix. Here an open file on a tmpfs plays the role of the
// busy reference.
//
// Run with: sudo go test -tags integration -run Integration ./pkg/mountmanager/
func TestIntegrationLazyUnmountDetachesBusyMount(t *testing.T) {
	skipIfNotRoot(t)

	root := t.TempDir()
	staging := filepath.Join(root, "staging")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", staging, err)
	}

	if err := unix.Mount("tmpfs", staging, "tmpfs", 0, ""); err != nil {
		if errors.Is(err, unix.EPERM) {
			t.Skipf("skipping: mount requires CAP_SYS_ADMIN: %v", err)
		}
		t.Fatalf("mount tmpfs at %s: %v", staging, err)
	}
	defer unix.Unmount(staging, unix.MNT_DETACH)

	// Hold an open file on the mount so a regular umount returns EBUSY,
	// simulating the busy reference publish bind mounts create on a dead
	// FUSE staging mount.
	busyFile := filepath.Join(staging, "busy")
	f, err := os.Create(busyFile)
	if err != nil {
		t.Fatalf("create busy file: %v", err)
	}
	defer f.Close()

	if err := unix.Unmount(staging, 0); err == nil {
		t.Skip("regular umount succeeded; cannot simulate the busy-stale condition")
	} else if !errors.Is(err, unix.EBUSY) {
		t.Fatalf("expected EBUSY from regular umount, got: %v", err)
	}

	if err := LazyUnmount(staging); err != nil {
		t.Fatalf("LazyUnmount of busy staging mount failed: %v", err)
	}

	notMnt, err := kubeMounter.IsLikelyNotMountPoint(staging)
	if err != nil {
		t.Fatalf("IsLikelyNotMountPoint after lazy unmount: %v", err)
	}
	if !notMnt {
		t.Fatal("staging path is still a mount point after lazy unmount")
	}
}
