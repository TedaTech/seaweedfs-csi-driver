package driver

import (
	"context"
	"errors"
	"io/fs"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/seaweedfs/seaweedfs/weed/glog"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// VolumeUsage is one filesystem's occupancy, as statfs(2) reports it.
type VolumeUsage struct {
	TotalBytes     int64
	UsedBytes      int64
	AvailableBytes int64

	TotalInodes     int64
	UsedInodes      int64
	AvailableInodes int64
}

// StatfsFn reads usage for a mounted path. It is a field on NodeServer so
// tests can substitute a fake that does not touch a real mount.
type StatfsFn func(path string) (VolumeUsage, error)

// volumeStatsTimeout bounds the statfs call. Kubelet polls this RPC once a
// minute for every volume on the node, so a wedged FUSE mount must fail
// fast rather than hold the handler and stall collection for the volumes
// behind it. A variable so tests can shorten it.
var volumeStatsTimeout = 5 * time.Second

// NodeGetVolumeStats reports usage for a published volume, which is what
// kubelet turns into the kubelet_volume_stats_* series.
//
// This is deliberately stateless: it neither consults ns.volumes nor asks
// the Kubernetes API for the volume's capacity. Everything needed is
// already in the kernel's view of the mount — a weed mount's statfs is
// scoped to the volume's own collection and reports the quota as the total
// (see statfsUsage) — and at one call per volume per node per minute, an
// API lookup here would be a needless load multiplier.
func (ns *NodeServer) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	volumeID := req.GetVolumeId()
	volumePath := req.GetVolumePath()

	glog.V(4).Infof("node get volume stats for %s at %s", volumeID, volumePath)

	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	if volumePath == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume path missing in request")
	}

	usage, err := runWithTimeout(
		volumeStatsTimeout,
		"volume stats for "+volumePath,
		func() (VolumeUsage, error) { return ns.statfsFn(volumePath) },
	)
	if err != nil {
		return nil, volumeStatsError(volumePath, err)
	}

	return &csi.NodeGetVolumeStatsResponse{
		Usage: []*csi.VolumeUsage{
			{
				Unit:      csi.VolumeUsage_BYTES,
				Total:     usage.TotalBytes,
				Used:      usage.UsedBytes,
				Available: usage.AvailableBytes,
			},
			// On a filer-backed mount the inode totals are a
			// math.MaxInt64 sentinel for "unbounded", so total and
			// available carry no information and their ratio is pinned
			// at 1. Used is real: it is the collection's file count,
			// which nothing else on the platform exposes.
			{
				Unit:      csi.VolumeUsage_INODES,
				Total:     usage.TotalInodes,
				Used:      usage.UsedInodes,
				Available: usage.AvailableInodes,
			},
		},
	}, nil
}

// volumeStatsError maps a statfs failure onto the gRPC code kubelet acts
// on. Every branch returns an error rather than zeroed usage on purpose: a
// volume whose mount is gone must drop out of the metrics entirely, not
// report itself as empty and healthy.
func volumeStatsError(volumePath string, err error) error {
	switch {
	case errors.Is(err, errCallTimedOut):
		// The FUSE daemon is alive enough to accept the call but not to
		// answer it. The health monitor owns recovery; this RPC just
		// gets out of the way.
		return status.Errorf(codes.DeadlineExceeded, "volume path %s did not respond: %v", volumePath, err)
	case errors.Is(err, fs.ErrNotExist):
		return status.Errorf(codes.NotFound, "volume path %s does not exist: %v", volumePath, err)
	case errors.Is(err, unix.ENOTCONN):
		// Transport endpoint is not connected: the weed mount process
		// died and left the mount point behind.
		return status.Errorf(codes.Internal, "volume path %s has a dead mount: %v", volumePath, err)
	default:
		return status.Errorf(codes.Internal, "failed to get statistics for volume path %s: %v", volumePath, err)
	}
}
