package driver

import (
	"fmt"
	"math"

	"golang.org/x/sys/unix"
)

// statfsUsage reads byte and inode usage for a mounted path via statfs(2).
//
// On a weed mount the kernel forwards this to the FUSE daemon, which answers
// from the filer's collection statistics (see weed/mount/weedfs_stats.go):
// the block counts are quota-derived and per volume, while the inode counts
// are a math.MaxInt64 sentinel meaning "unbounded" — only the derived
// used-inode count carries real information.
func statfsUsage(path string) (VolumeUsage, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return VolumeUsage{}, fmt.Errorf("statfs %s: %w", path, err)
	}

	blockSize := uint64(stat.Bsize)

	return VolumeUsage{
		TotalBytes:     clampToInt64(stat.Blocks * blockSize),
		AvailableBytes: clampToInt64(stat.Bavail * blockSize),
		UsedBytes:      clampToInt64(saturatingSub(stat.Blocks, stat.Bfree) * blockSize),

		TotalInodes:     clampToInt64(stat.Files),
		AvailableInodes: clampToInt64(stat.Ffree),
		UsedInodes:      clampToInt64(saturatingSub(stat.Files, stat.Ffree)),
	}, nil
}

// saturatingSub keeps a filesystem that reports more free blocks or inodes
// than it has total from wrapping the unsigned subtraction into a huge
// "used" figure.
func saturatingSub(total, free uint64) uint64 {
	if free > total {
		return 0
	}
	return total - free
}

// clampToInt64 converts a statfs counter to the int64 the CSI spec uses.
// SeaweedFS reports math.MaxInt64 inodes, which is representable, but a
// filesystem reporting anything above it must saturate rather than wrap
// into a negative count.
func clampToInt64(value uint64) int64 {
	if value > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(value)
}
