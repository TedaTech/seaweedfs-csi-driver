//go:build !linux

package mountmanager

import "errors"

// LazyUnmount is unsupported on non-Linux platforms.
func LazyUnmount(target string) error {
	return errors.New("lazy unmount is not supported on this platform")
}
