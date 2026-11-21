//go:build darwin

package osm2mysql

import "golang.org/x/sys/unix"

// totalRAM returns total physical RAM in bytes, or 0 if it can't be determined.
func totalRAM() uint64 {
	v, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0
	}
	return v
}
