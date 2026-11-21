package osm2mysql

import (
	"strings"
	"testing"
)

func TestValidateTableName(t *testing.T) {
	for _, name := range []string{"poi", "My_POI_123", "123", strings.Repeat("x", 64)} {
		if err := validateTableName(name); err != nil {
			t.Errorf("valid table name %q rejected: %v", name, err)
		}
	}
	for _, name := range []string{"", strings.Repeat("x", 65), "foo-bar", "foo.bar", "foo bar", "foo`bar", "poi;drop", "café", "\x00"} {
		if err := validateTableName(name); err == nil {
			t.Errorf("invalid table name %q accepted", name)
		}
	}
}
