package osm2mysql

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type Logger struct{}

func NewLogger() *Logger {
	return &Logger{}
}

func formatDuration(d time.Duration) string {
	if d < 0 {
		return "-" + formatDuration(-d)
	}
	if d < time.Second {
		d = d.Round(100 * time.Millisecond)
		if d == 0 {
			return "0 s"
		}
		if d < time.Second {
			return fmt.Sprintf("%.1f s", d.Seconds())
		}
	}
	d = d.Round(time.Second)
	var parts []string
	if days := d / (24 * time.Hour); days > 0 {
		parts = append(parts, fmt.Sprintf("%d d", days))
		d %= 24 * time.Hour
	}
	if hours := d / time.Hour; hours > 0 {
		parts = append(parts, fmt.Sprintf("%d h", hours))
		d %= time.Hour
	}
	if minutes := d / time.Minute; minutes > 0 {
		parts = append(parts, fmt.Sprintf("%d m", minutes))
		d %= time.Minute
	}
	if d != 0 || len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%d s", d/time.Second))
	}
	return strings.Join(parts, " ")
}

func formatNumber(n int64) string {
	s := strconv.FormatInt(n, 10)
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	var result strings.Builder
	result.WriteString(sign)
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result.WriteByte(' ')
		}
		result.WriteRune(c)
	}
	return result.String()
}

func formatSize(bytes uint64) string {
	units := []string{"B", "KB", "MB", "GB", "TB", "PB", "EB"}
	value := float64(bytes)
	unit := 0
	for value >= 1000 && unit < len(units)-1 {
		value /= 1000
		unit++
	}
	if unit == 0 {
		return formatNumber(int64(bytes)) + " B"
	}
	if value >= 999.95 && unit < len(units)-1 {
		value /= 1000
		unit++
	}
	return fmt.Sprintf("%.1f %s", value, units[unit])
}

func (l *Logger) Log(msg string) {
	fmt.Println(formatLogLine(time.Now(), msg))
}

func formatLogLine(now time.Time, msg string) string {
	return now.Format("15:04:05") + " " + msg
}
