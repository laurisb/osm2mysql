//go:build !windows && !linux && !darwin

package osm2mysql

// totalRAM can't be determined on this platform; callers fall back to a
// conservative default sort budget.
func totalRAM() uint64 { return 0 }
