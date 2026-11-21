//go:build !windows

package osm2mysql

import "golang.org/x/sys/unix"

// freeDiskBytes returns the bytes available to the caller on the filesystem
// that holds path.
func freeDiskBytes(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
