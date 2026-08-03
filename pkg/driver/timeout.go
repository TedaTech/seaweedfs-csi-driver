package driver

import (
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/glog"
)

// errCallTimedOut wraps every runWithTimeout deadline miss so callers can
// distinguish "the filesystem is wedged" from a real error returned by fn.
var errCallTimedOut = errors.New("timed out")

// runWithTimeout runs fn and gives up after timeout, returning a
// errCallTimedOut-wrapped error. It exists because every call this driver
// makes into a FUSE mount can block in the kernel when the weed mount
// daemon is frozen, and a blocked syscall cannot be cancelled: the
// goroutine is deliberately leaked and exits whenever the call eventually
// returns. label names the call in log lines.
//
// A panic inside fn is recovered and surfaced as an error rather than
// taking down the driver process, and unblocks the caller immediately
// instead of making it wait out the timeout.
func runWithTimeout[T any](timeout time.Duration, label string, fn func() (T, error)) (T, error) {
	type outcome struct {
		value T
		err   error
	}

	done := make(chan outcome, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				glog.Errorf("%s panicked: %v\n%s", label, r, debug.Stack())
				// The channel is buffered (size 1) and nothing has been
				// sent on it yet, so this non-blocking send is always
				// safe.
				var zero T
				select {
				case done <- outcome{zero, fmt.Errorf("%s panicked: %v", label, r)}:
				default:
				}
			}
		}()
		value, err := fn()
		done <- outcome{value, err}
	}()

	select {
	case result := <-done:
		return result.value, result.err
	case <-time.After(timeout):
		glog.Warningf("%s timed out after %v", label, timeout)
		var zero T
		return zero, fmt.Errorf("%s %w after %v", label, errCallTimedOut, timeout)
	}
}
