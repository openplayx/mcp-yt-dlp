//go:build unix

package jobs

import "syscall"

// SystemDiskFree is the space available to unprivileged users.
func SystemDiskFree(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
