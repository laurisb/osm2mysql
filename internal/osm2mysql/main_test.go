package osm2mysql

import "testing"

func TestAutoSortMem(t *testing.T) {
	tests := []struct {
		name string
		ram  uint64
		pbf  uint64
		want int64
	}{
		{"RAM budget", 32 << 30, 10 << 30, 4 << 30},
		{"small input caps RAM budget", 32 << 30, 1 << 30, 1 << 30},
		{"RAM detection fallback", 0, 5 << 30, 2 << 30},
		{"low RAM budget", 2 << 30, 5 << 30, 256 << 20},
		{"small input caps RAM budget", 32 << 30, 256 << 20, 256 << 20},
		{"RAM maximum", 128 << 30, 100 << 30, 8 << 30},
		{"unknown input size", 0, 0, 2 << 30},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := autoSortMem(tt.ram, tt.pbf); got != tt.want {
				t.Errorf("autoSortMem(%d, %d) = %d, want %d", tt.ram, tt.pbf, got, tt.want)
			}
		})
	}
}

func TestAutoCentroidWorkers(t *testing.T) {
	tests := []struct {
		name string
		cpus int
		ram  uint64
		pbf  uint64
		want int
	}{
		{"Germany-sized input", 24, 32 << 30, 5 << 30, 3},
		{"small input", 24, 32 << 30, 1 << 30, 1},
		{"RAM limited", 24, 8 << 30, 20 << 30, 2},
		{"CPU limited", 2, 32 << 30, 20 << 30, 2},
		{"large input ceiling", 64, 128 << 30, 100 << 30, 8},
		{"RAM unavailable", 24, 0, 20 << 30, 4},
		{"CPU unavailable", 0, 0, 20 << 30, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := autoCentroidWorkers(tt.cpus, tt.ram, tt.pbf); got != tt.want {
				t.Errorf("autoCentroidWorkers(%d, %d, %d) = %d, want %d",
					tt.cpus, tt.ram, tt.pbf, got, tt.want)
			}
		})
	}
}
