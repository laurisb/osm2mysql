package osm2mysql

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBlockLayoutMixedAndEmptyBlocks(t *testing.T) {
	tests := []struct {
		name        string
		kinds       [][]int
		nodes, ways int
	}{
		{"mixed boundaries", [][]int{{1}, {1, 3}, {3}, {3, 4}, {4}}, 2, 3},
		{"empty between ways", [][]int{{1}, {3}, {}, {3}, {4}}, 1, 3},
		{"empty after ways", [][]int{{1}, {3}, {3}, {}, {4}}, 1, 3},
		{"empty before ways", [][]int{{}, {1}, {}, {3}, {4}}, 3, 1},
		{"only nodes", [][]int{{1}, {1}}, 2, 0},
		{"only relations", [][]int{{4}}, 0, 0},
		{"empty input", nil, 0, 0},
		{"mixed all", [][]int{{1, 3, 4}}, 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "sample.pbf")
			var data []byte
			for _, fields := range tt.kinds {
				var group []byte
				for _, field := range fields {
					group = appendBytesField(group, field, nil)
				}
				primitive := appendBytesField(nil, 2, group)
				data = addTestBlob(data, "OSMData", primitive)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			layout, err := discoverBlockLayout(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(layout.nodes) != tt.nodes || len(layout.ways) != tt.ways {
				t.Fatalf("node blocks=%d, way blocks=%d; want %d, %d", len(layout.nodes), len(layout.ways), tt.nodes, tt.ways)
			}
		})
	}
}
