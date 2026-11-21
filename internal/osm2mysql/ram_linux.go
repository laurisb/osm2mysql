//go:build linux

package osm2mysql

import "golang.org/x/sys/unix"

// totalRAM returns total physical RAM in bytes, or 0 if it can't be determined.
func totalRAM() uint64 {
	var si unix.Sysinfo_t
	if err := unix.Sysinfo(&si); err != nil {
		return 0
	}
	unit := uint64(si.Unit)
	if unit == 0 {
		unit = 1
	}
	return uint64(si.Totalram) * unit
}
