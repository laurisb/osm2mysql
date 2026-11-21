package osm2mysql

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestSelectedAreaWithNoNodeBlocks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "no-nodes.osm.pbf")
	header := appendBytesField(nil, 4, []byte("OsmSchema-V0.6"))
	header = appendBytesField(header, 5, []byte("Sort.Type_then_ID"))
	data := addTestBlob(nil, "OSMHeader", header)
	table := appendBytesField(nil, 1, nil)
	table = appendBytesField(table, 1, []byte("historic"))
	table = appendBytesField(table, 1, []byte("castle"))
	way := appendVarintField(nil, 1, math.MaxInt64)
	way = appendBytesField(way, 2, packedValues(1))
	way = appendBytesField(way, 3, packedValues(2))
	way = appendBytesField(way, 8, packedValues(zigzag(math.MaxInt64), zigzag(-1), zigzag(1)))
	block := appendBytesField(nil, 1, table)
	block = appendBytesField(block, 2, appendBytesField(nil, 3, way))
	data = addTestBlob(data, "OSMData", block)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	p := NewParser(path, dir, NewLogger(), 2, 1<<20, 2)
	p.selections = tagSelections{{"historic", "castle"}}
	if err := p.Parse(); err != nil {
		t.Fatal(err)
	}
	if p.incompleteAreas != 1 || p.invalidAreas != 0 {
		t.Fatalf("dropped areas: incomplete=%d invalid=%d", p.incompleteAreas, p.invalidAreas)
	}
	for _, file := range p.PoiFiles() {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) != 0 {
			t.Fatalf("unexpected output %q", raw)
		}
	}
}
