//go:build unix

package inventory

import "syscall"

func diskUsage(dir string) (total, free uint64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, false
	}
	bs := uint64(st.Bsize)
	return st.Blocks * bs, st.Bavail * bs, true
}
