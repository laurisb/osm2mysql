package osm2mysql

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestWildcardNodeValues(t *testing.T) {
	header := appendBytesField(nil, 4, []byte("OsmSchema-V0.6"))
	header = appendBytesField(header, 5, []byte("Sort.Type_then_ID"))
	data := addTestBlob(nil, "OSMHeader", header)
	table := appendBytesField(nil, 1, nil)
	for _, s := range []string{"GEOLOGICAL", "cave", "peak", " rock\t\n ", strings.Repeat("é", 201)} {
		table = appendBytesField(table, 1, []byte(s))
	}
	var group []byte
	for i, valueIndex := range []uint64{2, 3, 0, 4, 5} {
		node := appendVarintField(nil, 1, zigzag(int64(i+1)))
		node = appendVarintField(node, 8, 0)
		node = appendVarintField(node, 9, 0)
		node = appendBytesField(node, 2, packedValues(1))
		node = appendBytesField(node, 3, packedValues(valueIndex))
		group = appendBytesField(group, 1, node)
	}
	block := appendBytesField(nil, 1, table)
	data = addTestBlob(data, "OSMData", appendBytesField(block, 2, group))
	path := filepath.Join(t.TempDir(), "wildcard.pbf")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	p := NewParser(path, t.TempDir(), NewLogger(), 2, 1<<20, 1)
	p.selections = tagSelections{{"geological", "*"}, {"geological", "cave"}}
	if err := p.Parse(); err != nil {
		t.Fatal(err)
	}
	got := make(map[string]string)
	for _, file := range p.PoiFiles() {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if line == "" {
				continue
			}
			fields := strings.Split(line, "\t")
			if len(fields) != 7 || fields[0] != "node" {
				t.Fatalf("invalid row: %q", line)
			}
			key, errKey := hex.DecodeString(fields[2])
			value, errValue := hex.DecodeString(fields[3])
			if errKey != nil || errValue != nil || string(key) != "geological" {
				t.Fatalf("invalid pair: %q", line)
			}
			if _, exists := got[fields[1]]; exists {
				t.Fatalf("duplicate node: %q", line)
			}
			got[fields[1]] = string(value)
		}
	}
	want := map[string]string{"1": "cave", "2": "peak", "4": " rock\t\n ", "5": strings.Repeat("é", 200)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}
