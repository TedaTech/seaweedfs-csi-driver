package driver

import (
	"fmt"
	"runtime/debug"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"k8s.io/mount-utils"
)

const (
	defaultHealthCheckInterval = 30 * time.Second
	// defaultHealthCheckTimeout bounds any single isHealthyFn call so a
	// frozen FUSE daemon (os.ReadDir blocked in the kernel) cannot stall
	// the health monitor goroutine indefinitely.
	defaultHealthCheckTimeout = 5 * time.Second

	// recoveryBackoffBase and recoveryBackoffMax bound how often one
	// volume may be recovered. Recovery is node-wide and destructive, so
	// repeated attempts double the wait rather than retrying every sweep.
	recoveryBackoffBase = 1 * time.Minute
	recoveryBackoffMax  = 30 * time.Minute

	// recoveryBackoffMaxShift caps the exponent so the shift below cannot
	// overflow time.Duration.
	recoveryBackoffMaxShift = 20
)

// recoveryBackoffState is the per-volume throttle recorded in
// NodeServer.recoveryBackoff.
type recoveryBackoffState struct {
	attempts  int
	notBefore time.Time
}

// corrupted reports whether the staging path's FUSE daemon is provably dead.
// A nil hook falls back to the real check — this gates a destructive action,
// so an unset field must not quietly disable it.
func (ns *NodeServer) corrupted(stagingPath string) bool {
	if ns.isCorruptedFn != nil {
		return ns.isCorruptedFn(stagingPath)
	}
	return isStagingPathCorrupted(stagingPath)
}

func (ns *NodeServer) now() time.Time {
	if ns.nowFn != nil {
		return ns.nowFn()
	}
	return time.Now()
}

// recoveryThrottled reports whether the volume is still inside the backoff
// window opened by a previous recovery attempt.
func (ns *NodeServer) recoveryThrottled(volumeID string) bool {
	v, ok := ns.recoveryBackoff.Load(volumeID)
	if !ok {
		return false
	}
	return ns.now().Before(v.(recoveryBackoffState).notBefore)
}

// noteRecoveryAttempt records an attempt and widens the backoff window
// exponentially.
func (ns *NodeServer) noteRecoveryAttempt(volumeID string) {
	attempts := 1
	if v, ok := ns.recoveryBackoff.Load(volumeID); ok {
		attempts = v.(recoveryBackoffState).attempts + 1
	}
	shift := attempts - 1
	if shift > recoveryBackoffMaxShift {
		shift = recoveryBackoffMaxShift
	}
	wait := recoveryBackoffBase << shift
	if wait > recoveryBackoffMax {
		wait = recoveryBackoffMax
	}
	ns.recoveryBackoff.Store(volumeID, recoveryBackoffState{
		attempts:  attempts,
		notBefore: ns.now().Add(wait),
	})
}

// resetRecoveryBackoff clears the throttle once a volume is observed
// healthy again.
func (ns *NodeServer) resetRecoveryBackoff(volumeID string) {
	ns.recoveryBackoff.Delete(volumeID)
}

func (ns *NodeServer) startHealthMonitor(interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		glog.Infof("health monitor started with interval %v", interval)
		for {
			select {
			case <-ticker.C:
				ns.runHealthCheckTick()
			case <-ns.stopCh:
				glog.Infof("health monitor stopped")
				return
			}
		}
	}()
}

// runHealthCheckTick runs one health check pass with panic recovery so
// that an unexpected crash inside checkAndRecoverVolumes or recoverVolume
// does not silently disable self-healing for the lifetime of the pod.
func (ns *NodeServer) runHealthCheckTick() {
	defer func() {
		if r := recover(); r != nil {
			glog.Errorf("health monitor: recovered from panic: %v\n%s", r, debug.Stack())
		}
	}()
	ns.checkAndRecoverVolumes()
}

