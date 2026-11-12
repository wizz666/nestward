//go:build linux

package hardware

import "syscall"

func statfs(p string) (total, free uint64, err error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(p, &s); err != nil {
		return 0, 0, err
	}
	bs := uint64(s.Bsize)
	return s.Blocks * bs, s.Bavail * bs, nil
}
