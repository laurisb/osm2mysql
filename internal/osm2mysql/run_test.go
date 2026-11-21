package osm2mysql

import (
	"bytes"
	"strings"
	"testing"
)

func runForTest(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runCLI("test", args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestHelpExitsZero(t *testing.T) {
	code, stdout, stderr := runForTest(t, "-h")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d", code, ExitOK)
	}
	for _, want := range []string{"Usage:", "-pbf", "-tag", "Exit codes:"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("help output missing %q:\n%s", want, stdout)
		}
	}
	if stderr != "" {
		t.Errorf("help wrote to stderr: %q", stderr)
	}
}

func TestVersionExitsZero(t *testing.T) {
	code, stdout, _ := runForTest(t, "-version")
	if code != ExitOK || stdout != "osm2mysql test\n" {
		t.Fatalf("version: code=%d stdout=%q", code, stdout)
	}
}

func TestUsageErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"missing flags", nil, "missing required -pbf"},
		{"missing tag", []string{"-pbf", "region.osm.pbf"}, "missing required -tag"},
		{"unexpected argument", []string{"-pbf", "region.osm.pbf", "-tag", "a=b", "extra"}, "unexpected arguments"},
		{"unknown flag", []string{"-nope"}, "flag provided but not defined"},
		{"invalid tag", []string{"-pbf", "region.osm.pbf", "-tag", "castle"}, "tag must be a non-empty name=value pair"},
		{"invalid table name", []string{"-pbf", "region.osm.pbf", "-tag", "a=b", "-table-name", "bad-name"}, "invalid table name"},
		{"invalid port", []string{"-pbf", "region.osm.pbf", "-tag", "a=b", "-db-port", "0"}, "invalid -db-port"},
		{"negative threads", []string{"-pbf", "region.osm.pbf", "-tag", "a=b", "-threads", "-1"}, "invalid -threads"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, stderr := runForTest(t, tt.args...)
			if code != ExitUsage {
				t.Fatalf("exit code = %d, want %d", code, ExitUsage)
			}
			if !strings.Contains(stderr, tt.want) {
				t.Errorf("stderr = %q, want substring %q", stderr, tt.want)
			}
			if !strings.Contains(stderr, "Run 'osm2mysql -h' for usage.") {
				t.Errorf("stderr missing usage hint: %q", stderr)
			}
		})
	}
}

func TestMissingPBFFileIsRuntimeFailure(t *testing.T) {
	code, _, stderr := runForTest(t, "-pbf", "does-not-exist.osm.pbf", "-tag", "a=b")
	if code != ExitFailure {
		t.Fatalf("exit code = %d, want %d", code, ExitFailure)
	}
	if !strings.Contains(stderr, "PBF file not found") {
		t.Errorf("stderr = %q", stderr)
	}
}