// checkHealth runs isHealthyFn with a timeout so a hung FUSE daemon
// cannot stall the monitor sweep. Both a timeout and a panic inside
// isHealthyFn count as unhealthy, which either triggers recovery (if the
// mount really is dead) or is harmlessly retried on the next tick (if it
// was just slow).
func (ns *NodeServer) checkHealth(path string) bool {
	healthy, _ := ns.checkHealthDetailed(path)
	return healthy
}

// checkHealthDetailed additionally reports whether the probe ran out of time,
// which is a materially weaker statement than "the mount is dead" and is kept
// separate so metrics and recovery decisions can tell them apart.
func (ns *NodeServer) checkHealthDetailed(path string) (healthy bool, timedOut bool) {
	healthy, err := runWithTimeout(
		defaultHealthCheckTimeout,
		fmt.Sprintf("health monitor: health check for %s", path),
		func() (bool, error) { return ns.isHealthyFn(path), nil },
	)
	if err != nil {
		return false, true
	}
	return healthy, false
}

// probeStaging runs the staging-mount probe and records what it found. The
// returned bool is the same verdict checkHealth would give.
func (ns *NodeServer) probeStaging(volumeID, path string) bool {
	healthy, timedOut := ns.checkHealthDetailed(path)
	if healthy {
		recordHealthProbe(volumeID, probeHealthy)
		setSessionUp(volumeID, true)
		return true
	}

	if ns.corrupted(path) {
		recordHealthProbe(volumeID, probeCorrupted)
		setSessionUp(volumeID, false)
		return false
	}

	// Not corrupted: the daemon is still there as far as we can prove.
	if timedOut {
		recordHealthProbe(volumeID, probeTimeout)
	} else {
		recordHealthProbe(volumeID, probeUnmounted)
	}
	setSessionUp(volumeID, true)
	return false
}

// checkAndRecoverVolumes iterates the volume map and launches one
// background goroutine per volume to perform its health check and any
// needed recovery work. The sweep loop itself only touches the
// in-flight set — it does not block on health checks, so a single hung
// FUSE can never stall inspection of other volumes.
//
// ns.activeRecoveries deduplicates in-flight work: if a recovery for a
// given volumeID is already running (e.g. because the previous sweep's
// goroutine is blocked on a slow unmount), the new sweep skips it
// instead of spawning a second goroutine that would just pile up on
// the same mutex. This prevents goroutine exhaustion when a volume is
// stuck.
func (ns *NodeServer) checkAndRecoverVolumes() {
	ns.volumes.Range(func(key, value interface{}) bool {
		volumeID := key.(string)
		vol := value.(*Volume)

		if vol.StagedPath == "" {
			return true
		}
		ns.launchVolumeHealthCheck(volumeID)
		return true
	})
}

// launchVolumeHealthCheck spawns the per-volume health check goroutine
// if one is not already running for this volumeID. The goroutine is the
// place where slow work (checkHealth, unmount, re-mount, re-bind) runs.
// Tests synchronize via ns.recoveryWg.
func (ns *NodeServer) launchVolumeHealthCheck(volumeID string) {
	if _, loaded := ns.activeRecoveries.LoadOrStore(volumeID, struct{}{}); loaded {
		glog.V(4).Infof("health monitor: health check already in progress for volume %s, skipping", volumeID)
		return
	}

	ns.recoveryWg.Add(1)
	go func() {
		defer ns.recoveryWg.Done()
		defer ns.activeRecoveries.Delete(volumeID)
		defer func() {
			if r := recover(); r != nil {
				glog.Errorf("health monitor: health check for volume %s panicked: %v\n%s", volumeID, r, debug.Stack())
			}
		}()
		ns.performVolumeHealthCheck(volumeID)
	}()
}

