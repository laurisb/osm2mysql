package osm2mysql

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNodeWorkersBeyondDefaultBuckets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "many-blocks.pbf")
	header := appendBytesField(nil, 4, []byte("OsmSchema-V0.6"))
	header = appendBytesField(header, 5, []byte("Sort.Type_then_ID"))
	data := addTestBlob(nil, "OSMHeader", header)
	table := appendBytesField(nil, 1, nil)
	table = appendBytesField(table, 1, []byte("historic"))
	table = appendBytesField(table, 1, []byte("castle"))
	for i := 1; i <= 70; i++ {
		node := appendVarintField(nil, 1, zigzag(int64(i)))
		lat, lon := int64(i*100), int64(i*100)
		if i == 2 {
			lat = 100
		}
		node = appendVarintField(node, 8, zigzag(lat))
		node = appendVarintField(node, 9, zigzag(lon))
		if i == 70 {
			node = appendBytesField(node, 2, packedValues(1))
			node = appendBytesField(node, 3, packedValues(2))
		}
		block := appendBytesField(nil, 1, table)
		block = appendBytesField(block, 2, appendBytesField(nil, 1, node))
		data = addTestBlob(data, "OSMData", block)
	}
	way := appendVarintField(nil, 1, 100)
	way = appendBytesField(way, 2, packedValues(1))
	way = appendBytesField(way, 3, packedValues(2))
	way = appendBytesField(way, 8, packedValues(zigzag(1), zigzag(1), zigzag(1), zigzag(-2)))
	block := appendBytesField(nil, 1, table)
	data = addTestBlob(data, "OSMData", appendBytesField(block, 2, appendBytesField(nil, 3, way)))
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	p := NewParser(path, dir, NewLogger(), 70, 1<<20, 1)
	p.selections = tagSelections{{"historic", "castle"}}
	ext := p.newRefExtractor(70)
	ext.bucketWidth = 1
	if got := ext.bucketOf(69); got != 69 {
		t.Fatalf("bucketOf(69) = %d, want 69", got)
	}
	if err := p.Parse(); err != nil {
		t.Fatal(err)
	}
	var lines string
	for _, file := range p.PoiFiles() {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		lines += string(raw)
	}
	if strings.Count(lines, "\n") != 2 || !strings.Contains(lines, fmt.Sprintf("node\t%d\t", 70)) || !strings.Contains(lines, "way\t100\t") {
		t.Fatalf("unexpected output %q", lines)
	}
	if p.invalidAreas != 0 || p.incompleteAreas != 0 {
		t.Fatalf("invalid=%d incomplete=%d", p.invalidAreas, p.incompleteAreas)
	}
}
