//go:build !linux

package hardware

import "errors"

// statfs is only implemented on Linux, which is where Nestward runs.
func statfs(string) (uint64, uint64, error) {
	return 0, 0, errors.New("statfs: not supported on this system")
}
