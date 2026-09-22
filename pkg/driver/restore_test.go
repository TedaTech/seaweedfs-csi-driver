//go:build linux

package driver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seaweedfs/seaweedfs-csi-driver/pkg/mountmanager"
	"k8s.io/mount-utils"
)

func TestIsCSIPublishPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/var/lib/kubelet/pods/uid-1/volumes/kubernetes.io~csi/pvc-x/mount", true},
		// The staging mount lives under plugins/, not pods/.
		{"/var/lib/kubelet/plugins/kubernetes.io/csi/seaweedfs.csi.seaweedfs.com/abc/globalmount", false},
		// A subPath bind shares the device but is not unpublished as a unit.
		{"/var/lib/kubelet/pods/uid-1/volume-subpaths/pvc-x/ctr/0", false},
		{"/var/lib/kubelet/pods/uid-1/volumes/kubernetes.io~csi/pvc-x", false},
		{"/some/other/mount", false},
		{"", false},
	}

	for _, tc := range cases {
		if got := isCSIPublishPath(tc.path); got != tc.want {
			t.Errorf("isCSIPublishPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestFilterPublishPaths(t *testing.T) {
	staging := "/var/lib/kubelet/plugins/kubernetes.io/csi/seaweedfs.csi.seaweedfs.com/abc/globalmount"
	entries := []mountInfoEntry{
		{device: "0:55", mountpoint: staging, fstype: "fuse.seaweedfs"},
		{device: "0:55", mountpoint: "/var/lib/kubelet/pods/uid-1/volumes/kubernetes.io~csi/pvc-x/mount", fstype: "fuse.seaweedfs"},
		{device: "0:55", mountpoint: "/var/lib/kubelet/pods/uid-2/volumes/kubernetes.io~csi/pvc-x/mount", fstype: "fuse.seaweedfs", readOnly: true},
		// Same device, but a subPath bind rather than a publish target.
		{device: "0:55", mountpoint: "/var/lib/kubelet/pods/uid-1/volume-subpaths/pvc-x/ctr/0", fstype: "fuse.seaweedfs"},
		// A different volume's FUSE session.
		{device: "0:56", mountpoint: "/var/lib/kubelet/pods/uid-3/volumes/kubernetes.io~csi/pvc-y/mount", fstype: "fuse.seaweedfs"},
		// Not FUSE at all.
		{device: "0:55", mountpoint: "/var/lib/kubelet/pods/uid-4/volumes/kubernetes.io~csi/pvc-z/mount", fstype: "ext4"},
	}

	got := filterPublishPaths(entries, "0:55", staging)
	if len(got) != 2 {
		t.Fatalf("expected 2 publish paths, got %d: %v", len(got), got)
	}
	if ro, ok := got["/var/lib/kubelet/pods/uid-1/volumes/kubernetes.io~csi/pvc-x/mount"]; !ok || ro {
		t.Errorf("uid-1 bind should be present and read-write, got ok=%v ro=%v", ok, ro)
	}
	// One pod may bind read-only while another binds read-write; re-publishing
	// must not silently widen access.
	if ro, ok := got["/var/lib/kubelet/pods/uid-2/volumes/kubernetes.io~csi/pvc-x/mount"]; !ok || !ro {
		t.Errorf("uid-2 bind should be present and read-only, got ok=%v ro=%v", ok, ro)
	}
}

func TestParseMountInfoReadsReadOnlyOption(t *testing.T) {
	content := strings.Join([]string{
		"100 50 0:55 / /mnt/rw rw,relatime - fuse.seaweedfs seaweedfs rw",
		"101 50 0:55 / /mnt/ro ro,nosuid,relatime - fuse.seaweedfs seaweedfs ro",
		// "ro" must match the whole option, not a prefix of "rootcontext".
		"102 50 0:55 / /mnt/ctx rw,rootcontext=x - fuse.seaweedfs seaweedfs rw",
	}, "\n") + "\n"

	entries, err := parseMountInfoReader(strings.NewReader(content))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	for i, want := range []bool{false, true, false} {
		if entries[i].readOnly != want {
			t.Errorf("entry %d (%s): readOnly = %v, want %v", i, entries[i].mountpoint, entries[i].readOnly, want)
		}
	}
}

func newRestoreServer(t *testing.T) *NodeServer {
	t.Helper()
	return &NodeServer{
		Driver:        &SeaweedFsDriver{mountEndpoint: "unix:///tmp/does-not-need-to-exist.sock"},
		volumeMutexes: NewKeyMutex(),
		stopCh:        make(chan struct{}),
	}
}

// The whole point: a volume the mount service still owns becomes visible to
// the health monitor again, so it is probed and exports metrics.
func TestRestoreVolumeAdoptsLiveMount(t *testing.T) {
	staging := filepath.Join(t.TempDir(), "globalmount")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	orig := mountutil
	mountutil = mount.NewFakeMounter([]mount.MountPoint{{Path: staging}})
	defer func() { mountutil = orig }()

	ns := newRestoreServer(t)
	ok := ns.restoreVolume(mountmanager.MountInfo{
		VolumeID:      "vol-live",
		TargetPath:    staging,
		VolumeContext: map[string]string{"collection": "c"},
		ReadOnly:      true,
	})
	if !ok {
		t.Fatal("expected the live mount to be adopted")
	}

	stored, found := ns.volumes.Load("vol-live")
	if !found {
		t.Fatal("volume missing from the map after restore")
	}
	vol := stored.(*Volume)
	if vol.StagedPath != staging {
		t.Errorf("StagedPath = %q, want %q", vol.StagedPath, staging)
	}
	if !vol.readOnly {
		t.Error("readOnly not restored")
	}
	if vol.volContext["collection"] != "c" {
		t.Errorf("volContext not restored: %v", vol.volContext)
	}
	// Without an unmounter, recoverVolume cannot tear the mount down through
	// the manager and aborts on the still-mounted staging path.
	if vol.unmounter == nil {
		t.Error("restored volume has no unmounter")
	}
}

// A staging path the service lists but whose FUSE session is gone must not be
// adopted: there is no context to re-stage it from, and NodeStageVolume
// already self-heals that case.
func TestRestoreVolumeSkipsDeadStagingPath(t *testing.T) {
	staging := filepath.Join(t.TempDir(), "globalmount")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	orig := mountutil
	mountutil = mount.NewFakeMounter(nil) // not a mount point
	defer func() { mountutil = orig }()

	ns := newRestoreServer(t)
	if ns.restoreVolume(mountmanager.MountInfo{VolumeID: "vol-dead", TargetPath: staging}) {
		t.Fatal("expected a non-live staging path to be skipped")
	}
	if _, found := ns.volumes.Load("vol-dead"); found {
		t.Error("a dead volume was added to the map")
	}
}

func TestRestoreVolumeRejectsIncompleteEntry(t *testing.T) {
	ns := newRestoreServer(t)
	for _, info := range []mountmanager.MountInfo{
		{VolumeID: "", TargetPath: "/staging"},
		{VolumeID: "vol", TargetPath: ""},
	} {
		if ns.restoreVolume(info) {
			t.Errorf("expected %+v to be rejected", info)
		}
	}
}