// performVolumeHealthCheck runs inside the per-volume goroutine. It
// does the potentially-slow health checks off the sweep thread and
// dispatches to full recovery or publish-retry as needed.
func (ns *NodeServer) performVolumeHealthCheck(volumeID string) {
	val, ok := ns.volumes.Load(volumeID)
	if !ok {
		return
	}
	vol := val.(*Volume)
	if vol.StagedPath == "" {
		return
	}

	if !ns.probeStaging(volumeID, vol.StagedPath) {
		glog.Warningf("health monitor: detected unhealthy staging mount for volume %s at %s", volumeID, vol.StagedPath)
		ns.recoverVolume(volumeID)
		return
	}
	ns.resetRecoveryBackoff(volumeID)

	// Staging is alive; check whether any publish bind mounts have
	// been dropped (e.g. from a previous partial recovery) and need
	// to be re-bound without tearing down the FUSE mount.
	if ns.hasUnhealthyPublishPath(vol) {
		glog.Warningf("health monitor: detected unhealthy publish mount for volume %s", volumeID)
		ns.retryPublishPaths(volumeID)
	}
}

// hasUnhealthyPublishPath returns true if any of the Volume's tracked
// publish paths is not currently a live, readable mount point. It uses
// the same isHealthyFn as staging so behavior stays consistent across
// both mount levels. This runs inside the per-volume goroutine so a
// slow check for one volume does not affect others.
func (ns *NodeServer) hasUnhealthyPublishPath(vol *Volume) bool {
	unhealthy := false
	vol.publishPaths.Range(func(k, _ interface{}) bool {
		if !ns.checkHealth(k.(string)) {
			unhealthy = true
			return false
		}
		return true
	})
	return unhealthy
}

// tearDownStalePublishBind unmounts a publish bind, falling back to
// mount.CleanupMountPoint if the regular unmount fails. Returns true
// only if the path is no longer a mount point — callers must skip
// re-publishing when false, since Volume.Publish short-circuits on
// any pre-existing mount and would falsely claim success against the
// dead FUSE.
func (ns *NodeServer) tearDownStalePublishBind(path, volumeID string) bool {
	if err := ns.unmountFn(path); err == nil {
		return true
	} else {
		glog.Warningf("health monitor: unmount publish path %s for volume %s failed: %v, trying force cleanup", path, volumeID, err)
	}
	if cleanupErr := mount.CleanupMountPoint(path, mountutil, true); cleanupErr != nil {
		glog.Errorf("health monitor: force cleanup of publish path %s for volume %s also failed: %v; skipping re-publish to avoid Publish() falsely satisfying the stale mount", path, volumeID, cleanupErr)
		return false
	}
	return true
}

// retryPublishPaths re-binds publish paths whose bind mount has gone
// missing while the underlying staging FUSE mount is still alive. This
// is the second-chance path for publish failures that happened during a
// previous recoverVolume sweep.
func (ns *NodeServer) retryPublishPaths(volumeID string) {
	volumeMutex := ns.getVolumeMutex(volumeID)
	volumeMutex.Lock()
	defer volumeMutex.Unlock()

	val, ok := ns.volumes.Load(volumeID)
	if !ok {
		return
	}
	vol := val.(*Volume)

	// If staging has died since the sweep, let the next tick's
	// full-recovery path handle it instead of fighting the race here.
	if !ns.checkHealth(vol.StagedPath) {
		glog.Infof("health monitor: staging for volume %s became unhealthy before publish retry; deferring to full recovery", volumeID)
		return
	}

	vol.publishPaths.Range(func(k, v interface{}) bool {
		path := k.(string)
		readOnly := v.(bool)
		if ns.checkHealth(path) {
			return true
		}
		glog.Warningf("health monitor: re-binding publish path %s for volume %s", path, volumeID)
		// Volume.Publish short-circuits on any pre-existing mount; if we
		// cannot tear the stale bind down, calling it would falsely
		// claim success against the dead FUSE. Defer to the next sweep.
		if !ns.tearDownStalePublishBind(path, volumeID) {
			return true
		}
		if err := vol.Publish(vol.StagedPath, path, readOnly); err != nil {
			glog.Errorf("health monitor: failed to re-bind publish path %s for volume %s: %v", path, volumeID, err)
			return true
		}
		glog.Infof("health monitor: successfully re-bound publish path %s for volume %s", path, volumeID)

		remountStaleFuseInContainers(path, vol.StagedPath, readOnly)
		return true
	})
}

