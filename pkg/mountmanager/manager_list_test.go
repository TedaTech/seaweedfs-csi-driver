package mountmanager

import "testing"

func TestListReturnsOwnedMounts(t *testing.T) {
	m := NewManager(Config{})
	m.mounts["vol-a"] = &mountEntry{
		volumeID:      "vol-a",
		targetPath:    "/staging/a",
		cacheDir:      "/cache/a",
		localSocket:   "/sock/a",
		volumeContext: map[string]string{"collection": "c"},
		readOnly:      true,
	}

	resp := m.List()
	if len(resp.Mounts) != 1 {
		t.Fatalf("expected 1 mount, got %d", len(resp.Mounts))
	}

	got := resp.Mounts[0]
	if got.VolumeID != "vol-a" || got.TargetPath != "/staging/a" {
		t.Errorf("unexpected identity: %+v", got)
	}
	if !got.ReadOnly {
		t.Error("readOnly not carried through")
	}
	if got.VolumeContext["collection"] != "c" {
		t.Errorf("volume context not carried through: %+v", got.VolumeContext)
	}
}

// A process that has already exited is on its way out of the map; handing it
// to a restarting node plugin would have it adopt a volume about to vanish.
func TestListSkipsExitedProcesses(t *testing.T) {
	m := NewManager(Config{})
	exited := make(chan struct{})
	close(exited)

	m.mounts["vol-dead"] = &mountEntry{
		volumeID:   "vol-dead",
		targetPath: "/staging/dead",
		process:    &weedMountProcess{exited: exited, done: make(chan struct{})},
	}
	m.mounts["vol-live"] = &mountEntry{
		volumeID:   "vol-live",
		targetPath: "/staging/live",
		process:    &weedMountProcess{exited: make(chan struct{}), done: make(chan struct{})},
	}

	resp := m.List()
	if len(resp.Mounts) != 1 {
		t.Fatalf("expected only the live mount, got %d", len(resp.Mounts))
	}
	if resp.Mounts[0].VolumeID != "vol-live" {
		t.Errorf("wrong mount survived: %s", resp.Mounts[0].VolumeID)
	}
}

func TestListEmptyManager(t *testing.T) {
	if got := NewManager(Config{}).List(); len(got.Mounts) != 0 {
		t.Errorf("expected no mounts, got %d", len(got.Mounts))
	}
}
