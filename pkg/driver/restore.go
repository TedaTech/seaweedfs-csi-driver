package driver

import (
	"github.com/seaweedfs/seaweedfs-csi-driver/pkg/mountmanager"
	"github.com/seaweedfs/seaweedfs/weed/glog"
)

// restoreVolumes rebuilds the in-memory volume map from the mount service.
//
// ns.volumes is otherwise populated only by NodeStageVolume and
// NodePublishVolume, so restarting the node plugin empties it and kubelet
// never calls either again for a pod that is already running. The health
// monitor then has nothing to iterate: no probes, no recovery, and no
// seaweedfs_csi_mount_* series for volumes that are very much still mounted.
// On 2026-09-22 that left 7 of 9 live RWX mounts unmonitored after a routine
// plugin roll.
//
// The mount service is a separate DaemonSet that outlives the plugin, so it is
// the authority on what this node has mounted. Failures here are logged and
// swallowed: an unrestored volume behaves exactly as it does today, and
// refusing to start the plugin over it would be far worse.
func (ns *NodeServer) restoreVolumes() {
	client, err := mountmanager.NewClient(ns.Driver.mountEndpoint)
	if err != nil {
		glog.Warningf("volume restore: cannot reach the mount service: %v", err)
		return
	}

	listing, err := client.List()
	if err != nil {
		if err == mountmanager.ErrListUnsupported {
			glog.Infof("volume restore: mount service predates /list, starting with an empty volume map")
			return
		}
		glog.Warningf("volume restore: listing mounts failed: %v", err)
		return
	}

	restored := 0
	for _, info := range listing.Mounts {
		if ns.restoreVolume(info) {
			restored++
		}
	}
	glog.Infof("volume restore: restored %d of %d volume(s) from the mount service", restored, len(listing.Mounts))
}

// restoreVolume rebuilds one volume. It reports whether the volume was added.
func (ns *NodeServer) restoreVolume(info mountmanager.MountInfo) bool {
	if info.VolumeID == "" || info.TargetPath == "" {
		glog.Warningf("volume restore: skipping incomplete entry %+v", info)
		return false
	}

	// Only adopt a mount that is actually live. A staging path the service
	// still lists but whose FUSE session has gone is left alone: NodeStage
	// already has a self-heal path for it, and adopting it here would hand
	// the health monitor a volume with no context to re-stage from.
	if !isStagingPathLive(info.TargetPath) {
		glog.Infof("volume restore: staging path %s for volume %s is not a live mount, skipping", info.TargetPath, info.VolumeID)
		return false
	}

	unmounter, err := newUnmounter(ns.Driver, info.VolumeID)
	if err != nil {
		glog.Warningf("volume restore: building unmounter for volume %s: %v", info.VolumeID, err)
		return false
	}

	volume := ns.rebuildVolumeFromStaging(info.VolumeID, info.TargetPath)
	volume.unmounter = unmounter
	volume.volContext = cloneVolumeContext(info.VolumeContext)
	volume.readOnly = info.ReadOnly

	// Without the publish paths a restored volume looks unused, and
	// recoverVolume only withholds a teardown from a volume that has them —
	// so an empty set would make exactly these volumes tearable on a slow
	// probe, with pods still bound to them.
	device, err := getMountDevice(info.TargetPath)
	if err != nil || device == "" {
		glog.Warningf("volume restore: no device for staging path %s (%v); adopting volume %s without publish paths", info.TargetPath, err, info.VolumeID)
	} else {
		publishPaths, err := findPublishPathsByDevice(device, info.TargetPath)
		if err != nil {
			glog.Warningf("volume restore: discovering publish paths for volume %s: %v", info.VolumeID, err)
		}
		for path, readOnly := range publishPaths {
			volume.AddPublishPath(path, readOnly)
		}
		glog.Infof("volume restore: volume %s has %d publish path(s)", info.VolumeID, len(publishPaths))
	}

	ns.volumes.Store(info.VolumeID, volume)
	glog.Infof("volume restore: adopted volume %s staged at %s", info.VolumeID, info.TargetPath)
	return true
}
