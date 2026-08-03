package driver

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func nodeServerWithStatfs(fn StatfsFn) *NodeServer {
	return &NodeServer{volumeMutexes: NewKeyMutex(), statfsFn: fn}
}

func statsRequest() *csi.NodeGetVolumeStatsRequest {
	return &csi.NodeGetVolumeStatsRequest{
		VolumeId:   "filer://seaweedfs-filer.tenant-x.svc:8888/buckets/pvc-1234",
		VolumePath: "/var/lib/kubelet/pods/abc/volumes/kubernetes.io~csi/pvc-1234/mount",
	}
}

func usageByUnit(t *testing.T, resp *csi.NodeGetVolumeStatsResponse, unit csi.VolumeUsage_Unit) *csi.VolumeUsage {
	t.Helper()
	for _, u := range resp.GetUsage() {
		if u.GetUnit() == unit {
			return u
		}
	}
	t.Fatalf("response has no %v usage entry: %+v", unit, resp.GetUsage())
	return nil
}

// Kubelet only calls NodeGetVolumeStats for drivers that advertise the
// capability; without it the RPC below is dead code.
func TestNodeGetCapabilitiesAdvertisesVolumeStats(t *testing.T) {
	resp, err := (&NodeServer{}).NodeGetCapabilities(context.Background(), &csi.NodeGetCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("NodeGetCapabilities: %v", err)
	}

	for _, c := range resp.GetCapabilities() {
		if c.GetRpc().GetType() == csi.NodeServiceCapability_RPC_GET_VOLUME_STATS {
			return
		}
	}
	t.Fatalf("GET_VOLUME_STATS not advertised: %+v", resp.GetCapabilities())
}

func TestNodeGetVolumeStatsReportsBytesAndInodes(t *testing.T) {
	ns := nodeServerWithStatfs(func(string) (VolumeUsage, error) {
		return VolumeUsage{
			TotalBytes: 118111600640, UsedBytes: 102276262912, AvailableBytes: 15835337728,
			TotalInodes: math.MaxInt64, UsedInodes: 4217, AvailableInodes: math.MaxInt64 - 4217,
		}, nil
	})

	resp, err := ns.NodeGetVolumeStats(context.Background(), statsRequest())
	if err != nil {
		t.Fatalf("NodeGetVolumeStats: %v", err)
	}
	if got := len(resp.GetUsage()); got != 2 {
		t.Fatalf("got %d usage entries, want 2 (bytes and inodes)", got)
	}

	bytes := usageByUnit(t, resp, csi.VolumeUsage_BYTES)
	if bytes.GetTotal() != 118111600640 || bytes.GetUsed() != 102276262912 || bytes.GetAvailable() != 15835337728 {
		t.Errorf("byte usage = %+v, want the statfs values unmodified", bytes)
	}

	// The inode totals are SeaweedFS's "unbounded" sentinel. They have to
	// survive intact: PersistentVolumeInodesCritical pages on
	// inodes_free/inodes < 0.10, and that ratio is only pinned at 1
	// because both operands are MaxInt64.
	inodes := usageByUnit(t, resp, csi.VolumeUsage_INODES)
	if inodes.GetTotal() != math.MaxInt64 {
		t.Errorf("inode total = %d, want MaxInt64", inodes.GetTotal())
	}
	if inodes.GetUsed() != 4217 {
		t.Errorf("inode used = %d, want the real file count 4217", inodes.GetUsed())
	}
	if inodes.GetAvailable() != math.MaxInt64-4217 {
		t.Errorf("inode available = %d, want MaxInt64-4217", inodes.GetAvailable())
	}
}

func TestNodeGetVolumeStatsRejectsIncompleteRequests(t *testing.T) {
	ns := nodeServerWithStatfs(func(string) (VolumeUsage, error) {
		t.Fatal("statfs must not run for an invalid request")
		return VolumeUsage{}, nil
	})

	for name, req := range map[string]*csi.NodeGetVolumeStatsRequest{
		"no volume id":   {VolumePath: "/mnt/x"},
		"no volume path": {VolumeId: "pvc-1234"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ns.NodeGetVolumeStats(context.Background(), req)
			if status.Code(err) != codes.InvalidArgument {
				t.Errorf("code = %v, want InvalidArgument (err: %v)", status.Code(err), err)
			}
		})
	}
}

// Every failure must surface as an error rather than zeroed usage: a
// volume whose mount is gone has to disappear from the metrics, not report
// itself as empty and healthy.
func TestNodeGetVolumeStatsMapsFailuresToCodes(t *testing.T) {
	cases := map[string]struct {
		err  error
		want codes.Code
	}{
		"missing path": {fmt.Errorf("statfs: %w", fs.ErrNotExist), codes.NotFound},
		"dead mount":   {fmt.Errorf("statfs: %w", unix.ENOTCONN), codes.Internal},
		"other":        {errors.New("boom"), codes.Internal},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ns := nodeServerWithStatfs(func(string) (VolumeUsage, error) { return VolumeUsage{}, tc.err })
			resp, err := ns.NodeGetVolumeStats(context.Background(), statsRequest())
			if status.Code(err) != tc.want {
				t.Errorf("code = %v, want %v (err: %v)", status.Code(err), tc.want, err)
			}
			if resp != nil {
				t.Errorf("got a response alongside the error: %+v", resp)
			}
		})
	}
}

// A frozen FUSE daemon blocks statfs in the kernel. The RPC must give up
// instead of holding kubelet's stats collection for every other volume on
// the node.
func TestNodeGetVolumeStatsGivesUpOnAWedgedMount(t *testing.T) {
	original := volumeStatsTimeout
	volumeStatsTimeout = 100 * time.Millisecond
	defer func() { volumeStatsTimeout = original }()

	release := make(chan struct{})
	defer close(release)

	ns := nodeServerWithStatfs(func(string) (VolumeUsage, error) {
		<-release
		return VolumeUsage{}, nil
	})

	done := make(chan error, 1)
	go func() {
		_, err := ns.NodeGetVolumeStats(context.Background(), statsRequest())
		done <- err
	}()

	select {
	case err := <-done:
		if status.Code(err) != codes.DeadlineExceeded {
			t.Errorf("code = %v, want DeadlineExceeded (err: %v)", status.Code(err), err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("NodeGetVolumeStats never returned on a wedged mount")
	}
}

func TestNodeGetVolumeStatsSurvivesAPanickingStatfs(t *testing.T) {
	ns := nodeServerWithStatfs(func(string) (VolumeUsage, error) { panic("statfs exploded") })

	_, err := ns.NodeGetVolumeStats(context.Background(), statsRequest())
	if status.Code(err) != codes.Internal {
		t.Errorf("code = %v, want Internal (err: %v)", status.Code(err), err)
	}
}
