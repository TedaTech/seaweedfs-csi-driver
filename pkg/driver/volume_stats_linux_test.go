package driver

import (
	"errors"
	"io/fs"
	"math"
	"path/filepath"
	"testing"
)

func TestStatfsUsageReadsARealFilesystem(t *testing.T) {
	usage, err := statfsUsage(t.TempDir())
	if err != nil {
		t.Fatalf("statfsUsage: %v", err)
	}

	if usage.TotalBytes <= 0 {
		t.Errorf("total bytes = %d, want a positive size", usage.TotalBytes)
	}
	if usage.UsedBytes+usage.AvailableBytes > usage.TotalBytes {
		t.Errorf("used %d + available %d exceeds total %d",
			usage.UsedBytes, usage.AvailableBytes, usage.TotalBytes)
	}
	if usage.TotalInodes <= 0 {
		t.Errorf("total inodes = %d, want a positive count", usage.TotalInodes)
	}
	if usage.UsedInodes < 0 || usage.UsedInodes > usage.TotalInodes {
		t.Errorf("used inodes = %d, want 0..%d", usage.UsedInodes, usage.TotalInodes)
	}
}

func TestStatfsUsageOnAMissingPath(t *testing.T) {
	_, err := statfsUsage(filepath.Join(t.TempDir(), "gone"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want it to wrap fs.ErrNotExist so the RPC maps it to NotFound", err)
	}
}

// SeaweedFS reports math.MaxInt64 inodes. The conversion has to carry that
// through untouched, and saturate rather than wrap for anything larger.
func TestClampToInt64(t *testing.T) {
	for _, tc := range []struct {
		in   uint64
		want int64
	}{
		{0, 0},
		{math.MaxInt64, math.MaxInt64},
		{math.MaxInt64 + 1, math.MaxInt64},
		{math.MaxUint64, math.MaxInt64},
	} {
		if got := clampToInt64(tc.in); got != tc.want {
			t.Errorf("clampToInt64(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestSaturatingSub(t *testing.T) {
	if got := saturatingSub(10, 4); got != 6 {
		t.Errorf("saturatingSub(10, 4) = %d, want 6", got)
	}
	if got := saturatingSub(4, 10); got != 0 {
		t.Errorf("saturatingSub(4, 10) = %d, want 0 — an unsigned wrap would report a huge used count", got)
	}
}
