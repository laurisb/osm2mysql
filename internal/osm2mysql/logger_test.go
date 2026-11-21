package osm2mysql

import (
	"math"
	"testing"
	"time"
)

func TestLogFormatting(t *testing.T) {
	for _, test := range []struct {
		bytes uint64
		want  string
	}{
		{0, "0 B"}, {999, "999 B"}, {1000, "1.0 KB"},
		{530017534, "530.0 MB"}, {999949999, "999.9 MB"},
		{999950000, "1.0 GB"}, {1000000000, "1.0 GB"},
		{2489300000, "2.5 GB"}, {1000000000000, "1.0 TB"},
	} {
		if got := formatSize(test.bytes); got != test.want {
			t.Errorf("formatSize(%d) = %q, want %q", test.bytes, got, test.want)
		}
	}
	numbers := []struct {
		value int64
		want  string
	}{
		{0, "0"}, {999, "999"}, {1000, "1 000"}, {55305, "55 305"},
		{-1000, "-1 000"}, {math.MinInt64, "-9 223 372 036 854 775 808"},
	}
	for _, test := range numbers {
		if got := formatNumber(test.value); got != test.want {
			t.Errorf("formatNumber(%d) = %q, want %q", test.value, got, test.want)
		}
	}
	durations := []struct {
		value time.Duration
		want  string
	}{
		{0, "0 s"},
		{49 * time.Millisecond, "0 s"},
		{50 * time.Millisecond, "0.1 s"},
		{500 * time.Millisecond, "0.5 s"},
		{949 * time.Millisecond, "0.9 s"},
		{950 * time.Millisecond, "1 s"},
		{7655 * time.Millisecond, "8 s"},
		{20386 * time.Millisecond, "20 s"},
		{59999600 * time.Microsecond, "1 m"},
		{2 * time.Minute, "2 m"},
		{24 * time.Hour, "1 d"},
		{time.Hour + 2*time.Minute + 3386*time.Millisecond, "1 h 2 m 3 s"},
		{25*time.Hour + 7*time.Minute + 7100*time.Millisecond, "1 d 1 h 7 m 7 s"},
		{-time.Second, "-1 s"},
	}
	for _, test := range durations {
		if got := formatDuration(test.value); got != test.want {
			t.Errorf("formatDuration(%v) = %q, want %q", test.value, got, test.want)
		}
	}
	now := time.Date(2026, 11, 7, 22, 42, 29, 735000000, time.UTC)
	if got := formatLogLine(now, "Pass 2: complete in 20 s"); got != "22:42:29 Pass 2: complete in 20 s" {
		t.Errorf("unexpected log line: %q", got)
	}
}
