//go:build !linux

package driver

import "errors"

// statfsUsage is unavailable off Linux: unix.Statfs_t has a different shape
// on every other platform and the driver only ever runs on Linux nodes.
func statfsUsage(path string) (VolumeUsage, error) {
	return VolumeUsage{}, errors.New("volume statistics are only supported on Linux")
}
