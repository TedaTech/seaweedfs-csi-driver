//go:build linux

package mountmanager

import "golang.org/x/sys/unix"

// LazyUnmount detaches the mount via MNT_DETACH (umount -l) without
// waiting for references to drain. Used as a fallback when a regular
// umount fails with EBUSY on a stale FUSE mount whose daemon has died.
func LazyUnmount(target string) error {
	return unix.Unmount(target, unix.MNT_DETACH)
}