func (ns *NodeServer) recoverVolume(volumeID string) {
	if ns.recoveryThrottled(volumeID) {
		recordRecovery(volumeID, recoveryOutcomeThrottled)
		glog.V(4).Infof("health monitor: volume %s is within its recovery backoff window, skipping", volumeID)
		return
	}

	volumeMutex := ns.getVolumeMutex(volumeID)
	volumeMutex.Lock()
	defer volumeMutex.Unlock()

	// Re-load from map after acquiring lock (another goroutine may have replaced it)
	val, ok := ns.volumes.Load(volumeID)
	if !ok {
		glog.Infof("health monitor: volume %s no longer exists, skipping recovery", volumeID)
		return
	}
	vol := val.(*Volume)

	// Re-check health after acquiring lock
	if ns.checkHealth(vol.StagedPath) {
		glog.Infof("health monitor: volume %s is now healthy, skipping recovery", volumeID)
		ns.resetRecoveryBackoff(volumeID)
		return
	}

	if vol.volContext == nil {
		glog.Warningf("health monitor: cannot recover volume %s - no volume context available (volume was rebuilt from existing mount after CSI driver restart)", volumeID)
		return
	}

	stagingPath := vol.StagedPath

	// Capture the old FUSE mount's device identifier before cleanup.
	// After recovery the staging path will have a new device, so this
	// is the only chance to learn which device the containers' stale
	// bind mounts still reference.
	oldDevice, _ := getMountDevice(stagingPath)

	// Collect publish paths before recovery
	type publishInfo struct {
		path     string
		readOnly bool
	}
	var publishes []publishInfo
	vol.publishPaths.Range(func(k, v interface{}) bool {
		publishes = append(publishes, publishInfo{k.(string), v.(bool)})
		return true
	})

	// If staging is already unmounted, derive the device from any
	// publish bind — they target the same FUSE. Device-keyed remount
	// avoids replacing a different volume's mount when a pod has
	// multiple CSI volumes.
	if oldDevice == "" {
		for _, p := range publishes {
			if d, err := getMountDevice(p.path); err == nil && d != "" {
				oldDevice = d
				break
			}
		}
	}

	// A slow probe is not authority to destroy a live FUSE session.
	// checkHealth reports unhealthy for a mount that merely missed its
	// timeout budget, and recovery below stops the weed mount process
	// that every consumer pod on this node shares — killing their bind
	// mounts and any writes in flight. Only act when the daemon is
	// provably dead, or when nothing is published against it and the
	// blast radius is therefore empty.
	if len(publishes) > 0 && !ns.corrupted(stagingPath) {
		ns.noteRecoveryAttempt(volumeID)
		recordRecovery(volumeID, recoveryOutcomeSkippedNotCorrupted)
		glog.Warningf("health monitor: volume %s failed its health probe but the FUSE daemon at %s is not corrupted and %d publish path(s) are live; skipping recovery", volumeID, stagingPath, len(publishes))
		return
	}

	ns.noteRecoveryAttempt(volumeID)
	glog.Infof("health monitor: recovering volume %s (%d publish paths)", volumeID, len(publishes))

	// Re-stage before touching publish binds: if re-stage fails, the
	// (broken) binds stay in place rather than leaving kubelet seeing
	// empty publish paths.

	// Step 1: Manager-level unmount. cleanupStagingFn only does a host
	// unmount + RemoveAll, leaving the manager thinking the volume is
	// still mounted, so the follow-up Mount would no-op onto a dead path.
	if vol.unmounter != nil {
		if err := vol.unmounter.Unmount(); err != nil {
			// Aborting is safer than continuing: RemoveAll on a path the
			// manager still considers mounted risks deleting user data
			// through a live FUSE.
			recordRecovery(volumeID, recoveryOutcomeFailed)
			glog.Errorf("health monitor: unmount via mount manager failed for volume %s, aborting recovery: %v", volumeID, err)
			return
		}
	}

	// RemoveAll on a still-mounted FUSE would delete remote data via
	// gRPC. FUSE can still be alive here if vol.unmounter was nil
	// (rebuilt volume) or wait()'s kubeMounter.Unmount silently failed.
	if notMnt, err := mountutil.IsLikelyNotMountPoint(stagingPath); err == nil && !notMnt {
		recordRecovery(volumeID, recoveryOutcomeFailed)
		glog.Errorf("health monitor: refusing to clean up staging path %s for volume %s — still a mount point; aborting recovery to avoid data deletion", stagingPath, volumeID)
		return
	}

	// Step 2: Clean up stale staging path
	if err := ns.cleanupStagingFn(stagingPath); err != nil {
		recordRecovery(volumeID, recoveryOutcomeFailed)
		glog.Errorf("health monitor: failed to cleanup stale staging for volume %s: %v", volumeID, err)
		return
	}

	// Step 3: Re-stage with a fresh FUSE mount.
	newVol, err := ns.stageNewVolume(volumeID, stagingPath, vol.volContext, vol.readOnly)
	if err != nil {
		recordRecovery(volumeID, recoveryOutcomeFailed)
		glog.Errorf("health monitor: failed to re-stage volume %s: %v", volumeID, err)
		return
	}

	// Step 4: Tear down stale publish binds. Volume.Publish short-circuits
	// on any pre-existing mount, so track which were actually unmounted —
	// re-publishing onto a stale bind would falsely claim success against
	// the dead FUSE.
	unmounted := make(map[string]bool, len(publishes))
	for _, p := range publishes {
		glog.Infof("health monitor: unmounting stale publish path %s for volume %s", p.path, volumeID)
		unmounted[p.path] = ns.tearDownStalePublishBind(p.path, volumeID)
	}

	// Step 5: Re-bind publish paths. Track failures and successes
	// separately so container-side mounts still get fixed for paths
	// that did recover.
	var failed, recovered []publishInfo
	for _, p := range publishes {
		newVol.AddPublishPath(p.path, p.readOnly)
		if !unmounted[p.path] {
			failed = append(failed, p)
			continue
		}
		glog.Infof("health monitor: re-publishing %s for volume %s", p.path, volumeID)
		if err := newVol.Publish(stagingPath, p.path, p.readOnly); err != nil {
			glog.Errorf("health monitor: failed to re-publish %s for volume %s: %v", p.path, volumeID, err)
			failed = append(failed, p)
		} else {
			recovered = append(recovered, p)
		}
	}

	// Step 6: Replace the volume in the map
	ns.volumes.Store(volumeID, newVol)

	// Step 7: Replace stale FUSE mounts inside pod containers (rprivate
	// propagation blocks host-side changes from reaching them).
	// Device-keyed so we never touch another volume's mount in the
	// same pod. Without oldDevice there is no safe identifier and no
	// automatic retry: hasUnhealthyPublishPath only sees the (now
	// healthy) host bind, so containers stay broken until pod restart.
	if oldDevice == "" {
		glog.Errorf("health monitor: container-side remount for volume %s could not run — no device captured for the old FUSE mount; affected pods will need to be restarted manually because hasUnhealthyPublishPath only sees the (now healthy) host bind", volumeID)
	} else {
		for _, p := range recovered {
			remountInContainers(p.path, stagingPath, oldDevice, p.readOnly)
		}
	}

	if len(failed) > 0 {
		recordRecovery(volumeID, recoveryOutcomeFailed)
		glog.Warningf("health monitor: volume %s recovered with %d publish path failure(s); retryPublishPaths will retry on the next sweep", volumeID, len(failed))
		return
	}

	recordRecovery(volumeID, recoveryOutcomeSuccess)
	setSessionUp(volumeID, true)

	// Deliberately not clearing the backoff here. A recovery that reports
	// success and then fails again two minutes later is precisely the flap
	// this throttle exists to damp — 15 "successfully recovered" cycles in
	// 40 minutes on talos-d15-b62 on 2026-09-16. The throttle is cleared by
	// performVolumeHealthCheck once a sweep actually observes the staging
	// mount healthy, which for a genuinely repaired volume is the very next
	// tick.
	glog.Infof("health monitor: volume %s successfully recovered", volumeID)
}
