package mountmanager

// MountRequest contains all information needed to start a weed mount process.
//
// VolumeContext and ReadOnly are not used to build the mount — MountArgs
// already carries everything weed needs. They are recorded so the manager can
// hand them back through List, letting a restarted node plugin rebuild a
// volume it can actually re-stage. Both are optional: an older mount service
// ignores them, and an older plugin simply never asks for them back.
type MountRequest struct {
	VolumeID      string            `json:"volumeId"`
	TargetPath    string            `json:"targetPath"`
	CacheDir      string            `json:"cacheDir"`
	MountArgs     []string          `json:"mountArgs"`
	LocalSocket   string            `json:"localSocket"`
	VolumeContext map[string]string `json:"volumeContext,omitempty"`
	ReadOnly      bool              `json:"readOnly,omitempty"`
}

// MountInfo describes one volume the mount service currently owns.
type MountInfo struct {
	VolumeID      string            `json:"volumeId"`
	TargetPath    string            `json:"targetPath"`
	CacheDir      string            `json:"cacheDir"`
	LocalSocket   string            `json:"localSocket"`
	VolumeContext map[string]string `json:"volumeContext,omitempty"`
	ReadOnly      bool              `json:"readOnly,omitempty"`
}

// ListResponse enumerates the mounts the service currently owns.
type ListResponse struct {
	Mounts []MountInfo `json:"mounts"`
}

// MountResponse is returned after a successful mount request.
type MountResponse struct {
	LocalSocket string `json:"localSocket"`
}

// UnmountRequest contains the information needed to stop a weed mount process.
type UnmountRequest struct {
	VolumeID string `json:"volumeId"`
}

// UnmountResponse is the response of a successful unmount request.
type UnmountResponse struct{}

// ErrorResponse is returned when the mount service encounters a failure.
type ErrorResponse struct {
	Error string `json:"error"`
}

const (
	// DefaultWeedBinary is the default executable name used to spawn weed mount processes.
	DefaultWeedBinary = "weed"
)
